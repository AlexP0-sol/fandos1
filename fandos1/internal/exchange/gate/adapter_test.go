// Контрактные тесты адаптера Gate.io V4 USDT-perpetual.
//
// Проверяется:
//   - Парсинг contracts: quanto_multiplier, конверсия baseQty↔contracts (оба направления)
//   - GetTicker / GetOrderBookSnapshot / GetFunding — парсинг полей
//   - PlaceOrder: наличие заголовков KEY/Timestamp/SIGN; знак size для short;
//     text="t-"+clientID; конверсия baseQty→contracts
//   - CancelOrder: DELETE по order_id; ORDER_NOT_FOUND → ErrOrderNotFound
//   - GetOrder: fallback map→REST; ErrOrderNotFound
//   - Маппинг ошибок: HTTP 429 → ErrRateLimited; BALANCE_NOT_ENOUGH → ErrInsufficientMargin
//   - GetPositions: sign(size) → long/short; abs(size)×multiplier=baseQty
//   - GetBalances: парсинг total/available
//   - GetServerTime: парсинг server_time
package gate

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
// Тестовые вспомогательные типы
// ============================================================

// testHTTPDoer — HTTPDoer, проксирующий запросы к httptest-серверу.
type testHTTPDoer struct {
	baseURL string
	lastReq *capturedReq
}

type capturedReq struct {
	req     *http.Request
	body    []byte
	method  string
	path    string
	query   string
	headers map[string]string
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

	// Сохраняем последний запрос.
	captured := &capturedReq{
		req:     httpReq,
		body:    bodyBytes,
		method:  req.Method,
		path:    req.Path,
		query:   req.Query,
		headers: req.Headers,
	}
	d.lastReq = captured

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

// mkAdapter создаёт адаптер для тестов с фиксированными часами.
func mkAdapter(srv *httptest.Server) (*Adapter, *testHTTPDoer) {
	doer := newTestDoer(srv.URL)
	a, err := New(Config{
		RESTBaseURL: srv.URL,
		APIKey:      "test-api-key",
		APISecret:   "test-secret",
		HTTPDoer:    doer,
		Clock:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	})
	if err != nil {
		panic("mkAdapter: " + err.Error())
	}
	return a, doer
}

// gateErrorBody — Gate.io error response.
func gateErrorBody(label, message string) []byte {
	b, _ := json.Marshal(map[string]string{"label": label, "message": message})
	return b
}

// ============================================================
// GetServerTime
// ============================================================

func TestGetServerTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/spot/time" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"server_time":1700000000}`))
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
// GetInstruments — парсинг quanto_multiplier, конверсия
// ============================================================

func TestGetInstruments_QuantoMultiplier(t *testing.T) {
	contracts := []map[string]interface{}{
		{
			"name":               "BTC_USDT",
			"quanto_multiplier":  "0.0001", // 1 контракт = 0.0001 BTC
			"order_size_min":     1,
			"order_price_round":  "0.1",
			"funding_rate":       "0.0001",
			"funding_interval":   28800,
			"funding_next_apply": 1700000000.0,
			"in_delisting":       false,
			"trade_status":       "trading",
			"leverage_max":       "100",
		},
		{
			"name":               "ETH_USDT",
			"quanto_multiplier":  "0.01", // 1 контракт = 0.01 ETH
			"order_size_min":     1,
			"order_price_round":  "0.01",
			"funding_rate":       "0.0002",
			"funding_interval":   28800,
			"funding_next_apply": 1700000000.0,
			"in_delisting":       false,
			"trade_status":       "trading",
			"leverage_max":       "50",
		},
		{
			// Исключается: trade_status != "trading"
			"name":              "OLD_USDT",
			"quanto_multiplier": "1",
			"order_price_round": "0.1",
			"trade_status":      "closed",
			"in_delisting":      false,
		},
	}
	body, _ := json.Marshal(contracts)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/futures/usdt/contracts" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	instruments, err := a.GetInstruments(context.Background())
	if err != nil {
		t.Fatalf("GetInstruments: %v", err)
	}
	if len(instruments) != 2 {
		t.Fatalf("want 2 instruments, got %d", len(instruments))
	}

	btc := instruments[0]
	if btc.ExchangeSymbol != "BTC_USDT" {
		t.Errorf("instruments[0].Symbol = %v", btc.ExchangeSymbol)
	}
	if btc.ContractMultiplier.String() != "0.0001" {
		t.Errorf("BTC_USDT ContractMultiplier = %v, want 0.0001", btc.ContractMultiplier)
	}
	if btc.TickSize.String() != "0.1" {
		t.Errorf("BTC_USDT TickSize = %v, want 0.1", btc.TickSize)
	}

	eth := instruments[1]
	if eth.ExchangeSymbol != "ETH_USDT" {
		t.Errorf("instruments[1].Symbol = %v", eth.ExchangeSymbol)
	}
	if eth.ContractMultiplier.String() != "0.01" {
		t.Errorf("ETH_USDT ContractMultiplier = %v, want 0.01", eth.ContractMultiplier)
	}
}

// TestContractConversion — конверсия baseQty↔contracts в обоих направлениях.
// BTC_USDT: 1 контракт = 0.0001 BTC
//
//	10 BTC → 10 / 0.0001 = 100000 контрактов
//	100000 контрактов × 0.0001 = 10 BTC
func TestContractConversion(t *testing.T) {
	multiplier, _ := decimal.FromString("0.0001")

	cases := []struct {
		name          string
		baseQty       string
		wantContracts int64
		wantBackQty   string
	}{
		{"10 BTC", "10", 100000, "10"},
		{"0.5 BTC", "0.5", 5000, "0.5"},
		{"0.00005 BTC (less than 1 contract)", "0.00005", 0, "0"},
		{"0.0001 BTC (exactly 1 contract)", "0.0001", 1, "0.0001"},
		{"0.00015 BTC (floor=1 contract)", "0.00015", 1, "0.0001"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			baseQty, _ := decimal.FromString(tc.baseQty)
			contracts := baseQtyToContracts(baseQty, multiplier)
			if contracts != tc.wantContracts {
				t.Errorf("baseQtyToContracts(%s, %s) = %d, want %d",
					tc.baseQty, multiplier, contracts, tc.wantContracts)
			}
			if contracts > 0 {
				backQty := contractsToBaseQty(contracts, multiplier)
				if backQty.String() != tc.wantBackQty {
					t.Errorf("contractsToBaseQty(%d, %s) = %s, want %s",
						contracts, multiplier, backQty, tc.wantBackQty)
				}
			}
		})
	}
}

// ============================================================
// GetTicker
// ============================================================

func TestGetTicker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/futures/usdt/tickers" {
			http.Error(w, "not found", 404)
			return
		}
		if r.URL.Query().Get("contract") != "BTC_USDT" {
			http.Error(w, "bad contract", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{
			"contract": "BTC_USDT",
			"last": "50000.5",
			"mark_price": "50002.0",
			"index_price": "49999.0",
			"volume_24h_settle": "1234567.89"
		}]`))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ticker, err := a.GetTicker(context.Background(), "BTC_USDT")
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
}

func TestGetTicker_InvalidSymbol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`)) // пустой список
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetTicker(context.Background(), "NONEXISTENT_USDT")
	if err == nil {
		t.Fatal("expected error for empty ticker list")
	}
	if !errors.Is(err, exchange.ErrInvalidSymbol) {
		t.Errorf("expected ErrInvalidSymbol, got: %v", err)
	}
}

// ============================================================
// GetOrderBookSnapshot
// ============================================================

func TestGetOrderBookSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/futures/usdt/order_book" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": 12345,
			"current": 1700000000.5,
			"update": 1700000000.4,
			"asks": [["50001.0","10"],["50002.0","20"]],
			"bids": [["50000.0","15"],["49999.0","25"]]
		}`))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ob, err := a.GetOrderBookSnapshot(context.Background(), "BTC_USDT", 50)
	if err != nil {
		t.Fatalf("GetOrderBookSnapshot: %v", err)
	}
	if len(ob.Asks) != 2 {
		t.Errorf("asks count = %d, want 2", len(ob.Asks))
	}
	if len(ob.Bids) != 2 {
		t.Errorf("bids count = %d, want 2", len(ob.Bids))
	}
	if ob.Bids[0].Price.String() != "50000" {
		t.Errorf("bids[0].price = %v, want 50000", ob.Bids[0].Price)
	}
	if ob.Asks[0].Qty.String() != "10" {
		t.Errorf("asks[0].qty = %v, want 10", ob.Asks[0].Qty)
	}
	if !ob.IsSnapshot {
		t.Error("IsSnapshot should be true")
	}
	if ob.Sequence != 12345 {
		t.Errorf("Sequence = %d, want 12345", ob.Sequence)
	}
}

// ============================================================
// GetFunding — парсинг funding_rate, funding_next_apply, Confidence
// ============================================================

func TestGetFunding_Parsing(t *testing.T) {
	// funding_next_apply = 15 мин в будущем → HIGH confidence
	nextApply := float64(time.Now().Add(15 * time.Minute).Unix())

	contract := map[string]interface{}{
		"name":               "BTC_USDT",
		"quanto_multiplier":  "0.0001",
		"order_size_min":     1,
		"order_price_round":  "0.1",
		"funding_rate":       "0.0001",
		"funding_interval":   28800,
		"funding_next_apply": nextApply,
		"in_delisting":       false,
		"trade_status":       "trading",
	}
	body, _ := json.Marshal(contract)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/futures/usdt/contracts/BTC_USDT" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	fi, err := a.GetFunding(context.Background(), "BTC_USDT")
	if err != nil {
		t.Fatalf("GetFunding: %v", err)
	}
	if fi.RealizedFundingRate.String() != "0.0001" {
		t.Errorf("FundingRate = %v, want 0.0001", fi.RealizedFundingRate)
	}
	if fi.Confidence != domain.ConfidenceHigh {
		t.Errorf("Confidence = %v, want High", fi.Confidence)
	}
	if fi.FundingIntervalSec != 28800 {
		t.Errorf("FundingIntervalSec = %d, want 28800", fi.FundingIntervalSec)
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
			nextApply := float64(time.Now().Add(time.Duration(tc.minutesUntil) * time.Minute).Unix())
			body, _ := json.Marshal(map[string]interface{}{
				"name":               "BTC_USDT",
				"quanto_multiplier":  "0.0001",
				"order_price_round":  "0.1",
				"funding_rate":       "0.0001",
				"funding_interval":   28800,
				"funding_next_apply": nextApply,
			})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write(body)
			}))
			defer srv.Close()

			a, _ := mkAdapter(srv)
			fi, err := a.GetFunding(context.Background(), "BTC_USDT")
			if err != nil {
				t.Fatalf("GetFunding: %v", err)
			}
			if fi.Confidence != tc.wantConfidence {
				t.Errorf("Confidence = %v, want %v", fi.Confidence, tc.wantConfidence)
			}
		})
	}
}

// ============================================================
// GetBalances
// ============================================================

func TestGetBalances(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/futures/usdt/accounts" {
			http.Error(w, "not found", 404)
			return
		}
		// Проверяем наличие заголовков аутентификации.
		if r.Header.Get("KEY") == "" || r.Header.Get("SIGN") == "" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":"10000.5","available":"8000.25","currency":"USDT"}`))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	balances, err := a.GetBalances(context.Background())
	if err != nil {
		t.Fatalf("GetBalances: %v", err)
	}
	if len(balances) != 1 {
		t.Fatalf("want 1 balance, got %d", len(balances))
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
// GetPositions — знак size → long/short; abs(size)×multiplier=baseQty
// ============================================================

func TestGetPositions_SignAndConversion(t *testing.T) {
	// BTC_USDT: multiplier=0.0001, long size=100 контрактов
	// baseQty = 100 × 0.0001 = 0.01 BTC
	// ETH_USDT: multiplier=0.01, short size=-50 контрактов
	// baseQty = 50 × 0.01 = 0.5 ETH
	positions := []map[string]interface{}{
		{
			"contract":       "BTC_USDT",
			"size":           100,
			"entry_price":    "50000",
			"mark_price":     "51000",
			"liq_price":      "30000",
			"unrealised_pnl": "10",
			"margin":         "500",
			"leverage":       "10",
			"mode":           "single",
		},
		{
			"contract":       "ETH_USDT",
			"size":           -50,
			"entry_price":    "3000",
			"mark_price":     "3100",
			"liq_price":      "5000",
			"unrealised_pnl": "-50",
			"margin":         "150",
			"leverage":       "5",
			"mode":           "single",
		},
		{
			// Пустая позиция — должна быть пропущена.
			"contract": "SOL_USDT",
			"size":     0,
		},
	}
	body, _ := json.Marshal(positions)

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/futures/usdt/positions":
			callCount++
			w.Write(body)
		case "/api/v4/futures/usdt/contracts/BTC_USDT":
			w.Write([]byte(`{"name":"BTC_USDT","quanto_multiplier":"0.0001","order_price_round":"0.1","trade_status":"trading"}`))
		case "/api/v4/futures/usdt/contracts/ETH_USDT":
			w.Write([]byte(`{"name":"ETH_USDT","quanto_multiplier":"0.01","order_price_round":"0.01","trade_status":"trading"}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	positionsResult, err := a.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(positionsResult) != 2 {
		t.Fatalf("want 2 positions (SOL_USDT size=0 excluded), got %d", len(positionsResult))
	}

	btc := positionsResult[0]
	if btc.Side != domain.SideLong {
		t.Errorf("BTC side = %v, want Long", btc.Side)
	}
	if btc.ContractQty.String() != "100" {
		t.Errorf("BTC ContractQty = %v, want 100", btc.ContractQty)
	}
	// 100 × 0.0001 = 0.01 BTC
	if btc.BaseQty.String() != "0.01" {
		t.Errorf("BTC BaseQty = %v, want 0.01", btc.BaseQty)
	}

	eth := positionsResult[1]
	if eth.Side != domain.SideShort {
		t.Errorf("ETH side = %v, want Short", eth.Side)
	}
	if eth.ContractQty.String() != "50" {
		t.Errorf("ETH ContractQty = %v, want 50", eth.ContractQty)
	}
	// 50 × 0.01 = 0.5 ETH
	if eth.BaseQty.String() != "0.5" {
		t.Errorf("ETH BaseQty = %v, want 0.5", eth.BaseQty)
	}
}

// ============================================================
// PlaceOrder — заголовки KEY/Timestamp/SIGN; знак size для short; text=t-...
// ============================================================

func TestPlaceOrder_HeadersAndBodyLong(t *testing.T) {
	var capturedBody []byte
	var capturedKey, capturedSign, capturedTimestamp string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/futures/usdt/contracts/BTC_USDT":
			w.Write([]byte(`{"name":"BTC_USDT","quanto_multiplier":"0.0001","order_price_round":"0.1","trade_status":"trading"}`))
		case "/api/v4/futures/usdt/orders":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", 405)
				return
			}
			capturedKey = r.Header.Get("KEY")
			capturedSign = r.Header.Get("SIGN")
			capturedTimestamp = r.Header.Get("Timestamp")
			capturedBody, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"id":12345678,"contract":"BTC_USDT","size":100,"price":"50000","tif":"ioc","text":"t-myclientid","status":"open","finish_as":"","create_time":1700000000}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)

	// Сначала кэшируем multiplier.
	_, _ = a.getQuantoMultiplier(context.Background(), "BTC_USDT")

	qty, _ := decimal.FromString("0.01") // 0.01 BTC = 100 контрактов при multiplier=0.0001
	price, _ := decimal.FromString("50000")

	req := domain.PlaceOrderRequest{
		ClientOrderID: "myclientid",
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

	// Проверяем заголовки.
	if capturedKey == "" {
		t.Error("KEY header missing")
	}
	if capturedSign == "" {
		t.Error("SIGN header missing")
	}
	if capturedTimestamp == "" {
		t.Error("Timestamp header missing")
	}

	// Проверяем тело.
	var body map[string]interface{}
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	// size должно быть положительным для long.
	size, ok := body["size"].(float64)
	if !ok {
		t.Errorf("body.size missing or not number: %v", body["size"])
	} else if size != 100 {
		t.Errorf("body.size = %v, want 100 (long, 0.01/0.0001)", size)
	}
	// text = "t-" + clientID.
	text, _ := body["text"].(string)
	if text != "t-myclientid" {
		t.Errorf("body.text = %q, want t-myclientid", text)
	}
	// contract.
	if body["contract"] != "BTC_USDT" {
		t.Errorf("body.contract = %v, want BTC_USDT", body["contract"])
	}
}

func TestPlaceOrder_ShortNegativeSize(t *testing.T) {
	var capturedSize float64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/futures/usdt/contracts/ETH_USDT":
			w.Write([]byte(`{"name":"ETH_USDT","quanto_multiplier":"0.01","order_price_round":"0.01","trade_status":"trading"}`))
		case "/api/v4/futures/usdt/orders":
			b, _ := io.ReadAll(r.Body)
			var body map[string]interface{}
			json.Unmarshal(b, &body)
			capturedSize, _ = body["size"].(float64)
			w.Write([]byte(`{"id":99999,"contract":"ETH_USDT","size":-50,"price":"0","tif":"ioc","text":"t-shortorder","status":"open","finish_as":"","create_time":1700000000}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, _ = a.getQuantoMultiplier(context.Background(), "ETH_USDT")

	qty, _ := decimal.FromString("0.5") // 0.5 ETH = 50 контрактов
	req := domain.PlaceOrderRequest{
		ClientOrderID: "shortorder",
		Symbol:        "ETH_USDT",
		Side:          domain.SideShort,
		OrderMode:     domain.OrderMarket,
		BaseQty:       qty,
	}

	_, err := a.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder short: %v", err)
	}
	// Для short: size должен быть -50.
	if capturedSize != -50 {
		t.Errorf("size for short = %v, want -50", capturedSize)
	}
}

// TestPlaceOrder_TextTruncation — clientID длиннее 28 символов обрезается.
func TestPlaceOrder_TextTruncation(t *testing.T) {
	var capturedText string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/futures/usdt/contracts/BTC_USDT":
			w.Write([]byte(`{"name":"BTC_USDT","quanto_multiplier":"0.0001","order_price_round":"0.1","trade_status":"trading"}`))
		case "/api/v4/futures/usdt/orders":
			b, _ := io.ReadAll(r.Body)
			var body map[string]interface{}
			json.Unmarshal(b, &body)
			capturedText, _ = body["text"].(string)
			w.Write([]byte(`{"id":1,"contract":"BTC_USDT","size":1,"price":"0","tif":"ioc","text":"","status":"open","create_time":1700000000}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, _ = a.getQuantoMultiplier(context.Background(), "BTC_USDT")

	// 30 символов clientID (>28) → должно быть обрезано.
	longID := "abcdefghijklmnopqrstuvwxyz0123456789" // 36 символов
	qty, _ := decimal.FromString("0.0001")
	req := domain.PlaceOrderRequest{
		ClientOrderID: domain.ClientOrderID(longID),
		Symbol:        "BTC_USDT",
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarket,
		BaseQty:       qty,
	}

	_, err := a.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	// text = "t-" + first 28 chars of sanitized clientID = 30 chars total.
	wantText := "t-" + longID[:28]
	if capturedText != wantText {
		t.Errorf("text = %q, want %q (truncated to 30 total)", capturedText, wantText)
	}
}

// ============================================================
// CancelOrder — DELETE; ORDER_NOT_FOUND → ErrOrderNotFound
// ============================================================

func TestCancelOrder_Success(t *testing.T) {
	var capturedMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		if r.URL.Path != "/api/v4/futures/usdt/orders/12345678" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Gate.io возвращает отменённый ордер.
		w.Write([]byte(`{"id":12345678,"contract":"BTC_USDT","size":100,"status":"finished","finish_as":"cancelled","create_time":1700000000}`))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ExchangeOrderID: "12345678",
		Symbol:          "BTC_USDT",
	})
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if capturedMethod != http.MethodDelete {
		t.Errorf("method = %v, want DELETE", capturedMethod)
	}
}

func TestCancelOrder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		w.Write(gateErrorBody("ORDER_NOT_FOUND", "order not found"))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	err := a.CancelOrder(context.Background(), domain.CancelOrderRequest{
		ExchangeOrderID: "99999",
		Symbol:          "BTC_USDT",
	})
	if err == nil {
		t.Fatal("expected ErrOrderNotFound, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

// ============================================================
// GetOrder — fallback через map; ErrOrderNotFound
// ============================================================

func TestGetOrder_ViaExchangeID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/futures/usdt/orders/12345" {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":12345,"contract":"BTC_USDT","size":100,"price":"50000","tif":"ioc","text":"t-myorder","status":"open","finish_as":"","create_time":1700000000}`))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	ord, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ExchangeOrderID: "12345",
		Symbol:          "BTC_USDT",
	})
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if ord.ExchangeOrderID != "12345" {
		t.Errorf("ExchangeOrderID = %v, want 12345", ord.ExchangeOrderID)
	}
}

func TestGetOrder_ViaClientIDMap(t *testing.T) {
	// Симулируем: PlaceOrder сохранил маппинг, теперь GetOrder через clientID.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/futures/usdt/contracts/BTC_USDT":
			w.Write([]byte(`{"name":"BTC_USDT","quanto_multiplier":"0.0001","order_price_round":"0.1","trade_status":"trading"}`))
		case "/api/v4/futures/usdt/orders":
			if r.Method == http.MethodPost {
				w.Write([]byte(`{"id":77777,"contract":"BTC_USDT","size":1,"price":"0","tif":"ioc","text":"t-clientabc","status":"open","create_time":1700000000}`))
			}
		case "/api/v4/futures/usdt/orders/77777":
			w.Write([]byte(`{"id":77777,"contract":"BTC_USDT","size":1,"price":"0","tif":"ioc","text":"t-clientabc","status":"open","finish_as":"","create_time":1700000000}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)

	// Размещаем ордер → сохраняется маппинг clientabc → 77777.
	qty, _ := decimal.FromString("0.0001")
	_, err := a.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "clientabc",
		Symbol:        "BTC_USDT",
		Side:          domain.SideLong,
		OrderMode:     domain.OrderMarket,
		BaseQty:       qty,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}

	// GetOrder через clientID.
	ord, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "clientabc",
		Symbol:        "BTC_USDT",
	})
	if err != nil {
		t.Fatalf("GetOrder via clientID map: %v", err)
	}
	if ord.ExchangeOrderID != "77777" {
		t.Errorf("ExchangeOrderID = %v, want 77777", ord.ExchangeOrderID)
	}
}

func TestGetOrder_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Открытых ордеров нет.
		if strings.Contains(r.URL.RawQuery, "status=open") {
			w.Write([]byte(`[]`))
			return
		}
		// Завершённых ордеров нет.
		if strings.Contains(r.URL.RawQuery, "status=finished") {
			w.Write([]byte(`[]`))
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ClientOrderID: "no-such-order",
		Symbol:        "BTC_USDT",
	})
	if err == nil {
		t.Fatal("expected ErrOrderNotFound, got nil")
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
		w.WriteHeader(429)
		w.Write(gateErrorBody("RATE_LIMITED", "too many requests"))
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

func TestErrorMapping_Unauthorized_InvalidKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write(gateErrorBody("INVALID_KEY", "invalid api key"))
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

func TestErrorMapping_Unauthorized_InvalidSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write(gateErrorBody("INVALID_SIGNATURE", "invalid signature"))
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

func TestErrorMapping_BalanceNotEnough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/futures/usdt/contracts/BTC_USDT":
			w.Write([]byte(`{"name":"BTC_USDT","quanto_multiplier":"0.0001","order_price_round":"0.1","trade_status":"trading"}`))
		default:
			w.WriteHeader(400)
			w.Write(gateErrorBody("BALANCE_NOT_ENOUGH", "insufficient balance"))
		}
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	qty, _ := decimal.FromString("10")
	_, err := a.PlaceOrder(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "test",
		Symbol:        "BTC_USDT",
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

func TestErrorMapping_ContractNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		w.Write(gateErrorBody("CONTRACT_NOT_FOUND", "contract not found"))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetTicker(context.Background(), "INVALID_USDT")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Проверяем ErrInvalidSymbol или ErrOrderNotFound (через CONTRACT_NOT_FOUND mapping).
	if !errors.Is(err, exchange.ErrInvalidSymbol) {
		t.Errorf("expected ErrInvalidSymbol, got: %v", err)
	}
}

func TestErrorMapping_OrderNotFound_Label(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		w.Write(gateErrorBody("ORDER_NOT_FOUND", "order not found"))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.GetOrder(context.Background(), domain.OrderQuery{
		ExchangeOrderID: "00001",
		Symbol:          "BTC_USDT",
	})
	if err == nil {
		t.Fatal("expected ErrOrderNotFound, got nil")
	}
	if !errors.Is(err, exchange.ErrOrderNotFound) {
		t.Errorf("expected ErrOrderNotFound, got: %v", err)
	}
}

// ============================================================
// GetOpenOrders — парсинг нескольких ордеров
// ============================================================

func TestGetOpenOrders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/futures/usdt/orders" {
			http.Error(w, "not found", 404)
			return
		}
		if r.URL.Query().Get("status") != "open" {
			http.Error(w, "bad status", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
			{
				"id": 111,
				"contract": "BTC_USDT",
				"size": 50,
				"price": "50000",
				"tif": "ioc",
				"text": "t-order1",
				"status": "open",
				"finish_as": "",
				"fill_price": "0",
				"left": 50,
				"is_reduce_only": false,
				"create_time": 1700000000
			},
			{
				"id": 222,
				"contract": "BTC_USDT",
				"size": -30,
				"price": "0",
				"tif": "ioc",
				"text": "t-order2",
				"status": "open",
				"finish_as": "",
				"fill_price": "0",
				"left": -30,
				"is_reduce_only": true,
				"create_time": 1700000001
			}
		]`))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	orders, err := a.GetOpenOrders(context.Background(), "BTC_USDT")
	if err != nil {
		t.Fatalf("GetOpenOrders: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("want 2 orders, got %d", len(orders))
	}

	o1 := orders[0]
	if o1.Side != domain.SideLong {
		t.Errorf("order1 side = %v, want Long", o1.Side)
	}
	if o1.ClientOrderID != "order1" {
		t.Errorf("order1 ClientOrderID = %v, want order1", o1.ClientOrderID)
	}

	o2 := orders[1]
	if o2.Side != domain.SideShort {
		t.Errorf("order2 side = %v, want Short", o2.Side)
	}
	if !o2.ReduceOnly {
		t.Error("order2 should be reduce_only")
	}
}

// ============================================================
// New — обязательный HTTPDoer
// ============================================================

func TestNew_NilHTTPDoer(t *testing.T) {
	_, err := New(Config{
		APIKey:    "k",
		APISecret: "s",
		HTTPDoer:  nil,
	})
	if err == nil {
		t.Fatal("expected error for nil HTTPDoer")
	}
}

// ============================================================
// clientIDToText — форматирование поля text
// ============================================================

func TestClientIDToText(t *testing.T) {
	cases := []struct {
		clientID string
		wantText string
	}{
		{"abc123", "t-abc123"},
		{"my-order.id_1", "t-my-order.id_1"},
		// Длинный ID → обрезка до 28 после "t-" = 30 суммарно
		{"abcdefghijklmnopqrstuvwxyz0123456789", "t-abcdefghijklmnopqrstuvwxyz01"},
		// Спецсимволы удаляются
		{"order:colon/slash", "t-ordercolonslash"},
	}
	for _, tc := range cases {
		got := clientIDToText(tc.clientID)
		if got != tc.wantText {
			t.Errorf("clientIDToText(%q) = %q, want %q", tc.clientID, got, tc.wantText)
		}
		// Суммарно ≤ 30 символов.
		if len(got) > 30 {
			t.Errorf("text %q exceeds 30 chars: len=%d", got, len(got))
		}
	}
}

// ============================================================
// SubscribePublic/SubscribePrivate — возвращают ErrWSNotImplemented
// ============================================================

func TestSubscribePublic_ReturnsNotImplemented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.SubscribePublic(context.Background(), []exchange.PublicSubscription{
		{Channel: exchange.ChannelTicker, Symbol: "BTC_USDT"},
	})
	if err == nil {
		t.Fatal("expected ErrWSNotImplemented, got nil")
	}
	if !errors.Is(err, ErrWSNotImplemented) {
		t.Errorf("expected ErrWSNotImplemented, got: %v", err)
	}
}

func TestSubscribePrivate_ReturnsNotImplemented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	_, err := a.SubscribePrivate(context.Background(), domain.CredentialRef{})
	if err == nil {
		t.Fatal("expected ErrWSNotImplemented, got nil")
	}
	if !errors.Is(err, ErrWSNotImplemented) {
		t.Errorf("expected ErrWSNotImplemented, got: %v", err)
	}
}

// ============================================================
// SetLeverage — проверка пути и query
// ============================================================

func TestSetLeverage(t *testing.T) {
	var capturedPath, capturedQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"leverage":"10","cross_leverage_limit":"0"}`))
	}))
	defer srv.Close()

	a, _ := mkAdapter(srv)
	lev, _ := decimal.FromString("10")
	err := a.SetLeverage(context.Background(), domain.SetLeverageRequest{
		Symbol:   "BTC_USDT",
		Leverage: lev,
	})
	if err != nil {
		t.Fatalf("SetLeverage: %v", err)
	}
	if capturedPath != "/api/v4/futures/usdt/positions/BTC_USDT/leverage" {
		t.Errorf("path = %v", capturedPath)
	}
	if capturedQuery != "leverage=10" {
		t.Errorf("query = %v, want leverage=10", capturedQuery)
	}
}
