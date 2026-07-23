package bitget

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
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// ============================================================
// Helpers: fake Bitget WS server
// ============================================================

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// fakeServer runs a fake Bitget public WebSocket server.
// serverFunc receives the accepted conn and drives the test scenario.
func fakeServer(t *testing.T, serverFunc func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		serverFunc(conn)
	}))
	return srv
}

// wsURLFromHTTP converts an httptest server URL (http://...) to ws://...
func wsURLFromHTTP(u string) string {
	return "ws" + strings.TrimPrefix(u, "http")
}

// makeAdapter builds a minimal Adapter pointing at a custom WS URL.
// We bypass New() to avoid needing real credentials for WS tests.
func makeAdapter(wsURL string) *Adapter {
	return &Adapter{
		wsURL: wsURL,
		clock: time.Now,
	}
}

// readSubscribeMsg reads and decodes the subscribe message the client sends.
func readSubscribeMsg(t *testing.T, conn *websocket.Conn) wsSubscribeMsg {
	t.Helper()
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("readSubscribeMsg: %v", err)
	}
	var msg wsSubscribeMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode subscribe msg: %v", err)
	}
	return msg
}

// sendJSON sends a JSON-encoded value as a text message.
func sendJSON(t *testing.T, conn *websocket.Conn, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("sendJSON marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Logf("sendJSON write (may be ok if client gone): %v", err)
	}
}

// drainCh collects up to n events from ch with a per-event timeout of deadline.
func drainCh(ch <-chan exchange.PublicEvent, n int, deadline time.Duration) []exchange.PublicEvent {
	var events []exchange.PublicEvent
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		if len(events) >= n {
			return events
		}
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timer.C:
			return events
		}
	}
}

// ============================================================
// Test: subscribe args are well-formed
// ============================================================

func TestSubscribePublic_SubscribeArgsFormat(t *testing.T) {
	subsDone := make(chan wsSubscribeMsg, 1)

	srv := fakeServer(t, func(conn *websocket.Conn) {
		msg := readSubscribeMsg(t, conn)
		subsDone <- msg
		// Keep conn open until client disconnects.
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer srv.Close()

	a := makeAdapter(wsURLFromHTTP(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	subs := []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTCUSDT"},
		{Channel: exchange.ChannelBBO, Symbol: "BTCUSDT"},     // dedup: same as ticker
		{Channel: exchange.ChannelFunding, Symbol: "BTCUSDT"}, // dedup
		{Channel: exchange.ChannelDepth, Symbol: "BTCUSDT"},
	}
	ch, err := a.SubscribePublic(ctx, subs)
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	_ = ch

	select {
	case msg := <-subsDone:
		if msg.Op != "subscribe" {
			t.Errorf("op = %q; want subscribe", msg.Op)
		}
		// Expect dedup: ticker+bbo+funding → one "ticker" arg + one "books5" arg.
		if len(msg.Args) != 2 {
			t.Errorf("args len = %d; want 2 (ticker + books5)", len(msg.Args))
		}
		for _, arg := range msg.Args {
			if arg.InstType != "USDT-FUTURES" {
				t.Errorf("instType = %q; want USDT-FUTURES", arg.InstType)
			}
			if arg.InstID != "BTCUSDT" {
				t.Errorf("instId = %q; want BTCUSDT", arg.InstID)
			}
			if arg.Channel != "ticker" && arg.Channel != "books5" {
				t.Errorf("channel = %q; want ticker or books5", arg.Channel)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for subscribe msg")
	}
}

// ============================================================
// Test: ticker snapshot emits Ticker + BBO + Funding events
// ============================================================

func TestSubscribePublic_TickerSnapshotEmitsEvents(t *testing.T) {
	tickerPush := map[string]interface{}{
		"action": "snapshot",
		"arg": map[string]string{
			"instType": "USDT-FUTURES",
			"channel":  "ticker",
			"instId":   "BTCUSDT",
		},
		"data": []map[string]string{
			{
				"symbol":          "BTCUSDT",
				"lastPr":          "30000.5",
				"bidPr":           "30000.0",
				"askPr":           "30001.0",
				"markPrice":       "30001.5",
				"indexPrice":      "30002.0",
				"fundingRate":     "0.0001",
				"nextFundingTime": "1700000000000",
				"usdtVolume":      "123456789.0",
				"ts":              "1695794098000",
			},
		},
		"ts": int64(1695794098000),
	}

	srv := fakeServer(t, func(conn *websocket.Conn) {
		readSubscribeMsg(t, conn) // consume subscribe
		// Send subscribe ack (should be ignored).
		sendJSON(t, conn, map[string]string{"event": "subscribe"})
		// Send ticker snapshot.
		sendJSON(t, conn, tickerPush)
		// Keep alive.
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer srv.Close()

	a := makeAdapter(wsURLFromHTTP(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTCUSDT"},
		{Channel: exchange.ChannelBBO, Symbol: "BTCUSDT"},
		{Channel: exchange.ChannelFunding, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// We expect 3 events: Ticker, BBO, Funding.
	events := drainCh(ch, 3, 3*time.Second)
	if len(events) < 3 {
		t.Fatalf("got %d events; want at least 3", len(events))
	}

	byChannel := make(map[exchange.Channel]exchange.PublicEvent)
	for _, ev := range events {
		byChannel[ev.Channel] = ev
	}

	// --- ChannelTicker ---
	tickerEv, ok := byChannel[exchange.ChannelTicker]
	if !ok {
		t.Fatal("no ChannelTicker event")
	}
	if tickerEv.Ticker == nil {
		t.Fatal("Ticker is nil")
	}
	if tickerEv.Symbol != "BTCUSDT" {
		t.Errorf("symbol = %q; want BTCUSDT", tickerEv.Symbol)
	}
	if tickerEv.Ticker.LastPrice.String() != "30000.5" {
		t.Errorf("lastPrice = %s; want 30000.5", tickerEv.Ticker.LastPrice)
	}
	if tickerEv.Ticker.MarkPrice.String() != "30001.5" {
		t.Errorf("markPrice = %s; want 30001.5", tickerEv.Ticker.MarkPrice)
	}
	if tickerEv.Ticker.IndexPrice.String() != "30002" {
		t.Errorf("indexPrice = %s; want 30002", tickerEv.Ticker.IndexPrice)
	}
	// Verify no float64 was used: decimal must be exact string representation.
	if tickerEv.Ticker.QuoteVolume24h.String() != "123456789" {
		t.Errorf("volume = %s; want 123456789", tickerEv.Ticker.QuoteVolume24h)
	}

	// --- ChannelBBO ---
	bboEv, ok := byChannel[exchange.ChannelBBO]
	if !ok {
		t.Fatal("no ChannelBBO event")
	}
	if bboEv.OrderBook == nil {
		t.Fatal("BBO OrderBook is nil")
	}
	if len(bboEv.OrderBook.Bids) == 0 {
		t.Fatal("BBO: no bids")
	}
	if bboEv.OrderBook.Bids[0].Price.String() != "30000" {
		t.Errorf("bid price = %s; want 30000", bboEv.OrderBook.Bids[0].Price)
	}
	if len(bboEv.OrderBook.Asks) == 0 {
		t.Fatal("BBO: no asks")
	}
	if bboEv.OrderBook.Asks[0].Price.String() != "30001" {
		t.Errorf("ask price = %s; want 30001", bboEv.OrderBook.Asks[0].Price)
	}

	// --- ChannelFunding ---
	fundingEv, ok := byChannel[exchange.ChannelFunding]
	if !ok {
		t.Fatal("no ChannelFunding event")
	}
	if fundingEv.Funding == nil {
		t.Fatal("Funding is nil")
	}
	if fundingEv.Funding.PredictedFundingRate.String() != "0.0001" {
		t.Errorf("fundingRate = %s; want 0.0001", fundingEv.Funding.PredictedFundingRate)
	}
	wantNextFunding := time.UnixMilli(1700000000000).UTC()
	if !fundingEv.Funding.NextFundingTime.Equal(wantNextFunding) {
		t.Errorf("nextFundingTime = %v; want %v", fundingEv.Funding.NextFundingTime, wantNextFunding)
	}
}

// ============================================================
// Test: funding fields parsed correctly with Confidence levels
// ============================================================

func TestSubscribePublic_FundingConfidence(t *testing.T) {
	// nextFundingTime = now + 15 min → ConfidenceHigh
	nextFundingMs := time.Now().Add(15 * time.Minute).UnixMilli()
	nextFundingStr := fmt.Sprintf("%d", nextFundingMs)

	tickerPush := map[string]interface{}{
		"action": "snapshot",
		"arg": map[string]string{
			"instType": "USDT-FUTURES",
			"channel":  "ticker",
			"instId":   "BTCUSDT",
		},
		"data": []map[string]string{
			{
				"symbol":          "BTCUSDT",
				"lastPr":          "50000",
				"fundingRate":     "0.00025",
				"nextFundingTime": nextFundingStr,
				"ts":              "1695794098000",
			},
		},
	}

	srv := fakeServer(t, func(conn *websocket.Conn) {
		readSubscribeMsg(t, conn)
		sendJSON(t, conn, tickerPush)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer srv.Close()

	a := makeAdapter(wsURLFromHTTP(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelFunding, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Expect Ticker + Funding (no BBO — no bidPr/askPr in payload).
	events := drainCh(ch, 2, 200*time.Millisecond)
	var fundingEv *exchange.PublicEvent
	for i := range events {
		if events[i].Channel == exchange.ChannelFunding {
			fundingEv = &events[i]
			break
		}
	}
	if fundingEv == nil {
		t.Fatal("no ChannelFunding event")
	}
	if fundingEv.Funding.Confidence != domain.ConfidenceHigh {
		t.Errorf("confidence = %v; want ConfidenceHigh (nextFunding in 15 min)", fundingEv.Funding.Confidence)
	}
	if fundingEv.Funding.PredictedFundingRate.String() != "0.00025" {
		t.Errorf("rate = %s; want 0.00025", fundingEv.Funding.PredictedFundingRate)
	}
}

// ============================================================
// Test: ctx cancel closes the channel and goroutine returns
// ============================================================

func TestSubscribePublic_CtxCancelClosesChannel(t *testing.T) {
	srv := fakeServer(t, func(conn *websocket.Conn) {
		readSubscribeMsg(t, conn)
		// Hold the connection open; client's ctx will cancel.
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer srv.Close()

	a := makeAdapter(wsURLFromHTTP(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Cancel immediately.
	cancel()

	// Channel should be closed within a short time.
	select {
	case _, ok := <-ch:
		if ok {
			// Consume remaining events silently.
		}
	case <-time.After(200 * time.Millisecond):
		// Allow up to 200 ms for the goroutine to notice ctx cancellation.
	}

	// Drain until closed.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed — test passes
			}
		case <-deadline:
			t.Fatal("channel not closed after ctx cancel within 3 s")
		}
	}
}

// ============================================================
// Test: control frames do not emit events
// ============================================================

func TestSubscribePublic_ControlFramesNoEmit(t *testing.T) {
	srv := fakeServer(t, func(conn *websocket.Conn) {
		readSubscribeMsg(t, conn)
		// Send control frames only.
		sendJSON(t, conn, map[string]string{"event": "subscribe"})
		sendJSON(t, conn, map[string]interface{}{"event": "error", "code": "30004", "msg": "channel doesn't exist"})
		// Send pong keepalive.
		conn.WriteMessage(websocket.TextMessage, []byte("pong")) //nolint:errcheck
		// Keep alive briefly, then close.
		time.Sleep(50 * time.Millisecond)
	})
	defer srv.Close()

	a := makeAdapter(wsURLFromHTTP(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Collect any events emitted in 200 ms.
	events := drainCh(ch, 1, 200*time.Millisecond)
	if len(events) != 0 {
		t.Errorf("got %d events from control frames; want 0", len(events))
	}
}

// ============================================================
// Test: orderbook snapshot is parsed correctly
// ============================================================

func TestSubscribePublic_OrderbookSnapshot(t *testing.T) {
	obPush := map[string]interface{}{
		"action": "snapshot",
		"arg": map[string]string{
			"instType": "USDT-FUTURES",
			"channel":  "books5",
			"instId":   "BTCUSDT",
		},
		"data": []map[string]interface{}{
			{
				"asks": [][]string{
					{"27001.0", "0.400"},
					{"27001.5", "1.200"},
				},
				"bids": [][]string{
					{"27000.0", "2.710"},
					{"26999.5", "1.460"},
				},
				"ts":  "1695716059516",
				"seq": int64(123),
			},
		},
		"ts": int64(1695716059516),
	}

	srv := fakeServer(t, func(conn *websocket.Conn) {
		readSubscribeMsg(t, conn)
		sendJSON(t, conn, obPush)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer srv.Close()

	a := makeAdapter(wsURLFromHTTP(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelDepth, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	events := drainCh(ch, 1, 2*time.Second)
	if len(events) == 0 {
		t.Fatal("no events received for orderbook snapshot")
	}
	ev := events[0]
	if ev.Channel != exchange.ChannelDepth {
		t.Errorf("channel = %v; want ChannelDepth", ev.Channel)
	}
	if ev.OrderBook == nil {
		t.Fatal("OrderBook is nil")
	}
	if !ev.OrderBook.IsSnapshot {
		t.Error("IsSnapshot should be true")
	}
	if len(ev.OrderBook.Bids) != 2 {
		t.Errorf("bids len = %d; want 2", len(ev.OrderBook.Bids))
	}
	if len(ev.OrderBook.Asks) != 2 {
		t.Errorf("asks len = %d; want 2", len(ev.OrderBook.Asks))
	}
	if ev.OrderBook.Bids[0].Price.String() != "27000" {
		t.Errorf("best bid = %s; want 27000", ev.OrderBook.Bids[0].Price)
	}
	if ev.OrderBook.Asks[0].Price.String() != "27001" {
		t.Errorf("best ask = %s; want 27001", ev.OrderBook.Asks[0].Price)
	}
	if ev.OrderBook.Sequence != 123 {
		t.Errorf("seq = %d; want 123", ev.OrderBook.Sequence)
	}
	// Verify exchange is tagged correctly.
	if ev.OrderBook.Exchange != domain.ExchangeBitget {
		t.Errorf("exchange = %v; want ExchangeBitget", ev.OrderBook.Exchange)
	}
}

// ============================================================
// Test: ping message handled by server (no crash)
// ============================================================

func TestSubscribePublic_PingPong(t *testing.T) {
	pingReceived := make(chan struct{}, 1)

	srv := fakeServer(t, func(conn *websocket.Conn) {
		readSubscribeMsg(t, conn)
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage && string(msg) == "ping" {
				select {
				case pingReceived <- struct{}{}:
				default:
				}
				conn.WriteMessage(websocket.TextMessage, []byte("pong")) //nolint:errcheck
			}
		}
	})
	defer srv.Close()

	// Override ping interval for this test by using a very short ping interval
	// isn't directly injectable here, so we just verify the client doesn't break.
	// The ping loop fires at wsPingInterval (20 s); we just verify the conn stays alive.
	a := makeAdapter(wsURLFromHTTP(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}
	// Drain to let goroutine run.
	drainCh(ch, 0, 200*time.Millisecond)
	// We just verify no panic occurred; ping loop fires after 20 s so we can't
	// observe it in a short test without injecting the interval.
}

// ============================================================
// Test: empty subscriptions returns error (no dial)
// ============================================================

func TestSubscribePublic_EmptySubscriptionsError(t *testing.T) {
	a := makeAdapter("ws://unused")
	ctx := context.Background()
	_, err := a.SubscribePublic(ctx, nil)
	if err == nil {
		t.Fatal("expected error for empty subscriptions")
	}
}

// ============================================================
// Test: SubscribePrivate still returns ErrWSNotImplemented
// ============================================================

func TestSubscribePrivate_StillStub(t *testing.T) {
	a := makeAdapter("ws://unused")
	ctx := context.Background()
	_, err := a.SubscribePrivate(ctx, domain.CredentialRef{})
	if err == nil {
		t.Fatal("expected error for SubscribePrivate stub")
	}
}

// ============================================================
// Test: decimal precision — no float64 usage
// ============================================================

func TestSubscribePublic_DecimalPrecision(t *testing.T) {
	// Use a value that would lose precision with float64.
	tickerPush := map[string]interface{}{
		"action": "snapshot",
		"arg":    map[string]string{"instType": "USDT-FUTURES", "channel": "ticker", "instId": "ETHUSDT"},
		"data": []map[string]string{
			{
				"symbol": "ETHUSDT",
				"lastPr": "1234.567890123456",
				"bidPr":  "1234.567890123455",
				"askPr":  "1234.567890123457",
				"ts":     "1695794098000",
			},
		},
	}

	srv := fakeServer(t, func(conn *websocket.Conn) {
		readSubscribeMsg(t, conn)
		sendJSON(t, conn, tickerPush)
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	})
	defer srv.Close()

	a := makeAdapter(wsURLFromHTTP(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "ETHUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	events := drainCh(ch, 1, 2*time.Second)
	var tickerEv *exchange.PublicEvent
	for i := range events {
		if events[i].Channel == exchange.ChannelTicker {
			tickerEv = &events[i]
			break
		}
	}
	if tickerEv == nil || tickerEv.Ticker == nil {
		t.Fatal("no ticker event")
	}
	// Ensure exact representation preserved.
	if tickerEv.Ticker.LastPrice.String() != "1234.567890123456" {
		t.Errorf("lastPrice = %s; want exact 1234.567890123456", tickerEv.Ticker.LastPrice)
	}
}
