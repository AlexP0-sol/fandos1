// ws.go реализует WebSocket-подписки для Gate.io V4 futures (USDT settle):
//
//   - SubscribePublic — публичные каналы: futures.tickers, futures.order_book.
//   - SubscribePrivate — stub; возвращает ErrWSNotImplemented.
//
// Mapping каналов:
//
//	ChannelTicker / ChannelBBO / ChannelFunding → futures.tickers
//	  Одна подписка futures.tickers покрывает все три; каждое push-сообщение
//	  генерирует события Ticker + (если есть bid/ask) BBO + Funding.
//
//	ChannelDepth → futures.order_book  [contract, "20", "0"]
//
// Keepalive:
//
//	Клиент отправляет {"time":<unix>,"channel":"futures.ping"} каждые 15 секунд.
//	Gate.io отвечает futures.pong; обрабатывается прозрачно (лог-skip).
//
// Gate V4 также инициирует WebSocket-уровень ping (protocol layer); gorilla
// обрабатывает его автоматически через SetPongHandler / default control handlers.
//
// Идентификация сообщений:
//   - Ack подписки: {"channel":..., "event":"subscribe", "error":null} — пропускаем.
//   - Error: {"channel":..., "event":"subscribe", "error":{"code":..., "message":...}} — логируем, пропускаем.
//   - Push: {"channel":..., "event":"update", "result":[...]} — обрабатываем.
//   - Pong: {"channel":"futures.pong"} — пропускаем.
//
// Числовые поля Gate.io: все строки согласно документации; для надёжности
// используем json.RawMessage и парсинг через rawToString (handles both string
// and number JSON values without float64).
//
// VERIFIED (гейт-документация 2026-07, https://www.gate.io/docs/developers/futures/ws/en/):
//   - WSS URL: wss://fx-ws.gateio.ws/v4/ws/usdt
//   - subscribe frame: {"time":<unix>,"channel":"futures.tickers","event":"subscribe","payload":["BTC_USDT"]}
//   - tickers update result fields: contract, last, mark_price, index_price, funding_rate, volume_24h_settle
//   - order_book subscribe payload: [contract, limit, interval] where interval="0"
//   - order_book notify result: {t, contract, id, asks:[{p,s}], bids:[{p,s}]}
//   - ping: {"time":<unix>,"channel":"futures.ping"}, pong channel="futures.pong"
//
// TODO:VERIFY: funding_next_apply не присутствует в futures.tickers WS push;
//
//	только funding_rate и funding_rate_indicative. NextFundingTime не устанавливается
//	по данным WS-стрима (будет нулевым). Используйте REST GetFunding для точного времени.
//
// TODO:VERIFY: Gate.io WS тикеры не включают best_bid/best_ask; BBO-событие
//
//	генерируется только если поля bid/ask присутствуют в результате (на практике —
//	нет в futures.tickers). ChannelBBO требует отдельной подписки futures.book_ticker.
//	Текущая реализация эмитирует BBO из futures.tickers, если поля присутствуют.
package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// gateWsPingInterval — интервал application-layer ping (futures.ping).
// TODO:VERIFY: Gate docs say server sends protocol-layer ping; 15s is conservative.
const gateWsPingInterval = 15 * time.Second

// gateWsPublicBufSize — буфер публичных событий (drop-on-full).
const gateWsPublicBufSize = 1024

// ============================================================
// Типы WS-сообщений Gate.io V4 futures
// ============================================================

// gateWsRequest — исходящее сообщение подписки / ping.
// VERIFIED: {"time":<unix>,"channel":"futures.tickers","event":"subscribe","payload":["BTC_USDT"]}
type gateWsRequest struct {
	Time    int64    `json:"time"`
	Channel string   `json:"channel"`
	Event   string   `json:"event"`
	Payload []string `json:"payload"`
}

// gateWsPingMsg — application-layer ping.
// VERIFIED: {"time":<unix>,"channel":"futures.ping"}
// event и payload не нужны для ping.
type gateWsPingMsg struct {
	Time    int64  `json:"time"`
	Channel string `json:"channel"`
}

// gateWsEnvelope — входящее сообщение (общий конверт).
// VERIFIED: {"time":...,"channel":"futures.tickers","event":"update","error":null,"result":[...]}
type gateWsEnvelope struct {
	Time    int64           `json:"time"`
	TimeMs  int64           `json:"time_ms"` // TODO:VERIFY: присутствует в новом API
	Channel string          `json:"channel"`
	Event   string          `json:"event"`
	Error   *gateWsError    `json:"error"`
	Result  json.RawMessage `json:"result"`
}

// gateWsError — поле error в конверте.
type gateWsError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// gateTickerResult — один элемент result[] в futures.tickers update.
// VERIFIED: поля из официальной документации Gate.io futures WS.
// Все числа передаются как строки согласно доке; используем json.RawMessage для безопасного
// парсинга без float64.
type gateTickerResult struct {
	Contract              string          `json:"contract"`
	Last                  json.RawMessage `json:"last"`
	MarkPrice             json.RawMessage `json:"mark_price"`
	IndexPrice            json.RawMessage `json:"index_price"`
	FundingRate           json.RawMessage `json:"funding_rate"`
	FundingRateIndicative json.RawMessage `json:"funding_rate_indicative"` // deprecated, fallback
	Volume24hSettle       json.RawMessage `json:"volume_24h_settle"`
	// TODO:VERIFY: best_bid / best_ask не документированы в futures.tickers WS;
	// поля ниже присутствуют на случай их появления.
	BestBid json.RawMessage `json:"best_bid"`
	BestAsk json.RawMessage `json:"best_ask"`
}

// gateOrderBookResult — result в futures.order_book notify.
// VERIFIED: {"t":...,"contract":"BTC_USDT","id":...,"asks":[{"p":"...","s":"..."}],"bids":[...]}
type gateOrderBookResult struct {
	T        int64                `json:"t"` // milliseconds
	Contract string               `json:"contract"`
	ID       int64                `json:"id"`
	Asks     []gateOrderBookLevel `json:"asks"`
	Bids     []gateOrderBookLevel `json:"bids"`
}

// gateOrderBookLevel — один уровень стакана {p: price, s: size}.
// VERIFIED: поля p и s (строки).
type gateOrderBookLevel struct {
	P json.RawMessage `json:"p"` // price string
	S json.RawMessage `json:"s"` // size string
}

// ============================================================
// SubscribePublic
// ============================================================

// SubscribePublic подключается к публичному WS Gate.io V4 futures и подписывается.
// Возвращает буферизованный канал публичных событий; при переполнении буфера — drop.
// На ctx.Done() или ошибке чтения — канал закрывается и горутина завершается.
// Reconnect — ответственность вызывающего (caller's job).
//
// Требует явной установки Config.WSBaseURL (рекомендуется defaultWSBase).
// Если WSBaseURL не задан — возвращает ErrWSNotImplemented.
func (a *Adapter) SubscribePublic(ctx context.Context, subscriptions []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	// Проверяем что WS URL явно задан.
	if a.wsBase == "" {
		return nil, fmt.Errorf("%w: задайте Config.WSBaseURL = %q", ErrWSNotImplemented, defaultWSBase)
	}

	// Строим список подписок (deduplicated).
	subs := buildGatePublicSubs(subscriptions)
	if len(subs) == 0 {
		return nil, fmt.Errorf("gate SubscribePublic: нет подписок")
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.wsBase, nil)
	if err != nil {
		return nil, fmt.Errorf("gate SubscribePublic: dial %s: %w", a.wsBase, err)
	}

	ch := make(chan exchange.PublicEvent, gateWsPublicBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

		// Закрываем соединение при отмене контекста,
		// чтобы разблокировать ReadMessage (иначе горутина зависнет).
		go func() {
			<-ctx.Done()
			conn.Close()
		}()

		now := a.clock().Unix()

		// Отправляем все подписки.
		for _, sub := range subs {
			req := gateWsRequest{
				Time:    now,
				Channel: sub.channel,
				Event:   "subscribe",
				Payload: sub.payload,
			}
			if err := conn.WriteJSON(req); err != nil {
				return
			}
		}

		// Запускаем application-layer ping loop.
		go gatePublicPingLoop(ctx, conn, a.clock)

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				// Ошибка чтения (соединение закрыто, ctx отменён и т.п.) → выходим.
				return
			}

			var env gateWsEnvelope
			if err := json.Unmarshal(raw, &env); err != nil {
				continue
			}

			// Обрабатываем error-frame (skip + log).
			if env.Error != nil && env.Error.Code != 0 {
				log.Printf("gate WS error: channel=%s code=%d msg=%s",
					env.Channel, env.Error.Code, env.Error.Message)
				continue
			}

			// Ack подписки (event="subscribe", error=null) — пропускаем.
			if env.Event == "subscribe" {
				continue
			}

			// Pong — пропускаем.
			if env.Channel == "futures.pong" {
				continue
			}

			// Push-сообщения.
			if env.Event != "update" && env.Event != "all" {
				continue
			}

			switch env.Channel {
			case "futures.tickers":
				events := handleGateTickersMsg(env, a)
				for _, ev := range events {
					select {
					case ch <- ev:
					default:
						// Drop-on-full: публичные события допускают потерю.
					}
				}

			case "futures.order_book":
				ev, ok := handleGateOrderBookMsg(env, a)
				if !ok {
					continue
				}
				select {
				case ch <- ev:
				default:
					// Drop-on-full.
				}
			}
		}
	}()

	return ch, nil
}

// ============================================================
// Subscription building
// ============================================================

// gateSubSpec — одна Gate.io WS-подписка (channel + payload).
type gateSubSpec struct {
	channel string
	payload []string
}

// buildGatePublicSubs строит deduplicated список подписок для Gate.io.
// VERIFIED: futures.tickers покрывает Ticker/BBO/Funding; futures.order_book — Depth.
func buildGatePublicSubs(subs []exchange.PublicSubscription) []gateSubSpec {
	// Группируем символы по каналу.
	tickerSymbols := make(map[string]struct{})
	depthSymbols := make(map[string]struct{})

	for _, s := range subs {
		sym := string(s.Symbol)
		switch s.Channel {
		case exchange.ChannelTicker, exchange.ChannelBBO, exchange.ChannelFunding:
			tickerSymbols[sym] = struct{}{}
		case exchange.ChannelDepth:
			depthSymbols[sym] = struct{}{}
		}
	}

	var result []gateSubSpec

	// futures.tickers: payload = list of contract names.
	if len(tickerSymbols) > 0 {
		payload := mapKeysToSlice(tickerSymbols)
		result = append(result, gateSubSpec{
			channel: "futures.tickers",
			payload: payload,
		})
	}

	// futures.order_book: одна подписка на один контракт; payload=[contract, limit, interval].
	// VERIFIED: payload format [contract, limit, interval] where interval="0".
	for sym := range depthSymbols {
		result = append(result, gateSubSpec{
			channel: "futures.order_book",
			// limit="20", interval="0" (legacy full snapshot + updates).
			payload: []string{sym, "20", "0"},
		})
	}

	return result
}

// mapKeysToSlice конвертирует ключи map в slice (порядок недетерминирован).
func mapKeysToSlice(m map[string]struct{}) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	return s
}

// ============================================================
// Ping loop
// ============================================================

// gatePublicPingLoop отправляет application-layer futures.ping каждые gateWsPingInterval.
// VERIFIED: {"time":<unix>,"channel":"futures.ping"} → ответ {"channel":"futures.pong"}.
func gatePublicPingLoop(ctx context.Context, conn *websocket.Conn, clock func() time.Time) {
	t := time.NewTicker(gateWsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ping := gateWsPingMsg{
				Time:    clock().Unix(),
				Channel: "futures.ping",
			}
			if err := conn.WriteJSON(ping); err != nil {
				return
			}
		}
	}
}

// ============================================================
// Обработка futures.tickers
// ============================================================

// handleGateTickersMsg парсит futures.tickers update и генерирует события.
// Возвращает 1-3 события на каждый контракт в result[]:
//   - ChannelTicker (всегда при наличии last)
//   - ChannelBBO (TODO:VERIFY: futures.tickers не содержит best_bid/ask; условный emit)
//   - ChannelFunding (при наличии funding_rate)
func handleGateTickersMsg(env gateWsEnvelope, a *Adapter) []exchange.PublicEvent {
	var tickers []gateTickerResult
	if err := json.Unmarshal(env.Result, &tickers); err != nil {
		return nil
	}

	now := a.clock()
	// Используем time_ms если есть, иначе time (секунды).
	var exTS time.Time
	if env.TimeMs != 0 {
		exTS = time.UnixMilli(env.TimeMs).UTC()
	} else {
		exTS = time.Unix(env.Time, 0).UTC()
	}

	var events []exchange.PublicEvent

	for _, t := range tickers {
		domSym := domain.ExchangeSymbol(t.Contract)

		last, _ := rawToDecimal(t.Last)
		markPrice, _ := rawToDecimal(t.MarkPrice)
		indexPrice, _ := rawToDecimal(t.IndexPrice)
		volume, _ := rawToDecimal(t.Volume24hSettle)

		// ChannelTicker — emit если есть last или mark_price.
		if !last.IsZero() || !markPrice.IsZero() {
			ticker := &domain.Ticker{
				Symbol:         domSym,
				LastPrice:      last,
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

		// ChannelBBO — TODO:VERIFY: futures.tickers WS обычно не содержит best_bid/ask.
		// Эмитируем только если поля реально присутствуют в result.
		bestBid, bidOk := rawToDecimal(t.BestBid)
		bestAsk, askOk := rawToDecimal(t.BestAsk)
		if bidOk || askOk {
			bboTicker := &domain.Ticker{
				Symbol:    domSym,
				LastPrice: last,
				Timestamp: exTS,
			}
			_ = bestBid
			_ = bestAsk
			events = append(events, exchange.PublicEvent{
				Channel:    exchange.ChannelBBO,
				Symbol:     domSym,
				Ticker:     bboTicker,
				ExchangeTS: exTS,
				ReceivedAt: now,
			})
		}

		// ChannelFunding — emit если есть funding_rate.
		fundingRate, fundingOk := rawToDecimal(t.FundingRate)
		if !fundingOk {
			// Fallback к funding_rate_indicative (deprecated).
			fundingRate, fundingOk = rawToDecimal(t.FundingRateIndicative)
		}
		if fundingOk {
			// TODO:VERIFY: futures.tickers WS не содержит funding_next_apply;
			// NextFundingTime остаётся нулевым. Для точного времени используйте REST GetFunding.
			var nextFunding time.Time
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
				ExchangeSymbol:       domSym,
				RealizedFundingRate:  fundingRate,
				PredictedFundingRate: fundingRate,
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
	}

	return events
}

// ============================================================
// Обработка futures.order_book
// ============================================================

// handleGateOrderBookMsg парсит futures.order_book notify и возвращает ChannelDepth событие.
// VERIFIED: result = {t, contract, id, asks:[{p,s}], bids:[{p,s}]}
func handleGateOrderBookMsg(env gateWsEnvelope, a *Adapter) (exchange.PublicEvent, bool) {
	var ob gateOrderBookResult
	if err := json.Unmarshal(env.Result, &ob); err != nil {
		return exchange.PublicEvent{}, false
	}

	bids, err := parseGatePriceLevels(ob.Bids)
	if err != nil {
		return exchange.PublicEvent{}, false
	}
	asks, err := parseGatePriceLevels(ob.Asks)
	if err != nil {
		return exchange.PublicEvent{}, false
	}

	sym := domain.ExchangeSymbol(ob.Contract)
	now := a.clock()

	var ts time.Time
	if ob.T != 0 {
		ts = time.UnixMilli(ob.T).UTC()
	} else {
		ts = now
	}

	snap := &domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeGate,
		Symbol:     sym,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  ts,
		Sequence:   ob.ID,
		IsSnapshot: true,
	}

	return exchange.PublicEvent{
		Channel:    exchange.ChannelDepth,
		Symbol:     sym,
		OrderBook:  snap,
		ExchangeTS: ts,
		ReceivedAt: now,
	}, true
}

// parseGatePriceLevels разбирает []gateOrderBookLevel в []domain.PriceLevel.
// VERIFIED: поля p (price) и s (size) — строки согласно документации.
func parseGatePriceLevels(levels []gateOrderBookLevel) ([]domain.PriceLevel, error) {
	result := make([]domain.PriceLevel, 0, len(levels))
	for _, l := range levels {
		priceStr, err := rawToString(l.P)
		if err != nil {
			return nil, fmt.Errorf("gate order book price: %w", err)
		}
		sizeStr, err := rawToString(l.S)
		if err != nil {
			return nil, fmt.Errorf("gate order book size: %w", err)
		}
		price, err := decimal.FromString(priceStr)
		if err != nil {
			return nil, fmt.Errorf("gate order book price parse: %w", err)
		}
		size, err := decimal.FromString(sizeStr)
		if err != nil {
			return nil, fmt.Errorf("gate order book size parse: %w", err)
		}
		result = append(result, domain.PriceLevel{Price: price, Qty: size})
	}
	return result, nil
}

// ============================================================
// SubscribePrivate — stub
// ============================================================

// SubscribePrivate — приватные WS Gate.io (TODO: не реализовано).
// Возвращает ErrWSNotImplemented.
func (a *Adapter) SubscribePrivate(_ context.Context, _ domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	return nil, fmt.Errorf("%w: используйте REST polling для Gate.io", ErrWSNotImplemented)
}

// ============================================================
// Вспомогательные функции парсинга
// ============================================================

// rawToString извлекает строковое значение из json.RawMessage.
// Поддерживает как JSON string ("1234.5"), так и JSON number (1234.5).
// Никогда не использует float64 — работает на уровне текста.
func rawToString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	// JSON string: "..."
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	// JSON number или null: используем как есть (без float64).
	s := strings.TrimSpace(string(raw))
	if s == "null" {
		return "", nil
	}
	return s, nil
}

// rawToDecimal конвертирует json.RawMessage → Decimal.
// Возвращает (Zero, false) для пустых/null/нулевых значений.
// Никогда не проходит через float64.
func rawToDecimal(raw json.RawMessage) (decimal.Decimal, bool) {
	if len(raw) == 0 {
		return decimal.Zero, false
	}
	s, err := rawToString(raw)
	if err != nil || s == "" {
		return decimal.Zero, false
	}
	d, err := decimal.FromString(s)
	if err != nil {
		return decimal.Zero, false
	}
	if d.IsZero() {
		return decimal.Zero, false
	}
	return d, true
}
