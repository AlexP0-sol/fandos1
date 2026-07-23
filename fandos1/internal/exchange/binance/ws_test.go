// WS-тесты для Binance адаптера.
//
// Проверяется:
//   - SubscribePublic: bookTicker BBO парсинг (b/B/a/A поля)
//   - SubscribePublic: markPrice → MarkPrice + Funding события
//   - ctx.Done() → канал закрывается
package binance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// wsUpgrader — общий upgrader для тестов.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// mkWSAdapter — создаёт адаптер с WS URL сервера.
func mkWSAdapter(wsURL string) *Adapter {
	signer := NewSigner([]byte("test-secret"))
	doer := newTestDoer("http://localhost", "http://localhost")
	return New(Config{
		RESTBaseURL:  "http://localhost",
		SAPIBaseURL:  "http://localhost",
		WSBaseURL:    wsURL,
		APIKey:       "test-api-key",
		Signer:       signer,
		HTTPDoer:     doer,
		RecvWindowMs: 5000,
		Clock:        time.Now,
	})
}

// ============================================================
// SubscribePublic — bookTicker BBO парсинг (b/B/a/A поля)
// ============================================================

func TestSubscribePublic_BookTickerBBO(t *testing.T) {
	// combined stream сообщение с bookTicker данными
	// VERIFIED: поля b=bidPrice, a=askPrice — ключи Binance bookTicker stream
	const bookTickerMsg = `{
		"stream": "btcusdt@bookTicker",
		"data": {
			"s": "BTCUSDT",
			"b": "49999.90",
			"B": "1.500",
			"a": "50000.10",
			"A": "0.800",
			"T": 1700000000000
		}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Отправляем bookTicker сообщение
		if err := conn.WriteMessage(websocket.TextMessage, []byte(bookTickerMsg)); err != nil {
			return
		}
		// Держим соединение открытым до закрытия теста
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a := mkWSAdapter(wsURL)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Ждём BBO событие
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed before receiving BBO event")
			}
			if ev.Channel != exchange.ChannelBBO {
				continue
			}
			if ev.OrderBook == nil {
				t.Fatal("BBO event: OrderBook is nil")
			}
			ob := ev.OrderBook
			if len(ob.Bids) == 0 {
				t.Fatal("BBO: no bids")
			}
			if len(ob.Asks) == 0 {
				t.Fatal("BBO: no asks")
			}
			// Проверяем точные значения из полей b/a
			// decimal.FromString нормализует trailing zeros: "49999.90" → "49999.9"
			if ob.Bids[0].Price.String() != "49999.9" {
				t.Errorf("BBO bid price = %v, want 49999.9", ob.Bids[0].Price)
			}
			if ob.Asks[0].Price.String() != "50000.1" {
				t.Errorf("BBO ask price = %v, want 50000.1", ob.Asks[0].Price)
			}
			if ev.Symbol != "BTCUSDT" {
				t.Errorf("Symbol = %v, want BTCUSDT", ev.Symbol)
			}
			return // успех
		case <-timeout:
			t.Fatal("timeout: did not receive BBO event")
		}
	}
}

// ============================================================
// SubscribePublic — markPrice → MarkPrice + Funding события
// ============================================================

func TestSubscribePublic_MarkPriceFunding(t *testing.T) {
	// nextFundingTime = 2 часа в будущем → MEDIUM confidence
	// T фиксировано в JSON (чтобы тест был детерминированным, используем относительное время)
	nextFundingMs := time.Now().Add(2 * time.Hour).UnixMilli()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Формируем JSON с реальным nextFundingTime
		import_fmt := "0.0001" // funding rate
		msg := `{"stream":"btcusdt@markPrice@1s","data":{"s":"BTCUSDT","p":"50100.00","i":"50099.50","P":"50000.00","r":"` +
			import_fmt + `","T":` + int64ToStr(nextFundingMs) + `,"E":1700000000000}}`

		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			return
		}
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a := mkWSAdapter(wsURL)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelMarkPrice, Symbol: "BTCUSDT"},
		{Channel: exchange.ChannelFunding, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	gotMarkPrice := false
	gotFunding := false
	timeout := time.After(2 * time.Second)

loop:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break loop
			}
			switch ev.Channel {
			case exchange.ChannelMarkPrice:
				if ev.MarkPrice == nil {
					t.Error("MarkPrice event: MarkPrice pointer is nil")
					break loop
				}
				// decimal нормализует: "50100.00" → "50100"
				if ev.MarkPrice.MarkPrice.String() != "50100" {
					t.Errorf("MarkPrice = %v, want 50100", ev.MarkPrice.MarkPrice)
				}
				if ev.Symbol != "BTCUSDT" {
					t.Errorf("MarkPrice Symbol = %v, want BTCUSDT", ev.Symbol)
				}
				gotMarkPrice = true
			case exchange.ChannelFunding:
				if ev.Funding == nil {
					t.Error("Funding event: Funding pointer is nil")
					break loop
				}
				if ev.Funding.RealizedFundingRate.String() != "0.0001" {
					t.Errorf("FundingRate = %v, want 0.0001", ev.Funding.RealizedFundingRate)
				}
				// 2 часа → MEDIUM
				if ev.Funding.Confidence != 2 { // domain.ConfidenceMedium == 2
					t.Errorf("Confidence = %v, want ConfidenceMedium(2)", ev.Funding.Confidence)
				}
				gotFunding = true
			}
			if gotMarkPrice && gotFunding {
				break loop
			}
		case <-timeout:
			break loop
		}
	}

	if !gotMarkPrice {
		t.Error("did not receive MarkPrice event")
	}
	if !gotFunding {
		t.Error("did not receive Funding event")
	}
}

// ============================================================
// ctx.Done() → канал закрывается
// ============================================================

func TestSubscribePublic_ContextCancel_ClosesChannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Держим соединение без данных
		time.Sleep(10 * time.Second)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	a := mkWSAdapter(wsURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelBBO, Symbol: "BTCUSDT"},
	})
	if err != nil {
		cancel()
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Отменяем контекст сразу
	cancel()

	// Канал должен закрыться в течение разумного времени
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // успех: канал закрылся
			}
		case <-deadline:
			t.Fatal("channel did not close after context cancel within 2s")
		}
	}
}

// int64ToStr преобразует int64 в строку для вставки в JSON.
func int64ToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	digits := make([]byte, 0, 20)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	// Разворачиваем
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
