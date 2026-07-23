// Контрактные тесты адаптера KuCoin Futures.
//
// Тесты используют httptest-сервер с точными KuCoin-конвертами.
// Проверяется:
//   - GetInstruments: XBT→BTC маппинг, multiplier, lot-конверсия
//   - GetFunding: парсинг value/predictedValue/fundingTime, Confidence policy
//   - GetTicker: парсинг price
//   - GetOrderBookSnapshot: bids/asks
//   - GetBalances: подписанный запрос, парсинг accountEquity/availableBalance
//   - GetPositions: currentQty знаковое → domain long/short abs, lot→base
//   - PlaceOrder: KC-API-* заголовки присутствуют, KC-API-PASSPHRASE HMAC-signed,
//     size в лотах, clientOid
//   - CancelOrder: resolve by clientOid → orderId
//   - GetOrder: по orderId и по clientOid
//   - Error mapping: 429000→ErrRateLimited, 400004→ErrUnauthorized, 100004→ErrOrderNotFound
//   - Signer known-vector (отдельно в signer_test.go)
package kucoin

import (
	"context"
	"encoding/json"
	"errors"
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
// Test helpers
// ============================================================

// testHTTPDoer — HTTPDoer, проксирующий запросы к httptest-серверу.
// Сохраняет последний запрос для инспекции.
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
	// Spot API paths имеют префикс [SPOT] — в тестах используем тот же сервер.
	path := req.Path
	if strings.HasPrefix(path, "[SPOT]") {
		path = strings.TrimPrefix(path, "[SPOT]")
	}

	url := d.baseURL + path
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

// kcResponse формирует тестовый ответ в формате KuCoin конверта.
func kcResponse(code string, data interface{}) []byte {
	d, _ := json.Marshal(data)
	env := map[string]interface{}{
		"code": code,
		"msg":  "",
		"data": json.RawMessage(d),
	}
	b, _ := json.Marshal(env)
	return b
}

// kcOK — успешный ответ.
func kcOK(data interface{}) []byte { return kcResponse("200000", data) }

// kcErr — ответ с ошибкой.
func kcErr(code, msg string) []byte {
	env := map[string]interface{}{"code": code, "msg": msg}
	b, _ := json.Marshal(env)
	return b
}

// mkAdapter создаёт адаптер для тестов.
func mkAdapter(srv *httptest.Server) (*Adapter, *testHTTPDoer) {
	doer := newTestDoer(srv.URL)
	a, err := New(Config{
		RESTBaseURL:  srv.URL,
		SpotBaseURL:  srv.URL,
		WSBaseURL:    "ws://localhost",
		APIKey:       "test-api-key",
		APISecret:    "test-secret",
		Passphrase:   "test-passphrase",
		HTTPDoer:     doer,
		RecvWindowMs: 5000,
		Clock:        func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if err != nil {
		panic("mkAdapter: " + err.Error())
	}
	// Заполняем кеш multiplier для XBTUSDTM (multiplier=0.001)
	mult, _ := decimal.FromString("0.001")
	a.instrMu.Lock()
	a.instrCache[domain.ExchangeSymbol("XBTUSDTM")] = instrCacheEntry{multiplier: mult}
	a.instrMu.Unlock()
	return a, doer
}

// ============================================================
// GetServerTime
// ============================================================

func TestGetServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/timestamp" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// data = JSON number (ms)
		w.Write(kcOK(json.Number("1700000000000")))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ts, err := a.GetServerTime(context.Background())
	if err != nil {
		t.Fatalf("GetServerTime: %v", err)
	}
	want := time.Unix(1700000000, 0).UTC()
	if !ts.Equal(want) {
		t.Fatalf("GetServerTime: got %v, want %v", ts, want)
	}
}

// ============================================================
// GetInstruments — XBT→BTC маппинг + multiplier
// ============================================================

func TestGetInstruments_XBTtoBTC(t *testing.T) {
	contracts := []map[string]interface{}{
		{
			"symbol":                 "XBTUSDTM",
			"baseCurrency":           "XBT", // ← должно стать BTC
			"quoteCurrency":          "USDT",
			"settleCurrency":         "USDT",
			"multiplier":             0.001,
			"lotSize":                1,
			"tickSize":               0.1,
			"maxLeverage":            125,
			"status":                 "Open",
			"isInverse":              false,
			"fundingFeeRate":         0.0001,
			"fundingRateGranularity": 28800000,
			"nextFundingRateTime":    3600000,
		},
		{
			// ETH контракт — baseCurrency уже ETH
			"symbol":                 "ETHUSDTM",
			"baseCurrency":           "ETH",
			"quoteCurrency":          "USDT",
			"settleCurrency":         "USDT",
			"multiplier":             0.01,
			"lotSize":                1,
			"tickSize":               0.01,
			"maxLeverage":            100,
			"status":                 "Open",
			"isInverse":              false,
			"fundingFeeRate":         0.00005,
			"fundingRateGranularity": 28800000,
			"nextFundingRateTime":    3600000,
		},
		{
			// Inverse — должен быть отфильтрован
			"symbol":         "XBTUSDM",
			"baseCurrency":   "XBT",
			"settleCurrency": "XBT",
			"multiplier":     1,
			"lotSize":        1,
			"tickSize":       0.5,
			"maxLeverage":    100,
			"status":         "Open",
			"isInverse":      true,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/contracts/active" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcOK(contracts))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	instruments, err := a.GetInstruments(context.Background())
	if err != nil {
		t.Fatalf("GetInstruments: %v", err)
	}

	if len(instruments) != 2 {
		t.Fatalf("expected 2 instruments (inverse filtered), got %d", len(instruments))
	}

	// XBTUSDTM: XBT → BTC
	xbt := instruments[0]
	if xbt.ExchangeSymbol != "XBTUSDTM" {
		t.Errorf("instruments[0].ExchangeSymbol = %v, want XBTUSDTM", xbt.ExchangeSymbol)
	}
	if xbt.CanonicalBaseAsset != "BTC" {
		t.Errorf("XBT→BTC маппинг: CanonicalBaseAsset = %v, want BTC", xbt.CanonicalBaseAsset)
	}
	if xbt.ContractMultiplier.String() != "0.001" {
		t.Errorf("ContractMultiplier = %v, want 0.001", xbt.ContractMultiplier)
	}
	// fundingIntervalSec = 28800000ms / 1000 = 28800 s
	if xbt.FundingIntervalSec != 28800 {
		t.Errorf("FundingIntervalSec = %d, want 28800", xbt.FundingIntervalSec)
	}

	// Кеш multiplier должен быть обновлён
	a.instrMu.RLock()
	cached, ok := a.instrCache["XBTUSDTM"]
	a.instrMu.RUnlock()
	if !ok {
		t.Error("instrCache missing XBTUSDTM after GetInstruments")
	} else if cached.multiplier.String() != "0.001" {
		t.Errorf("cached multiplier = %v, want 0.001", cached.multiplier)
	}

	// ETH: нет маппинга
	eth := instruments[1]
	if eth.CanonicalBaseAsset != "ETH" {
		t.Errorf("ETH: CanonicalBaseAsset = %v, want ETH", eth.CanonicalBaseAsset)
	}
}

// ============================================================
// GetFunding — парсинг + Confidence
// ============================================================

func TestGetFunding(t *testing.T) {
	// fundingTime: 15 минут в будущем → HIGH confidence
	nextFundingMs := time.Now().Add(15 * time.Minute).UnixMilli()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/funding-rate/XBTUSDTM/current" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcOK(map[string]interface{}{
			"symbol":         ".XBTUSDTMFPI8H",
			"granularity":    28800000,
			"timePoint":      1700000000000,
			"value":          0.0001,
			"predictedValue": 0.00015,
			"fundingTime":    nextFundingMs,
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	fi, err := a.GetFunding(context.Background(), "XBTUSDTM")
	if err != nil {
		t.Fatalf("GetFunding: %v", err)
	}
	if fi.RealizedFundingRate.String() != "0.0001" {
		t.Errorf("RealizedFundingRate = %v, want 0.0001", fi.RealizedFundingRate)
	}
	if fi.PredictedFundingRate.String() != "0.00015" {
		t.Errorf("PredictedFundingRate = %v, want 0.00015", fi.PredictedFundingRate)
	}
	if fi.Confidence != domain.ConfidenceHigh {
		t.Errorf("Confidence = %v, want ConfidenceHigh", fi.Confidence)
	}
	if fi.FundingIntervalSec != 28800 {
		t.Errorf("FundingIntervalSec = %d, want 28800", fi.FundingIntervalSec)
	}
}

func TestGetFunding_ConfidenceLevels(t *testing.T) {
	cases := []struct {
		name         string
		minutesUntil int
		want         domain.ConfidenceLevel
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
				w.Write(kcOK(map[string]interface{}{
					"value":       0.0001,
					"granularity": 28800000,
					"fundingTime": nextFundingMs,
				}))
			}))
			defer srv.Close()

			a, _ := mkAdapter(srv)
			fi, err := a.GetFunding(context.Background(), "XBTUSDTM")
			if err != nil {
				t.Fatalf("GetFunding: %v", err)
			}
			if fi.Confidence != tc.want {
				t.Errorf("confidence = %v, want %v", fi.Confidence, tc.want)
			}
		})
	}
}

// ============================================================
// GetTicker
// ============================================================

func TestGetTicker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ticker" {
			http.Error(w, "not found", 404)
			return
		}
		sym := r.URL.Query().Get("symbol")
		if sym != "XBTUSDTM" {
			http.Error(w, "bad symbol", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcOK(map[string]interface{}{
			"symbol":       "XBTUSDTM",
			"price":        "50000.5",
			"bestBidPrice": "49999.0",
			"bestAskPrice": "50001.0",
			"ts":           1700000000000000000,
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ticker, err := a.GetTicker(context.Background(), "XBTUSDTM")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if ticker.LastPrice.String() != "50000.5" {
		t.Errorf("LastPrice = %v, want 50000.5", ticker.LastPrice)
	}
	if ticker.Symbol != "XBTUSDTM" {
		t.Errorf("Symbol = %v, want XBTUSDTM", ticker.Symbol)
	}
}

// ============================================================
// GetOrderBookSnapshot
// ============================================================

func TestGetOrderBookSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/level2/depth20" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcOK(map[string]interface{}{
			"symbol":   "XBTUSDTM",
			"bids":     [][]string{{"50000", "10"}, {"49999", "20"}},
			"asks":     [][]string{{"50001", "15"}, {"50002", "25"}},
			"ts":       1700000000000,
			"sequence": 12345,
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ob, err := a.GetOrderBookSnapshot(context.Background(), "XBTUSDTM", 20)
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

// ============================================================
// GetBalances — подписанный запрос
// ============================================================

func TestGetBalances(t *testing.T) {
	var gotSign, gotPassphrase, gotKeyVer string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/account-overview" {
			http.Error(w, "not found", 404)
			return
		}
		gotSign = r.Header.Get("KC-API-SIGN")
		gotPassphrase = r.Header.Get("KC-API-PASSPHRASE")
		gotKeyVer = r.Header.Get("KC-API-KEY-VERSION")

		w.Header().Set("Content-Type", "application/json")
		w.Write(kcOK(map[string]interface{}{
			"accountEquity":    10000.5,
			"availableBalance": 8000.25,
			"currency":         "USDT",
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
	b := balances[0]
	if b.Asset != "USDT" {
		t.Errorf("Asset = %v, want USDT", b.Asset)
	}
	if b.WalletBalance.String() != "10000.5" {
		t.Errorf("WalletBalance = %v, want 10000.5", b.WalletBalance)
	}
	if b.AvailableBalance.String() != "8000.25" {
		t.Errorf("AvailableBalance = %v, want 8000.25", b.AvailableBalance)
	}

	// KC-API-SIGN обязателен
	if gotSign == "" {
		t.Error("KC-API-SIGN header missing")
	}
	// KC-API-PASSPHRASE должна быть HMAC-подписана (base64, не plain)
	if gotPassphrase == "" {
		t.Error("KC-API-PASSPHRASE header missing")
	}
	if gotPassphrase == "test-passphrase" {
		t.Error("KC-API-PASSPHRASE должна быть HMAC-signed, а не plain passphrase")
	}
	// KC-API-KEY-VERSION = "2"
	if gotKeyVer != "2" {
		t.Errorf("KC-API-KEY-VERSION = %q, want \"2\"", gotKeyVer)
	}
	// Проверяем конкретное значение HMAC passphrase (из known-vector signer_test.go)
	wantPassphrase := "UbgWiL7WdjQOVBl1OLuMgUbTl9VlKFsjFbLedtCDPrY="
	if gotPassphrase != wantPassphrase {
		t.Errorf("KC-API-PASSPHRASE:\n  got  %q\n  want %q", gotPassphrase, wantPassphrase)
	}
}

// ============================================================
// GetPositions — знаковые currentQty → long/short + lot→base
// ============================================================

func TestGetPositions_LongShortConversion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/positions" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcOK([]map[string]interface{}{
			{
				// Long: currentQty > 0 → SideLong; 100 лотов × 0.001 = 0.1 BTC
				"symbol":           "XBTUSDTM",
				"currentQty":       100,
				"avgEntryPrice":    50000,
				"markPrice":        50100,
				"liquidationPrice": 30000,
				"unrealisedPnl":    10.0,
				"maintMarginReq":   0.004,
				"realLeverage":     10.5,
				"isOpen":           true,
				"crossMode":        false,
				"settleCurrency":   "USDT",
				"posMargin":        500.0,
			},
			{
				// Short: currentQty < 0 → SideShort; -50 лотов → 50×0.001=0.05 BTC abs
				"symbol":           "XBTUSDTM",
				"currentQty":       -50,
				"avgEntryPrice":    51000,
				"markPrice":        50100,
				"liquidationPrice": 70000,
				"unrealisedPnl":    45.0,
				"maintMarginReq":   0.004,
				"realLeverage":     10.0,
				"isOpen":           true,
				"crossMode":        false,
				"settleCurrency":   "USDT",
				"posMargin":        255.0,
			},
			{
				// Закрытая позиция — должна быть пропущена
				"symbol":     "ETHUSDTM",
				"currentQty": 0,
				"isOpen":     false,
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	positions, err := a.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("expected 2 positions, got %d", len(positions))
	}

	long := positions[0]
	if long.Side != domain.SideLong {
		t.Errorf("positions[0].Side = %v, want SideLong", long.Side)
	}
	// BaseQty = 100 * 0.001 = 0.1
	if long.BaseQty.String() != "0.1" {
		t.Errorf("positions[0].BaseQty = %v, want 0.1", long.BaseQty)
	}
	// ContractQty = abs(100) = 100
	if long.ContractQty.String() != "100" {
		t.Errorf("positions[0].ContractQty = %v, want 100", long.ContractQty)
	}
	if long.MarginMode != domain.MarginIsolated {
		t.Errorf("MarginMode = %v, want MarginIsolated", long.MarginMode)
	}

	short := positions[1]
	if short.Side != domain.SideShort {
		t.Errorf("positions[1].Side = %v, want SideShort", short.Side)
	}
	// BaseQty = 50 * 0.001 = 0.05
	if short.BaseQty.String() != "0.05" {
		t.Errorf("positions[1].BaseQty = %v, want 0.05", short.BaseQty)
	}
}

// ============================================================
// PlaceOrder — KC-API-* заголовки, size в лотах, clientOid
// ============================================================

func TestPlaceOrder_HeadersAndLots(t *testing.T) {
	var capturedBody []byte
	var capturedSign, capturedPassphrase, capturedKeyVer string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/orders" || r.Method != http.MethodPost {
			http.Error(w, "not found", 404)
			return
		}
		capturedSign = r.Header.Get("KC-API-SIGN")
		capturedPassphrase = r.Header.Get("KC-API-PASSPHRASE")
		capturedKeyVer = r.Header.Get("KC-API-KEY-VERSION")

		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcOK(map[string]string{
			"orderId":   "exchange-order-123",
			"clientOid": "client-order-abc",
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)

	// BaseQty = 0.1 BTC; multiplier = 0.001 → 100 лотов
	qty, _ := decimal.FromString("0.1")
	price, _ := decimal.FromString("50000")

	req := domain.PlaceOrderRequest{
		ClientOrderID: "client-order-abc",
		Symbol:        "XBTUSDTM",
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

	// KC-API-* заголовки обязательны
	if capturedSign == "" {
		t.Error("KC-API-SIGN header missing")
	}
	if capturedPassphrase == "" {
		t.Error("KC-API-PASSPHRASE header missing")
	}
	if capturedPassphrase == "test-passphrase" {
		t.Error("KC-API-PASSPHRASE должна быть HMAC-signed")
	}
	if capturedKeyVer != "2" {
		t.Errorf("KC-API-KEY-VERSION = %q, want \"2\"", capturedKeyVer)
	}

	// Парсинг тела
	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}

	// Обязательные поля
	requiredFields := []string{"clientOid", "symbol", "side", "type", "size", "reduceOnly"}
	for _, f := range requiredFields {
		if _, ok := body[f]; !ok {
			t.Errorf("body missing required field: %s", f)
		}
	}

	// size должна быть в лотах (100, а не 0.1)
	sizeVal := body["size"]
	// json.Unmarshal в map[string]interface{} даёт float64
	sizeFloat, ok := sizeVal.(float64)
	if !ok {
		t.Errorf("body.size type = %T, want numeric", sizeVal)
	} else if int64(sizeFloat) != 100 {
		t.Errorf("body.size = %v, want 100 (lots)", sizeFloat)
	}

	if body["clientOid"] != "client-order-abc" {
		t.Errorf("body.clientOid = %v, want client-order-abc", body["clientOid"])
	}
	if body["symbol"] != "XBTUSDTM" {
		t.Errorf("body.symbol = %v, want XBTUSDTM", body["symbol"])
	}
	if body["side"] != "buy" {
		t.Errorf("body.side = %v, want buy", body["side"])
	}
}

// ============================================================
// CancelOrder — resolve clientOid → orderId
// ============================================================

func TestCancelOrder_ResolveByClientOid(t *testing.T) {
	var deletedOrderId string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/orders/byClientOid" && r.Method == http.MethodGet:
			// Resolve clientOid → orderId
			clientOid := r.URL.Query().Get("clientOid")
			if clientOid != "my-client-id" {
				http.Error(w, "not found", 404)
				return
			}
			w.Write(kcOK(map[string]interface{}{
				"orderId":   "exchange-order-999",
				"clientOid": "my-client-id",
				"symbol":    "XBTUSDTM",
				"side":      "buy",
				"type":      "limit",
				"size":      100,
				"price":     "50000",
				"status":    "open",
				"isOpen":    true,
				"createdAt": 1700000000000,
			}))
		case strings.HasPrefix(r.URL.Path, "/api/v1/orders/") && r.Method == http.MethodDelete:
			deletedOrderId = strings.TrimPrefix(r.URL.Path, "/api/v1/orders/")
			w.Write(kcOK(map[string]string{"cancelledOrderIds": deletedOrderId}))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ClientOrderID: "my-client-id",
		Symbol:        "XBTUSDTM",
	})
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if deletedOrderId != "exchange-order-999" {
		t.Errorf("deleted orderId = %q, want exchange-order-999", deletedOrderId)
	}
}

// ============================================================
// GetOrder — по orderId и по clientOid
// ============================================================

func TestGetOrder_ByClientOid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/orders/byClientOid" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcOK(map[string]interface{}{
			"orderId":      "exchange-567",
			"clientOid":    "client-567",
			"symbol":       "XBTUSDTM",
			"side":         "sell",
			"type":         "limit",
			"size":         50,
			"price":        "50000",
			"filledSize":   50,
			"avgDealPrice": 50100,
			"fee":          0.5,
			"status":       "done",
			"reduceOnly":   true,
			"createdAt":    1700000000000,
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ord, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "client-567",
		Symbol:        "XBTUSDTM",
	})
	if err != nil {
		t.Fatalf("GetOrder by clientOid: %v", err)
	}
	if ord.ExchangeOrderID != "exchange-567" {
		t.Errorf("ExchangeOrderID = %v, want exchange-567", ord.ExchangeOrderID)
	}
	if ord.Side != domain.SideShort {
		t.Errorf("Side = %v, want SideShort", ord.Side)
	}
	if ord.Status != domain.OrderStatusFilled {
		t.Errorf("Status = %v, want OrderStatusFilled", ord.Status)
	}
	// FilledQty = 50 лотов × 0.001 = 0.05
	if ord.FilledQty.String() != "0.05" {
		t.Errorf("FilledQty = %v, want 0.05", ord.FilledQty)
	}
}

func TestGetOrder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcErr("100004", "order not found"))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "no-such-order",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

// ============================================================
// Error mapping
// ============================================================

func TestErrorMapping_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcErr("429000", "too many requests"))
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

func TestErrorMapping_Unauthorized_Passphrase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcErr("400004", "KC-API-PASSPHRASE not match"))
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

func TestErrorMapping_Unauthorized_Sign(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcErr("400005", "KC-API-SIGN not match"))
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

func TestErrorMapping_OrderNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(kcErr("100004", "order not found"))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ExchangeOrderID: "no-exist",
		Symbol:          "XBTUSDTM",
	})
	if err == nil {
		t.Fatal("expected ErrOrderNotFound, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

// ============================================================
// New() — validation
// ============================================================

func TestNew_MissingPassphrase(t *testing.T) {
	doer := newTestDoer("http://localhost")
	_, err := New(Config{
		APIKey:    "key",
		APISecret: "secret",
		// Passphrase отсутствует
		HTTPDoer: doer,
	})
	if err == nil {
		t.Fatal("expected error for missing passphrase")
	}
	if !strings.Contains(err.Error(), "Passphrase") {
		t.Errorf("error should mention Passphrase, got: %v", err)
	}
}

func TestNew_MissingHTTPDoer(t *testing.T) {
	_, err := New(Config{
		APIKey:     "key",
		APISecret:  "secret",
		Passphrase: "pass",
		// HTTPDoer отсутствует
	})
	if err == nil {
		t.Fatal("expected error for missing HTTPDoer")
	}
}

// ============================================================
// GetADLState — нулевой (нет публичного API)
// ============================================================

func TestGetADLState_ReturnsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	state, err := a.GetADLState(context.Background(), "XBTUSDTM")
	if err != nil {
		t.Fatalf("GetADLState: %v", err)
	}
	if !state.LongQueue.IsZero() {
		t.Errorf("LongQueue = %v, want 0", state.LongQueue)
	}
	if !state.ShortQueue.IsZero() {
		t.Errorf("ShortQueue = %v, want 0", state.ShortQueue)
	}
}

// ============================================================
// baseQtyToLots — lot conversion
// ============================================================

func TestBaseQtyToLots(t *testing.T) {
	doer := newTestDoer("http://localhost")
	a, _ := New(Config{
		APIKey:     "k",
		APISecret:  "s",
		Passphrase: "p",
		HTTPDoer:   doer,
	})

	// XBTUSDTM: multiplier = 0.001
	mult, _ := decimal.FromString("0.001")
	a.instrMu.Lock()
	a.instrCache["XBTUSDTM"] = instrCacheEntry{multiplier: mult}
	a.instrMu.Unlock()

	cases := []struct {
		baseQty  string
		wantLots int64
	}{
		{"0.1", 100},
		{"0.001", 1},
		{"1.0", 1000},
		{"0.0015", 1}, // floor: 1.5 → 1
	}

	for _, tc := range cases {
		qty, _ := decimal.FromString(tc.baseQty)
		lots, err := a.baseQtyToLots("XBTUSDTM", qty)
		if err != nil {
			t.Errorf("baseQty %s: %v", tc.baseQty, err)
			continue
		}
		if lots != tc.wantLots {
			t.Errorf("baseQty %s → lots = %d, want %d", tc.baseQty, lots, tc.wantLots)
		}
	}

	// Слишком маленький qty → ошибка
	tiny, _ := decimal.FromString("0.0009")
	_, err := a.baseQtyToLots("XBTUSDTM", tiny)
	if err == nil {
		t.Error("expected error for qty < multiplier, got nil")
	}
}
