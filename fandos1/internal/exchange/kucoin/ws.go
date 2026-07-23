// Package kucoin — WebSocket-реализация для KuCoin Futures.
//
// Поток подключения (bullet-token flow):
//
//  1. POST /api/v1/bullet-public (публичный, без авторизации) через адаптерный HTTPDoer.
//     Ответ содержит token и instanceServers[0].{endpoint, pingInterval}.
//  2. Подключение: endpoint + "?token=" + token + "&connectId=" + uuid.
//     Сервер присылает {"type":"welcome"} — фрейм пропускается.
//  3. Подписка: {"id":"<uuid>","type":"subscribe","topic":"...","response":true}.
//     Тема тикер/BBO: /contractMarket/tickerV2:SYMBOL.
//     Тема инструмента (funding/mark price): /contract/instrument:SYMBOL.
//  4. Входящие {"type":"message","topic":"...","data":{...}}:
//     - /contractMarket/tickerV2:SYMBOL  → ChannelBBO (domain.OrderBookSnapshot с bid/ask)
//     - /contractMarket/ticker:SYMBOL    → ChannelTicker (domain.Ticker с lastPrice + BBO)
//     - /contract/instrument:SYMBOL (subject="funding.rate")      → ChannelFunding
//     - /contract/instrument:SYMBOL (subject="mark.index.price")  → ChannelMarkPrice
//  5. Ping: {"id":"<uuid>","type":"ping"} каждые pingInterval мс (из bullet-ответа, дефолт 18 с).
//     Pong: {"type":"pong"} — пропускается.
//
// Жизненный цикл:
//   - Одно соединение на вызов SubscribePublic.
//   - ctx.Done() или ошибка чтения → закрытие канала, возврат горутины.
//   - Переподключение — ответственность вызывающего.
//   - Переполнение буфера → drop (публичные события допускают потерю).
//
// VERIFIED: bullet-public endpoint, структура ответа, формат сообщений tickerV2 и
// /contract/instrument — по официальной документации KuCoin Futures (2026-07).
//
// ts в tickerV2/ticker приходит в наносекундах → делим на 1_000_000 для мс.
// TODO:VERIFY: ts в tickerV1 — документация показывает наносекунды, но это стоит
// проверить при живом подключении.
//
// SubscribePrivate оставлен заглушкой: приватный bullet требует авторизации (TODO).
package kucoin

import (
	"bytes"
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

// wsPublicBufSize — размер буфера публичных WS-событий (drop-on-full).
const wsPublicBufSize = 1024

// wsDefaultPingInterval — дефолтный интервал ping, если bullet не вернул pingInterval.
const wsDefaultPingInterval = 18 * time.Second

// ============================================================
// bullet-token types
// ============================================================

// bulletResp — структура data ответа POST /api/v1/bullet-public.
// VERIFIED: {"token":"...","instanceServers":[{"endpoint":"wss://...","pingInterval":18000,...}]}
type bulletResp struct {
	Token           string         `json:"token"`
	InstanceServers []bulletServer `json:"instanceServers"`
}

// bulletServer — один сервер из bullet-ответа.
type bulletServer struct {
	Endpoint     string `json:"endpoint"`
	PingInterval int    `json:"pingInterval"` // мс
}

// fetchBulletPublic выполняет POST /api/v1/bullet-public через адаптерный HTTPDoer.
// Возвращает token и первый подходящий endpoint + pingInterval.
// VERIFIED: endpoint без аутентификации, путь /api/v1/bullet-public.
func (a *Adapter) fetchBulletPublic(ctx context.Context) (token, endpoint string, pingInterval time.Duration, err error) {
	_, body, err := a.http.Do(ctx, HTTPRequest{
		Method: http.MethodPost,
		Path:   "/api/v1/bullet-public",
		Body:   bytes.NewReader([]byte{}),
		Safe:   false,
	})
	if err != nil {
		return "", "", 0, fmt.Errorf("kucoin bullet-public: %w", wrapNetErr(err))
	}

	env, err := decodeEnvelope(body)
	if err != nil {
		return "", "", 0, fmt.Errorf("kucoin bullet-public: envelope: %w", err)
	}

	var br bulletResp
	if err := json.Unmarshal(env.Data, &br); err != nil {
		return "", "", 0, fmt.Errorf("kucoin bullet-public: parse: %w", err)
	}
	if br.Token == "" {
		return "", "", 0, fmt.Errorf("kucoin bullet-public: empty token")
	}
	if len(br.InstanceServers) == 0 {
		return "", "", 0, fmt.Errorf("kucoin bullet-public: no instance servers")
	}

	srv := br.InstanceServers[0]
	pi := wsDefaultPingInterval
	if srv.PingInterval > 0 {
		pi = time.Duration(srv.PingInterval) * time.Millisecond
	}
	return br.Token, srv.Endpoint, pi, nil
}

// ============================================================
// WS message types
// ============================================================

// kcWSMessage — универсальное входящее WS-сообщение KuCoin Futures.
type kcWSMessage struct {
	Type    string          `json:"type"`    // "welcome","ack","pong","message","error"
	Topic   string          `json:"topic"`   // "/contractMarket/tickerV2:XBTUSDTM"
	Subject string          `json:"subject"` // "tickerV2","funding.rate","mark.index.price"
	Sn      int64           `json:"sn"`
	Data    json.RawMessage `json:"data"`
}

// kcSubMsg — сообщение подписки.
type kcSubMsg struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "subscribe"
	Topic    string `json:"topic"`
	Response bool   `json:"response"`
}

// kcPingMsg — ping-сообщение.
type kcPingMsg struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "ping"
}

// ============================================================
// ticker data types
// ============================================================

// kcTickerV1Data — данные /contractMarket/ticker:{symbol}.
// VERIFIED: price=lastPrice (строка), bestBidPrice, bestAskPrice, bestBidSize, bestAskSize, ts (нс).
// TODO:VERIFY: ts — наносекунды (официальная документация показывает нс, проверить при живом подключении).
type kcTickerV1Data struct {
	Symbol       string      `json:"symbol"`
	Price        string      `json:"price"`        // last trade price
	BestBidPrice string      `json:"bestBidPrice"` // строка
	BestAskPrice string      `json:"bestAskPrice"` // строка
	BestBidSize  json.Number `json:"bestBidSize"`
	BestAskSize  json.Number `json:"bestAskSize"`
	Ts           json.Number `json:"ts"` // TODO:VERIFY: нс?
}

// kcTickerV2Data — данные /contractMarket/tickerV2:{symbol}.
// VERIFIED: bestBidPrice, bestAskPrice (строки), bestBidSize, bestAskSize, ts (нс).
// TODO:VERIFY: ts — наносекунды.
type kcTickerV2Data struct {
	Symbol       string      `json:"symbol"`
	Sequence     json.Number `json:"sequence"`
	BestBidPrice string      `json:"bestBidPrice"` // строка
	BestAskPrice string      `json:"bestAskPrice"` // строка
	BestBidSize  json.Number `json:"bestBidSize"`
	BestAskSize  json.Number `json:"bestAskSize"`
	Ts           json.Number `json:"ts"` // TODO:VERIFY: нс
}

// kcInstrumentFundingData — данные /contract/instrument subject=funding.rate.
// VERIFIED: fundingRate (float), granularity (ms), timestamp (ms).
type kcInstrumentFundingData struct {
	FundingRate json.Number `json:"fundingRate"` // VERIFIED: число (float)
	Granularity json.Number `json:"granularity"` // ms
	Timestamp   json.Number `json:"timestamp"`   // ms
}

// kcInstrumentMarkPriceData — данные /contract/instrument subject=mark.index.price.
// VERIFIED: markPrice, indexPrice (float), granularity (ms), timestamp (ms).
type kcInstrumentMarkPriceData struct {
	MarkPrice   json.Number `json:"markPrice"`
	IndexPrice  json.Number `json:"indexPrice"`
	Granularity json.Number `json:"granularity"` // ms
	Timestamp   json.Number `json:"timestamp"`   // ms
}

// ============================================================
// SubscribePublic
// ============================================================

// SubscribePublic подключается к публичному KuCoin Futures WS через bullet-token.
// Возвращает канал публичных событий; при переполнении буфера — drop.
// ctx.Done() или ошибка чтения → канал закрывается.
func (a *Adapter) SubscribePublic(ctx context.Context, subs []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	if len(subs) == 0 {
		return nil, fmt.Errorf("kucoin SubscribePublic: нет подписок")
	}

	// Шаг 1: получаем bullet-token и endpoint.
	token, endpoint, pingInterval, err := a.fetchBulletPublic(ctx)
	if err != nil {
		return nil, fmt.Errorf("kucoin SubscribePublic: %w", err)
	}

	// Строим WS URL: endpoint + "?token=" + token + "&connectId=" + uuid.
	connectID := newUUID()
	wsURL := endpoint + "?token=" + token + "&connectId=" + connectID

	// Шаг 2: подключение.
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("kucoin SubscribePublic: dial: %w", err)
	}

	ch := make(chan exchange.PublicEvent, wsPublicBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

		// ctx-watcher: при отмене контекста закрываем соединение,
		// чтобы разблокировать ReadMessage().
		go func() {
			<-ctx.Done()
			conn.Close()
		}()

		// Ждём {"type":"welcome"} (первое сообщение после коннекта).
		// VERIFIED: KuCoin присылает welcome после успешного подключения.
		if !kcWaitWelcome(conn) {
			return
		}

		// Строим список топиков и отправляем подписки.
		topics := buildKCTopics(subs)
		for _, topic := range topics {
			subMsg := kcSubMsg{
				ID:       newUUID(),
				Type:     "subscribe",
				Topic:    topic,
				Response: true,
			}
			if err := conn.WriteJSON(subMsg); err != nil {
				return
			}
		}

		// Запускаем ping-loop.
		go kcPingLoop(ctx, conn, pingInterval)

		// Основной цикл чтения.
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				// Ошибка чтения: ctx отменён или соединение закрыто.
				return
			}

			var msg kcWSMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				continue
			}

			// Пропускаем служебные фреймы.
			switch msg.Type {
			case "welcome", "ack", "pong", "":
				continue
			case "error":
				// Логируем и пропускаем ошибки (нет логгера — просто пропускаем).
				continue
			}

			// Обрабатываем только type="message".
			if msg.Type != "message" {
				continue
			}

			events := parseKCMessage(msg, a)
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

// kcWaitWelcome ждёт {"type":"welcome"} с таймаутом 10 с.
// Возвращает false при ошибке или таймауте.
func kcWaitWelcome(conn *websocket.Conn) bool {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return false
		}
		var msg kcWSMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Type == "welcome" {
			return true
		}
	}
}

// kcPingLoop отправляет {"id":"...","type":"ping"} каждые interval.
func kcPingLoop(ctx context.Context, conn *websocket.Conn, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ping := kcPingMsg{
				ID:   newUUID(),
				Type: "ping",
			}
			if err := conn.WriteJSON(ping); err != nil {
				return
			}
		}
	}
}

// ============================================================
// Topic builder
// ============================================================

// buildKCTopics строит список KuCoin WS топиков из PublicSubscription.
// Маппинг каналов:
//   - ChannelTicker  → /contractMarket/ticker:SYMBOL (v1, includes lastPrice + BBO)
//   - ChannelBBO     → /contractMarket/tickerV2:SYMBOL (v2, BBO only)
//   - ChannelFunding, ChannelMarkPrice → /contract/instrument:SYMBOL
//
// Дублирующиеся топики удаляются.
func buildKCTopics(subs []exchange.PublicSubscription) []string {
	seen := make(map[string]bool)
	var topics []string
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			topics = append(topics, t)
		}
	}
	for _, s := range subs {
		sym := string(s.Symbol)
		switch s.Channel {
		case exchange.ChannelTicker:
			// Используем tickerV1 для lastPrice + BBO.
			// TODO:VERIFY: если lastPrice не нужен, tickerV2 предпочтительней.
			add("/contractMarket/ticker:" + sym)
		case exchange.ChannelBBO:
			// tickerV2 — BBO-only топик (рекомендован KuCoin).
			// VERIFIED: /contractMarket/tickerV2:{symbol}
			add("/contractMarket/tickerV2:" + sym)
		case exchange.ChannelFunding, exchange.ChannelMarkPrice:
			// Инструментный топик: funding.rate + mark.index.price.
			// VERIFIED: /contract/instrument:{symbol}
			add("/contract/instrument:" + sym)
		}
	}
	return topics
}

// ============================================================
// Message parsers
// ============================================================

// parseKCMessage обрабатывает входящее WS-сообщение и возвращает события.
func parseKCMessage(msg kcWSMessage, a *Adapter) []exchange.PublicEvent {
	topic := msg.Topic
	switch {
	case strings.HasPrefix(topic, "/contractMarket/tickerV2:"):
		sym := strings.TrimPrefix(topic, "/contractMarket/tickerV2:")
		return parseTickerV2(msg, domain.ExchangeSymbol(sym), a)

	case strings.HasPrefix(topic, "/contractMarket/ticker:"):
		sym := strings.TrimPrefix(topic, "/contractMarket/ticker:")
		return parseTickerV1(msg, domain.ExchangeSymbol(sym), a)

	case strings.HasPrefix(topic, "/contract/instrument:"):
		sym := strings.TrimPrefix(topic, "/contract/instrument:")
		return parseInstrumentMsg(msg, domain.ExchangeSymbol(sym), a)
	}
	return nil
}

// parseTickerV2 обрабатывает /contractMarket/tickerV2:{symbol}.
// Эмитирует ChannelBBO через OrderBookSnapshot с bid[0] и ask[0].
// VERIFIED: поля bestBidPrice/bestAskPrice — строки; ts — нс.
func parseTickerV2(msg kcWSMessage, sym domain.ExchangeSymbol, a *Adapter) []exchange.PublicEvent {
	var data kcTickerV2Data
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		return nil
	}

	bid, err := decimal.FromString(data.BestBidPrice)
	if err != nil || bid.IsZero() {
		return nil
	}
	ask, err := decimal.FromString(data.BestAskPrice)
	if err != nil || ask.IsZero() {
		return nil
	}

	// Размеры — целые числа лотов; парсим в Decimal.
	bidSz, _ := parseJSONNumber(data.BestBidSize)
	askSz, _ := parseJSONNumber(data.BestAskSize)

	// ts в нс → мс.
	exTS := kcNsToTime(data.Ts)
	now := a.clock()

	ob := &domain.OrderBookSnapshot{
		Exchange:  domain.ExchangeKuCoin,
		Symbol:    sym,
		Bids:      []domain.PriceLevel{{Price: bid, Qty: bidSz}},
		Asks:      []domain.PriceLevel{{Price: ask, Qty: askSz}},
		Timestamp: exTS,
	}

	return []exchange.PublicEvent{{
		Channel:    exchange.ChannelBBO,
		Symbol:     sym,
		OrderBook:  ob,
		ExchangeTS: exTS,
		ReceivedAt: now,
	}}
}

// parseTickerV1 обрабатывает /contractMarket/ticker:{symbol}.
// Эмитирует ChannelTicker (lastPrice) + ChannelBBO.
// VERIFIED: поля price, bestBidPrice, bestAskPrice — строки; ts — нс.
func parseTickerV1(msg kcWSMessage, sym domain.ExchangeSymbol, a *Adapter) []exchange.PublicEvent {
	var data kcTickerV1Data
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		return nil
	}

	lastPrice, _ := parseDecimalOrZero(data.Price)
	bid, _ := parseDecimalOrZero(data.BestBidPrice)
	ask, _ := parseDecimalOrZero(data.BestAskPrice)
	bidSz, _ := parseJSONNumber(data.BestBidSize)
	askSz, _ := parseJSONNumber(data.BestAskSize)

	exTS := kcNsToTime(data.Ts)
	now := a.clock()

	var events []exchange.PublicEvent

	// Ticker-событие (lastPrice).
	if !lastPrice.IsZero() {
		ticker := &domain.Ticker{
			Symbol:    sym,
			LastPrice: lastPrice,
			Timestamp: exTS,
		}
		events = append(events, exchange.PublicEvent{
			Channel:    exchange.ChannelTicker,
			Symbol:     sym,
			Ticker:     ticker,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}

	// BBO-событие.
	if !bid.IsZero() || !ask.IsZero() {
		ob := &domain.OrderBookSnapshot{
			Exchange:  domain.ExchangeKuCoin,
			Symbol:    sym,
			Bids:      []domain.PriceLevel{{Price: bid, Qty: bidSz}},
			Asks:      []domain.PriceLevel{{Price: ask, Qty: askSz}},
			Timestamp: exTS,
		}
		events = append(events, exchange.PublicEvent{
			Channel:    exchange.ChannelBBO,
			Symbol:     sym,
			OrderBook:  ob,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}

	return events
}

// parseInstrumentMsg обрабатывает /contract/instrument:{symbol}.
// VERIFIED: subject="funding.rate" → kcInstrumentFundingData;
// subject="mark.index.price" → kcInstrumentMarkPriceData.
func parseInstrumentMsg(msg kcWSMessage, sym domain.ExchangeSymbol, a *Adapter) []exchange.PublicEvent {
	now := a.clock()
	switch msg.Subject {
	case "funding.rate":
		return parseFundingRate(msg.Data, sym, now, a)
	case "mark.index.price":
		return parseMarkIndexPrice(msg.Data, sym, now, a)
	}
	return nil
}

// parseFundingRate обрабатывает subject=funding.rate.
// VERIFIED: fundingRate — число (float), granularity — ms, timestamp — ms.
func parseFundingRate(data json.RawMessage, sym domain.ExchangeSymbol, now time.Time, a *Adapter) []exchange.PublicEvent {
	var d kcInstrumentFundingData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}

	rate, err := parseJSONNumber(d.FundingRate)
	if err != nil {
		return nil
	}

	var ts time.Time
	if ms, err2 := d.Timestamp.Int64(); err2 == nil && ms > 0 {
		ts = time.UnixMilli(ms).UTC()
	}

	// granularity в ms → interval в секундах.
	var intervalSec int64
	if gran, err2 := d.Granularity.Int64(); err2 == nil && gran > 0 {
		intervalSec = gran / 1000
	}
	if intervalSec == 0 {
		intervalSec = 8 * 3600
	}

	// Следующий funding = ts + intervalSec (TODO:VERIFY: KuCoin инструмент timestamp = уже состоявшийся).
	nextFunding := ts.Add(time.Duration(intervalSec) * time.Second)
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
		RealizedFundingRate:  rate,
		PredictedFundingRate: rate,
		RateType:             domain.FundingRatePredicted,
		FundingIntervalSec:   intervalSec,
		NextFundingTime:      nextFunding,
		Confidence:           confidence,
		FundingPriceType:     domain.FundingPriceMark,
	}

	return []exchange.PublicEvent{{
		Channel:    exchange.ChannelFunding,
		Symbol:     sym,
		Funding:    funding,
		ExchangeTS: ts,
		ReceivedAt: now,
	}}
}

// parseMarkIndexPrice обрабатывает subject=mark.index.price.
// VERIFIED: markPrice, indexPrice — числа (float), timestamp — ms.
func parseMarkIndexPrice(data json.RawMessage, sym domain.ExchangeSymbol, now time.Time, a *Adapter) []exchange.PublicEvent {
	var d kcInstrumentMarkPriceData
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}

	mp, err := parseJSONNumber(d.MarkPrice)
	if err != nil || mp.IsZero() {
		return nil
	}

	var ts time.Time
	if ms, err2 := d.Timestamp.Int64(); err2 == nil && ms > 0 {
		ts = time.UnixMilli(ms).UTC()
	}

	markTicker := &domain.Ticker{
		Symbol:    sym,
		MarkPrice: mp,
		Timestamp: ts,
	}

	return []exchange.PublicEvent{{
		Channel:    exchange.ChannelMarkPrice,
		Symbol:     sym,
		MarkPrice:  markTicker,
		ExchangeTS: ts,
		ReceivedAt: now,
	}}
}

// ============================================================
// Helpers
// ============================================================

// kcNsToTime конвертирует timestamp из наносекунд (json.Number) в time.Time.
// TODO:VERIFY: ts в tickerV1/V2 — наносекунды (проверить при живом подключении).
// Если значение <= 0 или ошибка парсинга — возвращает zero time.
func kcNsToTime(ns json.Number) time.Time {
	nsI, err := ns.Int64()
	if err != nil || nsI <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(nsI / 1_000_000).UTC()
}

// newUUID генерирует уникальный ID для WS-сообщений.
// TODO:VERIFY: KuCoin принимает любую уникальную строку как id сообщения.
// Используем монотонное время + xorshift для уникальности в пределах процесса.
func newUUID() string {
	ns := time.Now().UnixNano()
	// Простой xorshift64 от ns для второй половины ID.
	x := uint64(ns)
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	return fmt.Sprintf("%016x%016x", uint64(ns), x)
}

// ============================================================
// SubscribePrivate — заглушка
// ============================================================

// SubscribePrivate — TODO: приватный WS требует bullet-private (POST /api/v1/bullet-private) + auth.
// VERIFIED: /api/v1/bullet-private существует, но аутентификация bullet-private TODO:VERIFY.
func (a *Adapter) SubscribePrivate(ctx context.Context, creds domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	return nil, fmt.Errorf("kucoin SubscribePrivate: %w: TODO bullet-private + auth", errWSNotImplemented)
}
