// Контрактные тесты адаптера Bitget V2.
//
// Тесты используют httptest-сервер с Bitget V2 конвертами.
// Проверяется:
//   - Парсинг GetInstruments (contracts endpoint)
//   - GetFunding + funding-time: парсинг, Confidence policy
//   - GetTicker: парсинг полей lastPr, markPrice
//   - GetOrderBookSnapshot: парсинг bids/asks
//   - PlaceOrder: тело содержит symbol/productType/side/orderType/size/clientOid/force,
//     заголовки ACCESS-KEY/ACCESS-SIGN/ACCESS-TIMESTAMP/ACCESS-PASSPHRASE присутствуют
//   - CancelOrder: тело содержит clientOid
//   - GetOrder: ErrOrderNotFound при code=40109
//   - Маппинг ошибок: rate limit, margin, unauthorized
//   - GetPositions: парсинг нескольких позиций, пустые пропускаются
//   - GetBalances: подписанный запрос, парсинг
//   - GetADLState: нулевой ответ (ADL не реализован на Bitget)
//   - signer: known-vector (см. signer_test.go)
package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// ============================================================
// Тестовые вспомогательные типы
// ============================================================

// testHTTPDoer — HTTPDoer, проксирующий запросы к httptest-серверу.
type testHTTPDoer struct {
	baseURL string
	lastReq *capturedReq
}

type capturedReq struct {
	req  *http.Request
	body []byte
}

func newTestDoer(baseURL string) *testHTTPDoer {
	return &testHTTPDoer{baseURL: baseURL}
}

func (d *testHTTPDoer) Do(ctx context.Context, req HTTPRequest) (int, []byte, error) {
	url := d.baseURL + req.Path
	if req.Query != "" {
		url += "?" + req.Query
	}

	var bodyBytes []byte
	var bodyReader io.Reader
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return 0, nil, err
		}
		bodyBytes = b
		bodyReader = strings.NewReader(string(b))
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	d.lastReq = &capturedReq{req: httpReq, body: bodyBytes}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// bitgetResponse формирует тестовый ответ в формате Bitget V2.
func bitgetResponse(code, msg string, data interface{}) []byte {
	var raw json.RawMessage
	if data != nil {
		r, _ := json.Marshal(data)
		raw = json.RawMessage(r)
	} else {
		raw = json.RawMessage("null")
	}
	env := map[string]interface{}{
		"code":        code,
		"msg":         msg,
		"data":        raw,
		"requestTime": 1700000000000,
	}
	b, _ := json.Marshal(env)
	return b
}

// bitgetOK — успешный ответ.
func bitgetOK(data interface{}) []byte {
	return bitgetResponse("00000", "success", data)
}

// mkAdapter создаёт адаптер для тестов.
func mkAdapter(srv *httptest.Server) (*Adapter, *testHTTPDoer) {
	doer := newTestDoer(srv.URL)
	a, err := New(Config{
		RESTBaseURL:  srv.URL,
		WSBaseURL:    "ws://localhost",
		APIKey:       "test-api-key",
		APISecret:    "test-secret",
		Passphrase:   "test-passphrase",
		HTTPDoer:     doer,
		RecvWindowMs: 5000,
		Clock:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if err != nil {
		panic(fmt.Sprintf("mkAdapter: %v", err))
	}
	return a, doer
}

// ============================================================
// New — конструктор
// ============================================================

func TestNew_MissingAPIKey(t *testing.T) {
	_, err := New(Config{
		APISecret:  "sec",
		Passphrase: "pass",
		HTTPDoer:   newTestDoer("http://localhost"),
	})
	if err == nil {
		t.Fatal("expected error for missing APIKey")
	}
}

func TestNew_MissingPassphrase(t *testing.T) {
	_, err := New(Config{
		APIKey:    "key",
		APISecret: "sec",
		HTTPDoer:  newTestDoer("http://localhost"),
	})
	if err == nil {
		t.Fatal("expected error for missing Passphrase")
	}
}

func TestNew_DefaultRESTBase(t *testing.T) {
	doer := newTestDoer("http://localhost")
	a, err := New(Config{
		APIKey:     "k",
		APISecret:  "s",
		Passphrase: "p",
		HTTPDoer:   doer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.restBase != defaultRESTBase {
		t.Errorf("restBase = %q, want %q", a.restBase, defaultRESTBase)
	}
}

func TestNew_ID(t *testing.T) {
	doer := newTestDoer("http://localhost")
	a, _ := New(Config{
		APIKey:     "k",
		APISecret:  "s",
		Passphrase: "p",
		HTTPDoer:   doer,
	})
	if a.ID() != domain.ExchangeBitget {
		t.Errorf("ID() = %v, want bitget", a.ID())
	}
}

// ============================================================
// GetServerTime
// ============================================================

func TestGetServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/public/time" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(map[string]string{"serverTime": "1700000000000"}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ts, err := a.GetServerTime(context.Background())
	if err != nil {
		t.Fatalf("GetServerTime: %v", err)
	}
	expected := time.Unix(1700000000, 0).UTC()
	if !ts.Equal(expected) {
		t.Fatalf("GetServerTime: got %v, want %v", ts, expected)
	}
}

// ============================================================
// GetInstruments — парсинг контрактов
// ============================================================

func TestGetInstruments_Parse(t *testing.T) {
	contracts := []map[string]interface{}{
		{
			"symbol":          "BTCUSDT",
			"baseCoin":        "BTC",
			"quoteCoin":       "USDT",
			"settleCoin":      "USDT",
			"symbolStatus":    "normal",
			"minTradeNum":     "0.001",
			"pricePlace":      1,
			"priceEndStep":    1,
			"volumePlace":     3,
			"sizeMultiplier":  "1",
			"maxLeverageOver": "125",
			"fundingInterval": "8",
		},
		{
			// Неактивный — должен быть пропущен
			"symbol":       "DELISTUSDT",
			"baseCoin":     "DELIST",
			"quoteCoin":    "USDT",
			"settleCoin":   "USDT",
			"symbolStatus": "settle", // не "normal"
			"minTradeNum":  "0.1",
			"pricePlace":   2,
			"priceEndStep": 1,
			"volumePlace":  1,
		},
		{
			"symbol":          "ETHUSDT",
			"baseCoin":        "ETH",
			"quoteCoin":       "USDT",
			"settleCoin":      "USDT",
			"symbolStatus":    "normal",
			"minTradeNum":     "0.01",
			"pricePlace":      2,
			"priceEndStep":    1,
			"volumePlace":     2,
			"maxLeverageOver": "75",
			"fundingInterval": "8",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/market/contracts" {
			http.Error(w, "not found", 404)
			return
		}
		if r.URL.Query().Get("productType") != "USDT-FUTURES" {
			http.Error(w, "missing productType", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(contracts))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	instruments, err := a.GetInstruments(context.Background())
	if err != nil {
		t.Fatalf("GetInstruments: %v", err)
	}
	if len(instruments) != 2 {
		t.Fatalf("expected 2 active instruments, got %d", len(instruments))
	}
	btc := instruments[0]
	if btc.ExchangeSymbol != "BTCUSDT" {
		t.Errorf("instruments[0].Symbol = %v, want BTCUSDT", btc.ExchangeSymbol)
	}
	if btc.Exchange != domain.ExchangeBitget {
		t.Errorf("Exchange = %v, want bitget", btc.Exchange)
	}
	if btc.InstrumentType != domain.InstrumentLinearUSDTPerpetual {
		t.Errorf("InstrumentType = %v, want LINEAR_USDT_PERPETUAL", btc.InstrumentType)
	}
	if btc.Status != domain.InstrumentStatusActive {
		t.Errorf("Status = %v, want active", btc.Status)
	}
	eth := instruments[1]
	if eth.ExchangeSymbol != "ETHUSDT" {
		t.Errorf("instruments[1].Symbol = %v, want ETHUSDT", eth.ExchangeSymbol)
	}
}

// ============================================================
// GetFunding — парсинг + Confidence policy
// ============================================================

func TestGetFunding_Parsing(t *testing.T) {
	// nextFundingTime: 15 минут в будущем → HIGH confidence
	nextFundingMs := time.Now().Add(15 * time.Minute).UnixMilli()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/mix/market/current-fund-rate":
			if r.URL.Query().Get("symbol") == "" {
				http.Error(w, "missing symbol", 400)
				return
			}
			w.Write(bitgetOK(map[string]interface{}{
				"symbol":      "BTCUSDT",
				"fundingRate": "0.0001",
			}))
		case "/api/v2/mix/market/funding-time":
			w.Write(bitgetOK(map[string]interface{}{
				"symbol":          "BTCUSDT",
				"nextFundingTime": fmt.Sprintf("%d", nextFundingMs),
			}))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	fi, err := a.GetFunding(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetFunding: %v", err)
	}
	if fi.RealizedFundingRate.String() != "0.0001" {
		t.Errorf("FundingRate = %v, want 0.0001", fi.RealizedFundingRate)
	}
	if fi.Confidence != domain.ConfidenceHigh {
		t.Errorf("Confidence = %v, want ConfidenceHigh", fi.Confidence)
	}
	if fi.ExchangeSymbol != "BTCUSDT" {
		t.Errorf("ExchangeSymbol = %v, want BTCUSDT", fi.ExchangeSymbol)
	}
}

func TestGetFunding_ConfidenceLevels(t *testing.T) {
	cases := []struct {
		name           string
		minutesUntil   int
		wantConfidence domain.ConfidenceLevel
	}{
		{"HIGH_<30min", 15, domain.ConfidenceHigh},
		{"MEDIUM_<4h", 60, domain.ConfidenceMedium},
		{"LOW_>4h", 300, domain.ConfidenceLow},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			nextFundingMs := time.Now().Add(time.Duration(tc.minutesUntil) * time.Minute).UnixMilli()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v2/mix/market/current-fund-rate":
					w.Write(bitgetOK(map[string]interface{}{
						"symbol":      "BTCUSDT",
						"fundingRate": "0.0001",
					}))
				case "/api/v2/mix/market/funding-time":
					w.Write(bitgetOK(map[string]interface{}{
						"symbol":          "BTCUSDT",
						"nextFundingTime": fmt.Sprintf("%d", nextFundingMs),
					}))
				default:
					http.Error(w, "not found", 404)
				}
			}))
			defer srv.Close()

			a, _ := mkAdapter(srv)
			fi, err := a.GetFunding(context.Background(), "BTCUSDT")
			if err != nil {
				t.Fatalf("GetFunding: %v", err)
			}
			if fi.Confidence != tc.wantConfidence {
				t.Errorf("confidence = %v, want %v", fi.Confidence, tc.wantConfidence)
			}
		})
	}
}

// ============================================================
// GetTicker
// ============================================================

func TestGetTicker_Parse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/market/ticker" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(map[string]interface{}{
			"symbol":     "BTCUSDT",
			"lastPr":     "50000.5",
			"bidPr":      "49999.9",
			"askPr":      "50000.1",
			"markPrice":  "50001.0",
			"indexPrice": "50000.0",
			"usdtVolume": "1234567890.12",
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ticker, err := a.GetTicker(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if ticker.LastPrice.String() != "50000.5" {
		t.Errorf("LastPrice = %v, want 50000.5", ticker.LastPrice)
	}
	if ticker.MarkPrice.String() != "50001" {
		t.Errorf("MarkPrice = %v, want 50001", ticker.MarkPrice)
	}
	if ticker.Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %v, want BTCUSDT", ticker.Symbol)
	}
}

// ============================================================
// GetOrderBookSnapshot
// ============================================================

func TestGetOrderBookSnapshot_Parse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/market/merge-depth" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(map[string]interface{}{
			"ts":   "1700000000000",
			"bids": [][]string{{"50000", "1.5"}, {"49999", "2.0"}},
			"asks": [][]string{{"50001", "1.0"}, {"50002", "3.0"}},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ob, err := a.GetOrderBookSnapshot(context.Background(), "BTCUSDT", 50)
	if err != nil {
		t.Fatalf("GetOrderBookSnapshot: %v", err)
	}
	if len(ob.Bids) != 2 {
		t.Errorf("bids count = %d, want 2", len(ob.Bids))
	}
	if len(ob.Asks) != 2 {
		t.Errorf("asks count = %d, want 2", len(ob.Asks))
	}
	if ob.Bids[0].Price.String() != "50000" {
		t.Errorf("bids[0].price = %v, want 50000", ob.Bids[0].Price)
	}
	if ob.Asks[0].Qty.String() != "1" {
		t.Errorf("asks[0].qty = %v, want 1", ob.Asks[0].Qty)
	}
	if !ob.IsSnapshot {
		t.Error("IsSnapshot should be true")
	}
}

// ============================================================
// PlaceOrder — проверка тела и заголовков ACCESS-*
// ============================================================

func TestPlaceOrder_BodyAndAuthHeaders(t *testing.T) {
	var capturedBody []byte
	var capturedHeaders map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/order/place-order" {
			http.Error(w, "not found", 404)
			return
		}
		// Сохраняем заголовки
		capturedHeaders = map[string]string{
			"ACCESS-KEY":        r.Header.Get("ACCESS-KEY"),
			"ACCESS-SIGN":       r.Header.Get("ACCESS-SIGN"),
			"ACCESS-TIMESTAMP":  r.Header.Get("ACCESS-TIMESTAMP"),
			"ACCESS-PASSPHRASE": r.Header.Get("ACCESS-PASSPHRASE"),
		}
		body, _ := io.ReadAll(r.Body)
		capturedBody = body

		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(map[string]string{
			"orderId":   "exchange-order-123",
			"clientOid": "client-order-abc",
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)

	qty, _ := decimal.FromString("0.001")
	price, _ := decimal.FromString("50000")

	req := domain.PlaceOrderRequest{
		ClientOrderID: "client-order-abc",
		Symbol:        "BTCUSDT",
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarketableLimitIOC,
		BaseQty:       qty,
		Price:         price,
		ReduceOnly:    false,
		TimeInForce:   domain.TIFIOC,
	}

	ack, err := a.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if ack.ExchangeOrderID != "exchange-order-123" {
		t.Errorf("ExchangeOrderID = %v, want exchange-order-123", ack.ExchangeOrderID)
	}

	// Проверяем обязательные заголовки
	for _, hdr := range []string{"ACCESS-KEY", "ACCESS-SIGN", "ACCESS-TIMESTAMP", "ACCESS-PASSPHRASE"} {
		if capturedHeaders[hdr] == "" {
			t.Errorf("header %s missing in request", hdr)
		}
	}
	if capturedHeaders["ACCESS-KEY"] != "test-api-key" {
		t.Errorf("ACCESS-KEY = %q, want test-api-key", capturedHeaders["ACCESS-KEY"])
	}
	if capturedHeaders["ACCESS-PASSPHRASE"] != "test-passphrase" {
		t.Errorf("ACCESS-PASSPHRASE = %q, want test-passphrase", capturedHeaders["ACCESS-PASSPHRASE"])
	}

	// Проверяем обязательные поля тела
	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	requiredFields := []string{"symbol", "productType", "side", "orderType", "size", "clientOid", "force"}
	for _, field := range requiredFields {
		if _, ok := body[field]; !ok {
			t.Errorf("body missing required field: %s", field)
		}
	}
	if body["symbol"] != "BTCUSDT" {
		t.Errorf("body.symbol = %v, want BTCUSDT", body["symbol"])
	}
	if body["productType"] != "USDT-FUTURES" {
		t.Errorf("body.productType = %v, want USDT-FUTURES", body["productType"])
	}
	if body["side"] != "buy" {
		t.Errorf("body.side = %v, want buy", body["side"])
	}
	if body["orderType"] != "limit" {
		t.Errorf("body.orderType = %v, want limit", body["orderType"])
	}
	if body["clientOid"] != "client-order-abc" {
		t.Errorf("body.clientOid = %v, want client-order-abc", body["clientOid"])
	}
	if body["force"] != "ioc" {
		t.Errorf("body.force = %v, want ioc", body["force"])
	}
}

func TestPlaceOrder_ShortSide(t *testing.T) {
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(map[string]string{"orderId": "order-sell-123", "clientOid": "sell-order"}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	qty, _ := decimal.FromString("0.01")
	req := domain.PlaceOrderRequest{
		ClientOrderID: "sell-order",
		Symbol:        "ETHUSDT",
		Side:          domain.SideShort,
		OrderMode:     domain.OrderMarket,
		BaseQty:       qty,
	}
	_, err := a.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder (short): %v", err)
	}

	var body map[string]interface{}
	json.Unmarshal(capturedBody, &body)
	if body["side"] != "sell" {
		t.Errorf("body.side = %v, want sell", body["side"])
	}
	if body["orderType"] != "market" {
		t.Errorf("body.orderType = %v, want market", body["orderType"])
	}
}

// ============================================================
// CancelOrder
// ============================================================

func TestCancelOrder_Body(t *testing.T) {
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/order/cancel-order" {
			http.Error(w, "not found", 404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(map[string]string{"orderId": "order-123", "clientOid": "cancel-order"}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ClientOrderID: "cancel-order",
		Symbol:        "BTCUSDT",
	})
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if body["clientOid"] != "cancel-order" {
		t.Errorf("body.clientOid = %v, want cancel-order", body["clientOid"])
	}
	if body["symbol"] != "BTCUSDT" {
		t.Errorf("body.symbol = %v, want BTCUSDT", body["symbol"])
	}
}

// ============================================================
// GetOrder — ErrOrderNotFound при code=40109
// ============================================================

func TestGetOrder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetResponse("40109", "order not found", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "no-such-order",
		Symbol:        "BTCUSDT",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

// ============================================================
// Маппинг ошибок
// ============================================================

func TestErrorMapping_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetResponse("40429", "too many requests", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetServerTime(context.Background())
	if err == nil {
		t.Fatal("expected ErrRateLimited, got nil")
	}
	if !errors.Is(err, exchange.ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got: %v", err)
	}
}

func TestErrorMapping_InsufficientMargin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetResponse("40754", "insufficient margin", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	qty, _ := decimal.FromString("100")
	_, err := a.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "test",
		Symbol:        "BTCUSDT",
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarket,
		BaseQty:       qty,
	})
	if err == nil {
		t.Fatal("expected ErrInsufficientMargin, got nil")
	}
	if !errors.Is(err, exchange.ErrInsufficientMargin) {
		t.Errorf("expected ErrInsufficientMargin, got: %v", err)
	}
}

func TestErrorMapping_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetResponse("40001", "api key not found", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetBalances(context.Background())
	if err == nil {
		t.Fatal("expected ErrUnauthorized, got nil")
	}
	if !errors.Is(err, exchange.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got: %v", err)
	}
}

func TestErrorMapping_Unauthorized_40009(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetResponse("40009", "sign error", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetBalances(context.Background())
	if !errors.Is(err, exchange.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for 40009, got: %v", err)
	}
}

func TestErrorMapping_InvalidSymbol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetResponse("40034", "symbol not exist", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetTicker(context.Background(), "INVALIDUSDT")
	if !errors.Is(err, exchange.ErrInvalidSymbol) {
		t.Errorf("expected ErrInvalidSymbol, got: %v", err)
	}
}

// ============================================================
// GetPositions — парсинг, пропуск пустых
// ============================================================

func TestGetPositions_Parse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/position/all-position" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK([]map[string]interface{}{
			{
				"symbol":           "BTCUSDT",
				"holdSide":         "long",
				"total":            "0.1",
				"openPriceAvg":     "50000",
				"liquidationPrice": "30000",
				"markPrice":        "51000",
				"unrealizedPL":     "100",
				"marginMode":       "crossed",
				"leverage":         "10",
				"margin":           "500",
				"marginRatio":      "0.05",
			},
			{
				// Пустая позиция — должна быть пропущена
				"symbol":       "ETHUSDT",
				"holdSide":     "short",
				"total":        "0",
				"openPriceAvg": "3000",
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	positions, err := a.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 non-zero position, got %d", len(positions))
	}
	p := positions[0]
	if p.Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %v, want BTCUSDT", p.Symbol)
	}
	if p.Side != domain.SideLong {
		t.Errorf("Side = %v, want long", p.Side)
	}
	if p.EntryPrice.String() != "50000" {
		t.Errorf("EntryPrice = %v, want 50000", p.EntryPrice)
	}
	if p.MarginMode != domain.MarginCross {
		t.Errorf("MarginMode = %v, want cross", p.MarginMode)
	}
}

// ============================================================
// GetBalances — подписанный запрос + парсинг
// ============================================================

func TestGetBalances_Parse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/account/accounts" {
			http.Error(w, "not found", 404)
			return
		}
		// Проверяем наличие ACCESS-SIGN
		if r.Header.Get("ACCESS-SIGN") == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK([]map[string]interface{}{
			{
				"marginCoin": "USDT",
				"available":  "8000.25",
				"equity":     "10000.5",
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	balances, err := a.GetBalances(context.Background())
	if err != nil {
		t.Fatalf("GetBalances: %v", err)
	}
	if len(balances) != 1 {
		t.Fatalf("expected 1 balance, got %d", len(balances))
	}
	if balances[0].Asset != "USDT" {
		t.Errorf("Asset = %v, want USDT", balances[0].Asset)
	}
	if balances[0].WalletBalance.String() != "10000.5" {
		t.Errorf("WalletBalance = %v, want 10000.5", balances[0].WalletBalance)
	}
	if balances[0].AvailableBalance.String() != "8000.25" {
		t.Errorf("AvailableBalance = %v, want 8000.25", balances[0].AvailableBalance)
	}
}

// ============================================================
// GetADLState — нулевой ответ (ADL не реализован)
// ============================================================

func TestGetADLState_Zero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "should not be called", 500)
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	adl, err := a.GetADLState(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetADLState: %v", err)
	}
	if adl.Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %v, want BTCUSDT", adl.Symbol)
	}
	if !adl.LongQueue.IsZero() || !adl.ShortQueue.IsZero() {
		t.Errorf("expected zero ADL queues, got long=%v short=%v", adl.LongQueue, adl.ShortQueue)
	}
}

// ============================================================
// SetLeverage
// ============================================================

func TestSetLeverage_Body(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(map[string]string{}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	lev, _ := decimal.FromString("10")
	err := a.SetLeverage(context.Background(), domain.SetLeverageRequest{
		Symbol:   "BTCUSDT",
		Leverage: lev,
	})
	if err != nil {
		t.Fatalf("SetLeverage: %v", err)
	}

	var body map[string]interface{}
	json.Unmarshal(capturedBody, &body)
	if body["leverage"] != "10" {
		t.Errorf("body.leverage = %v, want 10", body["leverage"])
	}
	if body["symbol"] != "BTCUSDT" {
		t.Errorf("body.symbol = %v, want BTCUSDT", body["symbol"])
	}
	if body["productType"] != "USDT-FUTURES" {
		t.Errorf("body.productType = %v, want USDT-FUTURES", body["productType"])
	}
}

// ============================================================
// SetPositionMode
// ============================================================

func TestSetPositionMode_HedgeMode(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.SetPositionMode(context.Background(), domain.SetPositionModeRequest{
		Mode: domain.PositionHedge,
	})
	if err != nil {
		t.Fatalf("SetPositionMode: %v", err)
	}

	var body map[string]interface{}
	json.Unmarshal(capturedBody, &body)
	if body["posMode"] != "hedge_mode" {
		t.Errorf("body.posMode = %v, want hedge_mode", body["posMode"])
	}
}

func TestSetPositionMode_OneWay(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK(nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.SetPositionMode(context.Background(), domain.SetPositionModeRequest{
		Mode: domain.PositionOneWay,
	})
	if err != nil {
		t.Fatalf("SetPositionMode: %v", err)
	}

	var body map[string]interface{}
	json.Unmarshal(capturedBody, &body)
	if body["posMode"] != "one_way_mode" {
		t.Errorf("body.posMode = %v, want one_way_mode", body["posMode"])
	}
}

// ============================================================
// SubscribePublic — должен возвращать ошибку (WS не реализован)
// ============================================================

func TestSubscribePublic_NotImplemented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.SubscribePublic(context.Background(), []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTCUSDT"},
	})
	if err == nil {
		t.Fatal("expected error for unimplemented WS")
	}
	if !errors.Is(err, errWSNotImplemented) {
		t.Errorf("expected errWSNotImplemented, got: %v", err)
	}
}

// ============================================================
// Envelope decoding — некорректный JSON
// ============================================================

func TestDecodeEnvelope_InvalidJSON(t *testing.T) {
	_, err := decodeEnvelope([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDecodeEnvelope_ErrorCode(t *testing.T) {
	body := bitgetResponse("40001", "api error", nil)
	_, err := decodeEnvelope(body)
	if err == nil {
		t.Fatal("expected error for non-00000 code")
	}
	if !errors.Is(err, exchange.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got: %v", err)
	}
}

// ============================================================
// GetOpenOrders — базовый парсинг
// ============================================================

func TestGetOpenOrders_Parse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/order/orders-pending" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(bitgetOK([]map[string]interface{}{
			{
				"orderId":    "order-123",
				"clientOid":  "client-123",
				"symbol":     "BTCUSDT",
				"side":       "buy",
				"tradeSide":  "open",
				"orderType":  "limit",
				"price":      "50000",
				"size":       "0.001",
				"baseVolume": "0",
				"fillPrice":  "0",
				"status":     "live",
				"cTime":      "1700000000000",
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	orders, err := a.GetOpenOrders(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetOpenOrders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	ord := orders[0]
	if ord.ExchangeOrderID != "order-123" {
		t.Errorf("ExchangeOrderID = %v, want order-123", ord.ExchangeOrderID)
	}
	if ord.Side != domain.SideLong {
		t.Errorf("Side = %v, want long", ord.Side)
	}
	if ord.Status != domain.OrderStatusAcknowledged {
		t.Errorf("Status = %v, want acknowledged", ord.Status)
	}
}
