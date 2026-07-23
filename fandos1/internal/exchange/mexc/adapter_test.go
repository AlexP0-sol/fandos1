// Контрактные тесты адаптера MEXC Contract API v1.
//
// Тесты используют httptest-сервер с точными конвертами MEXC.
// Проверяется:
//   - Парсинг GetInstruments (contractSize кеш + конверсия)
//   - Парсинг GetFunding (fundingRate, nextSettleTime, Confidence policy)
//   - Парсинг GetTicker
//   - Парсинг GetOrderBookSnapshot
//   - PlaceOrder: заголовки ApiKey/Request-Time/Signature; side-код (open long/close short);
//     vol в контрактах; externalOid
//   - GetOrder (по externalOid) — 404 → ErrOrderNotFound
//   - Маппинг ошибок: 429 → ErrRateLimited, 1002 → ErrUnauthorized, margin → ErrInsufficientMargin
//   - GetPositions → domain.Position
//   - Signer known-vector (в signer_test.go)
package mexc

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

// mexcResponse формирует тестовый ответ в формате MEXC Contract API v1.
func mexcResponse(success bool, code int64, data interface{}) []byte {
	var rawData json.RawMessage
	if data != nil {
		b, _ := json.Marshal(data)
		rawData = json.RawMessage(b)
	} else {
		rawData = json.RawMessage("null")
	}
	env := map[string]interface{}{
		"success": success,
		"code":    code,
		"data":    rawData,
	}
	if !success {
		env["message"] = fmt.Sprintf("error code=%d", code)
	}
	b, _ := json.Marshal(env)
	return b
}

// mkAdapter создаёт адаптер для тестов.
func mkAdapter(srv *httptest.Server) (*Adapter, *testHTTPDoer) {
	doer := newTestDoer(srv.URL)
	a, err := New(Config{
		RESTBaseURL: srv.URL,
		SpotBaseURL: srv.URL,
		WSBaseURL:   "ws://localhost",
		APIKey:      "TEST_API_KEY",
		APISecret:   "TEST_API_SECRET",
		HTTPDoer:    doer,
		Clock:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if err != nil {
		panic(fmt.Sprintf("mkAdapter: %v", err))
	}
	return a, doer
}

// ============================================================
// New — конструктор
// ============================================================

func TestNew_MissingFields(t *testing.T) {
	doer := newTestDoer("http://localhost")
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no HTTPDoer", Config{APIKey: "k", APISecret: "s"}},
		{"no APIKey", Config{HTTPDoer: doer, APISecret: "s"}},
		{"no APISecret", Config{HTTPDoer: doer, APIKey: "k"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Errorf("%s: expected error, got nil", tc.name)
			}
		})
	}
}

func TestNew_Defaults(t *testing.T) {
	doer := newTestDoer("http://localhost")
	a, err := New(Config{APIKey: "k", APISecret: "s", HTTPDoer: doer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.restBase != defaultRESTBase {
		t.Errorf("restBase = %s, want %s", a.restBase, defaultRESTBase)
	}
	if a.spotBase != defaultSpotBase {
		t.Errorf("spotBase = %s, want %s", a.spotBase, defaultSpotBase)
	}
	if a.ID() != domain.ExchangeMEXC {
		t.Errorf("ID = %v, want ExchangeMEXC", a.ID())
	}
}

// ============================================================
// GetServerTime
// ============================================================

func TestGetServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/contract/ping" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, map[string]int64{"serverTime": 1700000000000}))
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
// GetInstruments — парсинг + contractSize кеш + конверсия
// ============================================================

func TestGetInstruments_ParseAndContractSizeCache(t *testing.T) {
	entries := []map[string]interface{}{
		{
			"symbol":       "BTC_USDT",
			"baseCoin":     "BTC",
			"quoteCoin":    "USDT",
			"settleCoin":   "USDT",
			"contractSize": 0.0001, // 1 контракт = 0.0001 BTC
			"priceUnit":    0.5,    // tickSize
			"volUnit":      1,      // шаг = 1 контракт
			"minVol":       1,
			"maxVol":       400000,
			"maxLeverage":  200,
			"state":        0, // active
			"apiAllowed":   true,
		},
		{
			"symbol":       "ETH_USDT",
			"baseCoin":     "ETH",
			"quoteCoin":    "USDT",
			"settleCoin":   "USDT",
			"contractSize": 0.01,
			"priceUnit":    0.01,
			"volUnit":      1,
			"minVol":       1,
			"maxVol":       100000,
			"maxLeverage":  100,
			"state":        0,
			"apiAllowed":   true,
		},
		// Фильтруется: не USDT.
		{
			"symbol":       "BTC_USD",
			"baseCoin":     "BTC",
			"quoteCoin":    "USD",
			"settleCoin":   "BTC",
			"contractSize": 100,
			"priceUnit":    0.5,
			"volUnit":      1,
			"minVol":       1,
			"maxVol":       10000,
			"maxLeverage":  100,
			"state":        0,
			"apiAllowed":   true,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/contract/detail" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, entries))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	instruments, err := a.GetInstruments(context.Background())
	if err != nil {
		t.Fatalf("GetInstruments: %v", err)
	}

	// Только 2 USDT-settled инструмента.
	if len(instruments) != 2 {
		t.Fatalf("got %d instruments, want 2", len(instruments))
	}

	btc := instruments[0]
	if btc.ExchangeSymbol != "BTC_USDT" {
		t.Errorf("Symbol = %v, want BTC_USDT", btc.ExchangeSymbol)
	}
	if btc.CanonicalBaseAsset != "BTC" {
		t.Errorf("BaseCoin = %v, want BTC", btc.CanonicalBaseAsset)
	}
	// contractSize=0.0001, tickSize=0.5 → проверяем что не нулевые.
	if btc.TickSize.IsZero() {
		t.Error("TickSize is zero")
	}
	if btc.ContractMultiplier.IsZero() {
		t.Error("ContractMultiplier (contractSize) is zero")
	}
	// ContractMultiplier = 0.0001.
	if btc.ContractMultiplier.String() != "0.0001" {
		t.Errorf("ContractMultiplier = %v, want 0.0001", btc.ContractMultiplier)
	}
	if btc.Status != domain.InstrumentStatusActive {
		t.Errorf("Status = %v, want active", btc.Status)
	}

	// Проверяем кеш contractSize.
	a.mu.RLock()
	cs, ok := a.contractSizeMap["BTC_USDT"]
	a.mu.RUnlock()
	if !ok {
		t.Fatal("contractSizeMap['BTC_USDT'] not set")
	}
	if cs.String() != "0.0001" {
		t.Errorf("contractSize cached = %v, want 0.0001", cs)
	}
}

// TestGetInstruments_BaseQtyToVolConversion проверяет конверсию baseQty → vol.
func TestGetInstruments_BaseQtyToVolConversion(t *testing.T) {
	// Заполняем кеш напрямую (simulating GetInstruments call).
	a, _ := mkAdapter(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))

	cs, _ := decimal.FromString("0.0001") // BTC_USDT contractSize
	a.mu.Lock()
	a.contractSizeMap["BTC_USDT"] = cs
	a.mu.Unlock()

	// baseQty=0.05 BTC → vol = floor(0.05 / 0.0001) = 500 контрактов.
	baseQty, _ := decimal.FromString("0.05")
	vol, err := a.baseQtyToVol("BTC_USDT", baseQty)
	if err != nil {
		t.Fatalf("baseQtyToVol: %v", err)
	}
	if vol != 500 {
		t.Errorf("vol = %d, want 500", vol)
	}

	// Округление вниз: 0.00015 / 0.0001 = 1.5 → floor = 1.
	baseQtySmall, _ := decimal.FromString("0.00015")
	volSmall, err := a.baseQtyToVol("BTC_USDT", baseQtySmall)
	if err != nil {
		t.Fatalf("baseQtyToVol small: %v", err)
	}
	if volSmall != 1 {
		t.Errorf("vol small = %d, want 1 (floor)", volSmall)
	}
}

// ============================================================
// GetFunding — парсинг + Confidence policy
// ============================================================

func TestGetFunding_Parsing(t *testing.T) {
	// Используем реальное time.Now() (не фиксированный clock) для вычисления confidence.
	// mkAdapter фиксирует clock в прошлом, поэтому для тестов с Confidence
	// используем кастомный clock = time.Now.
	fixedNow := time.Now()
	// nextSettleTime: 15 минут в будущем относительно fixedNow → HIGH confidence.
	nextSettleMs := fixedNow.Add(15 * time.Minute).UnixMilli()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/contract/funding_rate/") {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, map[string]interface{}{
			"symbol":         "BTC_USDT",
			"fundingRate":    0.0001,
			"nextSettleTime": nextSettleMs,
			"collectCycle":   8,
			"timestamp":      1700000000000,
		}))
	}))
	defer srv.Close()

	doer := newTestDoer(srv.URL)
	a, _ := New(Config{
		RESTBaseURL: srv.URL,
		APIKey:      "TEST_API_KEY",
		APISecret:   "TEST_API_SECRET",
		HTTPDoer:    doer,
		Clock:       func() time.Time { return fixedNow }, // используем fixedNow
	})

	fi, err := a.GetFunding(context.Background(), "BTC_USDT")
	if err != nil {
		t.Fatalf("GetFunding: %v", err)
	}
	if fi.RealizedFundingRate.String() != "0.0001" {
		t.Errorf("FundingRate = %v, want 0.0001", fi.RealizedFundingRate)
	}
	if fi.Confidence != domain.ConfidenceHigh {
		t.Errorf("Confidence = %v, want ConfidenceHigh", fi.Confidence)
	}
	if fi.FundingIntervalSec != 8*3600 {
		t.Errorf("FundingIntervalSec = %d, want %d", fi.FundingIntervalSec, 8*3600)
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
			// Используем реальный time.Now() для расчёта будущего nextSettleTime.
			fixedNow := time.Now()
			nextMs := fixedNow.Add(time.Duration(tc.minutesUntil) * time.Minute).UnixMilli()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(mexcResponse(true, 0, map[string]interface{}{
					"symbol":         "BTC_USDT",
					"fundingRate":    0.0001,
					"nextSettleTime": nextMs,
					"collectCycle":   8,
				}))
			}))
			defer srv.Close()

			doer := newTestDoer(srv.URL)
			a, _ := New(Config{
				RESTBaseURL: srv.URL,
				APIKey:      "TEST_API_KEY",
				APISecret:   "TEST_API_SECRET",
				HTTPDoer:    doer,
				Clock:       func() time.Time { return fixedNow },
			})
			fi, err := a.GetFunding(context.Background(), "BTC_USDT")
			if err != nil {
				t.Fatalf("GetFunding: %v", err)
			}
			if fi.Confidence != tc.want {
				t.Errorf("Confidence = %v, want %v", fi.Confidence, tc.want)
			}
		})
	}
}

// ============================================================
// GetTicker
// ============================================================

func TestGetTicker_Parsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/contract/ticker" {
			http.Error(w, "not found", 404)
			return
		}
		sym := r.URL.Query().Get("symbol")
		if sym != "BTC_USDT" {
			http.Error(w, "bad symbol", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, map[string]interface{}{
			"symbol":      "BTC_USDT",
			"lastPrice":   43000.5,
			"bid1":        43000.0,
			"ask1":        43001.0,
			"volume24":    1234567890.12,
			"fundingRate": 0.0001,
			"timestamp":   1700000000000,
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ticker, err := a.GetTicker(context.Background(), "BTC_USDT")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if ticker.LastPrice.String() != "43000.5" {
		t.Errorf("LastPrice = %v, want 43000.5", ticker.LastPrice)
	}
	if ticker.Symbol != "BTC_USDT" {
		t.Errorf("Symbol = %v, want BTC_USDT", ticker.Symbol)
	}
}

// ============================================================
// GetOrderBookSnapshot
// ============================================================

func TestGetOrderBookSnapshot_Parsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/contract/depth/") {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, map[string]interface{}{
			"asks":      [][]interface{}{{43001.0, 1.5}, {43002.0, 2.0}},
			"bids":      [][]interface{}{{43000.0, 2.5}, {42999.0, 3.0}},
			"timestamp": 1700000000000,
			"version":   12345,
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ob, err := a.GetOrderBookSnapshot(context.Background(), "BTC_USDT", 50)
	if err != nil {
		t.Fatalf("GetOrderBookSnapshot: %v", err)
	}
	if len(ob.Bids) != 2 {
		t.Errorf("bids count = %d, want 2", len(ob.Bids))
	}
	if len(ob.Asks) != 2 {
		t.Errorf("asks count = %d, want 2", len(ob.Asks))
	}
	if ob.Bids[0].Price.String() != "43000" {
		t.Errorf("bids[0].price = %v, want 43000", ob.Bids[0].Price)
	}
	if !ob.IsSnapshot {
		t.Error("IsSnapshot should be true")
	}
	if ob.Sequence != 12345 {
		t.Errorf("Sequence = %d, want 12345", ob.Sequence)
	}
	if ob.Exchange != domain.ExchangeMEXC {
		t.Errorf("Exchange = %v, want ExchangeMEXC", ob.Exchange)
	}
}

// ============================================================
// PlaceOrder — заголовки + side-код + vol в контрактах + externalOid
// ============================================================

func TestPlaceOrder_BodyAndAuthHeaders(t *testing.T) {
	var capturedBody []byte
	var capturedAPIKey, capturedSignature, capturedRequestTime string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/private/order/submit" {
			http.Error(w, "not found", 404)
			return
		}
		capturedAPIKey = r.Header.Get("ApiKey")
		capturedSignature = r.Header.Get("Signature")
		capturedRequestTime = r.Header.Get("Request-Time")
		body, _ := io.ReadAll(r.Body)
		capturedBody = body

		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, map[string]interface{}{"orderId": 12345678}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)

	// Заполняем кеш contractSize: BTC_USDT → 0.0001 BTC/контракт.
	cs, _ := decimal.FromString("0.0001")
	a.mu.Lock()
	a.contractSizeMap["BTC_USDT"] = cs
	a.mu.Unlock()

	// baseQty = 0.001 BTC → vol = floor(0.001 / 0.0001) = 10 контрактов.
	qty, _ := decimal.FromString("0.001")
	price, _ := decimal.FromString("43000")

	req := domain.PlaceOrderRequest{
		ClientOrderID: "client-order-abc",
		Symbol:        "BTC_USDT",
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
	if ack.ExchangeOrderID != "12345678" {
		t.Errorf("ExchangeOrderID = %v, want 12345678", ack.ExchangeOrderID)
	}

	// Заголовки аутентификации обязательны.
	if capturedAPIKey == "" {
		t.Error("ApiKey header missing")
	}
	if capturedSignature == "" {
		t.Error("Signature header missing")
	}
	if capturedRequestTime == "" {
		t.Error("Request-Time header missing")
	}
	if capturedAPIKey != "TEST_API_KEY" {
		t.Errorf("ApiKey = %q, want TEST_API_KEY", capturedAPIKey)
	}

	// Тело запроса — декодируем числа как json.Number для точного сравнения.
	dec := json.NewDecoder(strings.NewReader(string(capturedBody)))
	dec.UseNumber()
	var body map[string]interface{}
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("parse body: %v", err)
	}

	// Обязательные поля.
	for _, field := range []string{"symbol", "vol", "side", "type", "openType", "externalOid"} {
		if _, ok := body[field]; !ok {
			t.Errorf("body missing required field: %s", field)
		}
	}
	if body["symbol"] != "BTC_USDT" {
		t.Errorf("body.symbol = %v, want BTC_USDT", body["symbol"])
	}
	// vol = 10 контрактов.
	if volVal, ok := body["vol"].(json.Number); !ok || volVal.String() != "10" {
		t.Errorf("body.vol = %v, want 10", body["vol"])
	}
	// side=1 (open long).
	if sideVal, ok := body["side"].(json.Number); !ok || sideVal.String() != "1" {
		t.Errorf("body.side = %v, want 1 (open long)", body["side"])
	}
	if body["externalOid"] != "client-order-abc" {
		t.Errorf("body.externalOid = %v, want client-order-abc", body["externalOid"])
	}
}

// TestPlaceOrder_SideCodes проверяет маппинг side для всех комбинаций.
func TestPlaceOrder_SideCodes(t *testing.T) {
	cases := []struct {
		name       string
		side       domain.Side
		reduceOnly bool
		wantCode   int
	}{
		{"open long", domain.SideLong, false, 1},
		{"close long", domain.SideLong, true, 4},
		{"open short", domain.SideShort, false, 3},
		{"close short", domain.SideShort, true, 2},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			code := domainSideToCode(tc.side, tc.reduceOnly)
			if code != tc.wantCode {
				t.Errorf("domainSideToCode(%s, %v) = %d, want %d", tc.side, tc.reduceOnly, code, tc.wantCode)
			}
			// Проверяем обратное маппирование.
			gotSide, gotReduce := parseSideCode(code)
			if gotSide != tc.side || gotReduce != tc.reduceOnly {
				t.Errorf("parseSideCode(%d) = (%s, %v), want (%s, %v)", code, gotSide, gotReduce, tc.side, tc.reduceOnly)
			}
		})
	}
}

// TestPlaceOrder_CloseLong проверяет side=4 (close long = ReduceOnly + Long).
func TestPlaceOrder_CloseLong(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, map[string]interface{}{"orderId": 99}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	cs, _ := decimal.FromString("0.01")
	a.mu.Lock()
	a.contractSizeMap["ETH_USDT"] = cs
	a.mu.Unlock()

	qty, _ := decimal.FromString("0.1") // 0.1 / 0.01 = 10 контрактов
	req := domain.PlaceOrderRequest{
		ClientOrderID: "close-long-001",
		Symbol:        "ETH_USDT",
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarket,
		BaseQty:       qty,
		ReduceOnly:    true,
	}
	_, err := a.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder close long: %v", err)
	}

	// Декодируем тело с UseNumber для точного сравнения числовых полей.
	dec := json.NewDecoder(strings.NewReader(string(capturedBody)))
	dec.UseNumber()
	var body map[string]interface{}
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if sideVal, ok := body["side"].(json.Number); !ok || sideVal.String() != "4" {
		t.Errorf("side = %v, want 4 (close long)", body["side"])
	}
	if volVal, ok := body["vol"].(json.Number); !ok || volVal.String() != "10" {
		t.Errorf("vol = %v, want 10", body["vol"])
	}
	if typeVal, ok := body["type"].(json.Number); !ok || typeVal.String() != "5" {
		t.Errorf("type = %v, want 5 (market)", body["type"])
	}
}

// ============================================================
// GetOrder — по externalOid + not found → ErrOrderNotFound
// ============================================================

func TestGetOrder_ByExternalOid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/private/order/external/") {
			http.Error(w, "not found", 404)
			return
		}
		// Проверяем структуру пути: /api/v1/private/order/external/{symbol}/{externalOid}.
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/private/order/external/"), "/")
		if len(parts) < 2 || parts[0] != "BTC_USDT" || parts[1] != "my-client-id" {
			http.Error(w, "bad path", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, map[string]interface{}{
			"orderId":      int64(987654321),
			"symbol":       "BTC_USDT",
			"externalOid":  "my-client-id",
			"side":         2, // close short
			"price":        43000.0,
			"vol":          10,
			"dealVol":      10,
			"dealAvgPrice": 43001.5,
			"type":         1,
			"state":        3, // filled
			"createTime":   int64(1700000000000),
			"closeFee":     0.43,
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	cs, _ := decimal.FromString("0.0001")
	a.mu.Lock()
	a.contractSizeMap["BTC_USDT"] = cs
	a.mu.Unlock()

	ord, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "my-client-id",
		Symbol:        "BTC_USDT",
	})
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if ord.ExchangeOrderID != "987654321" {
		t.Errorf("ExchangeOrderID = %v, want 987654321", ord.ExchangeOrderID)
	}
	if ord.ClientOrderID != "my-client-id" {
		t.Errorf("ClientOrderID = %v, want my-client-id", ord.ClientOrderID)
	}
	if ord.Status != domain.OrderStatusFilled {
		t.Errorf("Status = %v, want filled", ord.Status)
	}
	// side=2 → SideShort, reduceOnly=true.
	if ord.Side != domain.SideShort {
		t.Errorf("Side = %v, want SideShort", ord.Side)
	}
	if !ord.ReduceOnly {
		t.Error("ReduceOnly should be true for side=2 (close short)")
	}
}

// TestGetOrder_NotFound проверяет маппинг "not found" → ErrOrderNotFound.
func TestGetOrder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Симулируем ответ "ордер не найден" через code.
		w.Write(mexcResponse(false, 2011, nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "no-such-order",
		Symbol:        "BTC_USDT",
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
		w.Write(mexcResponse(false, 429, nil))
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
		w.Write(mexcResponse(false, 1002, nil))
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

func TestErrorMapping_InsufficientMargin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(false, 2013, nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	cs, _ := decimal.FromString("0.0001")
	a.mu.Lock()
	a.contractSizeMap["BTC_USDT"] = cs
	a.mu.Unlock()

	qty, _ := decimal.FromString("0.001")
	_, err := a.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		Symbol:        "BTC_USDT",
		Side:          domain.SideLong,
		BaseQty:       qty,
		ClientOrderID: "test",
	})
	if err == nil {
		t.Fatal("expected ErrInsufficientMargin, got nil")
	}
	if !errors.Is(err, exchange.ErrInsufficientMargin) {
		t.Errorf("expected ErrInsufficientMargin, got: %v", err)
	}
}

func TestErrorMapping_InvalidSymbol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(false, 2001, nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetFunding(context.Background(), "INVALID_USDT")
	if err == nil {
		t.Fatal("expected ErrInvalidSymbol, got nil")
	}
	if !errors.Is(err, exchange.ErrInvalidSymbol) {
		t.Errorf("expected ErrInvalidSymbol, got: %v", err)
	}
}

// ============================================================
// GetPositions — парсинг позиций
// ============================================================

func TestGetPositions_Parsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/private/position/open_positions" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, []map[string]interface{}{
			{
				"positionId":     int64(1001),
				"symbol":         "BTC_USDT",
				"positionType":   1, // long
				"holdVol":        500,
				"openVol":        500,
				"holdAvgPrice":   43000.0,
				"liquidatePrice": 30000.0,
				"unrealizedPnl":  50.5,
				"marginType":     2, // cross
				"leverage":       10,
				"im":             430.0,
				"updateTime":     int64(1700000000000),
				"openAvgPrice":   43000.0,
				"closeAvgPrice":  0,
				"closeVol":       0,
				"freezeVol":      0,
			},
			{
				// Нулевая позиция — должна быть пропущена.
				"positionId":   int64(1002),
				"symbol":       "ETH_USDT",
				"positionType": 2,
				"holdVol":      0,
				"updateTime":   int64(1700000000000),
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	// Заполняем кеш.
	cs, _ := decimal.FromString("0.0001")
	a.mu.Lock()
	a.contractSizeMap["BTC_USDT"] = cs
	a.mu.Unlock()

	positions, err := a.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 non-zero position, got %d", len(positions))
	}
	p := positions[0]
	if p.Symbol != "BTC_USDT" {
		t.Errorf("Symbol = %v, want BTC_USDT", p.Symbol)
	}
	if p.Side != domain.SideLong {
		t.Errorf("Side = %v, want SideLong", p.Side)
	}
	if p.MarginMode != domain.MarginCross {
		t.Errorf("MarginMode = %v, want MarginCross", p.MarginMode)
	}
	// holdVol=500, contractSize=0.0001 → baseQty=0.05 BTC.
	if p.BaseQty.String() != "0.05" {
		t.Errorf("BaseQty = %v, want 0.05", p.BaseQty)
	}
	if p.ContractQty.String() != "500" {
		t.Errorf("ContractQty = %v, want 500", p.ContractQty)
	}
}

// ============================================================
// GetADLState — нулевое состояние
// ============================================================

func TestGetADLState_ReturnsZero(t *testing.T) {
	a, _ := mkAdapter(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	adl, err := a.GetADLState(context.Background(), "BTC_USDT")
	if err != nil {
		t.Fatalf("GetADLState: %v", err)
	}
	if adl.Symbol != "BTC_USDT" {
		t.Errorf("Symbol = %v, want BTC_USDT", adl.Symbol)
	}
	if !adl.LongQueue.IsZero() {
		t.Errorf("LongQueue = %v, want 0 (no ADL indicator)", adl.LongQueue)
	}
	if !adl.ShortQueue.IsZero() {
		t.Errorf("ShortQueue = %v, want 0 (no ADL indicator)", adl.ShortQueue)
	}
}

// ============================================================
// SubscribePublic/SubscribePrivate — типизированная заглушка
// ============================================================

func TestSubscribePublic_ReturnsError(t *testing.T) {
	a, _ := mkAdapter(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	_, err := a.SubscribePublic(context.Background(), nil)
	if err == nil {
		t.Error("expected error for unimplemented SubscribePublic")
	}
}

func TestSubscribePrivate_ReturnsError(t *testing.T) {
	a, _ := mkAdapter(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	_, err := a.SubscribePrivate(context.Background(), domain.CredentialRef{})
	if err == nil {
		t.Error("expected error for unimplemented SubscribePrivate")
	}
}

// ============================================================
// GetBalances — подписанный запрос, парсинг
// ============================================================

func TestGetBalances_AuthRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/private/account/assets" {
			http.Error(w, "not found", 404)
			return
		}
		// Проверяем наличие заголовков аутентификации.
		if r.Header.Get("ApiKey") == "" || r.Header.Get("Signature") == "" {
			w.Header().Set("Content-Type", "application/json")
			w.Write(mexcResponse(false, 1002, nil))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mexcResponse(true, 0, []map[string]interface{}{
			{
				"currency":         "USDT",
				"equity":           10000.5,
				"availableBalance": 8000.25,
				"frozenBalance":    2000.25,
				"positionMargin":   2000.25,
				"cashBalance":      10000.5,
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
}

// ============================================================
// ID
// ============================================================

func TestAdapterID(t *testing.T) {
	a, _ := mkAdapter(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	if a.ID() != domain.ExchangeMEXC {
		t.Errorf("ID() = %v, want ExchangeMEXC", a.ID())
	}
}
