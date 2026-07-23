// ws_test.go — тесты WebSocket-реализации Gate.io V4 futures.
//
// Используем httptest + gorilla/websocket upgrader для эмуляции Gate.io WS-сервера.
// Каждый тест самодостаточен. Нет sleep > 200ms.
package gate

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
// WebSocket upgrader для тестов
// ============================================================

var testWsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// wsURLFromHTTP конвертирует http:// URL в ws://.
func wsURLFromHTTP(u string) string {
	return strings.Replace(u, "http://", "ws://", 1)
}

// newWSTestAdapter создаёт Adapter с wsBase = wsURL для тестов WS.
func newWSTestAdapter(wsURL string) *Adapter {
	return &Adapter{
		restBase:      "http://localhost",
		wsBase:        wsURL,
		signer:        NewSigner("key", []byte("secret")),
		http:          nil, // не нужен для WS-тестов
		clock:         time.Now,
		orderMap:      make(map[string]string),
		contractCache: make(map[string]decimal.Decimal),
	}
}

// ============================================================
// Test: subscribe frame содержит channel=futures.tickers и payload=[contract]
// ============================================================

func TestGateWS_SubscribeFrame_Tickers(t *testing.T) {
	// Канал для получения сообщений от клиента.
	serverMsgCh := make(chan []byte, 16)
	// Сигнал готовности сервера (upgrade выполнен).
	serverReadyCh := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		close(serverReadyCh) // upgrade complete
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case serverMsgCh <- msg:
			default:
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()

	// Ждём upgrade.
	select {
	case <-serverReadyCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("server did not upgrade in time")
	}

	// Ждём subscribe-фрейм.
	var raw []byte
	select {
	case raw = <-serverMsgCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("did not receive subscribe frame in time")
	}

	var req gateWsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal subscribe frame: %v", err)
	}

	if req.Channel != "futures.tickers" {
		t.Errorf("channel = %q, want futures.tickers", req.Channel)
	}
	if req.Event != "subscribe" {
		t.Errorf("event = %q, want subscribe", req.Event)
	}
	if len(req.Payload) == 0 || req.Payload[0] != "BTC_USDT" {
		t.Errorf("payload = %v, want [BTC_USDT]", req.Payload)
	}
	if req.Time == 0 {
		t.Error("time field is 0")
	}
}

// ============================================================
// Test: subscribe frame для ChannelDepth содержит futures.order_book + контракт
// ============================================================

func TestGateWS_SubscribeFrame_OrderBook(t *testing.T) {
	serverMsgCh := make(chan []byte, 16)
	serverReadyCh := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		close(serverReadyCh)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case serverMsgCh <- msg:
			default:
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelDepth, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()

	select {
	case <-serverReadyCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("server did not upgrade in time")
	}

	var raw []byte
	select {
	case raw = <-serverMsgCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("did not receive subscribe frame in time")
	}

	var req gateWsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if req.Channel != "futures.order_book" {
		t.Errorf("channel = %q, want futures.order_book", req.Channel)
	}
	if len(req.Payload) < 1 || req.Payload[0] != "BTC_USDT" {
		t.Errorf("payload[0] = %v, want BTC_USDT", req.Payload)
	}
}

// ============================================================
// Test: tickers update → ChannelTicker с корректными decimal last/mark
// ============================================================

func TestGateWS_TickersUpdate_Ticker(t *testing.T) {
	// serverSendCh: тест отправляет сообщения серверу → он пересылает клиенту.
	serverSendCh := make(chan interface{}, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Поглощаем входящие сообщения от клиента (не нужны для этого теста).
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		// Отправляем всё из serverSendCh клиенту.
		for msg := range serverSendCh {
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()

	// Даём время на установку соединения и отправку subscribe.
	time.Sleep(50 * time.Millisecond)

	// Отправляем ack подписки (должен быть пропущен).
	serverSendCh <- map[string]interface{}{
		"time":    time.Now().Unix(),
		"channel": "futures.tickers",
		"event":   "subscribe",
		"error":   nil,
		"result":  nil,
	}

	// Отправляем tickers update.
	tsMs := time.Now().UnixMilli()
	serverSendCh <- map[string]interface{}{
		"time":    time.Now().Unix(),
		"time_ms": tsMs,
		"channel": "futures.tickers",
		"event":   "update",
		"error":   nil,
		"result": []map[string]interface{}{
			{
				"contract":                "BTC_USDT",
				"last":                    "65432.1",
				"mark_price":              "65400.5",
				"index_price":             "65390.0",
				"funding_rate":            "0.0001",
				"funding_rate_indicative": "0.00015",
				"volume_24h_settle":       "123456",
			},
		},
	}

	// Ждём события ChannelTicker.
	wantLast, _ := decimal.FromString("65432.1")
	wantMark, _ := decimal.FromString("65400.5")

	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed unexpectedly")
			}
			if ev.Channel != exchange.ChannelTicker {
				continue
			}
			if ev.Ticker == nil {
				t.Fatal("Ticker is nil")
			}
			if !ev.Ticker.LastPrice.Equal(wantLast) {
				t.Errorf("LastPrice = %v, want %v", ev.Ticker.LastPrice, wantLast)
			}
			if !ev.Ticker.MarkPrice.Equal(wantMark) {
				t.Errorf("MarkPrice = %v, want %v", ev.Ticker.MarkPrice, wantMark)
			}
			if ev.Symbol != "BTC_USDT" {
				t.Errorf("Symbol = %q, want BTC_USDT", ev.Symbol)
			}
			close(serverSendCh)
			return
		case <-deadline:
			t.Fatal("timeout waiting for ChannelTicker event")
		}
	}
}

// ============================================================
// Test: tickers update → ChannelFunding с корректным funding_rate
// ============================================================

func TestGateWS_TickersUpdate_Funding(t *testing.T) {
	serverSendCh := make(chan interface{}, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for msg := range serverSendCh {
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelFunding, Symbol: "ETH_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()

	time.Sleep(50 * time.Millisecond)

	serverSendCh <- map[string]interface{}{
		"time":    time.Now().Unix(),
		"channel": "futures.tickers",
		"event":   "update",
		"error":   nil,
		"result": []map[string]interface{}{
			{
				"contract":          "ETH_USDT",
				"last":              "3200.0",
				"mark_price":        "3199.5",
				"index_price":       "3198.0",
				"funding_rate":      "-0.0002",
				"volume_24h_settle": "55000",
			},
		},
	}

	wantRate, _ := decimal.FromString("-0.0002")

	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed")
			}
			if ev.Channel != exchange.ChannelFunding {
				continue
			}
			if ev.Funding == nil {
				t.Fatal("Funding is nil")
			}
			if !ev.Funding.RealizedFundingRate.Equal(wantRate) {
				t.Errorf("FundingRate = %v, want %v", ev.Funding.RealizedFundingRate, wantRate)
			}
			if ev.Symbol != "ETH_USDT" {
				t.Errorf("Symbol = %q, want ETH_USDT", ev.Symbol)
			}
			close(serverSendCh)
			return
		case <-deadline:
			t.Fatal("timeout waiting for ChannelFunding event")
		}
	}
}

// ============================================================
// Test: ack и error фреймы пропускаются (не emit в ch)
// ============================================================

func TestGateWS_AckAndErrorFrames_Skipped(t *testing.T) {
	serverSendCh := make(chan interface{}, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for msg := range serverSendCh {
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()

	time.Sleep(50 * time.Millisecond)

	// Отправляем ack.
	serverSendCh <- map[string]interface{}{
		"time": time.Now().Unix(), "channel": "futures.tickers",
		"event": "subscribe", "error": nil, "result": nil,
	}
	// Отправляем error frame.
	serverSendCh <- map[string]interface{}{
		"time": time.Now().Unix(), "channel": "futures.tickers",
		"event":  "subscribe",
		"error":  map[string]interface{}{"code": 1, "message": "invalid contract"},
		"result": nil,
	}
	// Отправляем pong.
	serverSendCh <- map[string]interface{}{
		"time": time.Now().Unix(), "channel": "futures.pong",
		"event": "", "error": nil, "result": nil,
	}

	// Проверяем что никаких событий нет за 100 мс.
	select {
	case ev := <-ch:
		t.Errorf("unexpected event: channel=%s", ev.Channel)
	case <-time.After(100 * time.Millisecond):
		// Ожидаемо — событий нет.
	}
	close(serverSendCh)
}

// ============================================================
// Test: ctx cancel → канал закрывается, reader returns
// ============================================================

func TestGateWS_CtxCancel_ClosesChannel(t *testing.T) {
	connReadyCh := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		close(connReadyCh)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Ждём upgrade.
	select {
	case <-connReadyCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("server did not upgrade")
	}

	// Отменяем контекст.
	cancel()

	// Канал должен закрыться.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // success
			}
		case <-deadline:
			t.Fatal("channel not closed after ctx cancel")
		}
	}
}

// ============================================================
// Test: server-side connection close → канал закрывается
// ============================================================

func TestGateWS_ServerClose_ClosesChannel(t *testing.T) {
	serverCloseCh := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Дожидаемся сигнала и закрываем.
		<-serverCloseCh
		conn.Close()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()

	// Даём время на установку соединения.
	time.Sleep(50 * time.Millisecond)

	// Закрываем сервер со стороны сервера.
	close(serverCloseCh)

	// Канал должен закрыться.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // success
			}
		case <-deadline:
			t.Fatal("channel not closed after server close")
		}
	}
}

// ============================================================
// Test: order_book update → ChannelDepth event
// ============================================================

func TestGateWS_OrderBookUpdate_Depth(t *testing.T) {
	serverSendCh := make(chan interface{}, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for msg := range serverSendCh {
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelDepth, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()

	time.Sleep(50 * time.Millisecond)

	serverSendCh <- map[string]interface{}{
		"time":    time.Now().Unix(),
		"channel": "futures.order_book",
		"event":   "all",
		"error":   nil,
		"result": map[string]interface{}{
			"t":        time.Now().UnixMilli(),
			"contract": "BTC_USDT",
			"id":       1234,
			"asks": []map[string]interface{}{
				{"p": "65500.0", "s": "10"},
				{"p": "65510.0", "s": "5"},
			},
			"bids": []map[string]interface{}{
				{"p": "65490.0", "s": "8"},
				{"p": "65480.0", "s": "12"},
			},
		},
	}

	wantAsk0, _ := decimal.FromString("65500.0")
	wantBid0, _ := decimal.FromString("65490.0")

	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed")
			}
			if ev.Channel != exchange.ChannelDepth {
				continue
			}
			if ev.OrderBook == nil {
				t.Fatal("OrderBook is nil")
			}
			ob := ev.OrderBook
			if len(ob.Asks) != 2 {
				t.Errorf("len(Asks) = %d, want 2", len(ob.Asks))
			}
			if len(ob.Bids) != 2 {
				t.Errorf("len(Bids) = %d, want 2", len(ob.Bids))
			}
			if !ob.Asks[0].Price.Equal(wantAsk0) {
				t.Errorf("Asks[0].Price = %v, want %v", ob.Asks[0].Price, wantAsk0)
			}
			if !ob.Bids[0].Price.Equal(wantBid0) {
				t.Errorf("Bids[0].Price = %v, want %v", ob.Bids[0].Price, wantBid0)
			}
			if ob.Symbol != "BTC_USDT" {
				t.Errorf("Symbol = %q, want BTC_USDT", ob.Symbol)
			}
			close(serverSendCh)
			return
		case <-deadline:
			t.Fatal("timeout waiting for ChannelDepth event")
		}
	}
}

// ============================================================
// Test: числовые (не строковые) поля → декодируются без float64
// ============================================================

func TestGateWS_TickersUpdate_NumericFields(t *testing.T) {
	serverSendCh := make(chan []byte, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testWsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for raw := range serverSendCh {
			if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := newWSTestAdapter(wsURLFromHTTP(srv.URL))
	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	defer func() {
		cancel()
		for range ch {
		}
	}()

	time.Sleep(50 * time.Millisecond)

	// Числовые значения (не строки) — проверяем robustness.
	rawMsg := `{"time":` + decimal.FromInt(time.Now().Unix()).String() + `,"channel":"futures.tickers","event":"update","error":null,"result":[{"contract":"BTC_USDT","last":66000,"mark_price":65990,"index_price":65980,"funding_rate":"0.00005","volume_24h_settle":"200000"}]}`
	serverSendCh <- []byte(rawMsg)

	wantLast, _ := decimal.FromString("66000")

	deadline := time.After(150 * time.Millisecond)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed")
			}
			if ev.Channel != exchange.ChannelTicker {
				continue
			}
			if ev.Ticker == nil {
				t.Fatal("Ticker is nil")
			}
			if !ev.Ticker.LastPrice.Equal(wantLast) {
				t.Errorf("LastPrice = %v, want %v (numeric fields)", ev.Ticker.LastPrice, wantLast)
			}
			close(serverSendCh)
			return
		case <-deadline:
			t.Fatal("timeout waiting for ChannelTicker event (numeric fields)")
		}
	}
}

// ============================================================
// Test: SubscribePrivate всё ещё возвращает ErrWSNotImplemented
// ============================================================

func TestGateWS_SubscribePrivate_Stub(t *testing.T) {
	a := &Adapter{
		wsBase: "wss://localhost",
		clock:  time.Now,
	}
	_, err := a.SubscribePrivate(context.Background(), domain.CredentialRef{})
	if err == nil {
		t.Fatal("expected error from SubscribePrivate, got nil")
	}
	if !strings.Contains(err.Error(), "не реализован") {
		t.Errorf("error %q does not mention 'не реализован'", err.Error())
	}
}
