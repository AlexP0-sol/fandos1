// WS-тесты для KuCoin Futures адаптера.
//
// Проверяется:
//   - bullet-token запрашивается при вызове SubscribePublic
//   - Тема подписки содержит XBTUSDTM
//   - tickerV2 message → PublicEvent ChannelBBO с корректными Decimal и Symbol
//   - tickerV1 message → PublicEvent ChannelTicker + ChannelBBO
//   - /contract/instrument funding.rate → ChannelFunding
//   - ctx.Done() → канал закрывается
//   - welcome/ack фреймы пропускаются (не эмитируют событий)
//   - buildKCTopics / parseKCMessage unit-тесты (без сети)
package kucoin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// ============================================================
// Инфраструктура тестов
// ============================================================

// wsUpgrader — общий upgrader для WS-тестов.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// bulletJSON строит тело ответа bullet-public с заданным wsURL.
func bulletJSON(wsURL string) string {
	// pingInterval 100 мс для быстрых тестов.
	return `{"code":"200000","data":{"token":"test-token","instanceServers":[{"endpoint":"` +
		wsURL + `","pingInterval":100,"pingTimeout":10000}]}}`
}

// testDoerWithBullet — HTTPDoer, обслуживающий POST /api/v1/bullet-public
// и проксирующий остальные запросы на httptest-сервер.
type testDoerWithBullet struct {
	bulletURL string // URL до WS-сервера (ws://...), подставляемый в bullet-ответ
	baseURL   string // базовый HTTP-адрес сервера (для остальных запросов)
	// захваченные запросы для ассертов
	capturedPaths []string
}

func (d *testDoerWithBullet) Do(ctx context.Context, req HTTPRequest) (int, []byte, error) {
	d.capturedPaths = append(d.capturedPaths, req.Path)

	if req.Path == "/api/v1/bullet-public" {
		body := bulletJSON(d.bulletURL)
		return 200, []byte(body), nil
	}

	// Проксируем на httptest-сервер (если нужно).
	url := d.baseURL + req.Path
	if req.Query != "" {
		url += "?" + req.Query
	}
	var bodyReader io.Reader
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = strings.NewReader(string(b))
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, nil
}

// newKuCoinTestAdapter создаёт адаптер с testDoerWithBullet.
// wsURL — WS-адрес тестового сервера.
func newKuCoinTestAdapter(wsURL, httpBase string) (*Adapter, *testDoerWithBullet) {
	doer := &testDoerWithBullet{
		bulletURL: wsURL,
		baseURL:   httpBase,
	}
	a, _ := New(Config{
		RESTBaseURL: httpBase,
		APIKey:      "test-key",
		APISecret:   "dGVzdC1zZWNyZXQ=", // base64 для совместимости с Signer
		Passphrase:  "test-pass",
		HTTPDoer:    doer,
		Clock:       time.Now,
	})
	return a, doer
}

// ============================================================
// TestSubscribePublic_BulletTokenRequested
// ============================================================

// TestSubscribePublic_BulletTokenRequested проверяет, что bullet-token запрашивается.
func TestSubscribePublic_BulletTokenRequested(t *testing.T) {
	// WS-сервер: отправляет welcome, затем держит соединение.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Отправляем welcome.
		conn.WriteJSON(map[string]string{"type": "welcome"})
		// Держим до закрытия.
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, doer := newKuCoinTestAdapter(wsURL, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "XBTUSDTM"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Ждём немного, чтобы goroutine запустилась.
	time.Sleep(50 * time.Millisecond)

	// Проверяем, что bullet был вызван.
	found := false
	for _, p := range doer.capturedPaths {
		if p == "/api/v1/bullet-public" {
			found = true
			break
		}
	}
	if !found {
		t.Error("bullet-public не был запрошен")
	}
}

// ============================================================
// TestSubscribePublic_SubscribeTopicXBTUSDTM
// ============================================================

// TestSubscribePublic_SubscribeTopicXBTUSDTM проверяет, что тема подписки содержит XBTUSDTM.
func TestSubscribePublic_SubscribeTopicXBTUSDTM(t *testing.T) {
	// Используем канал для передачи топика из handler-горутины в тест.
	topicCh := make(chan string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Отправляем welcome.
		conn.WriteJSON(map[string]string{"type": "welcome"})
		// Читаем subscribe сообщение.
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var sub kcSubMsg
		if err := json.Unmarshal(raw, &sub); err != nil {
			return
		}
		select {
		case topicCh <- sub.Topic:
		default:
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, _ := newKuCoinTestAdapter(wsURL, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "XBTUSDTM"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Ждём топика из канала.
	var receivedSub string
	select {
	case receivedSub = <-topicCh:
	case <-time.After(400 * time.Millisecond):
		t.Fatal("таймаут ожидания subscribe сообщения")
	}

	if !strings.Contains(receivedSub, "XBTUSDTM") {
		t.Errorf("topic подписки = %q, ожидал XBTUSDTM", receivedSub)
	}
	// BBO → tickerV2.
	if !strings.Contains(receivedSub, "tickerV2") {
		t.Errorf("topic = %q, ожидал tickerV2 для ChannelBBO", receivedSub)
	}
}

// ============================================================
// TestSubscribePublic_TickerV2BBO
// ============================================================

// TestSubscribePublic_TickerV2BBO проверяет парсинг tickerV2 → ChannelBBO.
func TestSubscribePublic_TickerV2BBO(t *testing.T) {
	const tickerV2Msg = `{
		"type": "message",
		"topic": "/contractMarket/tickerV2:XBTUSDTM",
		"subject": "tickerV2",
		"sn": 12345,
		"data": {
			"symbol": "XBTUSDTM",
			"sequence": 12345,
			"bestBidSize": 10,
			"bestBidPrice": "50000.5",
			"bestAskPrice": "50001.0",
			"bestAskSize": 5,
			"ts": 1700000000000000000
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteJSON(map[string]string{"type": "welcome"})
		// Читаем subscribe.
		conn.ReadMessage()
		// Отправляем tickerV2.
		conn.WriteMessage(websocket.TextMessage, []byte(tickerV2Msg))
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, _ := newKuCoinTestAdapter(wsURL, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "XBTUSDTM"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	timeout := time.After(1 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("канал закрылся до получения BBO события")
			}
			if ev.Channel != exchange.ChannelBBO {
				continue
			}
			if ev.OrderBook == nil {
				t.Fatal("BBO event: OrderBook == nil")
			}
			ob := ev.OrderBook
			if len(ob.Bids) == 0 {
				t.Fatal("BBO: нет bids")
			}
			if len(ob.Asks) == 0 {
				t.Fatal("BBO: нет asks")
			}
			// decimal.FromString нормализует trailing zeros: "50000.5" → "50000.5"
			wantBid := "50000.5"
			wantAsk := "50001"
			if ob.Bids[0].Price.String() != wantBid {
				t.Errorf("bid price = %v, want %v", ob.Bids[0].Price, wantBid)
			}
			if ob.Asks[0].Price.String() != wantAsk {
				t.Errorf("ask price = %v, want %v", ob.Asks[0].Price, wantAsk)
			}
			if ev.Symbol != "XBTUSDTM" {
				t.Errorf("Symbol = %v, want XBTUSDTM", ev.Symbol)
			}
			return
		case <-timeout:
			t.Fatal("таймаут ожидания BBO события")
		}
	}
}

// ============================================================
// TestSubscribePublic_TickerV1
// ============================================================

// TestSubscribePublic_TickerV1 проверяет парсинг tickerV1 → ChannelTicker + ChannelBBO.
func TestSubscribePublic_TickerV1(t *testing.T) {
	const tickerV1Msg = `{
		"type": "message",
		"topic": "/contractMarket/ticker:XBTUSDTM",
		"subject": "ticker",
		"data": {
			"symbol": "XBTUSDTM",
			"price": "86429.7",
			"bestBidPrice": "86429.6",
			"bestAskPrice": "86429.7",
			"bestBidSize": 112,
			"bestAskSize": 1578,
			"ts": 1740642161735000000
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteJSON(map[string]string{"type": "welcome"})
		conn.ReadMessage()
		conn.WriteMessage(websocket.TextMessage, []byte(tickerV1Msg))
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, _ := newKuCoinTestAdapter(wsURL, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "XBTUSDTM"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	gotTicker := false
	timeout := time.After(1 * time.Second)
	for !gotTicker {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("канал закрылся до получения Ticker события")
			}
			if ev.Channel == exchange.ChannelTicker {
				if ev.Ticker == nil {
					t.Fatal("Ticker event: Ticker == nil")
				}
				want := "86429.7"
				if ev.Ticker.LastPrice.String() != want {
					t.Errorf("LastPrice = %v, want %v", ev.Ticker.LastPrice, want)
				}
				gotTicker = true
			}
		case <-timeout:
			t.Fatal("таймаут ожидания Ticker события")
		}
	}
}

// ============================================================
// TestSubscribePublic_FundingRate
// ============================================================

// TestSubscribePublic_FundingRate проверяет парсинг /contract/instrument funding.rate → ChannelFunding.
func TestSubscribePublic_FundingRate(t *testing.T) {
	const fundingMsg = `{
		"type": "message",
		"topic": "/contract/instrument:XBTUSDTM",
		"subject": "funding.rate",
		"data": {
			"granularity": 28800000,
			"fundingRate": 0.000105,
			"timestamp": 1705982400000
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteJSON(map[string]string{"type": "welcome"})
		conn.ReadMessage()
		conn.WriteMessage(websocket.TextMessage, []byte(fundingMsg))
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, _ := newKuCoinTestAdapter(wsURL, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelFunding, Symbol: "XBTUSDTM"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	timeout := time.After(1 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("канал закрылся до получения Funding события")
			}
			if ev.Channel != exchange.ChannelFunding {
				continue
			}
			if ev.Funding == nil {
				t.Fatal("Funding event: Funding == nil")
			}
			// fundingRate 0.000105 из числа → Decimal.
			wantRate, _ := decimal.FromString("0.000105")
			if !ev.Funding.RealizedFundingRate.Equal(wantRate) {
				t.Errorf("FundingRate = %v, want %v", ev.Funding.RealizedFundingRate, wantRate)
			}
			if ev.Symbol != "XBTUSDTM" {
				t.Errorf("Symbol = %v, want XBTUSDTM", ev.Symbol)
			}
			// granularity 28800000 ms = 28800 s = 8h
			if ev.Funding.FundingIntervalSec != 28800 {
				t.Errorf("FundingIntervalSec = %v, want 28800", ev.Funding.FundingIntervalSec)
			}
			return
		case <-timeout:
			t.Fatal("таймаут ожидания Funding события")
		}
	}
}

// ============================================================
// TestSubscribePublic_CtxCancelClosesChannel
// ============================================================

// TestSubscribePublic_CtxCancelClosesChannel проверяет, что ctx.Cancel() закрывает канал.
func TestSubscribePublic_CtxCancelClosesChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.WriteJSON(map[string]string{"type": "welcome"})
		// Держим соединение.
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, _ := newKuCoinTestAdapter(wsURL, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "XBTUSDTM"},
	})
	if err != nil {
		cancel()
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Отменяем контекст через 50 мс.
	time.AfterFunc(50*time.Millisecond, cancel)

	// Ждём закрытия канала (не более 1 с).
	timeout := time.After(1 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // успешно
			}
		case <-timeout:
			t.Fatal("канал не закрылся после ctx.Cancel()")
		}
	}
}

// ============================================================
// TestSubscribePublic_SkipWelcomeAck
// ============================================================

// TestSubscribePublic_SkipWelcomeAck проверяет, что welcome/ack фреймы не порождают событий.
// Посылаем welcome + ack + pong, затем реальный tickerV2 — ожидаем только одно событие.
func TestSubscribePublic_SkipWelcomeAck(t *testing.T) {
	const tickerV2Msg = `{
		"type": "message",
		"topic": "/contractMarket/tickerV2:XBTUSDTM",
		"subject": "tickerV2",
		"data": {
			"symbol": "XBTUSDTM",
			"bestBidPrice": "60000.0",
			"bestAskPrice": "60001.0",
			"bestBidSize": 1,
			"bestAskSize": 1,
			"ts": 1700000000000000000
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// welcome
		conn.WriteJSON(map[string]string{"type": "welcome"})
		// Читаем subscribe.
		conn.ReadMessage()
		// Ack (должен быть пропущен).
		conn.WriteJSON(map[string]interface{}{"type": "ack", "id": "test-id"})
		// Pong (должен быть пропущен).
		conn.WriteJSON(map[string]string{"type": "pong"})
		// Настоящее сообщение.
		conn.WriteMessage(websocket.TextMessage, []byte(tickerV2Msg))
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a, _ := newKuCoinTestAdapter(wsURL, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "XBTUSDTM"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	timeout := time.After(1 * time.Second)
	eventCount := 0
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				if eventCount != 1 {
					t.Errorf("получено событий: %d, ожидалось: 1", eventCount)
				}
				return
			}
			if ev.Channel == exchange.ChannelBBO {
				eventCount++
			}
		case <-timeout:
			// Проверяем что получили ровно 1 BBO событие.
			if eventCount != 1 {
				t.Errorf("получено BBO событий: %d, ожидалось: 1", eventCount)
			}
			return
		}
	}
}

// ============================================================
// Unit тесты buildKCTopics
// ============================================================

func TestBuildKCTopics_BBO(t *testing.T) {
	subs := []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "XBTUSDTM"},
	}
	topics := buildKCTopics(subs)
	if len(topics) != 1 {
		t.Fatalf("len(topics) = %d, want 1", len(topics))
	}
	if topics[0] != "/contractMarket/tickerV2:XBTUSDTM" {
		t.Errorf("topic = %q, want /contractMarket/tickerV2:XBTUSDTM", topics[0])
	}
}

func TestBuildKCTopics_Ticker(t *testing.T) {
	subs := []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "XBTUSDTM"},
	}
	topics := buildKCTopics(subs)
	if len(topics) != 1 {
		t.Fatalf("len(topics) = %d, want 1", len(topics))
	}
	if topics[0] != "/contractMarket/ticker:XBTUSDTM" {
		t.Errorf("topic = %q, want /contractMarket/ticker:XBTUSDTM", topics[0])
	}
}

func TestBuildKCTopics_FundingAndMarkPriceDedup(t *testing.T) {
	// Funding и MarkPrice → один /contract/instrument топик.
	subs := []exchange.PublicSubscription{
		{Channel: exchange.ChannelFunding, Symbol: "XBTUSDTM"},
		{Channel: exchange.ChannelMarkPrice, Symbol: "XBTUSDTM"},
	}
	topics := buildKCTopics(subs)
	if len(topics) != 1 {
		t.Fatalf("len(topics) = %d, want 1 (dedup)", len(topics))
	}
	if topics[0] != "/contract/instrument:XBTUSDTM" {
		t.Errorf("topic = %q, want /contract/instrument:XBTUSDTM", topics[0])
	}
}

func TestBuildKCTopics_DedupBBO(t *testing.T) {
	// Два одинаковых BBO запроса → один топик.
	subs := []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "XBTUSDTM"},
		{Channel: exchange.ChannelBBO, Symbol: "XBTUSDTM"},
	}
	topics := buildKCTopics(subs)
	if len(topics) != 1 {
		t.Fatalf("len(topics) = %d, want 1 (dedup)", len(topics))
	}
}

// ============================================================
// Unit тест parseKCMessage — tickerV2 parser
// ============================================================

func TestParseKCMessage_TickerV2(t *testing.T) {
	rawData := `{
		"symbol": "XBTUSDTM",
		"sequence": 9999,
		"bestBidSize": 10,
		"bestBidPrice": "50000.1",
		"bestAskPrice": "50001.2",
		"bestAskSize": 5,
		"ts": 1700000000000000000
	}`

	msg := kcWSMessage{
		Type:    "message",
		Topic:   "/contractMarket/tickerV2:XBTUSDTM",
		Subject: "tickerV2",
		Data:    json.RawMessage(rawData),
	}

	a, _ := New(Config{
		RESTBaseURL: "http://localhost",
		APIKey:      "k",
		APISecret:   "dGVzdA==",
		Passphrase:  "p",
		HTTPDoer:    &noopDoer{},
		Clock:       time.Now,
	})

	events := parseKCMessage(msg, a)
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Channel != exchange.ChannelBBO {
		t.Errorf("Channel = %v, want ChannelBBO", ev.Channel)
	}
	if ev.OrderBook == nil {
		t.Fatal("OrderBook == nil")
	}
	ob := ev.OrderBook
	if len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		t.Fatal("bids/asks пустые")
	}
	wantBid := "50000.1"
	if ob.Bids[0].Price.String() != wantBid {
		t.Errorf("bid = %v, want %v", ob.Bids[0].Price, wantBid)
	}
	wantAsk := "50001.2"
	if ob.Asks[0].Price.String() != wantAsk {
		t.Errorf("ask = %v, want %v", ob.Asks[0].Price, wantAsk)
	}
	// ts=1700000000000000000 нс → 1700000000000 мс
	wantMS := int64(1700000000000)
	gotMS := ev.ExchangeTS.UnixMilli()
	if gotMS != wantMS {
		t.Errorf("ExchangeTS.UnixMilli() = %v, want %v", gotMS, wantMS)
	}
}

// ============================================================
// Unit тест parseKCMessage — инструментальный funding
// ============================================================

func TestParseKCMessage_FundingRate(t *testing.T) {
	rawData := `{
		"granularity": 28800000,
		"fundingRate": -0.0001,
		"timestamp": 1700000000000
	}`

	msg := kcWSMessage{
		Type:    "message",
		Topic:   "/contract/instrument:XBTUSDTM",
		Subject: "funding.rate",
		Data:    json.RawMessage(rawData),
	}

	a, _ := New(Config{
		RESTBaseURL: "http://localhost",
		APIKey:      "k",
		APISecret:   "dGVzdA==",
		Passphrase:  "p",
		HTTPDoer:    &noopDoer{},
		Clock:       time.Now,
	})

	events := parseKCMessage(msg, a)
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Channel != exchange.ChannelFunding {
		t.Errorf("Channel = %v, want ChannelFunding", ev.Channel)
	}
	if ev.Funding == nil {
		t.Fatal("Funding == nil")
	}
	wantRate, _ := decimal.FromString("-0.0001")
	if !ev.Funding.RealizedFundingRate.Equal(wantRate) {
		t.Errorf("FundingRate = %v, want %v", ev.Funding.RealizedFundingRate, wantRate)
	}
	if ev.Funding.FundingIntervalSec != 28800 {
		t.Errorf("FundingIntervalSec = %v, want 28800", ev.Funding.FundingIntervalSec)
	}
	if ev.Symbol != "XBTUSDTM" {
		t.Errorf("Symbol = %v, want XBTUSDTM", ev.Symbol)
	}
}

// ============================================================
// Unit тест parseKCMessage — mark price
// ============================================================

func TestParseKCMessage_MarkPrice(t *testing.T) {
	rawData := `{
		"markPrice": 90445.02,
		"indexPrice": 90445.02,
		"granularity": 1000,
		"timestamp": 1731899129000
	}`

	msg := kcWSMessage{
		Type:    "message",
		Topic:   "/contract/instrument:XBTUSDTM",
		Subject: "mark.index.price",
		Data:    json.RawMessage(rawData),
	}

	a, _ := New(Config{
		RESTBaseURL: "http://localhost",
		APIKey:      "k",
		APISecret:   "dGVzdA==",
		Passphrase:  "p",
		HTTPDoer:    &noopDoer{},
		Clock:       time.Now,
	})

	events := parseKCMessage(msg, a)
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Channel != exchange.ChannelMarkPrice {
		t.Errorf("Channel = %v, want ChannelMarkPrice", ev.Channel)
	}
	if ev.MarkPrice == nil {
		t.Fatal("MarkPrice == nil")
	}
	wantMP, _ := decimal.FromString("90445.02")
	if !ev.MarkPrice.MarkPrice.Equal(wantMP) {
		t.Errorf("MarkPrice = %v, want %v", ev.MarkPrice.MarkPrice, wantMP)
	}
}

// ============================================================
// Unit тест kcNsToTime
// ============================================================

func TestKcNsToTime(t *testing.T) {
	// 1700000000000000000 нс = 1700000000000 мс
	ts := kcNsToTime(json.Number("1700000000000000000"))
	wantMs := int64(1700000000000)
	if ts.UnixMilli() != wantMs {
		t.Errorf("UnixMilli = %v, want %v", ts.UnixMilli(), wantMs)
	}

	// Нулевое значение → zero time.
	ts2 := kcNsToTime(json.Number("0"))
	if !ts2.IsZero() {
		t.Errorf("ожидал zero time для ts=0, получил %v", ts2)
	}
}

// ============================================================
// noopDoer — HTTPDoer-заглушка для unit-тестов без сети
// ============================================================

type noopDoer struct{}

func (d *noopDoer) Do(_ context.Context, _ HTTPRequest) (int, []byte, error) {
	return 200, []byte(`{"code":"200000","data":{}}`), nil
}
