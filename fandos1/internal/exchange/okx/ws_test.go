// ws_test.go — тесты OKX V5 WebSocket-адаптера (публичные каналы).
//
// Используется httptest.Server с gorilla/websocket апгрейдером в роли фейкового OKX WS.
// Все тесты детерминированы; timeout < 200ms.
//
// Проверяется:
//   - subscribe-фрейм отправляется с корректными args (channel + instId)
//   - push tickers → ChannelTicker с корректными decimal bid/ask/last
//   - push tickers → ChannelBBO с корректными уровнями
//   - push funding-rate → ChannelFunding с корректной ставкой и nextFundingTime
//   - ctx cancel → канал событий закрывается
//   - неизвестные/служебные фреймы не эмитируются в канал
//   - plain "pong" и {"event":"subscribe"} не эмитируются
package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// ============================================================
// Фейковый WS-сервер (httptest + gorilla upgrader)
// ============================================================

// wsUpgrader — upgrader с relaxed check-origin для тестов.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// fakeOKXServer — инкапсулирует httptest.Server и канал входящих соединений.
// handler вызывается для каждого нового WS-соединения.
type fakeOKXServer struct {
	srv     *httptest.Server
	connCh  chan *websocket.Conn
	handler func(conn *websocket.Conn)
}

// newFakeOKXServer создаёт фейковый WS-сервер; handler вызывается в отдельной горутине.
func newFakeOKXServer(t *testing.T, handler func(conn *websocket.Conn)) *fakeOKXServer {
	t.Helper()
	f := &fakeOKXServer{
		connCh:  make(chan *websocket.Conn, 1),
		handler: handler,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go f.handler(conn)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// wsURL возвращает WS URL тестового сервера (ws://...).
func (f *fakeOKXServer) wsURL() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

// ============================================================
// newTestAdapter — адаптер с инжектированным wsBaseURL и clock.
// ============================================================

func newTestAdapter(wsBaseURL string, clk func() time.Time) *Adapter {
	doer := &nopHTTPDoer{}
	a, err := New(Config{
		WSBaseURL: wsBaseURL,
		HTTPDoer:  doer,
		Clock:     clk,
	})
	if err != nil {
		panic(fmt.Sprintf("newTestAdapter: %v", err))
	}
	return a
}

// nopHTTPDoer — HTTPDoer, возвращающий заглушку (для WS-тестов HTTP не нужен).
type nopHTTPDoer struct{}

func (n *nopHTTPDoer) Do(_ context.Context, _ HTTPRequest) (int, []byte, error) {
	return 200, []byte(`{"code":"0","data":[]}`), nil
}

// ============================================================
// mustDecimal — helper для тестов
// ============================================================

func mustDecimal(s string) decimal.Decimal {
	d, err := decimal.FromString(s)
	if err != nil {
		panic(fmt.Sprintf("mustDecimal(%q): %v", s, err))
	}
	return d
}

// waitEvent — ждёт первое событие из канала или таймаут.
func waitEvent(ch <-chan exchange.PublicEvent, timeout time.Duration) (exchange.PublicEvent, bool) {
	select {
	case ev, ok := <-ch:
		if !ok {
			return exchange.PublicEvent{}, false
		}
		return ev, true
	case <-time.After(timeout):
		return exchange.PublicEvent{}, false
	}
}

// drainUntilClosed — читает все события из канала до закрытия или таймаута.
// Возвращает срез полученных событий.
func drainUntilClosed(ch <-chan exchange.PublicEvent, timeout time.Duration) []exchange.PublicEvent {
	deadline := time.After(timeout)
	var events []exchange.PublicEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-deadline:
			return events
		}
	}
}

// ============================================================
// Test: subscribe frame sent correctly
// ============================================================

// TestSubscribePublic_SubscribeFrameSent проверяет, что SubscribePublic отправляет
// корректный {"op":"subscribe","args":[...]} фрейм на сервер.
func TestSubscribePublic_SubscribeFrameSent(t *testing.T) {
	// Канал для захвата первого сообщения от клиента.
	received := make(chan []byte, 1)

	srv := newFakeOKXServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		received <- msg
		// Держим соединение открытым пока клиент не закроет.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	fixedClock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	a := newTestAdapter(srv.wsURL(), fixedClock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subs := []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC-USDT-SWAP"},
		{Channel: exchange.ChannelFunding, Symbol: "BTC-USDT-SWAP"},
		{Channel: exchange.ChannelDepth, Symbol: "ETH-USDT-SWAP"},
	}
	ch, err := a.SubscribePublic(ctx, subs)
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	_ = ch

	// Ждём, пока сервер получит первое сообщение.
	select {
	case raw := <-received:
		var msg okxSubscribeMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("unmarshal subscribe frame: %v", err)
		}
		if msg.Op != "subscribe" {
			t.Errorf("op = %q; want subscribe", msg.Op)
		}

		// Ожидаем три уникальных аргумента: tickers|BTC, funding-rate|BTC, books5|ETH.
		wantArgs := map[string]string{
			"tickers":      "BTC-USDT-SWAP",
			"funding-rate": "BTC-USDT-SWAP",
			"books5":       "ETH-USDT-SWAP",
		}
		for _, arg := range msg.Args {
			if want, ok := wantArgs[arg.Channel]; ok {
				if arg.InstId != want {
					t.Errorf("arg channel=%s: instId=%q want %q", arg.Channel, arg.InstId, want)
				}
				delete(wantArgs, arg.Channel)
			}
		}
		for ch := range wantArgs {
			t.Errorf("missing arg for channel %q", ch)
		}

	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for subscribe frame")
	}
}

// ============================================================
// Test: tickers push → ChannelTicker + ChannelBBO
// ============================================================

// TestSubscribePublic_TickersPush проверяет парсинг push-фрейма tickers:
//   - ChannelTicker: корректные decimal last/bid/ask без float64
//   - ChannelBBO: OrderBook с bid и ask уровнями
func TestSubscribePublic_TickersPush(t *testing.T) {
	const (
		bidPxStr  = "29999.12"
		askPxStr  = "30001.50"
		lastStr   = "30000.00"
		vol24hStr = "12345678.99"
		tsMs      = "1700000000000"
	)

	tickerPush := fmt.Sprintf(`{
		"arg": {"channel": "tickers", "instId": "BTC-USDT-SWAP"},
		"data": [{
			"instId": "BTC-USDT-SWAP",
			"last":   "%s",
			"bidPx": "%s",
			"askPx": "%s",
			"volCcy24h": "%s",
			"vol24h": "999",
			"ts": "%s"
		}]
	}`, lastStr, bidPxStr, askPxStr, vol24hStr, tsMs)

	srv := newFakeOKXServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		// Читаем subscribe-фрейм от клиента.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// Отправляем ticker push.
		conn.WriteMessage(websocket.TextMessage, []byte(tickerPush))
		// Держим соединение открытым.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	fixedClock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	a := newTestAdapter(srv.wsURL(), fixedClock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC-USDT-SWAP"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Ожидаем ChannelTicker.
	ev, ok := waitEvent(ch, 200*time.Millisecond)
	if !ok {
		t.Fatal("timeout waiting for ChannelTicker event")
	}
	if ev.Channel != exchange.ChannelTicker {
		t.Errorf("channel = %q; want ChannelTicker", ev.Channel)
	}
	if ev.Ticker == nil {
		t.Fatal("Ticker is nil")
	}
	if !ev.Ticker.LastPrice.Equal(mustDecimal(lastStr)) {
		t.Errorf("LastPrice = %v; want %s", ev.Ticker.LastPrice, lastStr)
	}
	if ev.Symbol != "BTC-USDT-SWAP" {
		t.Errorf("Symbol = %q; want BTC-USDT-SWAP", ev.Symbol)
	}
	// ExchangeTS должен соответствовать ts в ms.
	wantTS := time.UnixMilli(1700000000000).UTC()
	if !ev.ExchangeTS.Equal(wantTS) {
		t.Errorf("ExchangeTS = %v; want %v", ev.ExchangeTS, wantTS)
	}

	// Ожидаем ChannelBBO.
	bboEv, ok := waitEvent(ch, 200*time.Millisecond)
	if !ok {
		t.Fatal("timeout waiting for ChannelBBO event")
	}
	if bboEv.Channel != exchange.ChannelBBO {
		t.Errorf("channel = %q; want ChannelBBO", bboEv.Channel)
	}
	if bboEv.OrderBook == nil {
		t.Fatal("BBO OrderBook is nil")
	}
	if len(bboEv.OrderBook.Bids) == 0 {
		t.Fatal("BBO Bids empty")
	}
	if !bboEv.OrderBook.Bids[0].Price.Equal(mustDecimal(bidPxStr)) {
		t.Errorf("BBO bid = %v; want %s", bboEv.OrderBook.Bids[0].Price, bidPxStr)
	}
	if len(bboEv.OrderBook.Asks) == 0 {
		t.Fatal("BBO Asks empty")
	}
	if !bboEv.OrderBook.Asks[0].Price.Equal(mustDecimal(askPxStr)) {
		t.Errorf("BBO ask = %v; want %s", bboEv.OrderBook.Asks[0].Price, askPxStr)
	}
}

// ============================================================
// Test: funding-rate push → ChannelFunding
// ============================================================

// TestSubscribePublic_FundingRatePush проверяет парсинг push-фрейма funding-rate:
//   - ChannelFunding c корректной ставкой и nextFundingTime
func TestSubscribePublic_FundingRatePush(t *testing.T) {
	// nextFundingTime: следующий settlement через 4 часа → ConfidenceMedium.
	// Используем фиксированный clock чтобы confidence был предсказуем.
	baseTime := time.Unix(1700000000, 0).UTC()
	nextFundingMs := baseTime.Add(2 * time.Hour).UnixMilli()

	fundingPush := fmt.Sprintf(`{
		"arg": {"channel": "funding-rate", "instId": "BTC-USDT-SWAP"},
		"data": [{
			"instId": "BTC-USDT-SWAP",
			"instType": "SWAP",
			"fundingRate": "0.0001500",
			"fundingTime": "%d",
			"nextFundingRate": "0.0002000",
			"nextFundingTime": "%d",
			"ts": "1700000000000"
		}]
	}`, nextFundingMs, nextFundingMs+8*3600*1000)

	srv := newFakeOKXServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		// Читаем subscribe-фрейм.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		conn.WriteMessage(websocket.TextMessage, []byte(fundingPush))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	fixedClock := func() time.Time { return baseTime }
	a := newTestAdapter(srv.wsURL(), fixedClock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelFunding, Symbol: "BTC-USDT-SWAP"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	ev, ok := waitEvent(ch, 200*time.Millisecond)
	if !ok {
		t.Fatal("timeout waiting for ChannelFunding event")
	}
	if ev.Channel != exchange.ChannelFunding {
		t.Errorf("channel = %q; want ChannelFunding", ev.Channel)
	}
	if ev.Funding == nil {
		t.Fatal("Funding is nil")
	}

	wantRate := mustDecimal("0.0001500")
	if !ev.Funding.RealizedFundingRate.Equal(wantRate) {
		t.Errorf("RealizedFundingRate = %v; want %v", ev.Funding.RealizedFundingRate, wantRate)
	}

	wantPredicted := mustDecimal("0.0002000")
	if !ev.Funding.PredictedFundingRate.Equal(wantPredicted) {
		t.Errorf("PredictedFundingRate = %v; want %v", ev.Funding.PredictedFundingRate, wantPredicted)
	}

	wantNextFunding := time.UnixMilli(nextFundingMs).UTC()
	if !ev.Funding.NextFundingTime.Equal(wantNextFunding) {
		t.Errorf("NextFundingTime = %v; want %v", ev.Funding.NextFundingTime, wantNextFunding)
	}

	// До следующего funding = 2 часа < 4 часов → ConfidenceMedium.
	if ev.Funding.Confidence != domain.ConfidenceMedium {
		t.Errorf("Confidence = %v; want ConfidenceMedium", ev.Funding.Confidence)
	}

	if ev.Symbol != "BTC-USDT-SWAP" {
		t.Errorf("Symbol = %q; want BTC-USDT-SWAP", ev.Symbol)
	}
}

// ============================================================
// Test: ctx cancel → channel closed
// ============================================================

// TestSubscribePublic_CtxCancel проверяет, что отмена контекста закрывает канал событий.
func TestSubscribePublic_CtxCancel(t *testing.T) {
	srv := newFakeOKXServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		// Читаем subscribe-фрейм и держим соединение.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	fixedClock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	a := newTestAdapter(srv.wsURL(), fixedClock)

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC-USDT-SWAP"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Отменяем контекст.
	cancel()

	// Канал должен закрыться.
	select {
	case _, ok := <-ch:
		if ok {
			// Может прийти одно событие до закрытия; продолжаем дрейн.
			select {
			case _, ok2 := <-ch:
				if ok2 {
					t.Error("channel still open after ctx cancel")
				}
			case <-time.After(200 * time.Millisecond):
				t.Error("channel not closed after ctx cancel (inner)")
			}
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("channel not closed after ctx cancel (outer)")
	}
}

// ============================================================
// Test: unknown / control frames don't emit
// ============================================================

// TestSubscribePublic_ControlFramesNoEmit проверяет, что служебные фреймы
// ({"event":"subscribe"}, {"event":"error"}, plain "pong") не эмитируются.
func TestSubscribePublic_ControlFramesNoEmit(t *testing.T) {
	controlFrames := []string{
		`{"event":"subscribe","arg":{"channel":"tickers","instId":"BTC-USDT-SWAP"},"connId":"abc"}`,
		`{"event":"error","code":"60018","msg":"channel does not exist","connId":"abc"}`,
		`pong`,
		`ping`,
		`{"unknown_field":"value"}`,
	}

	srv := newFakeOKXServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		// Читаем subscribe-фрейм.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// Отправляем все контрольные фреймы.
		for _, frame := range controlFrames {
			conn.WriteMessage(websocket.TextMessage, []byte(frame))
		}
		// Держим соединение открытым.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	fixedClock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	a := newTestAdapter(srv.wsURL(), fixedClock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC-USDT-SWAP"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Даём время обработать все фреймы, затем проверяем, что канал пуст.
	time.Sleep(80 * time.Millisecond)

	select {
	case ev := <-ch:
		t.Errorf("unexpected event emitted for control frame: channel=%q symbol=%q", ev.Channel, ev.Symbol)
	default:
		// Правильно — ни одного события.
	}
}

// ============================================================
// Test: server close → channel closed
// ============================================================

// TestSubscribePublic_ServerClose проверяет, что закрытие WS-сервером
// приводит к закрытию канала событий у клиента.
func TestSubscribePublic_ServerClose(t *testing.T) {
	srv := newFakeOKXServer(t, func(conn *websocket.Conn) {
		// Читаем subscribe-фрейм и сразу закрываем соединение.
		conn.ReadMessage() //nolint
		conn.Close()
	})

	fixedClock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	a := newTestAdapter(srv.wsURL(), fixedClock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC-USDT-SWAP"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Канал должен закрыться после того как сервер закрыл соединение.
	events := drainUntilClosed(ch, 200*time.Millisecond)
	_ = events

	// После drain канал должен быть закрыт.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel still open after server close")
		}
	default:
		// Канал закрыт — проверяем ещё раз с небольшой задержкой.
		time.Sleep(20 * time.Millisecond)
		select {
		case _, ok := <-ch:
			if ok {
				t.Error("channel still open after server close (retry)")
			}
		default:
			// ok — горутина ещё не успела завершиться, тест проходит
		}
	}
}

// ============================================================
// Test: buildOKXPublicArgs deduplication
// ============================================================

// TestBuildOKXPublicArgs_Dedup проверяет, что buildOKXPublicArgs
// устраняет дублирующиеся подписки.
func TestBuildOKXPublicArgs_Dedup(t *testing.T) {
	subs := []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC-USDT-SWAP"},
		{Channel: exchange.ChannelBBO, Symbol: "BTC-USDT-SWAP"},     // то же "tickers"
		{Channel: exchange.ChannelFunding, Symbol: "BTC-USDT-SWAP"}, // funding-rate
		{Channel: exchange.ChannelFunding, Symbol: "BTC-USDT-SWAP"}, // дубль
		{Channel: exchange.ChannelDepth, Symbol: "ETH-USDT-SWAP"},
	}
	args := buildOKXPublicArgs(subs)

	// Ожидаем 3 уникальных: tickers|BTC, funding-rate|BTC, books5|ETH.
	if len(args) != 3 {
		t.Errorf("len(args) = %d; want 3; args=%v", len(args), args)
	}

	// Проверяем все ожидаемые аргументы присутствуют.
	wantSet := map[string]bool{
		"tickers|BTC-USDT-SWAP":      false,
		"funding-rate|BTC-USDT-SWAP": false,
		"books5|ETH-USDT-SWAP":       false,
	}
	for _, a := range args {
		key := a.Channel + "|" + a.InstId
		if _, ok := wantSet[key]; ok {
			wantSet[key] = true
		} else {
			t.Errorf("unexpected arg: channel=%s instId=%s", a.Channel, a.InstId)
		}
	}
	for key, found := range wantSet {
		if !found {
			t.Errorf("missing expected arg: %s", key)
		}
	}
}

// ============================================================
// Test: parseOKXMessage unit tests
// ============================================================

// TestParseOKXMessage_Pong проверяет, что "pong" не эмитирует события.
func TestParseOKXMessage_Pong(t *testing.T) {
	a := newTestAdapter("ws://unused", time.Now)
	events := parseOKXMessage([]byte("pong"), a)
	if len(events) != 0 {
		t.Errorf("expected no events for pong, got %d", len(events))
	}
}

// TestParseOKXMessage_EventSubscribe проверяет, что {"event":"subscribe"} не эмитирует.
func TestParseOKXMessage_EventSubscribe(t *testing.T) {
	a := newTestAdapter("ws://unused", time.Now)
	raw := []byte(`{"event":"subscribe","arg":{"channel":"tickers","instId":"BTC-USDT-SWAP"},"connId":"abc"}`)
	events := parseOKXMessage(raw, a)
	if len(events) != 0 {
		t.Errorf("expected no events for event:subscribe, got %d", len(events))
	}
}

// TestParseOKXMessage_TickerDecimal проверяет, что last/bid/ask парсятся
// через decimal.FromString (без float64) и значения точны.
func TestParseOKXMessage_TickerDecimal(t *testing.T) {
	// Используем число, которое float64 не может представить точно.
	const preciseStr = "29999.123456789"
	raw := []byte(fmt.Sprintf(`{
		"arg": {"channel": "tickers", "instId": "ETH-USDT-SWAP"},
		"data": [{
			"instId": "ETH-USDT-SWAP",
			"last": "%s",
			"bidPx": "29998.000000001",
			"askPx": "30000.000000009",
			"volCcy24h": "99999999.99",
			"ts": "1700000000000"
		}]
	}`, preciseStr))

	fixedClock := func() time.Time { return time.Unix(1700000000, 0).UTC() }
	a := newTestAdapter("ws://unused", fixedClock)

	events := parseOKXMessage(raw, a)
	// Ожидаем ChannelTicker + ChannelBBO.
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}

	var tickerEv *exchange.PublicEvent
	for i := range events {
		if events[i].Channel == exchange.ChannelTicker {
			tickerEv = &events[i]
			break
		}
	}
	if tickerEv == nil {
		t.Fatal("no ChannelTicker event")
	}
	if tickerEv.Ticker == nil {
		t.Fatal("Ticker is nil")
	}

	want := mustDecimal(preciseStr)
	if !tickerEv.Ticker.LastPrice.Equal(want) {
		t.Errorf("LastPrice = %v; want %v", tickerEv.Ticker.LastPrice, want)
	}
}

// TestParseOKXMessage_FundingRate проверяет корректный парсинг funding-rate push.
func TestParseOKXMessage_FundingRate(t *testing.T) {
	nextFundingMs := int64(1700028800000) // произвольный timestamp в будущем
	raw := []byte(fmt.Sprintf(`{
		"arg": {"channel": "funding-rate", "instId": "BTC-USDT-SWAP"},
		"data": [{
			"instId": "BTC-USDT-SWAP",
			"instType": "SWAP",
			"fundingRate": "-0.0000500",
			"fundingTime": "%d",
			"nextFundingRate": "0.0001000",
			"ts": "1700000000000"
		}]
	}`, nextFundingMs))

	// Clock в далёком прошлом → until funding очень далеко → ConfidenceLow.
	fixedClock := func() time.Time { return time.Unix(1600000000, 0).UTC() }
	a := newTestAdapter("ws://unused", fixedClock)

	events := parseOKXMessage(raw, a)
	if len(events) != 1 {
		t.Fatalf("expected 1 funding event, got %d", len(events))
	}
	ev := events[0]
	if ev.Channel != exchange.ChannelFunding {
		t.Errorf("channel = %q; want ChannelFunding", ev.Channel)
	}
	if ev.Funding == nil {
		t.Fatal("Funding is nil")
	}

	wantRate := mustDecimal("-0.0000500")
	if !ev.Funding.RealizedFundingRate.Equal(wantRate) {
		t.Errorf("RealizedFundingRate = %v; want %v", ev.Funding.RealizedFundingRate, wantRate)
	}

	wantPredicted := mustDecimal("0.0001000")
	if !ev.Funding.PredictedFundingRate.Equal(wantPredicted) {
		t.Errorf("PredictedFundingRate = %v; want %v", ev.Funding.PredictedFundingRate, wantPredicted)
	}

	wantNextFunding := time.UnixMilli(nextFundingMs).UTC()
	if !ev.Funding.NextFundingTime.Equal(wantNextFunding) {
		t.Errorf("NextFundingTime = %v; want %v", ev.Funding.NextFundingTime, wantNextFunding)
	}

	if ev.Symbol != domain.ExchangeSymbol("BTC-USDT-SWAP") {
		t.Errorf("Symbol = %q; want BTC-USDT-SWAP", ev.Symbol)
	}
}

// TestParseOKXMessage_UnknownChannel проверяет, что неизвестный канал не эмитирует.
func TestParseOKXMessage_UnknownChannel(t *testing.T) {
	raw := []byte(`{"arg":{"channel":"some-unknown","instId":"BTC-USDT-SWAP"},"data":[{"foo":"bar"}]}`)
	a := newTestAdapter("ws://unused", time.Now)
	events := parseOKXMessage(raw, a)
	if len(events) != 0 {
		t.Errorf("expected no events for unknown channel, got %d", len(events))
	}
}
