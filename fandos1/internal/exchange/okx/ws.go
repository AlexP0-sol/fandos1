// ws.go реализует WebSocket-подписки для OKX V5 Public WS.
//
// SubscribePublic — публичные каналы (tickers, funding-rate).
// SubscribePrivate — заглушка (ErrWSNotImplemented).
//
// Протокол OKX V5 Public WS:
//   - Подключение: wss://ws.okx.com:8443/ws/v5/public
//   - Подписка: {"op":"subscribe","args":[{"channel":"tickers","instId":"BTC-USDT-SWAP"},...]}
//   - Push-фрейм: {"arg":{"channel":"tickers","instId":"..."},"data":[{...}]}
//   - Keepalive: plain text "ping" каждые ~20s; сервер отвечает "pong".
//   - Служебные фреймы: {"event":"subscribe"/"error","arg":...,"code":"...","msg":"..."}
//
// Каналы:
//   - tickers  → ChannelTicker + ChannelBBO (bid/ask/last/vol24h) — VERIFIED (OKX V5 docs)
//   - funding-rate → ChannelFunding (fundingRate / fundingTime / nextFundingTime) — VERIFIED
//
// Маппинг каналов из PublicSubscription:
//   - ChannelTicker / ChannelBBO / ChannelMarkPrice → "tickers"
//   - ChannelFunding → "funding-rate"
//   - ChannelDepth   → "books5" (TODO: реализовать парсинг books5, сейчас подписка создаётся)
//
// Ping: текстовое "ping" каждые okxPingInterval. Ответ "pong" игнорируется.
// На ctx.Done() или ошибке чтения — канал событий закрывается и горутина завершается.
// Переподключение — ответственность вызывающего (marketdata.ConnectionManager).
// Drop-on-full: буфер publicBufSize=1024; при переполнении событие отбрасывается.
package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// okxPingInterval — интервал plain-text "ping" для OKX WS keepalive.
// VERIFIED: OKX docs рекомендуют ping каждые 20–25 секунд.
const okxPingInterval = 20 * time.Second

// okxPublicBufSize — размер буфера публичных событий.
const okxPublicBufSize = 1024

// okxDialTimeout — таймаут на установку WS-соединения.
// Используется как HandshakeTimeout в gorilla/websocket Dialer.
// Значение 5s достаточно для реального соединения с OKX и быстрого fail в тестах.
const okxDialTimeout = 5 * time.Second

// okxDialer — gorilla/websocket диалер с явным HandshakeTimeout.
// HandshakeTimeout обеспечивает быстрый фэйл при недоступном сервере
// (TCP SYN без ответа; connection refused возвращается быстрее).
var okxDialer = &websocket.Dialer{
	HandshakeTimeout: okxDialTimeout,
	Proxy:            websocket.DefaultDialer.Proxy,
}

// _unusedErrWSNotImplemented гарантирует, что ErrWSNotImplemented по-прежнему
// компилируется — sentinel объявлен в adapter.go и используется в SubscribePrivate.
var _ = ErrWSNotImplemented

// ============================================================
// Типы WS-сообщений OKX V5
// ============================================================

// okxSubscribeArg — аргумент подписки на один канал/инструмент.
// VERIFIED: {"channel":"tickers","instId":"BTC-USDT-SWAP"}
type okxSubscribeArg struct {
	Channel string `json:"channel"`
	InstId  string `json:"instId,omitempty"`
}

// okxSubscribeMsg — запрос подписки.
// VERIFIED: {"op":"subscribe","args":[...]}
type okxSubscribeMsg struct {
	Op   string            `json:"op"`
	Args []okxSubscribeArg `json:"args"`
}

// okxPushFrame — фрейм push-данных OKX V5.
// VERIFIED: {"arg":{"channel":"tickers","instId":"..."},"data":[...]}
type okxPushFrame struct {
	Arg  okxSubscribeArg `json:"arg"`
	Data json.RawMessage `json:"data"`
}

// okxEventFrame — служебный фрейм ("subscribe"/"error" и т.д.).
// VERIFIED: {"event":"subscribe","arg":{"channel":"...","instId":"..."},"connId":"..."}
// Ошибка: {"event":"error","code":"...","msg":"...","connId":"..."}
type okxEventFrame struct {
	Event  string          `json:"event"`
	Arg    okxSubscribeArg `json:"arg"`
	Code   string          `json:"code"`
	Msg    string          `json:"msg"`
	ConnId string          `json:"connId"`
}

// okxTickerData — одна запись из data[] канала "tickers" для SWAP.
// VERIFIED (OKX V5 API docs, 2026-07):
//
//	Fields: instId, last, lastSz, askPx, askSz, bidPx, bidSz,
//	        open24h, high24h, low24h, volCcy24h, vol24h,
//	        sodUtc0, sodUtc8, ts
//
// Для SWAP markPx недоступен в канале tickers; он приходит по каналу "mark-price".
// TODO:VERIFY: наличие markPx в tickers канале для SWAP контрактов.
type okxTickerData struct {
	InstId    string `json:"instId"`
	Last      string `json:"last"`      // цена последней сделки — VERIFIED
	AskPx     string `json:"askPx"`     // лучший ask — VERIFIED
	BidPx     string `json:"bidPx"`     // лучший bid — VERIFIED
	VolCcy24h string `json:"volCcy24h"` // объём в котируемой валюте за 24ч — VERIFIED
	Vol24h    string `json:"vol24h"`    // объём в базовой валюте за 24ч — VERIFIED
	MarkPx    string `json:"markPx"`    // TODO:VERIFY: присутствует ли в tickers для SWAP
	Ts        string `json:"ts"`        // timestamp в ms — VERIFIED
}

// okxFundingRateData — одна запись из data[] канала "funding-rate".
// VERIFIED (OKX V5 API docs, 2026-07):
//
//	Fields: instId, instType, fundingRate, fundingTime, nextFundingRate, nextFundingTime, ts
//
// fundingTime   — timestamp следующего funding settlement в ms — VERIFIED
// nextFundingTime — timestamp settlement после следующего в ms — VERIFIED
type okxFundingRateData struct {
	InstId          string `json:"instId"`          // VERIFIED
	InstType        string `json:"instType"`        // VERIFIED
	FundingRate     string `json:"fundingRate"`     // текущая ставка — VERIFIED
	FundingTime     string `json:"fundingTime"`     // следующий settlement ms — VERIFIED
	NextFundingRate string `json:"nextFundingRate"` // прогноз следующей ставки — VERIFIED
	NextFundingTime string `json:"nextFundingTime"` // TODO:VERIFY: присутствует ли всегда
	Ts              string `json:"ts"`              // timestamp сообщения ms — VERIFIED
}

// ============================================================
// buildOKXPublicArgs — построение args[] из []PublicSubscription
// ============================================================

// buildOKXPublicArgs строит уникальные аргументы подписки для OKX V5 WS.
// Маппинг каналов:
//   - ChannelTicker / ChannelBBO / ChannelMarkPrice → "tickers"
//   - ChannelFunding → "funding-rate"
//   - ChannelDepth   → "books5"
func buildOKXPublicArgs(subs []exchange.PublicSubscription) []okxSubscribeArg {
	seen := make(map[string]bool)
	var args []okxSubscribeArg

	addArg := func(channel, instId string) {
		key := channel + "|" + instId
		if seen[key] {
			return
		}
		seen[key] = true
		args = append(args, okxSubscribeArg{Channel: channel, InstId: instId})
	}

	for _, s := range subs {
		sym := string(s.Symbol)
		switch s.Channel {
		case exchange.ChannelTicker, exchange.ChannelBBO, exchange.ChannelMarkPrice:
			// tickers канал покрывает bid/ask/last/vol; markPx TODO:VERIFY
			addArg("tickers", sym)
		case exchange.ChannelFunding:
			addArg("funding-rate", sym)
		case exchange.ChannelDepth:
			addArg("books5", sym)
		}
	}
	return args
}

// ============================================================
// SubscribePublic
// ============================================================

// SubscribePublic подключается к OKX V5 публичному WS и подписывается на каналы.
// Реализует: tickers (→ ChannelTicker + ChannelBBO) и funding-rate (→ ChannelFunding).
// При ctx.Done() или ошибке чтения — канал закрывается, горутина завершается.
// Переподключение — ответственность вызывающего (marketdata.ConnectionManager).
func (a *Adapter) SubscribePublic(ctx context.Context, subscriptions []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	args := buildOKXPublicArgs(subscriptions)
	if len(args) == 0 {
		return nil, fmt.Errorf("okx SubscribePublic: no subscriptions")
	}

	conn, _, err := okxDialer.DialContext(ctx, a.wsBaseURL, nil)
	if err != nil {
		// Оборачиваем ошибку соединения в ErrWSNotImplemented для совместимости
		// с существующими вызывающими, которые проверяют этот sentinel.
		return nil, fmt.Errorf("%w: dial %s: %v", ErrWSNotImplemented, a.wsBaseURL, err)
	}

	ch := make(chan exchange.PublicEvent, okxPublicBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

		// Отправляем запрос подписки.
		subMsg := okxSubscribeMsg{Op: "subscribe", Args: args}
		if err := conn.WriteJSON(subMsg); err != nil {
			return
		}

		// Запускаем ping-loop (plain text "ping").
		go okxPingLoop(ctx, conn)

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

			events := parseOKXMessage(raw, a)
			for _, ev := range events {
				select {
				case ch <- ev:
				default:
					// Drop on full — публичные события допускают потерю.
				}
			}
		}
	}()

	return ch, nil
}

// ============================================================
// okxPingLoop — текстовый keepalive "ping" каждые okxPingInterval
// ============================================================

// okxPingLoop отправляет plain-text "ping" каждые okxPingInterval.
// OKX WS ожидает именно текстовый "ping", а не WebSocket Ping-frame.
// VERIFIED: OKX V5 docs: "Send "ping" to keep WS alive".
func okxPingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(okxPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
				return
			}
		}
	}
}

// ============================================================
// parseOKXMessage — маршрутизация входящих фреймов
// ============================================================

// parseOKXMessage разбирает входящий фрейм и возвращает события.
// Обрабатывает:
//   - plain "pong" → пропустить (ответ на нашу "ping")
//   - plain "ping" → пропустить (серверный ping, если случится)
//   - {"event":...} → лог/пропуск, не эмитировать
//   - {"arg":{"channel":"tickers"},"data":[...]} → ChannelTicker + ChannelBBO
//   - {"arg":{"channel":"funding-rate"},"data":[...]} → ChannelFunding
func parseOKXMessage(raw []byte, a *Adapter) []exchange.PublicEvent {
	// Plain text keepalive: "pong" или "ping".
	s := string(raw)
	if s == "pong" || s == "ping" {
		return nil
	}

	// Пробуем разобрать как служебный event-фрейм.
	var ef okxEventFrame
	if err := json.Unmarshal(raw, &ef); err == nil && ef.Event != "" {
		// {"event":"subscribe"} — подтверждение подписки; {"event":"error"} — ошибка.
		if ef.Event == "error" {
			log.Printf("okx ws error event: code=%s msg=%s", ef.Code, ef.Msg)
		}
		// Служебные фреймы не эмитируются в канал.
		return nil
	}

	// Пробуем разобрать как push-фрейм с данными.
	var pf okxPushFrame
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil
	}
	if pf.Arg.Channel == "" || pf.Data == nil {
		return nil
	}

	switch pf.Arg.Channel {
	case "tickers":
		return parseOKXTickers(pf, a)
	case "funding-rate":
		return parseOKXFundingRate(pf, a)
	default:
		// books5 и прочие каналы — TODO: добавить парсинг при необходимости.
		return nil
	}
}

// ============================================================
// parseOKXTickers — канал "tickers" → ChannelTicker + ChannelBBO
// ============================================================

// parseOKXTickers разбирает push-фрейм канала "tickers".
// Эмитирует:
//   - ChannelTicker: last/markPx/volCcy24h
//   - ChannelBBO:    bidPx/askPx
//
// VERIFIED: поля last, askPx, bidPx, volCcy24h, vol24h, ts.
// TODO:VERIFY: наличие markPx в tickers для SWAP контрактов.
func parseOKXTickers(pf okxPushFrame, a *Adapter) []exchange.PublicEvent {
	var items []okxTickerData
	if err := json.Unmarshal(pf.Data, &items); err != nil {
		return nil
	}

	now := a.clock()
	var events []exchange.PublicEvent

	for _, d := range items {
		sym := domain.ExchangeSymbol(d.InstId)

		// Парсим timestamp.
		exTS := now
		if d.Ts != "" {
			if ms, err := decimal.FromString(d.Ts); err == nil {
				exTS = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}

		lastPrice, _ := parseDecimalOrZero(d.Last)
		markPrice, _ := parseDecimalOrZero(d.MarkPx)
		volCcy24h, _ := parseDecimalOrZero(d.VolCcy24h)
		bidPx, _ := parseDecimalOrZero(d.BidPx)
		askPx, _ := parseDecimalOrZero(d.AskPx)

		// ChannelTicker — last/mark/volume.
		ticker := &domain.Ticker{
			Symbol:         sym,
			LastPrice:      lastPrice,
			MarkPrice:      markPrice,
			QuoteVolume24h: volCcy24h,
			Timestamp:      exTS,
		}
		events = append(events, exchange.PublicEvent{
			Channel:    exchange.ChannelTicker,
			Symbol:     sym,
			Ticker:     ticker,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})

		// ChannelBBO — best bid/ask.
		if !bidPx.IsZero() || !askPx.IsZero() {
			bboTicker := &domain.Ticker{
				Symbol:    sym,
				LastPrice: lastPrice,
				Timestamp: exTS,
			}
			// BBO хранится в OrderBook с одним уровнем bid и ask.
			var bids, asks []domain.PriceLevel
			if !bidPx.IsZero() {
				bids = []domain.PriceLevel{{Price: bidPx, Qty: decimal.Zero}}
			}
			if !askPx.IsZero() {
				asks = []domain.PriceLevel{{Price: askPx, Qty: decimal.Zero}}
			}
			bboBook := &domain.OrderBookSnapshot{
				Exchange:   domain.ExchangeOKX,
				Symbol:     sym,
				Bids:       bids,
				Asks:       asks,
				Timestamp:  exTS,
				IsSnapshot: true,
			}
			_ = bboTicker // используем bboBook для BBO события
			events = append(events, exchange.PublicEvent{
				Channel:    exchange.ChannelBBO,
				Symbol:     sym,
				OrderBook:  bboBook,
				ExchangeTS: exTS,
				ReceivedAt: now,
			})
		}
	}

	return events
}

// ============================================================
// parseOKXFundingRate — канал "funding-rate" → ChannelFunding
// ============================================================

// parseOKXFundingRate разбирает push-фрейм канала "funding-rate".
// VERIFIED: поля fundingRate, fundingTime (ms), nextFundingTime (ms), ts.
// nextFundingRate может быть пустым — используем fundingRate как predicted.
func parseOKXFundingRate(pf okxPushFrame, a *Adapter) []exchange.PublicEvent {
	var items []okxFundingRateData
	if err := json.Unmarshal(pf.Data, &items); err != nil {
		return nil
	}

	now := a.clock()
	var events []exchange.PublicEvent

	for _, d := range items {
		sym := domain.ExchangeSymbol(d.InstId)

		// Парсим timestamp сообщения.
		exTS := now
		if d.Ts != "" {
			if ms, err := decimal.FromString(d.Ts); err == nil {
				exTS = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}

		rate, _ := parseDecimalOrZero(d.FundingRate)

		// Predicted rate: nextFundingRate; при пустом — используем текущий.
		predicted, err := parseDecimalOrZero(d.NextFundingRate)
		if err != nil || predicted.IsZero() {
			predicted = rate
		}

		// fundingTime — следующий settlement в ms. VERIFIED.
		var nextFunding time.Time
		if d.FundingTime != "" {
			if ms, err := decimal.FromString(d.FundingTime); err == nil {
				nextFunding = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}

		// Confidence policy: используем a.clock() для тестируемости.
		untilFunding := nextFunding.Sub(now)
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
			RealizedFundingRate:  rate,
			PredictedFundingRate: predicted,
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
	}

	return events
}
