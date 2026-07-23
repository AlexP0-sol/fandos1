// Контрактные тесты адаптера Bybit V5.
//
// Тесты используют httptest-сервер с точными V5-конвертами.
// Проверяется:
//   - Пагинация GetInstruments (2 страницы через nextPageCursor)
//   - Парсинг GetFunding (поля fundingRate/nextFundingTime, Confidence policy)
//   - PlaceOrder: тело содержит category/symbol/side/orderType/qty/reduceOnly/orderLinkId,
//     заголовок X-BAPI-SIGN присутствует
//   - GetOrder: fallback realtime → history при пустом realtime
//   - Маппинг ошибок: 110001 → ErrOrderNotFound, 10006 → ErrRateLimited
//   - ADL-нормализация: adlRankIndicator [0..5] → [0,1]
//   - WS tickers delta-merge через тестовый WebSocket-сервер
package bybit

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

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// ============================================================
// Тестовые вспомогательные типы
// ============================================================

// testHTTPDoer — HTTPDoer, проксирующий запросы к httptest-серверу.
// Сохраняет последний запрос для инспекции в тестах.
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
	if len(bodyBytes) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	// Сохраняем последний запрос.
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

// v5Response формирует тестовый ответ в формате Bybit V5.
func v5Response(retCode int, retMsg string, result interface{}) []byte {
	r, _ := json.Marshal(result)
	env := map[string]interface{}{
		"retCode": retCode,
		"retMsg":  retMsg,
		"result":  json.RawMessage(r),
		"time":    1700000000000,
	}
	b, _ := json.Marshal(env)
	return b
}

// mkAdapter создаёт адаптер для тестов.
func mkAdapter(srv *httptest.Server) (*Adapter, *testHTTPDoer) {
	signer := NewSigner("test-api-key", []byte("test-secret"))
	doer := newTestDoer(srv.URL)
	a := New(Config{
		RESTBaseURL:  srv.URL,
		WSPublicURL:  "ws://localhost",
		WSPrivateURL: "ws://localhost",
		Signer:       signer,
		HTTPDoer:     doer,
		RecvWindowMs: 5000,
		Clock:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	return a, doer
}

// ============================================================
// GetServerTime
// ============================================================

func TestGetServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5/market/time" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(0, "OK", map[string]string{
			"timeSecond": "1700000000",
			"timeNano":   "1700000000000000000",
		}))
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
// GetInstruments — пагинация (2 страницы)
// ============================================================

func TestGetInstrumentsPagination(t *testing.T) {
	page1 := map[string]interface{}{
		"category":       "linear",
		"nextPageCursor": "page2cursor",
		"list": []map[string]interface{}{
			{
				"symbol":          "BTCUSDT",
				"contractType":    "LinearPerpetual",
				"status":          "Trading",
				"baseCoin":        "BTC",
				"quoteCoin":       "USDT",
				"settleCoin":      "USDT",
				"fundingInterval": 480,
				"lotSizeFilter":   map[string]string{"qtyStep": "0.001", "minOrderQty": "0.001", "maxOrderQty": "100"},
				"priceFilter":     map[string]string{"tickSize": "0.1", "minPrice": "0.1", "maxPrice": "1000000"},
				"leverageFilter":  map[string]string{"minLeverage": "1", "maxLeverage": "100", "leverageStep": "0.01"},
			},
		},
	}
	page2 := map[string]interface{}{
		"category":       "linear",
		"nextPageCursor": "",
		"list": []map[string]interface{}{
			{
				"symbol":          "ETHUSDT",
				"contractType":    "LinearPerpetual",
				"status":          "Trading",
				"baseCoin":        "ETH",
				"quoteCoin":       "USDT",
				"settleCoin":      "USDT",
				"fundingInterval": 480,
				"lotSizeFilter":   map[string]string{"qtyStep": "0.01", "minOrderQty": "0.01", "maxOrderQty": "1000"},
				"priceFilter":     map[string]string{"tickSize": "0.01", "minPrice": "0.01", "maxPrice": "100000"},
				"leverageFilter":  map[string]string{"minLeverage": "1", "maxLeverage": "75", "leverageStep": "0.01"},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5/market/instruments-info" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		cursor := r.URL.Query().Get("cursor")
		if cursor == "page2cursor" {
			w.Write(v5Response(0, "OK", page2))
		} else {
			w.Write(v5Response(0, "OK", page1))
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	instruments, err := a.GetInstruments(context.Background())
	if err != nil {
		t.Fatalf("GetInstruments: %v", err)
	}
	if len(instruments) != 2 {
		t.Fatalf("GetInstruments: got %d instruments, want 2", len(instruments))
	}
	btc := instruments[0]
	if btc.ExchangeSymbol != "BTCUSDT" {
		t.Errorf("instruments[0].Symbol = %v, want BTCUSDT", btc.ExchangeSymbol)
	}
	if btc.FundingIntervalSec != 480*60 {
		t.Errorf("FundingIntervalSec = %d, want %d", btc.FundingIntervalSec, 480*60)
	}
	if btc.QtyStep.String() != "0.001" {
		t.Errorf("QtyStep = %v, want 0.001", btc.QtyStep)
	}
	eth := instruments[1]
	if eth.ExchangeSymbol != "ETHUSDT" {
		t.Errorf("instruments[1].Symbol = %v, want ETHUSDT", eth.ExchangeSymbol)
	}
}

// ============================================================
// GetFunding — парсинг полей и Confidence policy
// ============================================================

func TestGetFunding_Parsing(t *testing.T) {
	// nextFundingTime: 15 минут в будущем → HIGH confidence
	nextFundingMs := time.Now().Add(15 * time.Minute).UnixMilli()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5/market/tickers" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(0, "OK", map[string]interface{}{
			"category": "linear",
			"list": []map[string]interface{}{
				{
					"symbol":          "BTCUSDT",
					"fundingRate":     "0.0001",
					"nextFundingTime": fmt.Sprintf("%d", nextFundingMs),
					"markPrice":       "50000.5",
					"indexPrice":      "50001.0",
					"bid1Price":       "49999.9",
					"ask1Price":       "50000.1",
					"lastPrice":       "50000.0",
					"turnover24h":     "1234567890.12",
				},
			},
		}))
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
		t.Errorf("Confidence = %v, want ConfidenceHigh(%d)", fi.Confidence, domain.ConfidenceHigh)
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
				w.Write(v5Response(0, "OK", map[string]interface{}{
					"category": "linear",
					"list": []map[string]interface{}{
						{
							"symbol":          "BTCUSDT",
							"fundingRate":     "0.0001",
							"nextFundingTime": fmt.Sprintf("%d", nextFundingMs),
							"markPrice":       "50000",
							"indexPrice":      "50001",
							"lastPrice":       "49999",
							"turnover24h":     "100000",
						},
					},
				}))
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
// PlaceOrder — проверка тела и заголовка X-BAPI-SIGN
// ============================================================

func TestPlaceOrder_BodyAndSignHeader(t *testing.T) {
	var capturedBody []byte
	var capturedSign string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5/order/create" {
			http.Error(w, "not found", 404)
			return
		}
		capturedSign = r.Header.Get("X-BAPI-SIGN")
		body, _ := io.ReadAll(r.Body)
		capturedBody = body

		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(0, "OK", map[string]string{
			"orderId":     "exchange-order-123",
			"orderLinkId": "client-order-abc",
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

	// X-BAPI-SIGN обязательно присутствует
	if capturedSign == "" {
		t.Error("X-BAPI-SIGN header missing in request")
	}

	// Обязательные поля тела
	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	requiredFields := []string{"category", "symbol", "side", "orderType", "qty", "reduceOnly", "orderLinkId"}
	for _, field := range requiredFields {
		if _, ok := body[field]; !ok {
			t.Errorf("body missing required field: %s", field)
		}
	}
	if body["category"] != "linear" {
		t.Errorf("body.category = %v, want linear", body["category"])
	}
	if body["symbol"] != "BTCUSDT" {
		t.Errorf("body.symbol = %v, want BTCUSDT", body["symbol"])
	}
	if body["side"] != "Buy" {
		t.Errorf("body.side = %v, want Buy", body["side"])
	}
	if body["orderLinkId"] != "client-order-abc" {
		t.Errorf("body.orderLinkId = %v, want client-order-abc", body["orderLinkId"])
	}
	if body["orderType"] != "Limit" {
		t.Errorf("body.orderType = %v, want Limit", body["orderType"])
	}
}

// ============================================================
// GetOrder — fallback realtime → history
// ============================================================

func TestGetOrder_RealtimeFallbackToHistory(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		callCount++

		switch r.URL.Path {
		case "/v5/order/realtime":
			// Realtime возвращает пустой список
			w.Write(v5Response(0, "OK", map[string]interface{}{
				"category":       "linear",
				"list":           []interface{}{},
				"nextPageCursor": "",
			}))
		case "/v5/order/history":
			// History возвращает ордер
			w.Write(v5Response(0, "OK", map[string]interface{}{
				"category": "linear",
				"list": []map[string]interface{}{
					{
						"orderId":     "hist-order-456",
						"orderLinkId": "client-hist-xyz",
						"symbol":      "BTCUSDT",
						"side":        "Sell",
						"orderType":   "Limit",
						"qty":         "0.01",
						"cumExecQty":  "0.01",
						"avgPrice":    "48000.5",
						"cumExecFee":  "0.48",
						"orderStatus": "Filled",
						"reduceOnly":  true,
						"createdTime": "1700000000000",
					},
				},
				"nextPageCursor": "",
			}))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ord, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "client-hist-xyz",
		Symbol:        "BTCUSDT",
	})
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if ord.ExchangeOrderID != "hist-order-456" {
		t.Errorf("ExchangeOrderID = %v, want hist-order-456", ord.ExchangeOrderID)
	}
	if callCount < 2 {
		t.Errorf("expected ≥2 HTTP calls (realtime+history), got %d", callCount)
	}
}

// ============================================================
// Маппинг ошибок
// ============================================================

func TestErrorMapping_OrderNotFound(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		callCount++
		switch r.URL.Path {
		case "/v5/order/realtime":
			// Realtime пуст
			w.Write(v5Response(0, "OK", map[string]interface{}{
				"category": "linear", "list": []interface{}{},
			}))
		default:
			// History → 110001
			w.Write(v5Response(110001, "order not exists or too late to cancel", nil))
		}
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

func TestErrorMapping_CancelOrderNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(110001, "order not exists or too late to cancel", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ClientOrderID: "no-order",
		Symbol:        "BTCUSDT",
	})
	if err == nil {
		t.Fatal("expected ErrOrderNotFound, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

func TestErrorMapping_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(10006, "too many requests", nil))
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

func TestErrorMapping_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(10003, "api key not found", nil))
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

// ============================================================
// ADL нормализация
// ============================================================

func TestADLNormalization(t *testing.T) {
	cases := []struct {
		rank     int
		expected string
	}{
		{0, "0"},
		{1, "0.2"},
		{2, "0.4"},
		{3, "0.6"},
		{4, "0.8"},
		{5, "1"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("rank_%d", tc.rank), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(v5Response(0, "OK", map[string]interface{}{
					"category": "linear",
					"list": []map[string]interface{}{
						{
							"symbol":           "BTCUSDT",
							"side":             "Buy",
							"size":             "0.001",
							"entryPrice":       "50000",
							"markPrice":        "50100",
							"liqPrice":         "30000",
							"unrealisedPnl":    "0.1",
							"tradeMode":        0,
							"leverage":         "10",
							"positionIM":       "5",
							"adlRankIndicator": tc.rank,
							"updatedTime":      "1700000000000",
						},
					},
				}))
			}))
			defer srv.Close()

			a, _ := mkAdapter(srv)
			adl, err := a.GetADLState(context.Background(), "BTCUSDT")
			if err != nil {
				t.Errorf("rank=%d: GetADLState: %v", tc.rank, err)
				return
			}
			if adl.LongQueue.String() != tc.expected {
				t.Errorf("rank=%d: LongQueue = %v, want %v", tc.rank, adl.LongQueue, tc.expected)
			}
		})
	}
}

// ============================================================
// SetLeverage — retCode 110043 → success
// ============================================================

func TestSetLeverage_AlreadySet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(110043, "leverage not modified", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	lev, _ := decimal.FromString("10")
	err := a.SetLeverage(context.Background(), domain.SetLeverageRequest{
		Symbol:   "BTCUSDT",
		Leverage: lev,
	})
	if err != nil {
		t.Errorf("SetLeverage with 110043 should succeed, got: %v", err)
	}
}

// ============================================================
// SetPositionMode — retCode 110025 → success
// ============================================================

func TestSetPositionMode_AlreadySet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(110025, "Position mode is not modified", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.SetPositionMode(context.Background(), domain.SetPositionModeRequest{
		Mode: domain.PositionOneWay,
	})
	if err != nil {
		t.Errorf("SetPositionMode with 110025 should succeed, got: %v", err)
	}
}

// ============================================================
// GetPositions — парсинг нескольких позиций
// ============================================================

func TestGetPositions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v5/position/list" {
			http.Error(w, "not found", 404)
			return
		}
		w.Write(v5Response(0, "OK", map[string]interface{}{
			"category": "linear",
			"list": []map[string]interface{}{
				{
					"symbol":           "BTCUSDT",
					"side":             "Buy",
					"size":             "0.1",
					"entryPrice":       "50000",
					"markPrice":        "51000",
					"liqPrice":         "30000",
					"unrealisedPnl":    "100",
					"tradeMode":        0,
					"leverage":         "10",
					"positionIM":       "500",
					"adlRankIndicator": 2,
					"updatedTime":      "1700000000000",
				},
				{
					// Пустая позиция (size=0) — должна быть пропущена
					"symbol":           "ETHUSDT",
					"side":             "Sell",
					"size":             "0",
					"entryPrice":       "3000",
					"markPrice":        "3100",
					"liqPrice":         "0",
					"unrealisedPnl":    "0",
					"tradeMode":        1,
					"leverage":         "5",
					"positionIM":       "0",
					"adlRankIndicator": 0,
					"updatedTime":      "1700000000000",
				},
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
	if p.ADLQueue == nil {
		t.Fatal("ADLQueue is nil")
	}
	// rank 2 / 5 = 0.4
	if p.ADLQueue.LongQueue.String() != "0.4" {
		t.Errorf("ADLQueue.LongQueue = %v, want 0.4", p.ADLQueue.LongQueue)
	}
}

// ============================================================
// GetBalances — подписанный запрос, парсинг UNIFIED
// ============================================================

func TestGetBalances(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Проверяем наличие X-BAPI-SIGN
		if r.Header.Get("X-BAPI-SIGN") == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Write(v5Response(0, "OK", map[string]interface{}{
			"list": []map[string]interface{}{
				{
					"accountType": "UNIFIED",
					"coin": []map[string]interface{}{
						{
							"coin":                "USDT",
							"walletBalance":       "10000.5",
							"availableBalance":    "8000.25",
							"availableToWithdraw": "7500",
						},
						{
							"coin":                "BTC",
							"walletBalance":       "0.5",
							"availableBalance":    "0.5",
							"availableToWithdraw": "0.5",
						},
					},
				},
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	balances, err := a.GetBalances(context.Background())
	if err != nil {
		t.Fatalf("GetBalances: %v", err)
	}
	if len(balances) != 2 {
		t.Fatalf("expected 2 balances, got %d", len(balances))
	}
	usdt := balances[0]
	if usdt.Asset != "USDT" {
		t.Errorf("Asset = %v, want USDT", usdt.Asset)
	}
	if usdt.WalletBalance.String() != "10000.5" {
		t.Errorf("WalletBalance = %v, want 10000.5", usdt.WalletBalance)
	}
	if usdt.AvailableBalance.String() != "8000.25" {
		t.Errorf("AvailableBalance = %v, want 8000.25", usdt.AvailableBalance)
	}
}

// ============================================================
// WS tickers delta-merge тест
// ============================================================

func TestWS_TickersDeltaMerge(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	snapshotMsg := `{
		"topic": "tickers.BTCUSDT",
		"type": "snapshot",
		"ts": 1700000000000,
		"data": {
			"symbol": "BTCUSDT",
			"bid1Price": "50000",
			"ask1Price": "50001",
			"lastPrice": "50000.5",
			"markPrice": "50002",
			"indexPrice": "49999",
			"turnover24h": "1000000",
			"fundingRate": "0.0001",
			"nextFundingTime": "1700028800000"
		}
	}`

	deltaMsg := `{
		"topic": "tickers.BTCUSDT",
		"type": "delta",
		"ts": 1700000001000,
		"data": {
			"symbol": "BTCUSDT",
			"bid1Price": "50100",
			"lastPrice": "50100.5"
		}
	}`

	subAck := `{"op": "subscribe", "retMsg": "OK", "connId": "test"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Читаем subscribe-запрос
		_, _, _ = conn.ReadMessage()
		conn.WriteMessage(websocket.TextMessage, []byte(subAck))

		// Шлём snapshot
		conn.WriteMessage(websocket.TextMessage, []byte(snapshotMsg))
		time.Sleep(30 * time.Millisecond)

		// Шлём delta
		conn.WriteMessage(websocket.TextMessage, []byte(deltaMsg))
		time.Sleep(30 * time.Millisecond)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	signer := NewSigner("test-key", []byte("test-secret"))
	doer := newTestDoer(srv.URL)
	a := New(Config{
		RESTBaseURL:  srv.URL,
		WSPublicURL:  wsURL,
		WSPrivateURL: wsURL,
		Signer:       signer,
		HTTPDoer:     doer,
		RecvWindowMs: 5000,
		Clock:        time.Now,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ch, err := a.SubscribePublic(ctx, []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTCUSDT"},
	})
	if err != nil {
		t.Fatalf("SubscribePublic: %v", err)
	}

	// Собираем ticker-события
	var tickerEvents []exchange.PublicEvent
	timeout := time.After(2 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break loop
			}
			if ev.Channel == exchange.ChannelTicker && ev.Ticker != nil {
				tickerEvents = append(tickerEvents, ev)
			}
		case <-timeout:
			break loop
		}
	}

	if len(tickerEvents) < 2 {
		t.Fatalf("expected ≥2 ticker events (snapshot+delta), got %d", len(tickerEvents))
	}

	// Последнее ticker-событие должно иметь данные из delta-мержа.
	// После delta: lastPrice = 50100.5, bid1Price = 50100.
	last := tickerEvents[len(tickerEvents)-1]
	if last.Ticker.LastPrice.String() != "50100.5" {
		t.Errorf("after delta: lastPrice = %v, want 50100.5", last.Ticker.LastPrice)
	}
}

// ============================================================
// GetOrderBookSnapshot — парсинг
// ============================================================

func TestGetOrderBookSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v5/market/orderbook" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(v5Response(0, "OK", map[string]interface{}{
			"s":   "BTCUSDT",
			"b":   [][]string{{"50000", "1.5"}, {"49999", "2.0"}},
			"a":   [][]string{{"50001", "1.0"}, {"50002", "3.0"}},
			"ts":  1700000000000,
			"seq": 12345,
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
	if !ob.IsSnapshot {
		t.Error("IsSnapshot should be true")
	}
	if ob.Sequence != 12345 {
		t.Errorf("Sequence = %d, want 12345", ob.Sequence)
	}
}
