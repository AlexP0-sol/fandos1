// ws.go реализует WebSocket-подписки публичных каналов MEXC Contract API v1.
//
// VERIFIED (官方文档 2024-01-31, wss://contract.mexc.com/edge):
//   - Подписка: {"method":"sub.ticker","param":{"symbol":"BTC_USDT"}}
//   - Подписка: {"method":"sub.depth","param":{"symbol":"BTC_USDT"}}
//   - Push ticker: {"channel":"push.ticker","data":{lastPrice,bid1,ask1,fundingRate,...},"symbol":"BTC_USDT"}
//   - Push depth:  {"channel":"push.depth","data":{asks,bids,version,cts},"symbol":"BTC_USDT","ts":...}
//   - Keepalive:   {"method":"ping"} → {"channel":"pong","data":1587453241453}
//   - Инт-л пинга: 10-20 с; сервер закрывает соединение через 1 минуту без пинга.
//   - Числовые поля (lastPrice, bid1, ask1, fundingRate, volume24) приходят как JSON-числа,
//     не строки. Используем json.Number для точного парсинга через decimal.FromString.
//
// TODO:VERIFY: nextSettleTime — в официальных docs ticker не указан этот полем;
//
//	поле может отсутствовать. Для ChannelFunding используем fundingRate из push.ticker.
//
// TODO:VERIFY: depth data: поле [price, ordersCount, qty] — заметим что в docs описано
//
//	"411.8 is price，10 is the order numbers of the contract, 1 is the order quantity".
//	Т.е. индекс 0=price, 1=ordersCount, 2=qty (контракты). Будем брать [0]=price, [2]=qty.
//
// TODO:VERIFY: rs.sub.ticker, rs.sub.depth, rs.error — контрольные кадры; игнорируются.
//
// Жизненный цикл: одно соединение; ctx cancel / read error → close(ch) + return.
// Reconnect — ответственность вызывающего.
// Drop-on-full буфер 1024.
package mexc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// wsPingInterval — интервал ping-сообщений.
// VERIFIED: send every 10-20s; server closes after 1 min without ping.
const wsPingInterval = 15 * time.Second

// wsPublicBufSize — буфер публичных событий (drop-on-full).
const wsPublicBufSize = 1024

// ============================================================
// Запросы подписки / ping
// ============================================================

// wsSubRequest — запрос подписки на канал.
// VERIFIED: {"method":"sub.ticker","param":{"symbol":"BTC_USDT"}}
type wsSubRequest struct {
	Method string     `json:"method"`
	Param  wsSubParam `json:"param"`
}

// wsSubParam — параметры подписки.
type wsSubParam struct {
	Symbol string `json:"symbol"`
}

// wsPingRequest — keepalive ping.
// VERIFIED: {"method":"ping"}
type wsPingRequest struct {
	Method string `json:"method"`
}

// ============================================================
// Push-сообщения
// ============================================================

// wsPushEnvelope — общая обёртка push-сообщения от MEXC Contract WS.
// VERIFIED: {"channel":"push.ticker","data":{...},"symbol":"BTC_USDT"}
//
//	также: {"channel":"push.depth","data":{...},"symbol":"BTC_USDT","ts":...}
//	также: {"channel":"pong","data":1587453241453}
//	также: {"channel":"rs.sub.ticker",...} — ack подписки
//	также: {"channel":"rs.error",...}      — ошибка
type wsPushEnvelope struct {
	Channel string          `json:"channel"`
	Symbol  string          `json:"symbol"`
	Data    json.RawMessage `json:"data"`
	TS      json.Number     `json:"ts"` // внешний ts (push.depth)
}

// wsTickerData — данные push.ticker.
// VERIFIED: все числовые поля приходят как JSON-числа (не строки).
// Поля: lastPrice, bid1, ask1, volume24, fundingRate, fairPrice, indexPrice, timestamp.
type wsTickerData struct {
	Symbol      string      `json:"symbol"`
	LastPrice   json.Number `json:"lastPrice"`
	Bid1        json.Number `json:"bid1"`
	Ask1        json.Number `json:"ask1"`
	Volume24    json.Number `json:"volume24"` // 24h объём в контрактах
	Amount24    json.Number `json:"amount24"` // 24h оборот в валюте
	FundingRate json.Number `json:"fundingRate"`
	FairPrice   json.Number `json:"fairPrice"`
	IndexPrice  json.Number `json:"indexPrice"`
	Timestamp   json.Number `json:"timestamp"` // время тикера (мс)
	// TODO:VERIFY: nextSettleTime — не задокументирован в push.ticker;
	// может присутствовать в некоторых версиях API.
	NextSettleTime json.Number `json:"nextSettleTime"`
}

// wsDepthData — данные push.depth.
// VERIFIED: {"asks":[[price,ordersCount,qty],...],"bids":[...],"version":...,"cts":...}
// Заметим: [0]=price, [1]=ordersCount (кол-во ордеров), [2]=qty в контрактах.
// TODO:VERIFY: точный порядок элементов; docs говорят [price, ordersCount, qty].
type wsDepthData struct {
	Asks    [][]json.Number `json:"asks"`
	Bids    [][]json.Number `json:"bids"`
	Version int64           `json:"version"`
	CTS     json.Number     `json:"cts"` // matching engine timestamp
}

// ============================================================
// SubscribePublic
// ============================================================

// SubscribePublic подключается к публичному WS MEXC Contract и подписывается на каналы.
//
// Маппинг каналов:
//   - ChannelTicker, ChannelBBO, ChannelFunding → sub.ticker (push.ticker несёт всё)
//   - ChannelDepth → sub.depth
//
// VERIFIED: метод sub.ticker и sub.depth, формат param.
// Публичные события буферизуются в канале wsPublicBufSize; при переполнении — drop.
// На ctx.Done() или ошибке чтения — канал закрывается.
// Reconnect — ответственность вызывающего.
func (a *Adapter) SubscribePublic(ctx context.Context, subscriptions []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	if len(subscriptions) == 0 {
		return nil, fmt.Errorf("mexc SubscribePublic: нет подписок")
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.wsBase, nil)
	if err != nil {
		return nil, fmt.Errorf("mexc SubscribePublic: dial: %w", err)
	}

	ch := make(chan exchange.PublicEvent, wsPublicBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

		// Отправляем подписки (дедупликация по method+symbol).
		if err := sendPublicSubscriptions(conn, subscriptions); err != nil {
			return
		}

		// Запускаем ping-loop.
		go wsPingLoop(ctx, conn)

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

			var env wsPushEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				continue
			}

			// Пропускаем служебные кадры.
			switch env.Channel {
			case "pong",
				"rs.sub.ticker", "rs.sub.tickers",
				"rs.sub.depth", "rs.sub.depth.full",
				"rs.error",
				"rs.unsub.ticker", "rs.unsub.depth":
				// TODO:VERIFY: полный список ack-каналов; можно расширить при необходимости.
				continue
			}

			if env.Channel == "" || env.Data == nil {
				continue
			}

			events := parseMEXCPushMessage(env, a)
			for _, ev := range events {
				select {
				case ch <- ev:
				default:
					// Drop-on-full: публичные события допускают потерю.
				}
			}
		}
	}()

	return ch, nil
}

// sendPublicSubscriptions отправляет запросы подписки на WS.
// Дедупликация: sub.ticker на символ не дублируется даже если запрошены
// ChannelTicker + ChannelBBO + ChannelFunding одновременно.
func sendPublicSubscriptions(conn *websocket.Conn, subs []exchange.PublicSubscription) error {
	type key struct{ method, symbol string }
	seen := make(map[key]bool)

	for _, s := range subs {
		sym := string(s.Symbol)
		var method string
		switch s.Channel {
		case exchange.ChannelTicker, exchange.ChannelBBO, exchange.ChannelFunding:
			// VERIFIED: sub.ticker покрывает lastPrice, bid1, ask1, fundingRate.
			method = "sub.ticker"
		case exchange.ChannelDepth:
			// VERIFIED: sub.depth — incremental depth updates.
			method = "sub.depth"
		default:
			// Неизвестный канал — пропускаем.
			continue
		}

		k := key{method, sym}
		if seen[k] {
			continue
		}
		seen[k] = true

		req := wsSubRequest{
			Method: method,
			Param:  wsSubParam{Symbol: sym},
		}
		if err := conn.WriteJSON(req); err != nil {
			return fmt.Errorf("mexc ws: sendSubscription %s %s: %w", method, sym, err)
		}
	}
	return nil
}

// wsPingLoop отправляет ping каждые wsPingInterval.
// VERIFIED: {"method":"ping"} → {"channel":"pong","data":...}
// Сервер закрывает соединение через 1 минуту без пинга; отправляем каждые 15 с.
func wsPingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.WriteJSON(wsPingRequest{Method: "ping"}); err != nil {
				return
			}
		}
	}
}

// ============================================================
// Парсинг push-сообщений
// ============================================================

// parseMEXCPushMessage разбирает push-сообщение в []exchange.PublicEvent.
func parseMEXCPushMessage(env wsPushEnvelope, a *Adapter) []exchange.PublicEvent {
	switch env.Channel {
	case "push.ticker":
		return parsePushTicker(env, a)
	case "push.depth":
		return parsePushDepth(env, a)
	}
	return nil
}

// parsePushTicker разбирает push.ticker → ChannelTicker + ChannelBBO + ChannelFunding.
//
// VERIFIED: данные числовые, парсим через json.Number → decimal.FromString.
// push.ticker несёт: lastPrice, bid1, ask1, volume24, fundingRate, timestamp, symbol.
func parsePushTicker(env wsPushEnvelope, a *Adapter) []exchange.PublicEvent {
	var d wsTickerData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil
	}

	now := a.clock()
	sym := domain.ExchangeSymbol(env.Symbol)
	if sym == "" {
		sym = domain.ExchangeSymbol(d.Symbol)
	}
	if sym == "" {
		return nil
	}

	// Timestamp тикера (мс). VERIFIED: поле timestamp в data.
	var exTS time.Time
	if d.Timestamp.String() != "" && d.Timestamp.String() != "0" {
		if ms, err := decimal.FromString(d.Timestamp.String()); err == nil {
			exTS = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}
	if exTS.IsZero() {
		exTS = now
	}

	// Парсим числовые поля. MEXC отправляет JSON-числа (не строки).
	lastPrice, err := decimalFromNumber(d.LastPrice)
	if err != nil {
		return nil
	}
	bid1, err := decimalFromNumber(d.Bid1)
	if err != nil {
		return nil
	}
	ask1, err := decimalFromNumber(d.Ask1)
	if err != nil {
		return nil
	}
	volume24, _ := decimalFromNumber(d.Volume24)
	fundingRate, _ := decimalFromNumber(d.FundingRate)

	var events []exchange.PublicEvent

	// ChannelTicker: lastPrice + volume24.
	ticker := &domain.Ticker{
		Symbol:         sym,
		LastPrice:      lastPrice,
		QuoteVolume24h: volume24,
		Timestamp:      exTS,
	}
	events = append(events, exchange.PublicEvent{
		Channel:    exchange.ChannelTicker,
		Symbol:     sym,
		Ticker:     ticker,
		ExchangeTS: exTS,
		ReceivedAt: now,
	})

	// ChannelBBO: bid1 / ask1 через OrderBookSnapshot (аналогично Binance WS).
	ob := &domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeMEXC,
		Symbol:     sym,
		Bids:       []domain.PriceLevel{{Price: bid1}},
		Asks:       []domain.PriceLevel{{Price: ask1}},
		Timestamp:  exTS,
		IsSnapshot: true,
	}
	events = append(events, exchange.PublicEvent{
		Channel:    exchange.ChannelBBO,
		Symbol:     sym,
		OrderBook:  ob,
		ExchangeTS: exTS,
		ReceivedAt: now,
	})

	// ChannelFunding: fundingRate (и nextSettleTime если присутствует).
	// TODO:VERIFY: nextSettleTime поле в push.ticker; может отсутствовать.
	var nextFunding time.Time
	if d.NextSettleTime.String() != "" && d.NextSettleTime.String() != "0" {
		if ms, err := decimal.FromString(d.NextSettleTime.String()); err == nil {
			nextFunding = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	untilFunding := time.Until(nextFunding)
	var confidence domain.ConfidenceLevel
	switch {
	case nextFunding.IsZero():
		confidence = domain.ConfidenceLow
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

// parsePushDepth разбирает push.depth → ChannelDepth.
//
// VERIFIED: {"channel":"push.depth","data":{"asks":[[price,ordersCount,qty],...],"bids":[...],"version":...,"cts":...},"symbol":"BTC_USDT","ts":...}
// TODO:VERIFY: порядок элементов в массиве — [price, ordersCount, qty].
// Берём [0]=price, [2]=qty (контракты); если длина < 3, берём [0]=price, [1]=qty.
func parsePushDepth(env wsPushEnvelope, a *Adapter) []exchange.PublicEvent {
	var d wsDepthData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil
	}

	now := a.clock()
	sym := domain.ExchangeSymbol(env.Symbol)
	if sym == "" {
		return nil
	}

	// Timestamp из внешнего ts или из cts.
	var exTS time.Time
	if env.TS.String() != "" && env.TS.String() != "0" {
		if ms, err := decimal.FromString(env.TS.String()); err == nil {
			exTS = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}
	if exTS.IsZero() && d.CTS.String() != "" && d.CTS.String() != "0" {
		if ms, err := decimal.FromString(d.CTS.String()); err == nil {
			exTS = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}
	if exTS.IsZero() {
		exTS = now
	}

	bids, err := parseDepthLevelsWS(d.Bids)
	if err != nil {
		return nil
	}
	asks, err := parseDepthLevelsWS(d.Asks)
	if err != nil {
		return nil
	}

	ob := &domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeMEXC,
		Symbol:     sym,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  exTS,
		Sequence:   d.Version,
		IsSnapshot: false, // incremental update
	}

	return []exchange.PublicEvent{
		{
			Channel:    exchange.ChannelDepth,
			Symbol:     sym,
			OrderBook:  ob,
			ExchangeTS: exTS,
			ReceivedAt: now,
		},
	}
}

// parseDepthLevelsWS разбирает depth levels из WS формата.
// TODO:VERIFY: docs говорят [price, ordersCount, qty]; берём [0]=price, [2]=qty если len>=3,
// иначе [0]=price, [1]=qty.
func parseDepthLevelsWS(raw [][]json.Number) ([]domain.PriceLevel, error) {
	levels := make([]domain.PriceLevel, 0, len(raw))
	for _, entry := range raw {
		if len(entry) < 2 {
			continue
		}
		price, err := decimal.FromString(entry[0].String())
		if err != nil {
			return nil, err
		}
		// Выбираем индекс qty: 2 если len>=3, иначе 1.
		qtyIdx := 1
		if len(entry) >= 3 {
			qtyIdx = 2
		}
		qty, err := decimal.FromString(entry[qtyIdx].String())
		if err != nil {
			return nil, err
		}
		levels = append(levels, domain.PriceLevel{Price: price, Qty: qty})
	}
	return levels, nil
}

// ============================================================
// SubscribePrivate — заглушка
// ============================================================

// SubscribePrivate — приватный WS MEXC не реализован в этой версии.
// TODO:VERIFY: приватная WS аутентификация и каналы MEXC Contract API.
func (a *Adapter) SubscribePrivate(_ context.Context, _ domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	return nil, errors.New("mexc: SubscribePrivate not implemented; use REST polling")
}

// ============================================================
// Вспомогательные функции
// ============================================================

// decimalFromNumber конвертирует json.Number в decimal.Decimal.
// Возвращает Zero при пустой строке или "0".
func decimalFromNumber(n json.Number) (decimal.Decimal, error) {
	s := n.String()
	if s == "" || s == "0" {
		return decimal.Zero, nil
	}
	return decimal.FromString(s)
}
