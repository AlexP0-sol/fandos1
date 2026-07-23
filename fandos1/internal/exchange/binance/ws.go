// Файл ws.go реализует WebSocket-подписки для Binance USDT-M Futures:
//   - SubscribePublic — combined streams (bookTicker + markPrice)
//   - SubscribePrivate — listenKey stream (ORDER_TRADE_UPDATE / ACCOUNT_UPDATE)
//
// Публичные потоки:
//   - <symbol>@bookTicker  → BBO (bestBid/bestAsk: b/B/a/A)
//   - <symbol>@markPrice@1s → MarkPrice + Funding (p=markPrice, r=fundingRate, T=nextFundingTime)
//
// Приватный поток:
//   - listenKey: POST /fapi/v1/listenKey (без подписи; только X-MBX-APIKEY)
//   - keepalive: PUT /fapi/v1/listenKey каждые 30 мин
//   - wss://fstream.binance.com/ws/<listenKey>
//   - События: ORDER_TRADE_UPDATE → Order/Fill, ACCOUNT_UPDATE → Position/Balance
//
// Reconnect — ответственность вызывающего (marketdata package).
// Публичный канал: drop-on-full буфер 1024.
// Приватный канал: блокирующий буфер 4096 (нельзя терять приватные события).
package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// Размеры буферов.
const (
	publicBufSize  = 1024
	privateBufSize = 4096
)

// listenKeyKeepaliveInterval — интервал обновления listenKey (30 мин).
const listenKeyKeepaliveInterval = 30 * time.Minute

// ============================================================
// SubscribePublic
// ============================================================

// wsStreamMsg — общая обёртка combined stream сообщения.
// Binance combined stream: {"stream":"<name>","data":{...}}
type wsStreamMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// wsBookTickerData — данные потока <symbol>@bookTicker.
// VERIFIED: поля b=bidPrice, B=bidQty, a=askPrice, A=askQty
type wsBookTickerData struct {
	Symbol   string `json:"s"`
	BidPrice string `json:"b"` // лучший bid
	BidQty   string `json:"B"`
	AskPrice string `json:"a"` // лучший ask
	AskQty   string `json:"A"`
	Time     int64  `json:"T"` // время транзакции
}

// wsMarkPriceData — данные потока <symbol>@markPrice.
// VERIFIED: поля p=markPrice, i=indexPrice, r=fundingRate, T=nextFundingTime
type wsMarkPriceData struct {
	Symbol          string `json:"s"`
	MarkPrice       string `json:"p"` // mark price
	IndexPrice      string `json:"i"` // index price
	EstimateSettle  string `json:"P"` // estimated settle price
	FundingRate     string `json:"r"` // funding rate
	NextFundingTime int64  `json:"T"` // следующий funding (мс)
	Time            int64  `json:"E"` // время события
}

// SubscribePublic подключается к combined stream Binance и подписывается.
// Стримы: <symbol>@bookTicker и <symbol>@markPrice@1s.
// При ctx.Done() или ошибке чтения канал закрывается.
// Переполнение буфера → drop (публичные события допускают потерю).
func (a *Adapter) SubscribePublic(ctx context.Context, subscriptions []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	// Строим имена стримов для combined stream.
	streams := buildPublicStreams(subscriptions)
	if len(streams) == 0 {
		return nil, fmt.Errorf("binance SubscribePublic: нет подписок")
	}

	// URL combined stream: wss://fstream.binance.com/stream?streams=s1/s2/...
	wsURL := a.wsBase + "/stream?streams=" + strings.Join(streams, "/")

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("binance SubscribePublic: dial: %w", err)
	}

	ch := make(chan exchange.PublicEvent, publicBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

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

			var msg wsStreamMsg
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}

			if msg.Stream == "" || msg.Data == nil {
				continue
			}

			events := parsePublicStream(msg, a)
			for _, ev := range events {
				select {
				case ch <- ev:
				default:
					// Переполнение — drop (публичные события допускают потерю).
				}
			}
		}
	}()

	return ch, nil
}

// buildPublicStreams строит имена стримов Binance из PublicSubscription.
func buildPublicStreams(subs []exchange.PublicSubscription) []string {
	seen := make(map[string]bool)
	var streams []string
	for _, s := range subs {
		sym := strings.ToLower(string(s.Symbol))
		switch s.Channel {
		case exchange.ChannelBBO, exchange.ChannelTicker:
			st := sym + "@bookTicker"
			if !seen[st] {
				seen[st] = true
				streams = append(streams, st)
			}
		case exchange.ChannelMarkPrice, exchange.ChannelFunding:
			st := sym + "@markPrice@1s"
			if !seen[st] {
				seen[st] = true
				streams = append(streams, st)
			}
		case exchange.ChannelDepth:
			st := sym + "@depth20@100ms"
			if !seen[st] {
				seen[st] = true
				streams = append(streams, st)
			}
		}
	}
	return streams
}

// parsePublicStream разбирает одно combined stream сообщение в публичные события.
func parsePublicStream(msg wsStreamMsg, a *Adapter) []exchange.PublicEvent {
	stream := msg.Stream
	now := a.clock()

	switch {
	case strings.HasSuffix(stream, "@bookTicker"):
		// BBO событие
		var d wsBookTickerData
		if err := json.Unmarshal(msg.Data, &d); err != nil {
			return nil
		}
		return parseBookTickerEvent(d, now)

	case strings.Contains(stream, "@markPrice"):
		// MarkPrice + Funding события
		var d wsMarkPriceData
		if err := json.Unmarshal(msg.Data, &d); err != nil {
			return nil
		}
		return parseMarkPriceEvent(d, now)
	}
	return nil
}

// parseBookTickerEvent строит BBO PublicEvent из bookTicker данных.
func parseBookTickerEvent(d wsBookTickerData, now time.Time) []exchange.PublicEvent {
	bidPrice, err := parseDecimalOrZero(d.BidPrice)
	if err != nil {
		return nil
	}
	askPrice, err := parseDecimalOrZero(d.AskPrice)
	if err != nil {
		return nil
	}

	exTS := now
	if d.Time > 0 {
		exTS = time.UnixMilli(d.Time).UTC()
	}

	sym := domain.ExchangeSymbol(strings.ToUpper(d.Symbol))

	// BBO через Ticker (bid/ask как LastPrice не доступен в bookTicker — используем bid как proxy)
	ticker := &domain.Ticker{
		Symbol:    sym,
		LastPrice: bidPrice, // приближение: bestBid
		Timestamp: exTS,
	}
	// Используем OrderBook для передачи точного BBO
	ob := &domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeBinance,
		Symbol:     sym,
		Bids:       []domain.PriceLevel{{Price: bidPrice}},
		Asks:       []domain.PriceLevel{{Price: askPrice}},
		Timestamp:  exTS,
		IsSnapshot: true,
	}
	_ = ticker // используем ob как основной носитель BBO

	return []exchange.PublicEvent{
		{
			Channel:    exchange.ChannelBBO,
			Symbol:     sym,
			OrderBook:  ob,
			ExchangeTS: exTS,
			ReceivedAt: now,
		},
	}
}

// parseMarkPriceEvent строит MarkPrice + Funding публичные события.
func parseMarkPriceEvent(d wsMarkPriceData, now time.Time) []exchange.PublicEvent {
	markPrice, err := parseDecimalOrZero(d.MarkPrice)
	if err != nil {
		return nil
	}
	indexPrice, err := parseDecimalOrZero(d.IndexPrice)
	if err != nil {
		return nil
	}
	fundingRate, err := parseDecimalOrZero(d.FundingRate)
	if err != nil {
		return nil
	}

	exTS := now
	if d.Time > 0 {
		exTS = time.UnixMilli(d.Time).UTC()
	}
	sym := domain.ExchangeSymbol(strings.ToUpper(d.Symbol))

	var events []exchange.PublicEvent

	// MarkPrice событие
	mpTicker := &domain.Ticker{
		Symbol:     sym,
		MarkPrice:  markPrice,
		IndexPrice: indexPrice,
		Timestamp:  exTS,
	}
	events = append(events, exchange.PublicEvent{
		Channel:    exchange.ChannelMarkPrice,
		Symbol:     sym,
		MarkPrice:  mpTicker,
		ExchangeTS: exTS,
		ReceivedAt: now,
	})

	// Funding событие
	nextFunding := time.UnixMilli(d.NextFundingTime).UTC()
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
		ExchangeSymbol:       sym,
		RealizedFundingRate:  fundingRate,
		PredictedFundingRate: fundingRate,
		RateType:             domain.FundingRatePredicted,
		NextFundingTime:      nextFunding,
		Confidence:           confidence,
		FundingPriceType:     domain.FundingPriceMark,
	}
	events = append(events, exchange.PublicEvent{
		Channel:    exchange.ChannelFunding,
		Symbol:     sym,
		Funding:    funding,
		ExchangeTS: exTS,
		ReceivedAt: now,
	})

	return events
}

// ============================================================
// SubscribePrivate
// ============================================================

// listenKeyResponse — ответ POST /fapi/v1/listenKey.
type listenKeyResponse struct {
	ListenKey string `json:"listenKey"`
}

// SubscribePrivate подключается к приватному WS через listenKey.
// listenKey создаётся через POST /fapi/v1/listenKey (только X-MBX-APIKEY, без подписи).
// Keepalive: PUT /fapi/v1/listenKey каждые 30 мин.
// Приватные события буферизуются в канале размером privateBufSize;
// при переполнении — блокируем (нельзя терять приватные события).
func (a *Adapter) SubscribePrivate(ctx context.Context, _ domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	// Шаг 1: создать listenKey
	listenKey, err := a.createListenKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("binance SubscribePrivate: createListenKey: %w", err)
	}

	// Шаг 2: подключиться к WS
	wsURL := a.wsBase + "/ws/" + listenKey
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("binance SubscribePrivate: dial: %w", err)
	}

	ch := make(chan exchange.PrivateEvent, privateBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

		// Keepalive loop в фоне
		go a.listenKeyKeepaliveLoop(ctx, listenKey)

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

			events := parsePrivateWsMessage(raw, a)
			for _, ev := range events {
				select {
				case ch <- ev:
					// Успешно доставлено.
				case <-ctx.Done():
					return
				}
				// Приватные события: блокируем, не дропаем.
			}
		}
	}()

	return ch, nil
}

// createListenKey создаёт listenKey через POST /fapi/v1/listenKey.
// VERIFIED: только X-MBX-APIKEY, без подписи (публичный POST с ключом).
func (a *Adapter) createListenKey(ctx context.Context) (string, error) {
	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    "/fapi/v1/listenKey",
		Headers: a.apiKeyHeaders(),
		Safe:    false,
	})
	if err != nil {
		return "", wrapNetErr(err)
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return "", err
	}
	var res listenKeyResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	return res.ListenKey, nil
}

// listenKeyKeepaliveLoop периодически обновляет listenKey через PUT /fapi/v1/listenKey.
func (a *Adapter) listenKeyKeepaliveLoop(ctx context.Context, listenKey string) {
	ticker := time.NewTicker(listenKeyKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// PUT /fapi/v1/listenKey — продляем срок действия ключа (игнорируем ошибку)
			_, _, _ = a.http.Do(ctx, HTTPRequest{
				Method:  http.MethodPut,
				Path:    "/fapi/v1/listenKey",
				Query:   buildQuery(map[string]string{"listenKey": listenKey}),
				Headers: a.apiKeyHeaders(),
				Safe:    true,
			})
		}
	}
}

// ============================================================
// Парсинг приватных WS-сообщений
// ============================================================

// wsPrivateEnvelope — общая обёртка приватного события Binance.
type wsPrivateEnvelope struct {
	EventType string          `json:"e"` // "ORDER_TRADE_UPDATE" / "ACCOUNT_UPDATE" / ...
	EventTime int64           `json:"E"` // время события (мс)
	Data      json.RawMessage `json:"-"`
}

// parsePrivateWsMessage маппирует сырой JSON в []exchange.PrivateEvent.
func parsePrivateWsMessage(raw []byte, a *Adapter) []exchange.PrivateEvent {
	now := a.clock()

	var env wsPrivateEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	exTS := time.UnixMilli(env.EventTime).UTC()

	switch env.EventType {
	case "ORDER_TRADE_UPDATE":
		return parseOrderTradeUpdate(raw, now, exTS)
	case "ACCOUNT_UPDATE":
		return parseAccountUpdate(raw, now, exTS)
	}
	return nil
}

// ---- ORDER_TRADE_UPDATE ----

// wsOrderTradeUpdate — событие ORDER_TRADE_UPDATE.
// VERIFIED: поля o (order object) с полями s/S/o/q/p/X/i/c/rp/L/z/n/N
type wsOrderTradeUpdate struct {
	EventType string `json:"e"`
	EventTime int64  `json:"E"`
	Order     struct {
		Symbol          string `json:"s"`  // символ
		Side            string `json:"S"`  // BUY / SELL
		Type            string `json:"o"`  // тип ордера
		Qty             string `json:"q"`  // количество
		Price           string `json:"p"`  // цена
		Status          string `json:"X"`  // статус ордера
		OrderID         int64  `json:"i"`  // exchange order id
		ClientOrderID   string `json:"c"`  // client order id
		LastFilledQty   string `json:"l"`  // объём последней сделки
		CumFilledQty    string `json:"z"`  // суммарный заполненный объём
		LastFilledPrice string `json:"L"`  // цена последней сделки
		Commission      string `json:"n"`  // комиссия
		CommissionAsset string `json:"N"`  // актив комиссии
		TradeTime       int64  `json:"T"`  // время сделки
		IsMaker         bool   `json:"m"`  // является ли maker
		ReduceOnly      bool   `json:"R"`  // reduce only
		RealizedPnL     string `json:"rp"` // реализованная прибыль
		TradeID         int64  `json:"t"`  // trade id
	} `json:"o"`
}

// parseOrderTradeUpdate разбирает ORDER_TRADE_UPDATE в Order и Fill события.
func parseOrderTradeUpdate(raw []byte, now, exTS time.Time) []exchange.PrivateEvent {
	var msg wsOrderTradeUpdate
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	o := msg.Order

	qty, err := parseDecimalOrZero(o.Qty)
	if err != nil {
		return nil
	}
	filledQty, _ := parseDecimalOrZero(o.CumFilledQty)
	avgPrice, _ := parseDecimalOrZero(o.LastFilledPrice)
	commission, _ := parseDecimalOrZero(o.Commission)

	side := parseSide(o.Side)
	status := parseOrderStatus(o.Status)
	orderMode := parseOrderMode(o.Type)

	ord := &domain.Order{
		ExchangeOrderID:   fmt.Sprintf("%d", o.OrderID),
		ClientOrderID:     domain.ClientOrderID(o.ClientOrderID),
		Symbol:            domain.ExchangeSymbol(o.Symbol),
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        o.ReduceOnly,
		RequestedQty:      qty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPrice,
		Fees:              commission,
		Status:            status,
		ExchangeTimestamp: exTS,
		AckState:          domain.AckStateAcked,
	}

	var events []exchange.PrivateEvent
	events = append(events, exchange.PrivateEvent{
		Kind:       exchange.PrivateEventOrder,
		Order:      ord,
		ExchangeTS: exTS,
		ReceivedAt: now,
	})

	// Если есть исполнение (lastFilledQty > 0) — генерируем Fill
	lastFilledQty, _ := parseDecimalOrZero(o.LastFilledQty)
	if !lastFilledQty.IsZero() {
		lastPrice, _ := parseDecimalOrZero(o.LastFilledPrice)
		tradeTS := exTS
		if o.TradeTime > 0 {
			tradeTS = time.UnixMilli(o.TradeTime).UTC()
		}
		fill := &exchange.Fill{
			ExchangeOrderID: fmt.Sprintf("%d", o.OrderID),
			ClientOrderID:   domain.ClientOrderID(o.ClientOrderID),
			Symbol:          domain.ExchangeSymbol(o.Symbol),
			Side:            side,
			BaseQty:         lastFilledQty,
			Price:           lastPrice,
			Fee:             commission,
			FeeAsset:        o.CommissionAsset,
			IsMaker:         o.IsMaker,
			Timestamp:       tradeTS,
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

// ---- ACCOUNT_UPDATE ----

// wsAccountUpdate — событие ACCOUNT_UPDATE.
// VERIFIED: поля a.B (балансы) и a.P (позиции)
type wsAccountUpdate struct {
	EventType string `json:"e"`
	EventTime int64  `json:"E"`
	Account   struct {
		Reason    string            `json:"m"` // причина обновления
		Balances  []wsBalanceEntry  `json:"B"`
		Positions []wsPositionEntry `json:"P"`
	} `json:"a"`
}

// wsBalanceEntry — баланс в ACCOUNT_UPDATE.
type wsBalanceEntry struct {
	Asset              string `json:"a"`  // актив
	WalletBalance      string `json:"wb"` // wallet balance
	CrossWalletBalance string `json:"cw"` // cross wallet balance
	BalanceChange      string `json:"bc"` // изменение за событие
}

// wsPositionEntry — позиция в ACCOUNT_UPDATE.
type wsPositionEntry struct {
	Symbol           string `json:"s"`  // символ
	PositionAmt      string `json:"pa"` // позиция (со знаком)
	EntryPrice       string `json:"ep"` // цена входа
	BreakEvenPrice   string `json:"bep"`
	AccumRealizedPnl string `json:"cr"` // accumulated realized pnl
	UnrealizedPnl    string `json:"up"` // нереализованная прибыль
	MarginType       string `json:"mt"` // cross / isolated
	IsolatedMargin   string `json:"iw"` // isolated wallet balance
	PositionSide     string `json:"ps"` // BOTH / LONG / SHORT
}

// parseAccountUpdate разбирает ACCOUNT_UPDATE в Balance и Position события.
func parseAccountUpdate(raw []byte, now, exTS time.Time) []exchange.PrivateEvent {
	var msg wsAccountUpdate
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}

	var events []exchange.PrivateEvent

	// Балансы
	for _, b := range msg.Account.Balances {
		wallet, err := parseDecimalOrZero(b.WalletBalance)
		if err != nil {
			continue
		}
		avail, _ := parseDecimalOrZero(b.CrossWalletBalance)
		bal := &domain.Balance{
			Asset:            b.Asset,
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

	// Позиции
	for _, p := range msg.Account.Positions {
		amt, err := decimal.FromString(p.PositionAmt)
		if err != nil {
			continue
		}
		entryPrice, _ := parseDecimalOrZero(p.EntryPrice)
		pnl, _ := parseDecimalOrZero(p.UnrealizedPnl)
		isolatedMargin, _ := parseDecimalOrZero(p.IsolatedMargin)

		var side domain.Side
		if amt.IsPositive() {
			side = domain.SideLong
		} else {
			side = domain.SideShort
		}
		qty := amt.Abs()

		marginMode := domain.MarginCross
		if strings.ToLower(p.MarginType) == "isolated" {
			marginMode = domain.MarginIsolated
		}

		pos := &domain.Position{
			Symbol:        domain.ExchangeSymbol(p.Symbol),
			Side:          side,
			ContractQty:   qty,
			BaseQty:       qty,
			EntryPrice:    entryPrice,
			UnrealizedPnL: pnl,
			MarginMode:    marginMode,
			Margin:        isolatedMargin,
			Updated:       exTS,
		}
		events = append(events, exchange.PrivateEvent{
			Kind:       exchange.PrivateEventPosition,
			Position:   pos,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}

	return events
}
