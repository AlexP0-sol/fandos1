// Контрактные тесты адаптера OKX V5.
//
// Тесты используют httptest-сервер с точными V5-конвертами.
// Проверяется:
//   - GetServerTime: парсинг поля ts
//   - GetInstruments: фильтр SWAP+USDT+linear, ctVal маппинг, qty-конверсия
//   - GetFunding: парсинг fundingRate/fundingTime, Confidence policy
//   - GetTicker: парсинг полей
//   - GetOrderBookSnapshot: парсинг [[price,qty,...]]
//   - PlaceOrder: заголовки OK-ACCESS-*, clOrdId транслирован, sz в контрактах
//   - CancelOrder: успех и ErrOrderNotFound
//   - GetOrder: парсинг + ErrOrderNotFound при code=51603
//   - Маппинг ошибок: code=50011 → ErrRateLimited, code=51008 → ErrInsufficientMargin
//   - ADL нормализация: adl [1..5] → (adl-1)/4 → [0,1]
//   - GetBalances: парсинг availEq/eq
//   - GetPositions: парсинг pos/avgPx/mgnRatio/liqPx/adl
//   - Signer known-vector: VERIFIED против Python-эталона
//   - clOrdId трансляция: удаление '_' и '-'
package okx

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

// okxResponse формирует тестовый ответ в формате OKX V5.
func okxResponse(code string, msg string, data interface{}) []byte {
	var raw json.RawMessage
	if data != nil {
		r, _ := json.Marshal(data)
		raw = r
	} else {
		raw = json.RawMessage("[]")
	}
	env := map[string]interface{}{
		"code": code,
		"msg":  msg,
		"data": raw,
	}
	b, _ := json.Marshal(env)
	return b
}

// fixedClock — статический clock для тестов.
var fixedClock = func() time.Time { return time.Unix(1700000000, 0).UTC() }

// mkAdapter создаёт адаптер для тестов.
func mkAdapter(srv *httptest.Server) (*Adapter, *testHTTPDoer) {
	doer := newTestDoer(srv.URL)
	a, _ := New(Config{
		RESTBaseURL: srv.URL,
		WSBaseURL:   "ws://localhost",
		APIKey:      "test-api-key",
		APISecret:   "test-secret-key",
		Passphrase:  "test-passphrase",
		HTTPDoer:    doer,
		Clock:       fixedClock,
	})
	return a, doer
}

// ============================================================
// Signer known-vector test — VERIFIED против Python-эталона
// ============================================================

// TestSignerKnownVector — проверяет подпись против эталона, вычисленного Python:
//
//	import hmac, hashlib, base64
//	secret = b'test-secret-key'
//	pre_hash = '2020-12-08T09:08:57.715Z' + 'GET' + '/api/v5/account/balance' + ''
//	sig = base64.b64encode(hmac.new(secret, pre_hash.encode(), hashlib.sha256).digest()).decode()
//	# → c9zTaVkCW3ZJSM5fGFJFDN0xSm/g12jMHLyGPNsVbDw=
func TestSignerKnownVector(t *testing.T) {
	s := NewSigner("test-api-key", []byte("test-secret-key"), "test-passphrase")

	// GET vector.
	ts := "2020-12-08T09:08:57.715Z"
	sig := s.Sign(ts, "GET", "/api/v5/account/balance", "")
	wantGET := "c9zTaVkCW3ZJSM5fGFJFDN0xSm/g12jMHLyGPNsVbDw="
	if sig != wantGET {
		t.Errorf("GET sign = %q, want %q", sig, wantGET)
	}

	// POST vector.
	body := `{"instId":"BTC-USDT-SWAP","tdMode":"cross","side":"buy","ordType":"market","sz":"1"}`
	sig2 := s.Sign(ts, "POST", "/api/v5/trade/order", body)
	wantPOST := "VQdzGdEYuXmDU3Zryoh0YQETmJWQ4lPHl3gX7YlFYdw="
	if sig2 != wantPOST {
		t.Errorf("POST sign = %q, want %q", sig2, wantPOST)
	}

	// GET with query vector.
	sig3 := s.Sign(ts, "GET", "/api/v5/market/ticker?instId=BTC-USDT-SWAP", "")
	wantGETQuery := "MHcfRiS8bzQx54Pbpubw/d/LqP5Voz23T5zaA1vObWA="
	if sig3 != wantGETQuery {
		t.Errorf("GET+query sign = %q, want %q", sig3, wantGETQuery)
	}
}

// TestSignerAuthHeaders — проверяет заголовки OK-ACCESS-*.
func TestSignerAuthHeaders(t *testing.T) {
	s := NewSigner("MY-KEY", []byte("MY-SECRET"), "MY-PASS")
	ts := "2020-12-08T09:08:57.715Z"
	sig := "test-sig"
	h := s.AuthHeaders(ts, sig)
	want := map[string]string{
		"OK-ACCESS-KEY":        "MY-KEY",
		"OK-ACCESS-SIGN":       "test-sig",
		"OK-ACCESS-TIMESTAMP":  "2020-12-08T09:08:57.715Z",
		"OK-ACCESS-PASSPHRASE": "MY-PASS",
	}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("header %s = %q, want %q", k, h[k], v)
		}
	}
}

// TestFormatTimestamp — проверяет формат ISO8601 UTC с ms.
func TestFormatTimestamp(t *testing.T) {
	// 2020-12-08T09:08:57.715Z
	ts := time.Date(2020, 12, 8, 9, 8, 57, 715_000_000, time.UTC)
	got := FormatTimestamp(ts)
	want := "2020-12-08T09:08:57.715Z"
	if got != want {
		t.Errorf("FormatTimestamp = %q, want %q", got, want)
	}
}

// TestSignerDeterministic — подпись детерминирована.
func TestSignerDeterministic(t *testing.T) {
	s := NewSigner("K", []byte("s"), "p")
	a := s.Sign("ts", "GET", "/path", "")
	b := s.Sign("ts", "GET", "/path", "")
	if a != b {
		t.Error("signing must be deterministic")
	}
}

// ============================================================
// clOrdId translation
// ============================================================

func TestTranslateClientOrderID(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"ABC123", "ABC123"},
		{"ABC_123", "ABC123"},  // '_' удаляется
		{"ABC-123", "ABC123"},  // '-' удаляется
		{"A_B-C123", "ABC123"}, // оба удаляются
		{"", ""},               // пустая строка
		{"abc_DEF-999", "abcDEF999"},
		// Обрезка до 32 символов.
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ123456", "ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"[:32]},
		// "ABCDEFGHIJKLMNOPQRSTUVWXYZ_1234567" → remove '_' → "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567" (33) → truncate → "ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"
		{"ABCDEFGHIJKLMNOPQRSTUVWXYZ_1234567", "ABCDEFGHIJKLMNOPQRSTUVWXYZ123456"},
	}
	for _, tc := range cases {
		got := translateClientOrderID(domain.ClientOrderID(tc.input))
		if got != tc.want {
			t.Errorf("translateClientOrderID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ============================================================
// GetServerTime — VERIFIED
// ============================================================

func TestGetServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/public/time" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("0", "", []map[string]string{
			{"ts": "1700000000000"},
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
// GetInstruments — фильтр, ctVal, qty-конверсия
// ============================================================

func TestGetInstruments_FilterAndCtVal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/public/instruments" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("0", "", []map[string]interface{}{
			{
				// USDT-margined linear SWAP — должен пройти фильтр.
				"instType":  "SWAP",
				"instId":    "BTC-USDT-SWAP",
				"uly":       "BTC-USDT",
				"settleCcy": "USDT",
				"ctType":    "linear",
				"ctVal":     "0.01", // 0.01 BTC per contract
				"lotSz":     "1",    // min 1 contract
				"minSz":     "1",
				"tickSz":    "0.1",
				"lever":     "125",
				"state":     "live",
				"baseCcy":   "BTC",
				"quoteCcy":  "USDT",
			},
			{
				// inverse SWAP (settleCcy=BTC) — должен быть отфильтрован.
				"instType":  "SWAP",
				"instId":    "BTC-USD-SWAP",
				"uly":       "BTC-USD",
				"settleCcy": "BTC",
				"ctType":    "inverse",
				"ctVal":     "100",
				"lotSz":     "1",
				"minSz":     "1",
				"tickSz":    "0.5",
				"lever":     "100",
				"state":     "live",
				"baseCcy":   "BTC",
				"quoteCcy":  "USD",
			},
			{
				// ETH USDT linear — должен пройти.
				"instType":  "SWAP",
				"instId":    "ETH-USDT-SWAP",
				"uly":       "ETH-USDT",
				"settleCcy": "USDT",
				"ctType":    "linear",
				"ctVal":     "0.1",
				"lotSz":     "1",
				"minSz":     "1",
				"tickSz":    "0.01",
				"lever":     "100",
				"state":     "live",
				"baseCcy":   "ETH",
				"quoteCcy":  "USDT",
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	instruments, err := a.GetInstruments(context.Background())
	if err != nil {
		t.Fatalf("GetInstruments: %v", err)
	}
	// Ожидаем 2 инструмента (BTC-USDT-SWAP и ETH-USDT-SWAP), без BTC-USD-SWAP.
	if len(instruments) != 2 {
		t.Fatalf("GetInstruments: got %d instruments, want 2 (filtered out inverse)", len(instruments))
	}

	btc := instruments[0]
	if btc.ExchangeSymbol != "BTC-USDT-SWAP" {
		t.Errorf("instruments[0].ExchangeSymbol = %v, want BTC-USDT-SWAP", btc.ExchangeSymbol)
	}
	// ContractMultiplier = ctVal = 0.01
	if btc.ContractMultiplier.String() != "0.01" {
		t.Errorf("ContractMultiplier = %v, want 0.01", btc.ContractMultiplier)
	}
	// QtyStep = lotSz * ctVal = 1 * 0.01 = 0.01
	if btc.QtyStep.String() != "0.01" {
		t.Errorf("QtyStep = %v, want 0.01", btc.QtyStep)
	}
	// MinQty = minSz * ctVal = 1 * 0.01 = 0.01
	if btc.MinQty.String() != "0.01" {
		t.Errorf("MinQty = %v, want 0.01", btc.MinQty)
	}
	if btc.TickSize.String() != "0.1" {
		t.Errorf("TickSize = %v, want 0.1", btc.TickSize)
	}
	if btc.MaxLeverage.String() != "125" {
		t.Errorf("MaxLeverage = %v, want 125", btc.MaxLeverage)
	}
	if btc.Exchange != domain.ExchangeOKX {
		t.Errorf("Exchange = %v, want okx", btc.Exchange)
	}
	// ctVal закешировался.
	a.ctValCacheMu.RLock()
	ctVal, ok := a.ctValCache["BTC-USDT-SWAP"]
	a.ctValCacheMu.RUnlock()
	if !ok {
		t.Error("ctVal not cached for BTC-USDT-SWAP")
	}
	if ctVal.String() != "0.01" {
		t.Errorf("cached ctVal = %v, want 0.01", ctVal)
	}
}

// TestGetInstruments_QtyConversion — проверяет конверсию qty.
func TestGetInstruments_QtyConversion(t *testing.T) {
	// ctVal = 0.01 BTC/contract
	ctVal, _ := decimal.FromString("0.01")

	// base qty → contracts (floor).
	baseQty, _ := decimal.FromString("0.025")
	contracts := baseQtyToContracts(baseQty, ctVal)
	// floor(0.025 / 0.01) = floor(2.5) = 2
	if contracts.String() != "2" {
		t.Errorf("baseQtyToContracts(0.025, 0.01) = %v, want 2", contracts)
	}

	// contracts → base qty.
	base := contractsToBaseQty(contracts, ctVal)
	// 2 * 0.01 = 0.02
	if base.String() != "0.02" {
		t.Errorf("contractsToBaseQty(2, 0.01) = %v, want 0.02", base)
	}

	// Edge: 1 contract.
	c1 := baseQtyToContracts(decimal.MustFromString("0.01"), ctVal)
	if c1.String() != "1" {
		t.Errorf("baseQtyToContracts(0.01, 0.01) = %v, want 1", c1)
	}

	// Edge: less than 1 contract → 0.
	c0 := baseQtyToContracts(decimal.MustFromString("0.009"), ctVal)
	if c0.String() != "0" {
		t.Errorf("baseQtyToContracts(0.009, 0.01) = %v, want 0", c0)
	}
}

// ============================================================
// GetFunding — парсинг + Confidence
// ============================================================

func TestGetFunding_Parsing(t *testing.T) {
	// Следующий funding через 15 минут → HIGH confidence.
	nextFundingMs := time.Now().Add(15 * time.Minute).UnixMilli()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/public/funding-rate" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("0", "", []map[string]interface{}{
			{
				"instId":          "BTC-USDT-SWAP",
				"fundingRate":     "0.0001",
				"nextFundingRate": "0.00012",
				"fundingTime":     fmt.Sprintf("%d", nextFundingMs),
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	fi, err := a.GetFunding(context.Background(), "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("GetFunding: %v", err)
	}
	if fi.RealizedFundingRate.String() != "0.0001" {
		t.Errorf("RealizedFundingRate = %v, want 0.0001", fi.RealizedFundingRate)
	}
	if fi.PredictedFundingRate.String() != "0.00012" {
		t.Errorf("PredictedFundingRate = %v, want 0.00012", fi.PredictedFundingRate)
	}
	if fi.Confidence != domain.ConfidenceHigh {
		t.Errorf("Confidence = %v, want ConfidenceHigh", fi.Confidence)
	}
}

func TestGetFunding_ConfidenceLevels(t *testing.T) {
	cases := []struct {
		name         string
		minutesUntil int
		wantConf     domain.ConfidenceLevel
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
				w.Header().Set("Content-Type", "application/json")
				w.Write(okxResponse("0", "", []map[string]interface{}{
					{
						"instId":      "BTC-USDT-SWAP",
						"fundingRate": "0.0001",
						"fundingTime": fmt.Sprintf("%d", nextFundingMs),
					},
				}))
			}))
			defer srv.Close()

			a, _ := mkAdapter(srv)
			fi, err := a.GetFunding(context.Background(), "BTC-USDT-SWAP")
			if err != nil {
				t.Fatalf("GetFunding: %v", err)
			}
			if fi.Confidence != tc.wantConf {
				t.Errorf("confidence = %v, want %v", fi.Confidence, tc.wantConf)
			}
		})
	}
}

// ============================================================
// GetTicker — парсинг полей
// ============================================================

func TestGetTicker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/market/ticker" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("0", "", []map[string]interface{}{
			{
				"instId":    "BTC-USDT-SWAP",
				"last":      "50000.5",
				"bidPx":     "50000.0",
				"askPx":     "50001.0",
				"volCcy24h": "1234567.89",
				"markPx":    "50002.0",
				"idxPx":     "49999.0",
				"ts":        "1700000000000",
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ticker, err := a.GetTicker(context.Background(), "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if ticker.LastPrice.String() != "50000.5" {
		t.Errorf("LastPrice = %v, want 50000.5", ticker.LastPrice)
	}
	if ticker.MarkPrice.String() != "50002" {
		t.Errorf("MarkPrice = %v, want 50002", ticker.MarkPrice)
	}
	if ticker.IndexPrice.String() != "49999" {
		t.Errorf("IndexPrice = %v, want 49999", ticker.IndexPrice)
	}
	if ticker.QuoteVolume24h.String() != "1234567.89" {
		t.Errorf("QuoteVolume24h = %v, want 1234567.89", ticker.QuoteVolume24h)
	}
	if ticker.Symbol != "BTC-USDT-SWAP" {
		t.Errorf("Symbol = %v, want BTC-USDT-SWAP", ticker.Symbol)
	}
}

// ============================================================
// GetOrderBookSnapshot — парсинг
// ============================================================

func TestGetOrderBookSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/market/books" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// OKX books: [[price, qty, liq_orders, num_orders], ...]
		w.Write(okxResponse("0", "", []map[string]interface{}{
			{
				"bids": [][]string{{"50000", "1.5", "0", "2"}, {"49999", "2.0", "0", "1"}},
				"asks": [][]string{{"50001", "1.0", "0", "3"}, {"50002", "3.0", "0", "1"}},
				"ts":   "1700000000000",
			},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ob, err := a.GetOrderBookSnapshot(context.Background(), "BTC-USDT-SWAP", 20)
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
	if ob.Bids[0].Qty.String() != "1.5" {
		t.Errorf("bids[0].qty = %v, want 1.5", ob.Bids[0].Qty)
	}
	if !ob.IsSnapshot {
		t.Error("IsSnapshot should be true")
	}
	if ob.Exchange != domain.ExchangeOKX {
		t.Errorf("Exchange = %v, want okx", ob.Exchange)
	}
}

// ============================================================
// PlaceOrder — заголовки OK-ACCESS-*, clOrdId, sz в контрактах
// ============================================================

func TestPlaceOrder_HeadersClOrdIdContracts(t *testing.T) {
	var capturedBody []byte
	var capturedHeaders http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v5/public/instruments":
			// Lazy ctVal lookup.
			w.Header().Set("Content-Type", "application/json")
			w.Write(okxResponse("0", "", []map[string]interface{}{
				{
					"instType":  "SWAP",
					"instId":    "BTC-USDT-SWAP",
					"uly":       "BTC-USDT",
					"settleCcy": "USDT",
					"ctType":    "linear",
					"ctVal":     "0.01",
					"lotSz":     "1",
					"minSz":     "1",
					"tickSz":    "0.1",
					"lever":     "125",
					"state":     "live",
					"baseCcy":   "BTC",
					"quoteCcy":  "USDT",
				},
			}))
		case "/api/v5/trade/order":
			capturedHeaders = r.Header.Clone()
			body, _ := io.ReadAll(r.Body)
			capturedBody = body
			w.Header().Set("Content-Type", "application/json")
			w.Write(okxResponse("0", "", []map[string]interface{}{
				{
					"ordId":   "exchange-order-123",
					"clOrdId": "CLIENTORDERABC",
					"sCode":   "0",
					"sMsg":    "",
				},
			}))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)

	qty, _ := decimal.FromString("0.05") // 0.05 BTC → 0.05/0.01 = 5 contracts
	price, _ := decimal.FromString("50000")

	req := domain.PlaceOrderRequest{
		ClientOrderID: "CLIENT_ORDER-ABC", // содержит '_' и '-'
		Symbol:        "BTC-USDT-SWAP",
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
	// ClientOrderID сохраняется оригинальный.
	if ack.ClientOrderID != "CLIENT_ORDER-ABC" {
		t.Errorf("ClientOrderID = %v, want CLIENT_ORDER-ABC", ack.ClientOrderID)
	}

	// Проверяем заголовки OK-ACCESS-*.
	if capturedHeaders.Get("Ok-Access-Key") == "" && capturedHeaders.Get("OK-ACCESS-KEY") == "" {
		// Проверяем case-insensitively.
		found := false
		for k := range capturedHeaders {
			if strings.ToUpper(k) == "OK-ACCESS-KEY" {
				found = true
				break
			}
		}
		if !found {
			t.Error("OK-ACCESS-KEY header missing")
		}
	}
	okSign := ""
	for k, v := range capturedHeaders {
		if strings.ToUpper(k) == "OK-ACCESS-SIGN" {
			okSign = v[0]
			break
		}
	}
	if okSign == "" {
		t.Error("OK-ACCESS-SIGN header missing")
	}

	// Проверяем тело запроса.
	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}

	// clOrdId должен быть без '_' и '-': "CLIENT_ORDER-ABC" → "CLIENTORDERABC".
	if body["clOrdId"] != "CLIENTORDERABC" {
		t.Errorf("body.clOrdId = %v, want CLIENTORDERABC", body["clOrdId"])
	}

	// sz должен быть в контрактах: 0.05 BTC / 0.01 BTC/contract = 5.
	if body["sz"] != "5" {
		t.Errorf("body.sz = %v, want 5 (contracts)", body["sz"])
	}

	// Обязательные поля.
	requiredFields := []string{"instId", "tdMode", "side", "ordType", "sz", "clOrdId"}
	for _, field := range requiredFields {
		if _, ok := body[field]; !ok {
			t.Errorf("body missing required field: %s", field)
		}
	}
	if body["instId"] != "BTC-USDT-SWAP" {
		t.Errorf("body.instId = %v, want BTC-USDT-SWAP", body["instId"])
	}
	if body["side"] != "buy" {
		t.Errorf("body.side = %v, want buy", body["side"])
	}
	if body["tdMode"] != "cross" {
		t.Errorf("body.tdMode = %v, want cross", body["tdMode"])
	}
	if body["ordType"] != "ioc" {
		t.Errorf("body.ordType = %v, want ioc", body["ordType"])
	}
}

// ============================================================
// CancelOrder — успех и ErrOrderNotFound
// ============================================================

func TestCancelOrder_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("0", "", []map[string]interface{}{
			{"ordId": "123", "clOrdId": "abc", "sCode": "0", "sMsg": ""},
		}))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ExchangeOrderID: "123",
		Symbol:          "BTC-USDT-SWAP",
	})
	if err != nil {
		t.Errorf("CancelOrder: %v", err)
	}
}

func TestCancelOrder_OrderNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("51603", "Order does not exist", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ExchangeOrderID: "no-such-order",
		Symbol:          "BTC-USDT-SWAP",
	})
	if err == nil {
		t.Fatal("expected ErrOrderNotFound, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

// ============================================================
// GetOrder — парсинг + ErrOrderNotFound
// ============================================================

func TestGetOrder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("51603", "Order does not exist", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "no-such",
		Symbol:        "BTC-USDT-SWAP",
	})
	if err == nil {
		t.Fatal("expected ErrOrderNotFound, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

func TestGetOrder_Found(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v5/public/instruments":
			w.Header().Set("Content-Type", "application/json")
			w.Write(okxResponse("0", "", []map[string]interface{}{
				{
					"instType": "SWAP", "instId": "BTC-USDT-SWAP", "uly": "BTC-USDT",
					"settleCcy": "USDT", "ctType": "linear", "ctVal": "0.01",
					"lotSz": "1", "minSz": "1", "tickSz": "0.1", "lever": "125",
					"state": "live", "baseCcy": "BTC", "quoteCcy": "USDT",
				},
			}))
		case "/api/v5/trade/order":
			w.Header().Set("Content-Type", "application/json")
			w.Write(okxResponse("0", "", []map[string]interface{}{
				{
					"instId":     "BTC-USDT-SWAP",
					"ordId":      "exchange-order-999",
					"clOrdId":    "CLIENTORDERABC",
					"side":       "buy",
					"posSide":    "long",
					"ordType":    "limit",
					"sz":         "5", // 5 contracts
					"px":         "50000",
					"accFillSz":  "5", // fully filled
					"avgPx":      "50000.5",
					"fee":        "-2.5",
					"feeCcy":     "USDT",
					"state":      "filled",
					"reduceOnly": "false",
					"tdMode":     "cross",
					"cTime":      "1700000000000",
				},
			}))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ord, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "CLIENTORDERABC",
		Symbol:        "BTC-USDT-SWAP",
	})
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if ord.ExchangeOrderID != "exchange-order-999" {
		t.Errorf("ExchangeOrderID = %v, want exchange-order-999", ord.ExchangeOrderID)
	}
	if ord.Status != domain.OrderStatusFilled {
		t.Errorf("Status = %v, want filled", ord.Status)
	}
	// 5 контрактов × 0.01 BTC/контракт = 0.05 BTC.
	if ord.RequestedQty.String() != "0.05" {
		t.Errorf("RequestedQty = %v, want 0.05 (5 contracts × 0.01 BTC)", ord.RequestedQty)
	}
	if ord.FilledQty.String() != "0.05" {
		t.Errorf("FilledQty = %v, want 0.05", ord.FilledQty)
	}
}

// ============================================================
// Маппинг ошибок
// ============================================================

func TestErrorMapping_RateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("50011", "Request too frequent", nil))
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
		w.Write(okxResponse("51008", "Insufficient balance", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetBalances(context.Background())
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
		w.Write(okxResponse("50111", "Invalid OK_ACCESS_KEY", nil))
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

func TestErrorMapping_InvalidSymbol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("51001", "Instrument ID does not exist", nil))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetTicker(context.Background(), "INVALID-SWAP")
	if err == nil {
		t.Fatal("expected ErrInvalidSymbol, got nil")
	}
	if !errors.Is(err, exchange.ErrInvalidSymbol) {
		t.Errorf("expected ErrInvalidSymbol, got: %v", err)
	}
}

// ============================================================
// ADL нормализация
// ============================================================

func TestADLNormalization(t *testing.T) {
	cases := []struct {
		adl      string // OKX adl field (1..5)
		expected string // normalized (adl-1)/4 → [0,1]
	}{
		{"1", "0"},    // (1-1)/4 = 0
		{"2", "0.25"}, // (2-1)/4 = 0.25
		{"3", "0.5"},  // (3-1)/4 = 0.5
		{"4", "0.75"}, // (4-1)/4 = 0.75
		{"5", "1"},    // (5-1)/4 = 1
	}

	for _, tc := range cases {
		tc := tc
		t.Run("adl_"+tc.adl, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v5/account/positions":
					w.Write(okxResponse("0", "", []map[string]interface{}{
						{
							"instId":  "BTC-USDT-SWAP",
							"posSide": "long",
							"pos":     "5",
							"avgPx":   "50000",
							"markPx":  "51000",
							"liqPx":   "30000",
							"upl":     "50",
							"mgnMode": "cross",
							"lever":   "10",
							"margin":  "500",
							"adl":     tc.adl,
							"uTime":   "1700000000000",
						},
					}))
				case "/api/v5/public/instruments":
					w.Write(okxResponse("0", "", []map[string]interface{}{
						{
							"instType": "SWAP", "instId": "BTC-USDT-SWAP", "uly": "BTC-USDT",
							"settleCcy": "USDT", "ctType": "linear", "ctVal": "0.01",
							"lotSz": "1", "minSz": "1", "tickSz": "0.1",
							"lever": "125", "state": "live", "baseCcy": "BTC", "quoteCcy": "USDT",
						},
					}))
				default:
					http.Error(w, "not found", 404)
				}
			}))
			defer srv.Close()

			a, _ := mkAdapter(srv)
			adl, err := a.GetADLState(context.Background(), "BTC-USDT-SWAP")
			if err != nil {
				t.Fatalf("adl=%s: GetADLState: %v", tc.adl, err)
			}
			if adl.LongQueue.String() != tc.expected {
				t.Errorf("adl=%s: LongQueue = %v, want %v", tc.adl, adl.LongQueue, tc.expected)
			}
		})
	}
}

// ============================================================
// GetBalances — парсинг availEq/eq
// ============================================================

func TestGetBalances(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/account/balance" {
			http.Error(w, "not found", 404)
			return
		}
		// Проверяем наличие OK-ACCESS-SIGN.
		okSign := r.Header.Get("OK-ACCESS-SIGN")
		if okSign == "" {
			http.Error(w, "missing sign", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("0", "", []map[string]interface{}{
			{
				"totalEq": "15000",
				"details": []map[string]interface{}{
					{"ccy": "USDT", "eq": "10000.5", "availEq": "8000.25"},
					{"ccy": "BTC", "eq": "0.5", "availEq": "0.5"},
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
// GetPositions — парсинг pos/avgPx/liqPx/adl, конверсия qty
// ============================================================

func TestGetPositions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v5/account/positions":
			w.Write(okxResponse("0", "", []map[string]interface{}{
				{
					"instId":  "BTC-USDT-SWAP",
					"posSide": "long",
					"pos":     "10", // 10 contracts
					"avgPx":   "50000",
					"markPx":  "51000",
					"liqPx":   "30000",
					"upl":     "100",
					"mgnMode": "cross",
					"lever":   "10",
					"margin":  "500",
					"adl":     "3",
					"uTime":   "1700000000000",
				},
				{
					// net-mode, pos=0 → должна быть пропущена.
					"instId":  "ETH-USDT-SWAP",
					"posSide": "net",
					"pos":     "0",
					"avgPx":   "3000",
					"markPx":  "3100",
					"liqPx":   "0",
					"upl":     "0",
					"mgnMode": "cross",
					"lever":   "5",
					"margin":  "0",
					"adl":     "1",
					"uTime":   "1700000000000",
				},
			}))
		case "/api/v5/public/instruments":
			w.Write(okxResponse("0", "", []map[string]interface{}{
				{
					"instType": "SWAP", "instId": "BTC-USDT-SWAP", "uly": "BTC-USDT",
					"settleCcy": "USDT", "ctType": "linear", "ctVal": "0.01",
					"lotSz": "1", "minSz": "1", "tickSz": "0.1",
					"lever": "125", "state": "live", "baseCcy": "BTC", "quoteCcy": "USDT",
				},
			}))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	positions, err := a.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	// ETH позиция с pos=0 должна быть пропущена.
	if len(positions) != 1 {
		t.Fatalf("expected 1 non-zero position, got %d", len(positions))
	}
	p := positions[0]
	if p.Symbol != "BTC-USDT-SWAP" {
		t.Errorf("Symbol = %v, want BTC-USDT-SWAP", p.Symbol)
	}
	if p.Side != domain.SideLong {
		t.Errorf("Side = %v, want long", p.Side)
	}
	// 10 contracts × 0.01 BTC = 0.1 BTC.
	if p.BaseQty.String() != "0.1" {
		t.Errorf("BaseQty = %v, want 0.1 (10 contracts × 0.01)", p.BaseQty)
	}
	if p.EntryPrice.String() != "50000" {
		t.Errorf("EntryPrice = %v, want 50000", p.EntryPrice)
	}
	if p.LiquidationPrice.String() != "30000" {
		t.Errorf("LiquidationPrice = %v, want 30000", p.LiquidationPrice)
	}
	// adl=3 → (3-1)/4 = 0.5.
	if p.ADLQueue == nil {
		t.Fatal("ADLQueue is nil")
	}
	if p.ADLQueue.LongQueue.String() != "0.5" {
		t.Errorf("ADLQueue.LongQueue = %v, want 0.5", p.ADLQueue.LongQueue)
	}
}

// ============================================================
// WS — ErrWSNotImplemented
// ============================================================

func TestSubscribePublic_NotImplemented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.SubscribePublic(context.Background(), []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC-USDT-SWAP"},
	})
	if err == nil {
		t.Fatal("expected ErrWSNotImplemented, got nil")
	}
	if !errors.Is(err, ErrWSNotImplemented) {
		t.Errorf("expected ErrWSNotImplemented, got: %v", err)
	}
}

func TestSubscribePrivate_NotImplemented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.SubscribePrivate(context.Background(), domain.CredentialRef{Exchange: domain.ExchangeOKX})
	if err == nil {
		t.Fatal("expected ErrWSNotImplemented, got nil")
	}
	if !errors.Is(err, ErrWSNotImplemented) {
		t.Errorf("expected ErrWSNotImplemented, got: %v", err)
	}
}

// ============================================================
// New — конструктор
// ============================================================

func TestNew_NilHTTPDoer(t *testing.T) {
	_, err := New(Config{
		RESTBaseURL: "https://www.okx.com",
		APIKey:      "k",
		APISecret:   "s",
		Passphrase:  "p",
		HTTPDoer:    nil,
	})
	if err == nil {
		t.Error("expected error when HTTPDoer is nil")
	}
}

func TestNew_Defaults(t *testing.T) {
	doer := newTestDoer("http://localhost")
	a, err := New(Config{HTTPDoer: doer})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.restBase != defaultRESTBase {
		t.Errorf("restBase = %v, want %v", a.restBase, defaultRESTBase)
	}
	if a.ID() != domain.ExchangeOKX {
		t.Errorf("ID = %v, want okx", a.ID())
	}
}

// ============================================================
// GetServerTime — public endpoint (no auth headers).
// ============================================================

func TestGetServerTime_NoAuthHeaders(t *testing.T) {
	var capturedHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write(okxResponse("0", "", []map[string]string{{"ts": "1700000000000"}}))
	}))
	defer srv.Close()

	// Создаём адаптер без ключей — публичные запросы должны работать.
	doer := newTestDoer(srv.URL)
	a, _ := New(Config{
		RESTBaseURL: srv.URL,
		HTTPDoer:    doer,
		Clock:       fixedClock,
	})
	_, err := a.GetServerTime(context.Background())
	if err != nil {
		t.Fatalf("GetServerTime (no auth): %v", err)
	}
	// Публичный endpoint не должен нести OK-ACCESS-* заголовки.
	if capturedHeaders.Get("OK-ACCESS-KEY") != "" {
		t.Error("public GET should not have OK-ACCESS-KEY header")
	}
}
