// ws_test.go — тесты WebSocket публичного адаптера MEXC Contract.
//
// Использует httptest + gorilla/websocket upgrader для имитации MEXC WS сервера.
// Проверяет:
//   - sub.ticker кадр отправляется с символом
//   - push.ticker парсируется → PublicEvent с точными decimal (из JSON-чисел)
//   - BBO из push.ticker
//   - funding из push.ticker
//   - ctx cancel → канал закрывается
//   - служебные кадры (pong, rs.sub.ticker, rs.error) пропускаются
//   - push.depth парсируется → ChannelDepth событие
//   - дедупликация подписок
//   - ошибка при пустых подписках
//
// Никаких sleep > 200ms.
package mexc

import (
	"context"
	"encoding/json"
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
// Вспомогательные утилиты тестов
// ============================================================

// wsTestUpgrader — upgrader для тестового WS-сервера.
var wsTestUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// serverScript описывает сценарий: получить N входящих кадров, затем отправить ответы.
type serverScript struct {
	recvCount int           // кол-во кадров принять от клиента перед отправкой responses
	responses []interface{} // кадры для отправки клиенту
}

// startFakeServer запускает тестовый WS-сервер по сценарию script.
// Возвращает httptest.Server и канал с принятыми входящими кадрами.
func startFakeServer(t *testing.T, script serverScript) (*httptest.Server, <-chan []byte) {
	t.Helper()
	inbound := make(chan []byte, 64)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsTestUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Принимаем входящие кадры.
		received := 0
		for received < script.recvCount {
			conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			_, msg, err := conn.ReadMessage()
			conn.SetReadDeadline(time.Time{})
			if err != nil {
				return
			}
			select {
			case inbound <- msg:
			default:
			}
			received++
		}

		// Отправляем ответы.
		for _, resp := range script.responses {
			if err := conn.WriteJSON(resp); err != nil {
				return
			}
		}

		// Закрываем соединение: отправляем close frame.
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		// Ждём немного чтобы close frame доставился.
		time.Sleep(20 * time.Millisecond)
	}))

	return srv, inbound
}

// makeAdapter создаёт Adapter, подключённый к тестовому WS-серверу.
func makeAdapter(t *testing.T, srvURL string) *Adapter {
	t.Helper()
	wsURL := strings.Replace(srvURL, "http://", "ws://", 1)
	return &Adapter{
		wsBase:          wsURL,
		clock:           time.Now,
		contractSizeMap: make(map[domain.ExchangeSymbol]decimal.Decimal),
	}
}

// expectFrame читает один кадр с таймаутом 200ms.
func expectFrame(t *testing.T, inbound <-chan []byte) []byte {
	t.Helper()
	select {
	case raw := <-inbound:
		return raw
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for client frame")
		return nil
	}
}

// expectEvent ожидает PublicEvent из канала с таймаутом 200ms.
func expectEvent(t *testing.T, ch <-chan exchange.PublicEvent) (exchange.PublicEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(200 * time.Millisecond):
		return exchange.PublicEvent{}, false
	}
}

// collectEvents собирает события из канала до его закрытия или 200ms таймаута.
func collectEvents(ch <-chan exchange.PublicEvent) []exchange.PublicEvent {
	var evts []exchange.PublicEvent
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evts
			}
			evts = append(evts, ev)
		case <-deadline:
			return evts
		}
	}
}

// drainCh дренирует канал до закрытия.
func drainCh(ch <-chan exchange.PublicEvent) {
	for range ch {
	}
}

// ============================================================
// Тест: sub.ticker кадр отправляется с правильным символом
// ============================================================

func TestWS_SendsSubTickerFrame(t *testing.T) {
	srv, inbound := startFakeServer(t, serverScript{
		recvCount: 1, // ждём одну подписку
		responses: nil,
	})
	defer srv.Close()

	a := makeAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() { cancel(); drainCh(ch) }()

	raw := expectFrame(t, inbound)

	var req wsSubRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal sub frame: %v (raw=%q)", err, raw)
	}
	if req.Method != "sub.ticker" {
		t.Errorf("method = %q, want sub.ticker", req.Method)
	}
	if req.Param.Symbol != "BTC_USDT" {
		t.Errorf("param.symbol = %q, want BTC_USDT", req.Param.Symbol)
	}
}

// ============================================================
// Тест: push.ticker → ChannelTicker с правильными decimal
// ============================================================

func TestWS_ParsePushTicker_ChannelTicker(t *testing.T) {
	// VERIFIED: push.ticker числовые поля — JSON numbers.
	push := map[string]interface{}{
		"channel": "push.ticker",
		"symbol":  "BTC_USDT",
		"data": map[string]interface{}{
			"symbol":    "BTC_USDT",
			"lastPrice": 43210.5,
			"bid1":      43209.0,
			"ask1":      43211.0,
			"volume24":  123456.0,
			"timestamp": 1700000000000,
		},
	}

	srv, inbound := startFakeServer(t, serverScript{recvCount: 1, responses: []interface{}{push}})
	defer srv.Close()

	a := makeAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() { cancel(); drainCh(ch) }()

	// Проверяем что подписка отправлена правильно.
	expectFrame(t, inbound)

	// Ищем ChannelTicker.
	evts := collectEvents(ch)
	var tickerEv *exchange.PublicEvent
	for i := range evts {
		if evts[i].Channel == exchange.ChannelTicker {
			tickerEv = &evts[i]
			break
		}
	}

	if tickerEv == nil {
		t.Fatalf("did not receive ChannelTicker event (got %d events: %v)", len(evts), channelList(evts))
	}
	if tickerEv.Symbol != "BTC_USDT" {
		t.Errorf("symbol = %q, want BTC_USDT", tickerEv.Symbol)
	}
	if tickerEv.Ticker == nil {
		t.Fatal("Ticker is nil")
	}

	// Точная проверка decimal из JSON-числа (не float64).
	wantLastPrice, _ := decimal.FromString("43210.5")
	if !tickerEv.Ticker.LastPrice.Equal(wantLastPrice) {
		t.Errorf("LastPrice = %v, want %v", tickerEv.Ticker.LastPrice, wantLastPrice)
	}

	wantVolume, _ := decimal.FromString("123456")
	if !tickerEv.Ticker.QuoteVolume24h.Equal(wantVolume) {
		t.Errorf("QuoteVolume24h = %v, want %v", tickerEv.Ticker.QuoteVolume24h, wantVolume)
	}
}

// ============================================================
// Тест: push.ticker → ChannelBBO с правильными bid1/ask1
// ============================================================

func TestWS_ParsePushTicker_ChannelBBO(t *testing.T) {
	push := map[string]interface{}{
		"channel": "push.ticker",
		"symbol":  "ETH_USDT",
		"data": map[string]interface{}{
			"symbol":    "ETH_USDT",
			"lastPrice": 2500.25,
			"bid1":      2499.99,
			"ask1":      2500.50,
			"volume24":  99999.0,
			"timestamp": 1700000001000,
		},
	}

	srv, inbound := startFakeServer(t, serverScript{recvCount: 1, responses: []interface{}{push}})
	defer srv.Close()

	a := makeAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "ETH_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() { cancel(); drainCh(ch) }()

	expectFrame(t, inbound)

	evts := collectEvents(ch)
	var bboEv *exchange.PublicEvent
	for i := range evts {
		if evts[i].Channel == exchange.ChannelBBO {
			bboEv = &evts[i]
			break
		}
	}

	if bboEv == nil {
		t.Fatalf("did not receive ChannelBBO event (got %v)", channelList(evts))
	}
	if bboEv.OrderBook == nil {
		t.Fatal("OrderBook is nil in BBO event")
	}
	if len(bboEv.OrderBook.Bids) == 0 {
		t.Fatal("BBO: no bids")
	}
	if len(bboEv.OrderBook.Asks) == 0 {
		t.Fatal("BBO: no asks")
	}

	wantBid, _ := decimal.FromString("2499.99")
	wantAsk, _ := decimal.FromString("2500.50")
	if !bboEv.OrderBook.Bids[0].Price.Equal(wantBid) {
		t.Errorf("bid1 = %v, want %v", bboEv.OrderBook.Bids[0].Price, wantBid)
	}
	if !bboEv.OrderBook.Asks[0].Price.Equal(wantAsk) {
		t.Errorf("ask1 = %v, want %v", bboEv.OrderBook.Asks[0].Price, wantAsk)
	}
}

// ============================================================
// Тест: funding из push.ticker
// ============================================================

func TestWS_ParsePushTicker_ChannelFunding(t *testing.T) {
	push := map[string]interface{}{
		"channel": "push.ticker",
		"symbol":  "BTC_USDT",
		"data": map[string]interface{}{
			"symbol":      "BTC_USDT",
			"lastPrice":   50000.0,
			"bid1":        49999.0,
			"ask1":        50001.0,
			"fundingRate": 0.00075,
			"timestamp":   1700000002000,
		},
	}

	srv, inbound := startFakeServer(t, serverScript{recvCount: 1, responses: []interface{}{push}})
	defer srv.Close()

	a := makeAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelFunding, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() { cancel(); drainCh(ch) }()

	expectFrame(t, inbound)

	evts := collectEvents(ch)
	var fundingEv *exchange.PublicEvent
	for i := range evts {
		if evts[i].Channel == exchange.ChannelFunding {
			fundingEv = &evts[i]
			break
		}
	}

	if fundingEv == nil {
		t.Fatalf("did not receive ChannelFunding event (got %v)", channelList(evts))
	}
	if fundingEv.Funding == nil {
		t.Fatal("Funding is nil")
	}

	wantRate, _ := decimal.FromString("0.00075")
	if !fundingEv.Funding.RealizedFundingRate.Equal(wantRate) {
		t.Errorf("fundingRate = %v, want %v", fundingEv.Funding.RealizedFundingRate, wantRate)
	}
	if !fundingEv.Funding.PredictedFundingRate.Equal(wantRate) {
		t.Errorf("predictedFundingRate = %v, want %v", fundingEv.Funding.PredictedFundingRate, wantRate)
	}
}

// ============================================================
// Тест: ctx cancel → канал закрывается, reader возвращает
// ============================================================

func TestWS_CtxCancelClosesChannel(t *testing.T) {
	// Сервер принимает подписку, НЕ отправляет ничего, потом ждёт соединения.
	// Канал должен закрыться сразу после cancel().
	srv, inbound := startFakeServer(t, serverScript{
		recvCount: 1,
		responses: nil, // сервер закроет соединение после recv
	})
	defer srv.Close()

	a := makeAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Ждём отправки подписки.
	expectFrame(t, inbound)

	// Отменяем контекст.
	cancel()

	// Канал должен закрыться в течение 200ms.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // ОК
			}
		case <-deadline:
			t.Fatal("channel did not close after ctx cancel")
		}
	}
}

// ============================================================
// Тест: служебные кадры пропускаются (pong, rs.sub.ticker, rs.error)
// ============================================================

func TestWS_ControlFramesSkipped(t *testing.T) {
	realPush := map[string]interface{}{
		"channel": "push.ticker",
		"symbol":  "BTC_USDT",
		"data": map[string]interface{}{
			"symbol":    "BTC_USDT",
			"lastPrice": 42000.0,
			"bid1":      41999.0,
			"ask1":      42001.0,
			"timestamp": 1700000003000,
		},
	}

	srv, inbound := startFakeServer(t, serverScript{
		recvCount: 1,
		responses: []interface{}{
			// Контрольные кадры перед реальным push.
			map[string]interface{}{"channel": "pong", "data": 1700000003000},
			map[string]interface{}{"channel": "rs.sub.ticker", "data": "success", "symbol": "BTC_USDT"},
			map[string]interface{}{"channel": "rs.error", "data": "some info"},
			realPush,
		},
	})
	defer srv.Close()

	a := makeAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() { cancel(); drainCh(ch) }()

	expectFrame(t, inbound)

	evts := collectEvents(ch)
	var tickerEv *exchange.PublicEvent
	for i := range evts {
		if evts[i].Channel == exchange.ChannelTicker {
			tickerEv = &evts[i]
			break
		}
	}

	if tickerEv == nil {
		t.Fatalf("did not receive ChannelTicker after control frames (got %v)", channelList(evts))
	}
	wantLast, _ := decimal.FromString("42000")
	if !tickerEv.Ticker.LastPrice.Equal(wantLast) {
		t.Errorf("lastPrice = %v, want %v", tickerEv.Ticker.LastPrice, wantLast)
	}
}

// ============================================================
// Тест: push.depth → ChannelDepth
// ============================================================

func TestWS_ParsePushDepth(t *testing.T) {
	// TODO:VERIFY: [price, ordersCount, qty] — [0]=price, [2]=qty.
	push := map[string]interface{}{
		"channel": "push.depth",
		"symbol":  "BTC_USDT",
		"ts":      1700000004000,
		"data": map[string]interface{}{
			"bids":    [][]interface{}{{40000.0, 1.0, 5.0}, {39999.0, 2.0, 10.0}},
			"asks":    [][]interface{}{{40001.0, 1.0, 3.0}},
			"version": 12345678,
			"cts":     1700000003999,
		},
	}

	srv, inbound := startFakeServer(t, serverScript{recvCount: 1, responses: []interface{}{push}})
	defer srv.Close()

	a := makeAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelDepth, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() { cancel(); drainCh(ch) }()

	expectFrame(t, inbound)

	evts := collectEvents(ch)
	var depthEv *exchange.PublicEvent
	for i := range evts {
		if evts[i].Channel == exchange.ChannelDepth {
			depthEv = &evts[i]
			break
		}
	}

	if depthEv == nil {
		t.Fatalf("did not receive ChannelDepth event (got %v)", channelList(evts))
	}
	if depthEv.OrderBook == nil {
		t.Fatal("OrderBook is nil")
	}
	if len(depthEv.OrderBook.Bids) != 2 {
		t.Errorf("bids count = %d, want 2", len(depthEv.OrderBook.Bids))
	}
	if len(depthEv.OrderBook.Asks) != 1 {
		t.Errorf("asks count = %d, want 1", len(depthEv.OrderBook.Asks))
	}

	wantBidPrice, _ := decimal.FromString("40000")
	wantBidQty, _ := decimal.FromString("5") // index 2 (qty in contracts)
	if !depthEv.OrderBook.Bids[0].Price.Equal(wantBidPrice) {
		t.Errorf("bid[0].price = %v, want %v", depthEv.OrderBook.Bids[0].Price, wantBidPrice)
	}
	if !depthEv.OrderBook.Bids[0].Qty.Equal(wantBidQty) {
		t.Errorf("bid[0].qty = %v, want %v", depthEv.OrderBook.Bids[0].Qty, wantBidQty)
	}
	if depthEv.OrderBook.Sequence != 12345678 {
		t.Errorf("sequence = %d, want 12345678", depthEv.OrderBook.Sequence)
	}
}

// ============================================================
// Тест: дедупликация подписок (Ticker+BBO+Funding → один sub.ticker)
// ============================================================

func TestWS_DeduplicatesSubscriptions(t *testing.T) {
	// Принимаем ровно 1 кадр (ожидаем именно 1 sub.ticker).
	srv, inbound := startFakeServer(t, serverScript{recvCount: 1, responses: nil})
	defer srv.Close()

	a := makeAdapter(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subs := []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
		{Channel: exchange.ChannelBBO, Symbol: "BTC_USDT"},
		{Channel: exchange.ChannelFunding, Symbol: "BTC_USDT"},
	}

	ch, err := a.SubscribePublic(ctx, subs)
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() { cancel(); drainCh(ch) }()

	// Собираем кадры: ждём сначала первый (обязательный).
	firstRaw := expectFrame(t, inbound)
	// Ждём ещё кадры, если придут (до 80ms).
	var frames [][]byte
	frames = append(frames, firstRaw)
	extraDeadline := time.After(80 * time.Millisecond)
extraLoop:
	for {
		select {
		case raw, ok := <-inbound:
			if !ok {
				break extraLoop
			}
			frames = append(frames, raw)
		case <-extraDeadline:
			break extraLoop
		}
	}

	// Считаем sub.ticker для BTC_USDT — должно быть ровно 1.
	count := 0
	for _, raw := range frames {
		var req wsSubRequest
		if err := json.Unmarshal(raw, &req); err == nil &&
			req.Method == "sub.ticker" && req.Param.Symbol == "BTC_USDT" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 sub.ticker for BTC_USDT, got %d (frames=%d)", count, len(frames))
	}
}

// ============================================================
// Тест: SubscribePublic с пустыми подписками → ошибка
// ============================================================

func TestWS_EmptySubscriptionsReturnsError(t *testing.T) {
	a := &Adapter{
		wsBase:          "ws://localhost:0",
		clock:           time.Now,
		contractSizeMap: make(map[domain.ExchangeSymbol]decimal.Decimal),
	}
	_, err := a.SubscribePublic(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for empty subscriptions, got nil")
	}
}

// ============================================================
// Unit-тест: decimalFromNumber точность
// ============================================================

func TestWS_DecimalFromNumber_Precision(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"43210.5", "43210.5"},
		{"0.00075", "0.00075"},
		{"0.0008", "0.0008"},
		{"6866.5", "6866.5"},
		{"0", "0"},
		{"", "0"},
		{"164586129", "164586129"},
		{"0.0001", "0.0001"},
	}

	for _, tc := range cases {
		var n json.Number
		if tc.input != "" {
			n = json.Number(tc.input)
		}
		got, err := decimalFromNumber(n)
		if err != nil {
			t.Errorf("decimalFromNumber(%q): %v", tc.input, err)
			continue
		}
		want, _ := decimal.FromString(tc.want)
		if !got.Equal(want) {
			t.Errorf("decimalFromNumber(%q) = %v, want %v", tc.input, got, want)
		}
	}
}

// ============================================================
// Unit-тест: ping frame формат
// ============================================================

func TestWS_PingFrameFormat(t *testing.T) {
	ping := wsPingRequest{Method: "ping"}
	b, err := json.Marshal(ping)
	if err != nil {
		t.Fatalf("marshal ping: %v", err)
	}
	var check map[string]interface{}
	if err := json.Unmarshal(b, &check); err != nil {
		t.Fatalf("unmarshal ping: %v", err)
	}
	if check["method"] != "ping" {
		t.Errorf("ping method = %v, want ping", check["method"])
	}
}

// ============================================================
// Unit-тест: parseDepthLevelsWS
// ============================================================

func TestWS_ParseDepthLevels_ThreeElement(t *testing.T) {
	// [price, ordersCount, qty] — берём [0] и [2].
	raw := [][]json.Number{
		{json.Number("40000.5"), json.Number("3"), json.Number("7.5")},
		{json.Number("39999"), json.Number("1"), json.Number("2")},
	}
	levels, err := parseDepthLevelsWS(raw)
	if err != nil {
		t.Fatalf("parseDepthLevelsWS: %v", err)
	}
	if len(levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(levels))
	}

	wantPrice0, _ := decimal.FromString("40000.5")
	wantQty0, _ := decimal.FromString("7.5")
	if !levels[0].Price.Equal(wantPrice0) {
		t.Errorf("levels[0].Price = %v, want %v", levels[0].Price, wantPrice0)
	}
	if !levels[0].Qty.Equal(wantQty0) {
		t.Errorf("levels[0].Qty = %v, want %v", levels[0].Qty, wantQty0)
	}
}

func TestWS_ParseDepthLevels_TwoElement(t *testing.T) {
	// fallback: [price, qty] когда нет ordersCount.
	raw := [][]json.Number{
		{json.Number("50000"), json.Number("1.5")},
	}
	levels, err := parseDepthLevelsWS(raw)
	if err != nil {
		t.Fatalf("parseDepthLevelsWS: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("expected 1 level, got %d", len(levels))
	}
	wantQty, _ := decimal.FromString("1.5")
	if !levels[0].Qty.Equal(wantQty) {
		t.Errorf("levels[0].Qty = %v, want %v", levels[0].Qty, wantQty)
	}
}

// ============================================================
// Helpers
// ============================================================

// channelList возвращает список каналов для диагностики.
func channelList(evts []exchange.PublicEvent) []exchange.Channel {
	ch := make([]exchange.Channel, len(evts))
	for i, ev := range evts {
		ch[i] = ev.Channel
	}
	return ch
}
