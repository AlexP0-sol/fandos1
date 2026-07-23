// Контрактные тесты адаптера Binance USDT-M Futures.
//
// Проверяется:
//   - GetServerTime: парсинг serverTime
//   - GetInstruments: фильтрация PERPETUAL+USDT, маппинг LOT_SIZE/PRICE_FILTER, decimal-конверсия
//   - GetFunding: парсинг lastFundingRate/nextFundingTime, Confidence policy
//   - GetTicker: два вызова bookTicker+24hr
//   - GetOrderBookSnapshot: bids/asks парсинг
//   - GetBalances: подписанный запрос, парсинг asset/balance/availableBalance
//   - GetPositions: парсинг positionAmt / entryPrice / marginType
//   - PlaceOrder: query params (symbol/side/type/quantity/reduceOnly/newClientOrderId+signature),
//     заголовок X-MBX-APIKEY присутствует, Safe=false
//   - GetOrder: -2013 → ErrOrderNotFound
//   - CancelOrder: -2011 → ErrOrderNotFound
//   - SetMarginMode: -4046 → success
//   - SetPositionMode: -4059 → success
//   - SetLeverage: -4028 → success
//   - HTTP 429 → ErrRateLimited
//   - GetADLState: quantile 0..4 ÷ 4 → [0,1]
package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
// SAPI-пути (|SAPI| prefix) маршрутизируются к sapiSrv (если задан).
type testHTTPDoer struct {
	fapiURL string
	sapiURL string
	lastReq *capturedReq
}

type capturedReq struct {
	req  *http.Request
	body []byte
}

func newTestDoer(fapiURL, sapiURL string) *testHTTPDoer {
	return &testHTTPDoer{fapiURL: fapiURL, sapiURL: sapiURL}
}

func (d *testHTTPDoer) Do(ctx context.Context, req HTTPRequest) (int, []byte, error) {
	// Определяем base URL по префиксу пути
	baseURL := d.fapiURL
	path := req.Path
	if strings.HasPrefix(path, "|SAPI|") {
		baseURL = d.sapiURL
		path = strings.TrimPrefix(path, "|SAPI|")
	}

	rawURL := baseURL + path
	if req.Query != "" {
		rawURL += "?" + req.Query
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

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, rawURL, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	// Сохраняем последний запрос для инспекции
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

// mkAdapter создаёт адаптер для тестов с одним httptest-сервером.
func mkAdapter(srv *httptest.Server) (*Adapter, *testHTTPDoer) {
	return mkAdapterSplit(srv, srv)
}

// mkAdapterSplit создаёт адаптер с раздельными fapi/sapi серверами.
func mkAdapterSplit(fapiSrv, sapiSrv *httptest.Server) (*Adapter, *testHTTPDoer) {
	signer := NewSigner([]byte("test-secret"))
	doer := newTestDoer(fapiSrv.URL, sapiSrv.URL)
	a := New(Config{
		RESTBaseURL:  fapiSrv.URL,
		SAPIBaseURL:  sapiSrv.URL,
		WSBaseURL:    "ws://localhost",
		APIKey:       "test-api-key",
		Signer:       signer,
		HTTPDoer:     doer,
		RecvWindowMs: 5000,
		Clock:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	return a, doer
}

// jsonResp записывает JSON-ответ с нужным кодом.
func jsonResp(w http.ResponseWriter, status int, v interface{}) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// apiError записывает Binance-формат ошибки {"code":<N>,"msg":"..."}
func apiError(w http.ResponseWriter, httpStatus int, code int, msg string) {
	jsonResp(w, httpStatus, map[string]interface{}{"code": code, "msg": msg})
}

// ============================================================
// GetServerTime
// ============================================================

func TestGetServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/time" {
			http.Error(w, "not found", 404)
			return
		}
		jsonResp(w, 200, map[string]interface{}{"serverTime": 1700000000000})
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ts, err := a.GetServerTime(context.Background())
	if err != nil {
		t.Fatalf("GetServerTime: %v", err)
	}
	want := time.Unix(1700000000, 0).UTC()
	if !ts.Equal(want) {
		t.Fatalf("GetServerTime = %v, want %v", ts, want)
	}
}

// ============================================================
// GetInstruments — фильтрация и decimal-маппинг
// ============================================================

func TestGetInstruments_FilterAndDecimalMapping(t *testing.T) {
	// Тест: фильтруем PERPETUAL+USDT; пропускаем CURRENT_QUARTER и non-USDT
	payload := map[string]interface{}{
		"symbols": []map[string]interface{}{
			{
				"symbol":       "BTCUSDT",
				"contractType": "PERPETUAL",
				"status":       "TRADING",
				"baseAsset":    "BTC",
				"quoteAsset":   "USDT",
				"marginAsset":  "USDT",
				"filters": []map[string]interface{}{
					{"filterType": "LOT_SIZE", "minQty": "0.001", "maxQty": "1000", "stepSize": "0.001"},
					{"filterType": "PRICE_FILTER", "minPrice": "100", "maxPrice": "1000000", "tickSize": "0.10"},
				},
			},
			{
				// Должен быть пропущен: CURRENT_QUARTER
				"symbol":       "BTCUSDT_230331",
				"contractType": "CURRENT_QUARTER",
				"status":       "TRADING",
				"baseAsset":    "BTC",
				"quoteAsset":   "USDT",
				"marginAsset":  "USDT",
				"filters":      []map[string]interface{}{},
			},
			{
				// Должен быть пропущен: non-USDT quote
				"symbol":       "BTCBUSD",
				"contractType": "PERPETUAL",
				"status":       "TRADING",
				"baseAsset":    "BTC",
				"quoteAsset":   "BUSD",
				"marginAsset":  "BUSD",
				"filters":      []map[string]interface{}{},
			},
			{
				"symbol":       "ETHUSDT",
				"contractType": "PERPETUAL",
				"status":       "TRADING",
				"baseAsset":    "ETH",
				"quoteAsset":   "USDT",
				"marginAsset":  "USDT",
				"filters": []map[string]interface{}{
					{"filterType": "LOT_SIZE", "minQty": "0.01", "maxQty": "10000", "stepSize": "0.01"},
					{"filterType": "PRICE_FILTER", "minPrice": "10", "maxPrice": "100000", "tickSize": "0.01"},
				},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/exchangeInfo":
			jsonResp(w, 200, payload)
		case "/fapi/v1/fundingInfo":
			// Симулируем fundingInfo с 4h интервалом для BTCUSDT
			jsonResp(w, 200, []map[string]interface{}{
				{"symbol": "BTCUSDT", "fundingIntervalHours": 4},
			})
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	instruments, err := a.GetInstruments(context.Background())
	if err != nil {
		t.Fatalf("GetInstruments: %v", err)
	}

	if len(instruments) != 2 {
		t.Fatalf("GetInstruments: got %d, want 2", len(instruments))
	}

	btc := instruments[0]
	if btc.ExchangeSymbol != "BTCUSDT" {
		t.Errorf("instruments[0].ExchangeSymbol = %v, want BTCUSDT", btc.ExchangeSymbol)
	}
	if btc.QtyStep.String() != "0.001" {
		t.Errorf("QtyStep = %v, want 0.001", btc.QtyStep)
	}
	if btc.MinQty.String() != "0.001" {
		t.Errorf("MinQty = %v, want 0.001", btc.MinQty)
	}
	if btc.TickSize.String() != "0.1" {
		t.Errorf("TickSize = %v, want 0.1", btc.TickSize)
	}
	// fundingInfo смерджен: 4h * 3600 = 14400
	if btc.FundingIntervalSec != 4*3600 {
		t.Errorf("FundingIntervalSec = %d, want %d", btc.FundingIntervalSec, 4*3600)
	}
	if btc.CanonicalBaseAsset != "BTC" {
		t.Errorf("CanonicalBaseAsset = %v, want BTC", btc.CanonicalBaseAsset)
	}
	if btc.InstrumentType != domain.InstrumentLinearUSDTPerpetual {
		t.Errorf("InstrumentType = %v, want %v", btc.InstrumentType, domain.InstrumentLinearUSDTPerpetual)
	}

	eth := instruments[1]
	if eth.ExchangeSymbol != "ETHUSDT" {
		t.Errorf("instruments[1].ExchangeSymbol = %v, want ETHUSDT", eth.ExchangeSymbol)
	}
	if eth.FundingIntervalSec != 8*3600 {
		t.Errorf("ETH FundingIntervalSec = %d, want %d (default)", eth.FundingIntervalSec, 8*3600)
	}
}

// ============================================================
// GetFunding — парсинг и Confidence policy
// ============================================================

func TestGetFunding_Parsing(t *testing.T) {
	// nextFundingTime: 15 мин в будущем → HIGH confidence
	nextFundingMs := time.Now().Add(15 * time.Minute).UnixMilli()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/premiumIndex" {
			http.Error(w, "not found", 404)
			return
		}
		jsonResp(w, 200, map[string]interface{}{
			"symbol":          "BTCUSDT",
			"markPrice":       "50000.50",
			"indexPrice":      "50001.00",
			"lastFundingRate": "0.0001",
			"nextFundingTime": nextFundingMs,
			"time":            1700000000000,
		})
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	fi, err := a.GetFunding(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetFunding: %v", err)
	}
	if fi.RealizedFundingRate.String() != "0.0001" {
		t.Errorf("RealizedFundingRate = %v, want 0.0001", fi.RealizedFundingRate)
	}
	if fi.Confidence != domain.ConfidenceHigh {
		t.Errorf("Confidence = %v, want ConfidenceHigh", fi.Confidence)
	}
	if fi.FundingPriceType != domain.FundingPriceMark {
		t.Errorf("FundingPriceType = %v, want FundingPriceMark", fi.FundingPriceType)
	}
}

func TestGetFunding_ConfidenceLevels(t *testing.T) {
	cases := []struct {
		name           string
		minutesUntil   int
		wantConfidence domain.ConfidenceLevel
	}{
		{"HIGH_15min", 15, domain.ConfidenceHigh},
		{"MEDIUM_60min", 60, domain.ConfidenceMedium},
		{"LOW_300min", 300, domain.ConfidenceLow},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			nextFundingMs := time.Now().Add(time.Duration(tc.minutesUntil) * time.Minute).UnixMilli()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				jsonResp(w, 200, map[string]interface{}{
					"symbol":          "BTCUSDT",
					"markPrice":       "50000",
					"indexPrice":      "50001",
					"lastFundingRate": "0.0001",
					"nextFundingTime": nextFundingMs,
					"time":            1700000000000,
				})
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
// PlaceOrder — проверка query params + заголовка X-MBX-APIKEY
// ============================================================

func TestPlaceOrder_QueryParamsAndAPIKeyHeader(t *testing.T) {
	var capturedQuery url.Values
	var capturedAPIKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/order" || r.Method != http.MethodPost {
			http.Error(w, "not found", 404)
			return
		}
		capturedAPIKey = r.Header.Get("X-MBX-APIKEY")
		capturedQuery = r.URL.Query()

		jsonResp(w, 200, map[string]interface{}{
			"orderId":       123456789,
			"clientOrderId": "test-client-order-1",
			"symbol":        "BTCUSDT",
			"status":        "NEW",
			"updateTime":    1700000000000,
		})
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	qty, _ := decimal.FromString("0.001")
	price, _ := decimal.FromString("50000.0")

	req := domain.PlaceOrderRequest{
		ClientOrderID: "test-client-order-1",
		Symbol:        "BTCUSDT",
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarketableLimitIOC,
		BaseQty:       qty,
		Price:         price,
		ReduceOnly:    false,
	}

	ack, err := a.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if ack.ExchangeOrderID != "123456789" {
		t.Errorf("ExchangeOrderID = %v, want 123456789", ack.ExchangeOrderID)
	}
	if ack.ClientOrderID != "test-client-order-1" {
		t.Errorf("ClientOrderID = %v, want test-client-order-1", ack.ClientOrderID)
	}

	// Проверяем X-MBX-APIKEY
	if capturedAPIKey != "test-api-key" {
		t.Errorf("X-MBX-APIKEY = %q, want test-api-key", capturedAPIKey)
	}

	// Проверяем обязательные параметры запроса
	requiredParams := []string{"symbol", "side", "type", "quantity", "newClientOrderId", "signature", "timestamp", "recvWindow"}
	for _, p := range requiredParams {
		if capturedQuery.Get(p) == "" {
			t.Errorf("query missing required param: %s", p)
		}
	}

	if capturedQuery.Get("symbol") != "BTCUSDT" {
		t.Errorf("query.symbol = %v, want BTCUSDT", capturedQuery.Get("symbol"))
	}
	if capturedQuery.Get("side") != "BUY" {
		t.Errorf("query.side = %v, want BUY", capturedQuery.Get("side"))
	}
	if capturedQuery.Get("type") != "LIMIT" {
		t.Errorf("query.type = %v, want LIMIT", capturedQuery.Get("type"))
	}
	if capturedQuery.Get("quantity") != "0.001" {
		t.Errorf("query.quantity = %v, want 0.001", capturedQuery.Get("quantity"))
	}
	if capturedQuery.Get("newClientOrderId") != "test-client-order-1" {
		t.Errorf("query.newClientOrderId = %v, want test-client-order-1", capturedQuery.Get("newClientOrderId"))
	}
	// signature должна присутствовать (непустая)
	if capturedQuery.Get("signature") == "" {
		t.Error("signature missing in query")
	}
}

func TestPlaceOrder_ReduceOnly(t *testing.T) {
	var capturedQuery url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		jsonResp(w, 200, map[string]interface{}{
			"orderId":       987,
			"clientOrderId": "reduce-order",
			"status":        "NEW",
		})
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	qty, _ := decimal.FromString("0.01")

	req := domain.PlaceOrderRequest{
		ClientOrderID: "reduce-order",
		Symbol:        "BTCUSDT",
		Side:          domain.SideShort,
		OrderMode:     domain.OrderMarket,
		BaseQty:       qty,
		ReduceOnly:    true,
	}

	_, err := a.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}

	if capturedQuery.Get("side") != "SELL" {
		t.Errorf("query.side = %v, want SELL", capturedQuery.Get("side"))
	}
	if capturedQuery.Get("type") != "MARKET" {
		t.Errorf("query.type = %v, want MARKET", capturedQuery.Get("type"))
	}
	if capturedQuery.Get("reduceOnly") != "true" {
		t.Errorf("query.reduceOnly = %v, want true", capturedQuery.Get("reduceOnly"))
	}
}

// ============================================================
// Маппинг ошибок
// ============================================================

func TestErrorMapping_GetOrder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 400, -2013, "Order does not exist.")
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

func TestErrorMapping_CancelOrder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 400, -2011, "Unknown order sent.")
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ClientOrderID: "ghost-order",
		Symbol:        "BTCUSDT",
	})
	if err == nil {
		t.Fatal("expected ErrOrderNotFound, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

func TestErrorMapping_HTTP429_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 429, map[string]string{"msg": "too many requests"})
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

func TestErrorMapping_Unauthorized_2015(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 401, -2015, "Invalid API-key, IP, or permissions for action.")
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

func TestErrorMapping_InvalidSymbol_1121(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 400, -1121, "Invalid symbol.")
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetFunding(context.Background(), "INVALIDUSDT")
	if err == nil {
		t.Fatal("expected ErrInvalidSymbol, got nil")
	}
	if !errors.Is(err, exchange.ErrInvalidSymbol) {
		t.Errorf("expected ErrInvalidSymbol, got: %v", err)
	}
}

// ============================================================
// SetMarginMode: -4046 → success
// ============================================================

func TestSetMarginMode_NoNeedToChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 400, -4046, "No need to change margin type.")
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.SetMarginMode(context.Background(), domain.SetMarginModeRequest{
		Symbol:     "BTCUSDT",
		MarginMode: domain.MarginIsolated,
	})
	if err != nil {
		t.Errorf("SetMarginMode with -4046 should succeed, got: %v", err)
	}
}

// ============================================================
// SetPositionMode: -4059 → success
// ============================================================

func TestSetPositionMode_NoNeedToChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 400, -4059, "No need to change position side.")
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.SetPositionMode(context.Background(), domain.SetPositionModeRequest{
		Mode: domain.PositionOneWay,
	})
	if err != nil {
		t.Errorf("SetPositionMode with -4059 should succeed, got: %v", err)
	}
}

// ============================================================
// SetLeverage: -4028 → success
// ============================================================

func TestSetLeverage_AlreadySet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiError(w, 400, -4028, "Leverage not changed.")
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	lev, _ := decimal.FromString("10")
	err := a.SetLeverage(context.Background(), domain.SetLeverageRequest{
		Symbol:   "BTCUSDT",
		Leverage: lev,
	})
	if err != nil {
		t.Errorf("SetLeverage with -4028 should succeed, got: %v", err)
	}
}

// ============================================================
// GetADLState — quantile ÷ 4 нормализация
// ============================================================

func TestADLNormalization(t *testing.T) {
	cases := []struct {
		quantile int
		expected string
	}{
		{0, "0"},
		{1, "0.25"},
		{2, "0.5"},
		{3, "0.75"},
		{4, "1"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("quantile_%d", tc.quantile), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/fapi/v1/adlQuantile" {
					http.Error(w, "not found", 404)
					return
				}
				jsonResp(w, 200, []map[string]interface{}{
					{
						"symbol": "BTCUSDT",
						"adlQuantile": map[string]interface{}{
							"LONG":  tc.quantile,
							"SHORT": tc.quantile,
							"BOTH":  0, // one-way mode: Both=0, используем LONG/SHORT
							"HEDGE": 0,
						},
					},
				})
			}))
			defer srv.Close()

			a, _ := mkAdapter(srv)
			adl, err := a.GetADLState(context.Background(), "BTCUSDT")
			if err != nil {
				t.Fatalf("GetADLState: %v", err)
			}
			if adl.LongQueue.String() != tc.expected {
				t.Errorf("quantile=%d: LongQueue = %v, want %v", tc.quantile, adl.LongQueue, tc.expected)
			}
			if adl.ShortQueue.String() != tc.expected {
				t.Errorf("quantile=%d: ShortQueue = %v, want %v", tc.quantile, adl.ShortQueue, tc.expected)
			}
		})
	}
}

// ============================================================
// GetBalances — подписанный запрос
// ============================================================

func TestGetBalances(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v2/balance" {
			http.Error(w, "not found", 404)
			return
		}
		// Проверяем наличие X-MBX-APIKEY и signature
		if r.Header.Get("X-MBX-APIKEY") == "" {
			apiError(w, 401, -2014, "API-key format invalid.")
			return
		}
		if r.URL.Query().Get("signature") == "" {
			apiError(w, 400, -2015, "Invalid API-key, IP, or permissions for action.")
			return
		}
		jsonResp(w, 200, []map[string]interface{}{
			{
				"asset":              "USDT",
				"balance":            "10000.50",
				"availableBalance":   "8000.25",
				"crossWalletBalance": "9000.00",
				"marginAvailable":    true,
				"updateTime":         1700000000000,
			},
			{
				"asset":              "BNB",
				"balance":            "5.0",
				"availableBalance":   "5.0",
				"crossWalletBalance": "5.0",
				"marginAvailable":    true,
				"updateTime":         1700000000000,
			},
		})
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
	// decimal.FromString("10000.50") → "10000.5" (trailing zero нормализуется)
	if usdt.WalletBalance.String() != "10000.5" {
		t.Errorf("WalletBalance = %v, want 10000.5", usdt.WalletBalance)
	}
	if usdt.AvailableBalance.String() != "8000.25" {
		t.Errorf("AvailableBalance = %v, want 8000.25", usdt.AvailableBalance)
	}
}

// ============================================================
// GetPositions — парсинг positionAmt со знаком
// ============================================================

func TestGetPositions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v2/positionRisk" {
			http.Error(w, "not found", 404)
			return
		}
		jsonResp(w, 200, []map[string]interface{}{
			{
				"symbol":           "BTCUSDT",
				"positionAmt":      "0.1", // long
				"entryPrice":       "50000",
				"breakEvenPrice":   "50010",
				"markPrice":        "51000",
				"unRealizedProfit": "100.0",
				"liquidationPrice": "30000",
				"leverage":         "10",
				"maxNotionalValue": "1000000",
				"marginType":       "cross",
				"isolatedMargin":   "0",
				"isAutoAddMargin":  "false",
				"positionSide":     "BOTH",
				"notional":         "5100",
				"isolatedWallet":   "0",
				"updateTime":       1700000000000,
			},
			{
				// Пустая позиция — должна быть пропущена
				"symbol":           "ETHUSDT",
				"positionAmt":      "0",
				"entryPrice":       "0",
				"markPrice":        "3000",
				"unRealizedProfit": "0",
				"liquidationPrice": "0",
				"leverage":         "5",
				"marginType":       "cross",
				"isolatedMargin":   "0",
				"positionSide":     "BOTH",
				"updateTime":       1700000000000,
			},
			{
				// Short: positionAmt отрицательный
				"symbol":           "SOLUSDT",
				"positionAmt":      "-10",
				"entryPrice":       "100",
				"markPrice":        "95",
				"unRealizedProfit": "50.0",
				"liquidationPrice": "200",
				"leverage":         "5",
				"marginType":       "isolated",
				"isolatedMargin":   "200",
				"positionSide":     "BOTH",
				"updateTime":       1700000000000,
			},
		})
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	positions, err := a.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("expected 2 non-zero positions, got %d", len(positions))
	}

	btc := positions[0]
	if btc.Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %v, want BTCUSDT", btc.Symbol)
	}
	if btc.Side != domain.SideLong {
		t.Errorf("Side = %v, want long", btc.Side)
	}
	if btc.ContractQty.String() != "0.1" {
		t.Errorf("ContractQty = %v, want 0.1", btc.ContractQty)
	}
	if btc.MarginMode != domain.MarginCross {
		t.Errorf("MarginMode = %v, want cross", btc.MarginMode)
	}

	sol := positions[1]
	if sol.Side != domain.SideShort {
		t.Errorf("SOL Side = %v, want short", sol.Side)
	}
	if sol.ContractQty.String() != "10" {
		t.Errorf("SOL ContractQty = %v, want 10 (abs value)", sol.ContractQty)
	}
	if sol.MarginMode != domain.MarginIsolated {
		t.Errorf("SOL MarginMode = %v, want isolated", sol.MarginMode)
	}
}

// ============================================================
// GetOrderBookSnapshot — парсинг bids/asks
// ============================================================

func TestGetOrderBookSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/depth" {
			http.Error(w, "not found", 404)
			return
		}
		jsonResp(w, 200, map[string]interface{}{
			"lastUpdateId": 999888777,
			"E":            1700000000000,
			"T":            1700000000001,
			"bids":         [][]string{{"50000.0", "1.5"}, {"49999.9", "2.0"}},
			"asks":         [][]string{{"50001.0", "0.8"}, {"50002.0", "3.0"}},
		})
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
	// decimal.FromString("50000.0") → "50000" (trailing zero нормализуется)
	if ob.Bids[0].Price.String() != "50000" {
		t.Errorf("bids[0].price = %v, want 50000", ob.Bids[0].Price)
	}
	if ob.Asks[0].Price.String() != "50001" {
		t.Errorf("asks[0].price = %v, want 50001", ob.Asks[0].Price)
	}
	if !ob.IsSnapshot {
		t.Error("IsSnapshot should be true")
	}
	if ob.Sequence != 999888777 {
		t.Errorf("Sequence = %d, want 999888777", ob.Sequence)
	}
	if ob.Exchange != domain.ExchangeBinance {
		t.Errorf("Exchange = %v, want binance", ob.Exchange)
	}
}

// ============================================================
// GetTicker — два вызова bookTicker + 24hr
// ============================================================

func TestGetTicker_TwoCalls(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch r.URL.Path {
		case "/fapi/v1/ticker/bookTicker":
			jsonResp(w, 200, map[string]interface{}{
				"symbol":   "BTCUSDT",
				"bidPrice": "49999.9",
				"bidQty":   "1.5",
				"askPrice": "50000.1",
				"askQty":   "1.0",
				"time":     1700000000000,
			})
		case "/fapi/v1/ticker/24hr":
			jsonResp(w, 200, map[string]interface{}{
				"symbol":      "BTCUSDT",
				"lastPrice":   "50000.0",
				"quoteVolume": "1234567890.00",
				"closeTime":   1700000000000,
			})
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ticker, err := a.GetTicker(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (bookTicker+24hr), got %d", callCount)
	}
	// decimal.FromString нормализует trailing zeros
	if ticker.LastPrice.String() != "50000.0" && ticker.LastPrice.String() != "50000" {
		t.Errorf("LastPrice = %v, want ~50000", ticker.LastPrice)
	}
	// quoteVolume тоже нормализуется
	want24hVol := "1234567890"
	if ticker.QuoteVolume24h.String() != want24hVol && ticker.QuoteVolume24h.String() != "1234567890.00" {
		t.Errorf("QuoteVolume24h = %v, want ~%v", ticker.QuoteVolume24h, want24hVol)
	}
	if ticker.Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %v, want BTCUSDT", ticker.Symbol)
	}
}

// ============================================================
// GetOpenOrders — парсинг массива
// ============================================================

func TestGetOpenOrders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/openOrders" {
			http.Error(w, "not found", 404)
			return
		}
		jsonResp(w, 200, []map[string]interface{}{
			{
				"orderId":       111,
				"clientOrderId": "my-order-1",
				"symbol":        "BTCUSDT",
				"side":          "BUY",
				"type":          "LIMIT",
				"origQty":       "0.01",
				"executedQty":   "0",
				"avgPrice":      "0",
				"price":         "50000",
				"status":        "NEW",
				"reduceOnly":    false,
				"timeInForce":   "IOC",
				"time":          1700000000000,
				"updateTime":    1700000000001,
			},
		})
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
	o := orders[0]
	if o.ClientOrderID != "my-order-1" {
		t.Errorf("ClientOrderID = %v, want my-order-1", o.ClientOrderID)
	}
	if o.Side != domain.SideLong {
		t.Errorf("Side = %v, want long", o.Side)
	}
	if o.Status != domain.OrderStatusAcknowledged {
		t.Errorf("Status = %v, want acknowledged", o.Status)
	}
	if o.RequestedQty.String() != "0.01" {
		t.Errorf("RequestedQty = %v, want 0.01", o.RequestedQty)
	}
}

// ============================================================
// Проверка: decimal.FromString используется, не float
// ============================================================

func TestDecimal_NoFloat(t *testing.T) {
	// Проверяем что точность сохраняется при малых значениях
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, 200, map[string]interface{}{
			"symbol":          "BTCUSDT",
			"markPrice":       "50000.123456789",
			"indexPrice":      "50001.987654321",
			"lastFundingRate": "0.000100001",
			"nextFundingTime": time.Now().Add(1 * time.Hour).UnixMilli(),
			"time":            1700000000000,
		})
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	fi, err := a.GetFunding(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetFunding: %v", err)
	}
	// decimal.FromString сохраняет точность
	if fi.RealizedFundingRate.String() != "0.000100001" {
		t.Errorf("RealizedFundingRate precision lost: %v", fi.RealizedFundingRate)
	}
}
