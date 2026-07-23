// Файл ws.go реализует WebSocket-подписки для Bybit V5:
// - SubscribePublic — публичные каналы (tickers, orderbook)
// - SubscribePrivate — приватные каналы (order, execution, position, wallet)
//
// Особенности реализации tickers:
//   - Bybit V5 tickers — DELTA-стрим: сначала приходит snapshot с полными данными,
//     затем delta-сообщения содержат только изменившиеся поля.
//   - Поддерживаем last-known-state per symbol и мержим delta поверх него.
//
// Особенности orderbook:
//   - v1 emit snapshots only (type=="snapshot"); delta-обновления TODO.
//
// Приватный WS:
//   - Auth frame: {"op":"auth","args":[apiKey, expires, signature]}
//     где signature=HMAC-SHA256(secret, "GET/realtime"+expires)
//   - TODO:VERIFY: точный формат auth payload.
//
// Ping loop: {"op":"ping"} каждые 20 секунд для поддержания соединения.
package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// pingInterval — интервал ping-сообщений для поддержания WS-соединения.
const pingInterval = 20 * time.Second

// publicBufSize — размер буфера публичных событий (drop-on-full).
const publicBufSize = 1024

// privateBufSize — размер буфера приватных событий (блокирование при переполнении).
const privateBufSize = 4096

// ============================================================
// Типы WS-сообщений Bybit V5
// ============================================================

// wsMessage — универсальное сообщение WebSocket.
type wsMessage struct {
	Topic  string          `json:"topic"`
	Type   string          `json:"type"` // "snapshot" / "delta"
	Data   json.RawMessage `json:"data"`
	Op     string          `json:"op"`
	RetMsg string          `json:"retMsg"`
	ConnId string          `json:"connId"`
	Ts     int64           `json:"ts"`
}

// wsSubscribeMsg — запрос подписки.
type wsSubscribeMsg struct {
	Op   string   `json:"op"`
	Args []string `json:"args"`
}

// wsPingMsg — ping-сообщение.
type wsPingMsg struct {
	Op string `json:"op"`
}

// ============================================================
// SubscribePublic
// ============================================================

// SubscribePublic подключается к публичному WS и подписывается на каналы.
// Публичные события отправляются в буферизованный канал; при переполнении — drop.
// На ctx.Done() или ошибке чтения — канал закрывается.
func (a *Adapter) SubscribePublic(ctx context.Context, subscriptions []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	// Строим аргументы подписки из PublicSubscription.
	args := buildPublicArgs(subscriptions)
	if len(args) == 0 {
		return nil, fmt.Errorf("bybit SubscribePublic: нет подписок")
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.wsPublicURL, nil)
	if err != nil {
		return nil, fmt.Errorf("bybit SubscribePublic: dial: %w", err)
	}

	ch := make(chan exchange.PublicEvent, publicBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

		// Отправляем подписку.
		subMsg := wsSubscribeMsg{Op: "subscribe", Args: args}
		if err := conn.WriteJSON(subMsg); err != nil {
			return
		}

		// Запускаем ping-loop.
		go publicPingLoop(ctx, conn)

		// Состояние last-known per symbol для delta-мержа tickers.
		tickerState := make(map[string]*wsTickerData)
		var tickerMu sync.Mutex

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}

			var msg wsMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}

			// Игнорируем служебные сообщения (pong, subscribe ack).
			if msg.Topic == "" {
				continue
			}

			topic := msg.Topic
			switch {
			case strings.HasPrefix(topic, "tickers."):
				// tickers — delta стрим: мержим в last-known state.
				sym := strings.TrimPrefix(topic, "tickers.")
				events := handleTickerMsg(msg, sym, tickerState, &tickerMu, a)
				for _, ev := range events {
					select {
					case ch <- ev:
					default:
						// Переполнение — drop (публичные события допускают потерю).
					}
				}

			case strings.HasPrefix(topic, "orderbook."):
				// orderbook — только snapshot (v1); delta TODO.
				if msg.Type != "snapshot" {
					continue
				}
				parts := strings.SplitN(topic, ".", 3)
				sym := ""
				if len(parts) == 3 {
					sym = parts[2]
				}
				ev, ok := handleOrderbookSnapshot(msg, domain.ExchangeSymbol(sym), a)
				if !ok {
					continue
				}
				select {
				case ch <- ev:
				default:
					// Drop on full.
				}
			}
		}
	}()

	return ch, nil
}

// buildPublicArgs строит аргументы подписки из PublicSubscription.
func buildPublicArgs(subs []exchange.PublicSubscription) []string {
	var args []string
	for _, s := range subs {
		sym := string(s.Symbol)
		switch s.Channel {
		case exchange.ChannelTicker, exchange.ChannelFunding, exchange.ChannelMarkPrice:
			// tickers-топик покрывает ticker, funding и mark price.
			args = append(args, "tickers."+sym)
		case exchange.ChannelDepth, exchange.ChannelBBO:
			// orderbook.50.<symbol> для depth/BBO.
			args = append(args, "orderbook.50."+sym)
		}
	}
	// Убираем дубликаты.
	seen := make(map[string]bool)
	unique := args[:0]
	for _, a := range args {
		if !seen[a] {
			seen[a] = true
			unique = append(unique, a)
		}
	}
	return unique
}

// publicPingLoop отправляет ping каждые pingInterval.
func publicPingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.WriteJSON(wsPingMsg{Op: "ping"}); err != nil {
				return
			}
		}
	}
}

// ============================================================
// tickers delta-merge
// ============================================================

// wsTickerData — last-known состояние тикера (для delta-мержа).
type wsTickerData struct {
	Symbol          string
	Bid1Price       string
	Ask1Price       string
	LastPrice       string
	MarkPrice       string
	IndexPrice      string
	Turnover24h     string
	FundingRate     string
	NextFundingTime string
}

// handleTickerMsg обрабатывает сообщение тикера (snapshot или delta).
// Возвращает одно или несколько событий (Ticker + Funding + MarkPrice).
func handleTickerMsg(msg wsMessage, sym string, state map[string]*wsTickerData, mu *sync.Mutex, a *Adapter) []exchange.PublicEvent {
	var partial wsTickerData
	if err := json.Unmarshal(msg.Data, &partial); err != nil {
		return nil
	}

	mu.Lock()
	cur, ok := state[sym]
	if !ok || msg.Type == "snapshot" {
		// Snapshot — полностью заменяем состояние.
		state[sym] = &partial
		cur = &partial
	} else {
		// Delta — мержим непустые поля поверх текущего состояния.
		mergeTicker(cur, &partial)
	}
	// Копируем для безопасной передачи из-под мютекса.
	snapshot := *cur
	mu.Unlock()

	now := a.clock()
	exTS := time.UnixMilli(msg.Ts).UTC()
	domSym := domain.ExchangeSymbol(sym)

	var events []exchange.PublicEvent

	// Ticker-событие (bid/ask/last/volume).
	if snapshot.Bid1Price != "" || snapshot.LastPrice != "" {
		lastPrice, _ := parseDecimalOrZero(snapshot.LastPrice)
		markPrice, _ := parseDecimalOrZero(snapshot.MarkPrice)
		indexPrice, _ := parseDecimalOrZero(snapshot.IndexPrice)
		volume, _ := parseDecimalOrZero(snapshot.Turnover24h)
		ticker := &domain.Ticker{
			Symbol:         domSym,
			LastPrice:      lastPrice,
			MarkPrice:      markPrice,
			IndexPrice:     indexPrice,
			QuoteVolume24h: volume,
			Timestamp:      exTS,
		}
		events = append(events, exchange.PublicEvent{
			Channel:    exchange.ChannelTicker,
			Symbol:     domSym,
			Ticker:     ticker,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}

	// Funding-событие.
	if snapshot.FundingRate != "" || snapshot.NextFundingTime != "" {
		rate, _ := parseDecimalOrZero(snapshot.FundingRate)
		var nextFunding time.Time
		if snapshot.NextFundingTime != "" {
			ms, err := decimal.FromString(snapshot.NextFundingTime)
			if err == nil {
				nextFunding = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}
		untilFunding := time.Until(nextFunding)
		var confidence domain.ConfidenceLevel
		switch {
		case untilFunding < 30*time.Minute:
			confidence = domain.ConfidenceHigh
		case untilFunding < 4*time.Hour:
			confidence = domain.ConfidenceMedium
		default:
			confidence = domain.ConfidenceLow
		}

		funding := &domain.FundingInfo{
			ExchangeSymbol:       domSym,
			RealizedFundingRate:  rate,
			PredictedFundingRate: rate,
			RateType:             domain.FundingRatePredicted,
			NextFundingTime:      nextFunding,
			Confidence:           confidence,
			FundingPriceType:     domain.FundingPriceMark,
		}
		events = append(events, exchange.PublicEvent{
			Channel:    exchange.ChannelFunding,
			Symbol:     domSym,
			Funding:    funding,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}

	// MarkPrice-событие.
	if snapshot.MarkPrice != "" {
		mp, _ := parseDecimalOrZero(snapshot.MarkPrice)
		markTicker := &domain.Ticker{
			Symbol:    domSym,
			MarkPrice: mp,
			Timestamp: exTS,
		}
		events = append(events, exchange.PublicEvent{
			Channel:    exchange.ChannelMarkPrice,
			Symbol:     domSym,
			MarkPrice:  markTicker,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}

	return events
}

// mergeTicker мержит непустые поля src поверх dst (delta semantics).
func mergeTicker(dst, src *wsTickerData) {
	if src.Bid1Price != "" {
		dst.Bid1Price = src.Bid1Price
	}
	if src.Ask1Price != "" {
		dst.Ask1Price = src.Ask1Price
	}
	if src.LastPrice != "" {
		dst.LastPrice = src.LastPrice
	}
	if src.MarkPrice != "" {
		dst.MarkPrice = src.MarkPrice
	}
	if src.IndexPrice != "" {
		dst.IndexPrice = src.IndexPrice
	}
	if src.Turnover24h != "" {
		dst.Turnover24h = src.Turnover24h
	}
	if src.FundingRate != "" {
		dst.FundingRate = src.FundingRate
	}
	if src.NextFundingTime != "" {
		dst.NextFundingTime = src.NextFundingTime
	}
}

// ============================================================
// orderbook snapshot
// ============================================================

// wsOrderbookData — данные orderbook из WS.
type wsOrderbookData struct {
	Symbol string     `json:"s"`
	Bids   [][]string `json:"b"`
	Asks   [][]string `json:"a"`
	Ts     int64      `json:"ts"`
	Seq    int64      `json:"seq"`
}

// handleOrderbookSnapshot обрабатывает snapshot стакана.
func handleOrderbookSnapshot(msg wsMessage, sym domain.ExchangeSymbol, a *Adapter) (exchange.PublicEvent, bool) {
	var data wsOrderbookData
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		return exchange.PublicEvent{}, false
	}

	bids, err := parsePriceLevels(data.Bids)
	if err != nil {
		return exchange.PublicEvent{}, false
	}
	asks, err := parsePriceLevels(data.Asks)
	if err != nil {
		return exchange.PublicEvent{}, false
	}

	now := a.clock()
	ob := &domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeBybit,
		Symbol:     sym,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  time.UnixMilli(data.Ts).UTC(),
		Sequence:   data.Seq,
		IsSnapshot: true,
	}

	return exchange.PublicEvent{
		Channel:    exchange.ChannelDepth,
		Symbol:     sym,
		OrderBook:  ob,
		ExchangeTS: time.UnixMilli(data.Ts).UTC(),
		ReceivedAt: now,
	}, true
}

// ============================================================
// SubscribePrivate
// ============================================================

// wsAuthMsg — фрейм аутентификации для приватного WS.
// TODO:VERIFY: точный формат expires и payload.
// Документация Bybit V5: signature = HMAC-SHA256(secret, "GET/realtime"+expires)
type wsAuthMsg struct {
	Op   string        `json:"op"`
	Args []interface{} `json:"args"`
}

// SubscribePrivate подключается к приватному WS, авторизуется и подписывается.
// Приватные события буферизуются в канале размером privateBufSize;
// переполнение блокирует (приватные события нельзя терять).
func (a *Adapter) SubscribePrivate(ctx context.Context, _ domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.wsPrivateURL, nil)
	if err != nil {
		return nil, fmt.Errorf("bybit SubscribePrivate: dial: %w", err)
	}

	// Аутентификация на WS.
	// TODO:VERIFY: expires = now + 10s (в миллисекундах), payload = "GET/realtime" + expires.
	expires := a.nowMs() + 10_000
	expiresStr := strconv.FormatInt(expires, 10)
	// payload для подписи: "GET/realtime" + expires
	payload := "GET/realtime" + expiresStr
	sig := a.signer.SignRaw(payload)

	authMsg := wsAuthMsg{
		Op:   "auth",
		Args: []interface{}{a.signer.APIKey(), expires, sig},
	}
	if err := conn.WriteJSON(authMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("bybit SubscribePrivate: auth send: %w", err)
	}

	ch := make(chan exchange.PrivateEvent, privateBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

		// Ждём auth-ответ.
		if !waitAuthResponse(conn) {
			return
		}

		// Подписываемся на топики.
		subMsg := wsSubscribeMsg{
			Op:   "subscribe",
			Args: []string{"order", "execution", "position", "wallet"},
		}
		if err := conn.WriteJSON(subMsg); err != nil {
			return
		}

		// Ping-loop для приватного WS.
		go publicPingLoop(ctx, conn)

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}

			var msg wsMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}

			if msg.Topic == "" {
				continue
			}

			events := parsePrivateMessage(msg, a)
			for _, ev := range events {
				select {
				case ch <- ev:
					// Успешно отправлено.
				case <-ctx.Done():
					return
				}
				// Приватные события: блокируем, не дропаем.
			}
		}
	}()

	return ch, nil
}

// waitAuthResponse ждёт auth-ответ от сервера.
func waitAuthResponse(conn *websocket.Conn) bool {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return false
		}
		var msg wsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Op == "auth" {
			return msg.RetMsg == "OK"
		}
	}
}

// ============================================================
// Парсинг приватных сообщений
// ============================================================

// parsePrivateMessage маппирует приватное WS-сообщение в []exchange.PrivateEvent.
func parsePrivateMessage(msg wsMessage, a *Adapter) []exchange.PrivateEvent {
	now := a.clock()
	exTS := time.UnixMilli(msg.Ts).UTC()

	switch msg.Topic {
	case "order":
		return parseOrderEvents(msg.Data, now, exTS)
	case "execution":
		return parseFillEvents(msg.Data, now, exTS)
	case "position":
		return parsePositionEvents(msg.Data, now, exTS)
	case "wallet":
		return parseWalletEvents(msg.Data, now, exTS)
	}
	return nil
}

// ---- order events ----

// wsPrivateOrder — приватное событие ордера.
type wsPrivateOrder struct {
	OrderId     string `json:"orderId"`
	OrderLinkId string `json:"orderLinkId"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	OrderType   string `json:"orderType"`
	Qty         string `json:"qty"`
	CumExecQty  string `json:"cumExecQty"`
	AvgPrice    string `json:"avgPrice"`
	CumExecFee  string `json:"cumExecFee"`
	OrderStatus string `json:"orderStatus"`
	ReduceOnly  bool   `json:"reduceOnly"`
	CreatedTime string `json:"createdTime"`
}

func parseOrderEvents(data json.RawMessage, now, exTS time.Time) []exchange.PrivateEvent {
	var orders []wsPrivateOrder
	if err := json.Unmarshal(data, &orders); err != nil {
		return nil
	}
	var events []exchange.PrivateEvent
	for _, o := range orders {
		ord, err := parseOrder(orderEntry{
			OrderId:     o.OrderId,
			OrderLinkId: o.OrderLinkId,
			Symbol:      o.Symbol,
			Side:        o.Side,
			OrderType:   o.OrderType,
			Qty:         o.Qty,
			CumExecQty:  o.CumExecQty,
			AvgPrice:    o.AvgPrice,
			CumExecFee:  o.CumExecFee,
			OrderStatus: o.OrderStatus,
			ReduceOnly:  o.ReduceOnly,
			CreatedTime: o.CreatedTime,
		})
		if err != nil {
			continue
		}
		events = append(events, exchange.PrivateEvent{
			Kind:       exchange.PrivateEventOrder,
			Order:      &ord,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}
	return events
}

// ---- execution (fill) events ----

// wsExecution — приватное событие исполнения ордера.
type wsExecution struct {
	OrderId     string `json:"orderId"`
	OrderLinkId string `json:"orderLinkId"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	ExecQty     string `json:"execQty"`
	ExecPrice   string `json:"execPrice"`
	ExecFee     string `json:"execFee"`
	FeeRate     string `json:"feeRate"`
	IsMaker     bool   `json:"isMaker"`
	ExecTime    string `json:"execTime"`
}

func parseFillEvents(data json.RawMessage, now, exTS time.Time) []exchange.PrivateEvent {
	var execs []wsExecution
	if err := json.Unmarshal(data, &execs); err != nil {
		return nil
	}
	var events []exchange.PrivateEvent
	for _, e := range execs {
		var side domain.Side
		if e.Side == "Buy" {
			side = domain.SideLong
		} else {
			side = domain.SideShort
		}
		qty, _ := parseDecimalOrZero(e.ExecQty)
		price, _ := parseDecimalOrZero(e.ExecPrice)
		fee, _ := parseDecimalOrZero(e.ExecFee)

		ts := exTS
		if e.ExecTime != "" {
			ms, err := decimal.FromString(e.ExecTime)
			if err == nil {
				ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}

		fill := &exchange.Fill{
			ExchangeOrderID: e.OrderId,
			ClientOrderID:   domain.ClientOrderID(e.OrderLinkId),
			Symbol:          domain.ExchangeSymbol(e.Symbol),
			Side:            side,
			BaseQty:         qty,
			Price:           price,
			Fee:             fee,
			FeeAsset:        "USDT",
			IsMaker:         e.IsMaker,
			Timestamp:       ts,
		}
		events = append(events, exchange.PrivateEvent{
			Kind:       exchange.PrivateEventFill,
			Fill:       fill,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}
	return events
}

// ---- position events ----

// wsPositionEvent — приватное событие позиции.
type wsPositionEvent struct {
	Symbol           string `json:"symbol"`
	Side             string `json:"side"`
	Size             string `json:"size"`
	EntryPrice       string `json:"entryPrice"`
	MarkPrice        string `json:"markPrice"`
	LiqPrice         string `json:"liqPrice"`
	UnrealisedPnl    string `json:"unrealisedPnl"`
	TradeMode        int    `json:"tradeMode"`
	Leverage         string `json:"leverage"`
	PositionIM       string `json:"positionIM"`
	AdlRankIndicator int    `json:"adlRankIndicator"`
	UpdatedTime      string `json:"updatedTime"`
}

func parsePositionEvents(data json.RawMessage, now, exTS time.Time) []exchange.PrivateEvent {
	var positions []wsPositionEvent
	if err := json.Unmarshal(data, &positions); err != nil {
		return nil
	}
	var events []exchange.PrivateEvent
	for _, p := range positions {
		pos, err := parsePosition(positionEntry{
			Symbol:           p.Symbol,
			Side:             p.Side,
			Size:             p.Size,
			EntryPrice:       p.EntryPrice,
			MarkPrice:        p.MarkPrice,
			LiqPrice:         p.LiqPrice,
			UnrealisedPnl:    p.UnrealisedPnl,
			TradeMode:        p.TradeMode,
			Leverage:         p.Leverage,
			PositionIM:       p.PositionIM,
			AdlRankIndicator: p.AdlRankIndicator,
			UpdatedTime:      p.UpdatedTime,
		})
		if err != nil {
			continue
		}
		events = append(events, exchange.PrivateEvent{
			Kind:       exchange.PrivateEventPosition,
			Position:   &pos,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}
	return events
}

// ---- wallet events ----

// wsWalletEvent — приватное событие кошелька.
type wsWalletEvent struct {
	AccountType string         `json:"accountType"`
	Coin        []wsWalletCoin `json:"coin"`
}

type wsWalletCoin struct {
	Coin             string `json:"coin"`
	WalletBalance    string `json:"walletBalance"`
	AvailableBalance string `json:"availableBalance"`
}

func parseWalletEvents(data json.RawMessage, now, exTS time.Time) []exchange.PrivateEvent {
	var wallets []wsWalletEvent
	if err := json.Unmarshal(data, &wallets); err != nil {
		return nil
	}
	var events []exchange.PrivateEvent
	for _, w := range wallets {
		for _, c := range w.Coin {
			wallet, _ := parseDecimalOrZero(c.WalletBalance)
			avail, _ := parseDecimalOrZero(c.AvailableBalance)
			bal := &domain.Balance{
				Asset:            c.Coin,
				WalletBalance:    wallet,
				AvailableBalance: avail,
			}
			events = append(events, exchange.PrivateEvent{
				Kind:       exchange.PrivateEventBalance,
				Balance:    bal,
				ExchangeTS: exTS,
				ReceivedAt: now,
			})
		}
	}
	return events
}
