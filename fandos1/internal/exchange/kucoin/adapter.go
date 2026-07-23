// Package kucoin реализует адаптер биржи KuCoin Futures для linear USDT perpetual.
//
// Все REST-запросы используют конверт:
//
//	{"code":"200000","data":...}
//
// code != "200000" маппируется в sentinel-ошибки через mapAPIError.
// Аутентификация — через заголовки KC-API-* v2 (KC-API-KEY-VERSION: 2).
//
// VERIFIED (KuCoin Futures docs 2026-07):
//   - GET /api/v1/timestamp
//   - GET /api/v1/contracts/active
//   - GET /api/v1/funding-rate/{symbol}/current
//   - GET /api/v1/ticker?symbol=
//   - GET /api/v1/level2/depth20?symbol=
//   - GET /api/v1/account-overview?currency=USDT
//   - GET /api/v1/positions
//   - GET /api/v1/orders?status=active&symbol=
//   - POST /api/v1/orders       (clientOid, size=лоты, leverage, marginMode)
//   - DELETE /api/v1/orders/{orderId}
//   - GET /api/v1/orders/{orderId}
//   - GET /api/v1/orders/byClientOid?clientOid=
//
// TODO:VERIFY:
//   - POST /api/v3/transfer-out (futures → main)
//   - POST /api/v3/transfer-in  (main → futures)
//   - POST /api/v2/position/changeLeverage (отдельный endpoint плеча)
//   - GET /api/v1/withdrawals, GET /api/v1/deposits (spot API api.kucoin.com)
//   - POST /api/v1/withdrawals (spot API)
//   - GET /api/v3/currencies/{currency} (spot API, сети вывода)
//   - WebSocket bullet-token: POST /api/v1/bullet-public / bullet-private
//
// XBT/BTC маппинг:
//
//	KuCoin называет Bitcoin "XBT" в futures символах (XBTUSDTM), но "BTC" в spot API.
//	CanonicalBaseAsset: XBT → BTC (канонизация при парсинге инструментов).
//	При конструировании exchange-символа из канонического: BTC → XBT.
//
// Лоты (size):
//
//	KuCoin Futures qty = целое число лотов (integer). 1 лот = multiplier базовой монеты.
//	Пример: XBTUSDTM, multiplier=0.001: 1 lot = 0.001 BTC.
//	PlaceOrder: BaseQty (в BTC) → лоты = floor(BaseQty / multiplier).
//	GetPositions: currentQty (в лотах, знаковое) → BaseQty = abs(currentQty) * multiplier.
//	Multiplier кешируется в instrCache при вызове GetInstruments.
package kucoin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// ============================================================
// Конфигурация и конструктор
// ============================================================

// Config — параметры адаптера KuCoin Futures.
type Config struct {
	RESTBaseURL  string           // default: https://api-futures.kucoin.com
	SpotBaseURL  string           // default: https://api.kucoin.com  (для withdraw/deposit/networks)
	WSBaseURL    string           // default: wss://ws-api-futures.kucoin.com
	APIKey       string           // обязательно
	APISecret    string           // обязательно
	Passphrase   string           // обязательно (KC-API v2)
	HTTPDoer     HTTPDoer         // обязательно
	RecvWindowMs int64            // не используется KuCoin (поле для совместимости с интерфейсом)
	Clock        func() time.Time // default: time.Now
}

// HTTPDoer — интерфейс HTTP-клиента для инъекции в тестах.
type HTTPDoer interface {
	Do(ctx context.Context, req HTTPRequest) (statusCode int, body []byte, err error)
}

// HTTPRequest — минимальный запрос для HTTPDoer.
type HTTPRequest struct {
	Method  string
	Path    string
	Query   string // без "?", для GET склеивается адаптером
	Body    io.Reader
	Headers map[string]string
	Safe    bool // true = GET, false = POST/DELETE
}

const (
	defaultRESTBase = "https://api-futures.kucoin.com"
	defaultSpotBase = "https://api.kucoin.com"
	defaultWSBase   = "wss://ws-api-futures.kucoin.com"
)

// instrCacheEntry хранит multiplier для конвертации лотов ↔ базовая монета.
type instrCacheEntry struct {
	multiplier decimal.Decimal
}

// Adapter — реализация exchange.ExchangeAdapter для KuCoin Futures.
type Adapter struct {
	restBase string
	spotBase string
	wsBase   string
	signer   *Signer
	http     HTTPDoer
	clock    func() time.Time

	// instrCache кешируется при GetInstruments.
	// key: ExchangeSymbol ("XBTUSDTM"), value: instrCacheEntry.
	instrMu    sync.RWMutex
	instrCache map[domain.ExchangeSymbol]instrCacheEntry
}

// New создаёт адаптер KuCoin Futures из конфига.
// Возвращает ошибку, если APIKey, APISecret или Passphrase пустые.
func New(cfg Config) (*Adapter, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("kucoin: APIKey обязателен")
	}
	if cfg.APISecret == "" {
		return nil, fmt.Errorf("kucoin: APISecret обязателен")
	}
	if cfg.Passphrase == "" {
		return nil, fmt.Errorf("kucoin: Passphrase обязательна")
	}
	if cfg.HTTPDoer == nil {
		return nil, fmt.Errorf("kucoin: HTTPDoer обязателен")
	}

	signer := NewSigner(cfg.APIKey, []byte(cfg.APISecret), cfg.Passphrase)

	a := &Adapter{
		restBase:   cfg.RESTBaseURL,
		spotBase:   cfg.SpotBaseURL,
		wsBase:     cfg.WSBaseURL,
		signer:     signer,
		http:       cfg.HTTPDoer,
		clock:      cfg.Clock,
		instrCache: make(map[domain.ExchangeSymbol]instrCacheEntry),
	}
	if a.restBase == "" {
		a.restBase = defaultRESTBase
	}
	if a.spotBase == "" {
		a.spotBase = defaultSpotBase
	}
	if a.wsBase == "" {
		a.wsBase = defaultWSBase
	}
	if a.clock == nil {
		a.clock = time.Now
	}
	return a, nil
}

// ID возвращает идентификатор биржи.
func (a *Adapter) ID() domain.ExchangeID { return domain.ExchangeKuCoin }

// ============================================================
// Конверт KuCoin
// ============================================================

// kcEnvelope — универсальная обёртка ответов KuCoin Futures.
// VERIFIED: {"code":"200000","data":...}
type kcEnvelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// decodeEnvelope декодирует конверт и проверяет code.
// code != "200000" → mapAPIError.
func decodeEnvelope(body []byte) (kcEnvelope, error) {
	var env kcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("kucoin: parse envelope: %w", err)
	}
	if env.Code != "200000" {
		return env, mapAPIError(env.Code, env.Msg)
	}
	return env, nil
}

// mapAPIError маппирует код ошибки KuCoin в sentinel-ошибки.
// VERIFIED: 429000 → ErrRateLimited; 400003/400004/400005 → ErrUnauthorized
// TODO:VERIFY: 100001 → ErrInvalidSymbol; 300003 → ErrInsufficientMargin; 100004 → ErrOrderNotFound
func mapAPIError(code, msg string) error {
	switch code {
	case "429000":
		return fmt.Errorf("%w: code=%s %s", exchange.ErrRateLimited, code, msg)
	case "400003", "400004", "400005":
		// 400003=key/IP, 400004=passphrase, 400005=signature
		return fmt.Errorf("%w: code=%s %s", exchange.ErrUnauthorized, code, msg)
	case "100001":
		// TODO:VERIFY: param invalid / symbol not found
		if strings.Contains(strings.ToLower(msg), "symbol") ||
			strings.Contains(strings.ToLower(msg), "invalid") {
			return fmt.Errorf("%w: code=%s %s", exchange.ErrInvalidSymbol, code, msg)
		}
		return fmt.Errorf("kucoin: invalid param code=%s %s", code, msg)
	case "300003":
		// TODO:VERIFY: insufficient margin
		return fmt.Errorf("%w: code=%s %s", exchange.ErrInsufficientMargin, code, msg)
	case "100004":
		// TODO:VERIFY: order not found
		return fmt.Errorf("%w: code=%s %s", exchange.ErrOrderNotFound, code, msg)
	case "200004":
		// Insufficient balance — маппируем как insufficient margin
		return fmt.Errorf("%w: code=%s %s", exchange.ErrInsufficientMargin, code, msg)
	default:
		return fmt.Errorf("kucoin: API error code=%s: %s", code, msg)
	}
}

// ============================================================
// HTTP helpers
// ============================================================

func (a *Adapter) nowMs() int64 { return a.clock().UnixMilli() }

// doPublicGET — GET без аутентификации (public endpoints).
func (a *Adapter) doPublicGET(ctx context.Context, path, query string) (kcEnvelope, error) {
	fullPath := path
	if query != "" {
		fullPath = path + "?" + query
	}
	_, body, err := a.http.Do(ctx, HTTPRequest{
		Method: http.MethodGet,
		Path:   path,
		Query:  query,
		Safe:   true,
	})
	if err != nil {
		return kcEnvelope{}, wrapNetErr(err)
	}
	_ = fullPath
	return decodeEnvelope(body)
}

// doSignedGET — GET с аутентификацией KC-API v2.
// VERIFIED: str_to_sign = timestamp + "GET" + endpoint?query (body пустой).
func (a *Adapter) doSignedGET(ctx context.Context, path, query string) (kcEnvelope, error) {
	ts := a.nowMs()
	endpointWithQuery := path
	if query != "" {
		endpointWithQuery = path + "?" + query
	}
	strToSign := StrToSignGET(ts, "GET", endpointWithQuery)
	headers := a.signer.AuthHeaders(ts, strToSign)

	_, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodGet,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    true,
	})
	if err != nil {
		return kcEnvelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(respBody)
}

// doSignedDELETE — DELETE с аутентификацией.
// VERIFIED: str_to_sign = timestamp + "DELETE" + endpoint (тело пустое).
func (a *Adapter) doSignedDELETE(ctx context.Context, path, query string) (kcEnvelope, error) {
	ts := a.nowMs()
	endpointWithQuery := path
	if query != "" {
		endpointWithQuery = path + "?" + query
	}
	strToSign := StrToSignGET(ts, "DELETE", endpointWithQuery)
	headers := a.signer.AuthHeaders(ts, strToSign)

	_, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodDelete,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return kcEnvelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(respBody)
}

// doSignedPOST — POST с JSON-телом и аутентификацией. Safe=false.
// VERIFIED: str_to_sign = timestamp + "POST" + endpoint + bodyJSON.
func (a *Adapter) doSignedPOST(ctx context.Context, path string, payload interface{}) (kcEnvelope, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return kcEnvelope{}, fmt.Errorf("kucoin: marshal: %w", err)
	}
	bodyStr := string(bodyBytes)

	ts := a.nowMs()
	strToSign := StrToSignPOST(ts, "POST", path, bodyStr)
	headers := a.signer.AuthHeaders(ts, strToSign)
	headers["Content-Type"] = "application/json"

	_, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Body:    bytes.NewReader(bodyBytes),
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return kcEnvelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(respBody)
}

// doSignedSpotGET — GET к spot API (api.kucoin.com) с аутентификацией.
// Используется для withdraw/deposit/networks.
func (a *Adapter) doSignedSpotGET(ctx context.Context, path, query string) (kcEnvelope, error) {
	ts := a.nowMs()
	endpointWithQuery := path
	if query != "" {
		endpointWithQuery = path + "?" + query
	}
	strToSign := StrToSignGET(ts, "GET", endpointWithQuery)
	headers := a.signer.AuthHeaders(ts, strToSign)

	_, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodGet,
		Path:    "[SPOT]" + path, // префикс сигнализирует HTTPDoer о spot base URL
		Query:   query,
		Headers: headers,
		Safe:    true,
	})
	if err != nil {
		return kcEnvelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(respBody)
}

// doSignedSpotPOST — POST к spot API.
func (a *Adapter) doSignedSpotPOST(ctx context.Context, path string, payload interface{}) (kcEnvelope, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return kcEnvelope{}, fmt.Errorf("kucoin: marshal: %w", err)
	}
	bodyStr := string(bodyBytes)

	ts := a.nowMs()
	strToSign := StrToSignPOST(ts, "POST", path, bodyStr)
	headers := a.signer.AuthHeaders(ts, strToSign)
	headers["Content-Type"] = "application/json"

	_, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    "[SPOT]" + path,
		Body:    bytes.NewReader(bodyBytes),
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return kcEnvelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(respBody)
}

// wrapNetErr оборачивает сетевые/таймаут-ошибки.
func wrapNetErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", exchange.ErrTimeout, err)
	}
	return fmt.Errorf("%w: %v", exchange.ErrNetwork, err)
}

// ============================================================
// GetServerTime — VERIFIED
// ============================================================

// serverTimeData — поле data ответа GET /api/v1/timestamp.
// VERIFIED: data — JSON number (миллисекунды).
type serverTimeData struct {
	// KuCoin возвращает data как JSON number (int), а не строку.
	// Используем json.Number для безопасного парсинга.
}

// GetServerTime возвращает серверное время KuCoin Futures.
// VERIFIED: GET /api/v1/timestamp → data=<ms как JSON number>
func (a *Adapter) GetServerTime(ctx context.Context) (time.Time, error) {
	env, err := a.doPublicGET(ctx, "/api/v1/timestamp", "")
	if err != nil {
		return time.Time{}, fmt.Errorf("kucoin GetServerTime: %w", err)
	}
	// data — JSON number (int64 ms)
	var ts json.Number
	if err := json.Unmarshal(env.Data, &ts); err != nil {
		return time.Time{}, fmt.Errorf("kucoin GetServerTime: parse data: %w", err)
	}
	ms, err := ts.Int64()
	if err != nil {
		return time.Time{}, fmt.Errorf("kucoin GetServerTime: parse ms: %w", err)
	}
	return time.UnixMilli(ms).UTC(), nil
}

// ============================================================
// CanonicalBaseAsset — XBT↔BTC маппинг
// ============================================================

// canonicalBaseAsset нормализует KuCoin-специфичное "XBT" → "BTC".
// Все остальные активы остаются без изменений.
// VERIFIED: KuCoin Futures использует XBT для Bitcoin в символах (XBTUSDTM).
func canonicalBaseAsset(kuCoinBase string) domain.AssetSymbol {
	if kuCoinBase == "XBT" {
		return "BTC"
	}
	return domain.AssetSymbol(kuCoinBase)
}

// ============================================================
// GetInstruments — VERIFIED (фильтр isInverse=false + status=Open + settleCurrency=USDT)
// ============================================================

// contractEntry — один контракт из GET /api/v1/contracts/active.
// VERIFIED поля: symbol, baseCurrency (XBT!), multiplier, lotSize, tickSize, maxLeverage,
// status ("Open"), isInverse, settleCurrency, fundingFeeRate, nextFundingRateTime,
// fundingRateGranularity (ms).
type contractEntry struct {
	Symbol                 string      `json:"symbol"`
	BaseCurrency           string      `json:"baseCurrency"`
	QuoteCurrency          string      `json:"quoteCurrency"`
	SettleCurrency         string      `json:"settleCurrency"`
	Multiplier             json.Number `json:"multiplier"`  // KuCoin шлёт как number
	LotSize                json.Number `json:"lotSize"`     // min lot size (integer)
	TickSize               json.Number `json:"tickSize"`    // price tick
	MaxLeverage            json.Number `json:"maxLeverage"` // max leverage
	Status                 string      `json:"status"`      // "Open" = active
	IsInverse              bool        `json:"isInverse"`
	FundingFeeRate         json.Number `json:"fundingFeeRate"`         // текущий funding rate
	FundingRateGranularity json.Number `json:"fundingRateGranularity"` // интервал в ms
	NextFundingRateTime    json.Number `json:"nextFundingRateTime"`    // мс до следующего funding
}

// GetInstruments возвращает все торгуемые linear USDT perpetual инструменты.
// VERIFIED: GET /api/v1/contracts/active
// Фильтр: isInverse=false + status="Open" + settleCurrency="USDT"
func (a *Adapter) GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error) {
	env, err := a.doPublicGET(ctx, "/api/v1/contracts/active", "")
	if err != nil {
		return nil, fmt.Errorf("kucoin GetInstruments: %w", err)
	}

	var contracts []contractEntry
	if err := json.Unmarshal(env.Data, &contracts); err != nil {
		return nil, fmt.Errorf("kucoin GetInstruments: parse: %w", err)
	}

	a.instrMu.Lock()
	// Полное обновление кеша.
	newCache := make(map[domain.ExchangeSymbol]instrCacheEntry, len(contracts))
	defer func() {
		a.instrCache = newCache
		a.instrMu.Unlock()
	}()

	var result []domain.CanonicalInstrument
	for _, c := range contracts {
		// Фильтруем inverse и не-USDT settle.
		if c.IsInverse || c.SettleCurrency != "USDT" || c.Status != "Open" {
			continue
		}

		instr, mult, err := parseContract(c)
		if err != nil {
			continue // пропускаем контракты с некорректными данными
		}
		newCache[instr.ExchangeSymbol] = instrCacheEntry{multiplier: mult}
		result = append(result, instr)
	}

	return result, nil
}

// parseContract преобразует contractEntry → domain.CanonicalInstrument + multiplier.
func parseContract(c contractEntry) (domain.CanonicalInstrument, decimal.Decimal, error) {
	mult, err := decimal.FromString(c.Multiplier.String())
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("multiplier: %w", err)
	}
	// lotSize — минимальный размер ордера в лотах (обычно 1)
	minLotStr := c.LotSize.String()
	if minLotStr == "" {
		minLotStr = "1"
	}
	minLot, err := decimal.FromString(minLotStr)
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("lotSize: %w", err)
	}
	tickSize, err := decimal.FromString(c.TickSize.String())
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("tickSize: %w", err)
	}
	maxLev, err := decimal.FromString(c.MaxLeverage.String())
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("maxLeverage: %w", err)
	}

	// Funding interval: granularity в ms → секунды
	var fundingIntervalSec int64
	if gran := c.FundingRateGranularity.String(); gran != "" && gran != "0" {
		granMs, err := decimal.FromString(gran)
		if err == nil {
			fundingIntervalSec = granMs.Underlying().IntPart() / 1000
		}
	}
	if fundingIntervalSec == 0 {
		fundingIntervalSec = 8 * 3600 // дефолт 8 часов
	}

	// MinQty в базовой монете = minLot * multiplier
	// QtyStep = multiplier (минимальный шаг в базовой монете)
	// Для USDT perpetual QtyStep в лотах = 1 (целое)
	// Для domain: QtyStep = multiplier (в базовой монете), MinQty = minLot*multiplier
	minQty := minLot.Mul(mult)
	qtyStep := mult

	sym := domain.ExchangeSymbol(c.Symbol)
	return domain.CanonicalInstrument{
		Exchange:           domain.ExchangeKuCoin,
		CanonicalBaseAsset: canonicalBaseAsset(c.BaseCurrency),
		ExchangeSymbol:     sym,
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		SettlementCurrency: c.SettleCurrency,
		ContractMultiplier: mult,
		QtyStep:            qtyStep,
		MinQty:             minQty,
		TickSize:           tickSize,
		MaxLeverage:        maxLev,
		FundingIntervalSec: fundingIntervalSec,
		FundingPriceType:   domain.FundingPriceMark,
		SupportsADL:        false, // TODO:VERIFY: нет публичного ADL endpoint у KuCoin
		Status:             domain.InstrumentStatusActive,
	}, mult, nil
}

// multiplierFor возвращает multiplier из кеша или decimal.Zero если нет.
func (a *Adapter) multiplierFor(sym domain.ExchangeSymbol) (decimal.Decimal, bool) {
	a.instrMu.RLock()
	defer a.instrMu.RUnlock()
	e, ok := a.instrCache[sym]
	if !ok {
		return decimal.Zero, false
	}
	return e.multiplier, true
}

// baseQtyToLots конвертирует базовую qty → целое число лотов (floor).
// Возвращает количество лотов как int64.
// Если multiplier не известен — ошибка.
func (a *Adapter) baseQtyToLots(sym domain.ExchangeSymbol, baseQty decimal.Decimal) (int64, error) {
	mult, ok := a.multiplierFor(sym)
	if !ok || mult.IsZero() {
		return 0, fmt.Errorf("kucoin: multiplier unknown for %s — call GetInstruments first", sym)
	}
	lotsD := baseQty.Div(mult)
	// Floor (truncate к нулю для положительных)
	lotsD = lotsD.Truncate(0)
	lots := lotsD.Underlying().IntPart()
	if lots <= 0 {
		return 0, fmt.Errorf("kucoin: baseQty %s too small for multiplier %s (0 lots)", baseQty, mult)
	}
	return lots, nil
}

// lotsToBaseQty конвертирует знаковое количество лотов → базовую qty (abs * multiplier).
func (a *Adapter) lotsToBaseQty(sym domain.ExchangeSymbol, lots int64) decimal.Decimal {
	mult, ok := a.multiplierFor(sym)
	if !ok || mult.IsZero() {
		return decimal.Zero
	}
	absLots := lots
	if absLots < 0 {
		absLots = -absLots
	}
	return decimal.FromInt(absLots).Mul(mult)
}

// ============================================================
// GetFunding — VERIFIED
// ============================================================

// fundingRateData — ответ GET /api/v1/funding-rate/{symbol}/current.
// VERIFIED: value = текущий rate, predictedValue = predicted rate, fundingTime = следующий funding (ms).
type fundingRateData struct {
	Symbol         string      `json:"symbol"`
	Granularity    json.Number `json:"granularity"`    // интервал ms
	TimePoint      json.Number `json:"timePoint"`      // время последнего funding (ms)
	Value          json.Number `json:"value"`          // realized rate
	PredictedValue json.Number `json:"predictedValue"` // predicted rate
	FundingTime    json.Number `json:"fundingTime"`    // следующий funding (ms)
}

// GetFunding возвращает нормализованную funding-информацию.
// VERIFIED: GET /api/v1/funding-rate/{symbol}/current
func (a *Adapter) GetFunding(ctx context.Context, symbol domain.ExchangeSymbol) (domain.FundingInfo, error) {
	path := "/api/v1/funding-rate/" + string(symbol) + "/current"
	env, err := a.doPublicGET(ctx, path, "")
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("kucoin GetFunding %s: %w", symbol, err)
	}
	var data fundingRateData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.FundingInfo{}, fmt.Errorf("kucoin GetFunding %s: parse: %w", symbol, err)
	}

	realized, err := parseJSONNumber(data.Value)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("kucoin GetFunding: value: %w", err)
	}
	predicted, err := parseJSONNumber(data.PredictedValue)
	if err != nil {
		// KuCoin может не присылать predictedValue — используем realized
		predicted = realized
	}

	var nextFunding time.Time
	if ft := data.FundingTime.String(); ft != "" && ft != "0" {
		ftMs, err2 := data.FundingTime.Int64()
		if err2 == nil {
			nextFunding = time.UnixMilli(ftMs).UTC()
		}
	}

	var fundingIntervalSec int64 = 8 * 3600
	if gran := data.Granularity.String(); gran != "" && gran != "0" {
		granMs, err2 := data.Granularity.Int64()
		if err2 == nil {
			fundingIntervalSec = granMs / 1000
		}
	}

	// Confidence policy: аналогично bybit
	untilFunding := time.Until(nextFunding)
	var confidence domain.ConfidenceLevel
	switch {
	case untilFunding < 30*time.Minute:
		confidence = domain.ConfidenceHigh
	case untilFunding < 4*time.Hour:
		confidence = domain.ConfidenceMedium
	default:
		confidence = domain.ConfidenceLow
	}

	return domain.FundingInfo{
		ExchangeSymbol:       symbol,
		RealizedFundingRate:  realized,
		PredictedFundingRate: predicted,
		RateType:             domain.FundingRatePredicted,
		FundingIntervalSec:   fundingIntervalSec,
		NextFundingTime:      nextFunding,
		Confidence:           confidence,
		FundingPriceType:     domain.FundingPriceMark,
	}, nil
}

// ============================================================
// GetTicker — VERIFIED
// ============================================================

// tickerData — поля данных GET /api/v1/ticker?symbol=.
// VERIFIED: price, bestBidPrice, bestAskPrice как строки.
type tickerData struct {
	Symbol       string      `json:"symbol"`
	Price        string      `json:"price"`        // last price
	BestBidPrice string      `json:"bestBidPrice"` // best bid
	BestAskPrice string      `json:"bestAskPrice"` // best ask
	TradeId      json.Number `json:"tradeId"`
	Ts           json.Number `json:"ts"` // timestamp ns
}

// GetTicker возвращает нормализованный тикер.
// VERIFIED: GET /api/v1/ticker?symbol=
func (a *Adapter) GetTicker(ctx context.Context, symbol domain.ExchangeSymbol) (domain.Ticker, error) {
	query := BuildSortedQuery(map[string]string{"symbol": string(symbol)})
	env, err := a.doPublicGET(ctx, "/api/v1/ticker", query)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("kucoin GetTicker %s: %w", symbol, err)
	}
	var data tickerData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.Ticker{}, fmt.Errorf("kucoin GetTicker %s: parse: %w", symbol, err)
	}

	lastPrice, err := parseDecimalOrZero(data.Price)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("kucoin GetTicker: price: %w", err)
	}

	return domain.Ticker{
		Symbol:    symbol,
		LastPrice: lastPrice,
		Timestamp: a.clock(),
	}, nil
}

// ============================================================
// GetOrderBookSnapshot — VERIFIED
// ============================================================

// depth20Data — ответ GET /api/v1/level2/depth20.
// VERIFIED: bids/asks как [[price, qty], ...] строки.
type depth20Data struct {
	Symbol   string      `json:"symbol"`
	Bids     [][]string  `json:"bids"` // [[price, qty], ...]
	Asks     [][]string  `json:"asks"`
	Ts       json.Number `json:"ts"` // timestamp ms
	Sequence json.Number `json:"sequence"`
}

// GetOrderBookSnapshot возвращает снимок стакана (depth20 или depth100).
// VERIFIED: GET /api/v1/level2/depth20?symbol= (depth=20)
// TODO:VERIFY: для depth>20 использовать /api/v1/level2/depth100?symbol=
func (a *Adapter) GetOrderBookSnapshot(ctx context.Context, symbol domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error) {
	path := "/api/v1/level2/depth20"
	if depth > 20 {
		path = "/api/v1/level2/depth100"
	}
	query := BuildSortedQuery(map[string]string{"symbol": string(symbol)})
	env, err := a.doPublicGET(ctx, path, query)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("kucoin GetOrderBookSnapshot %s: %w", symbol, err)
	}
	var data depth20Data
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("kucoin GetOrderBookSnapshot %s: parse: %w", symbol, err)
	}

	bids, err := parsePriceLevels(data.Bids)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("kucoin GetOrderBookSnapshot: bids: %w", err)
	}
	asks, err := parsePriceLevels(data.Asks)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("kucoin GetOrderBookSnapshot: asks: %w", err)
	}

	var ts time.Time
	if tsMs, err2 := data.Ts.Int64(); err2 == nil {
		ts = time.UnixMilli(tsMs).UTC()
	}
	var seq int64
	if seqV, err2 := data.Sequence.Int64(); err2 == nil {
		seq = seqV
	}

	return domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeKuCoin,
		Symbol:     symbol,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  ts,
		Sequence:   seq,
		IsSnapshot: true,
	}, nil
}

// parsePriceLevels разбирает [[price, qty], ...] в []domain.PriceLevel.
func parsePriceLevels(raw [][]string) ([]domain.PriceLevel, error) {
	levels := make([]domain.PriceLevel, 0, len(raw))
	for _, pair := range raw {
		if len(pair) < 2 {
			continue
		}
		price, err := decimal.FromString(pair[0])
		if err != nil {
			return nil, err
		}
		qty, err := decimal.FromString(pair[1])
		if err != nil {
			return nil, err
		}
		levels = append(levels, domain.PriceLevel{Price: price, Qty: qty})
	}
	return levels, nil
}

// ============================================================
// WS — реализовано в ws.go
// ============================================================

// errWSNotImplemented — WS-функциональность не реализована (используется заглушкой SubscribePrivate).
var errWSNotImplemented = errors.New("kucoin: WebSocket not implemented")

// ============================================================
// GetBalances — VERIFIED
// ============================================================

// accountOverviewData — ответ GET /api/v1/account-overview?currency=USDT.
// VERIFIED: accountEquity, availableBalance как числа.
type accountOverviewData struct {
	AccountEquity    json.Number `json:"accountEquity"`    // total equity
	AvailableBalance json.Number `json:"availableBalance"` // available margin
	Currency         string      `json:"currency"`
}

// GetBalances возвращает баланс futures-аккаунта (USDT).
// VERIFIED: GET /api/v1/account-overview?currency=USDT
func (a *Adapter) GetBalances(ctx context.Context) ([]domain.Balance, error) {
	query := BuildSortedQuery(map[string]string{"currency": "USDT"})
	env, err := a.doSignedGET(ctx, "/api/v1/account-overview", query)
	if err != nil {
		return nil, fmt.Errorf("kucoin GetBalances: %w", err)
	}
	var data accountOverviewData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("kucoin GetBalances: parse: %w", err)
	}

	wallet, err := parseJSONNumber(data.AccountEquity)
	if err != nil {
		return nil, fmt.Errorf("kucoin GetBalances: accountEquity: %w", err)
	}
	avail, err := parseJSONNumber(data.AvailableBalance)
	if err != nil {
		avail = decimal.Zero
	}

	currency := data.Currency
	if currency == "" {
		currency = "USDT"
	}
	return []domain.Balance{
		{
			Asset:            currency,
			WalletBalance:    wallet,
			AvailableBalance: avail,
		},
	}, nil
}

// ============================================================
// GetPositions — VERIFIED
// ============================================================

// positionEntry — один элемент из GET /api/v1/positions.
// VERIFIED: currentQty (знаковое, в лотах), avgEntryPrice, markPrice,
// liquidationPrice, unrealisedPnl, maintMarginReq, leverage, marginMode.
type positionEntry struct {
	Symbol           string      `json:"symbol"`
	CurrentQty       json.Number `json:"currentQty"` // знаковое, в лотах
	AvgEntryPrice    json.Number `json:"avgEntryPrice"`
	MarkPrice        json.Number `json:"markPrice"`
	LiquidationPrice json.Number `json:"liquidationPrice"` // TODO:VERIFY поле
	UnrealisedPnl    json.Number `json:"unrealisedPnl"`
	MaintMarginReq   json.Number `json:"maintMarginReq"` // maintenance margin
	Leverage         json.Number `json:"realLeverage"`   // реальное плечо
	IsOpen           bool        `json:"isOpen"`
	CrossMode        bool        `json:"crossMode"` // true = cross
	SettleCurrency   string      `json:"settleCurrency"`
	PosMargin        json.Number `json:"posMargin"` // margin
}

// GetPositions возвращает все открытые позиции.
// VERIFIED: GET /api/v1/positions
// currentQty знаковое: >0 = long, <0 = short.
// BaseQty = abs(currentQty) * multiplier.
func (a *Adapter) GetPositions(ctx context.Context) ([]domain.Position, error) {
	env, err := a.doSignedGET(ctx, "/api/v1/positions", "")
	if err != nil {
		return nil, fmt.Errorf("kucoin GetPositions: %w", err)
	}

	var entries []positionEntry
	if err := json.Unmarshal(env.Data, &entries); err != nil {
		return nil, fmt.Errorf("kucoin GetPositions: parse: %w", err)
	}

	var positions []domain.Position
	for _, e := range entries {
		if !e.IsOpen {
			continue
		}
		qty, err := e.CurrentQty.Int64()
		if err != nil || qty == 0 {
			continue
		}
		pos, err := a.parsePosition(e)
		if err != nil {
			continue
		}
		positions = append(positions, pos)
	}
	return positions, nil
}

// parsePosition преобразует positionEntry в domain.Position.
func (a *Adapter) parsePosition(e positionEntry) (domain.Position, error) {
	qtyN, err := e.CurrentQty.Int64()
	if err != nil {
		return domain.Position{}, fmt.Errorf("currentQty: %w", err)
	}

	var side domain.Side
	if qtyN > 0 {
		side = domain.SideLong
	} else {
		side = domain.SideShort
	}

	sym := domain.ExchangeSymbol(e.Symbol)
	baseQty := a.lotsToBaseQty(sym, qtyN)
	contractQty := decimal.FromInt(qtyN).Abs()

	entryPrice, _ := parseJSONNumber(e.AvgEntryPrice)
	markPrice, _ := parseJSONNumber(e.MarkPrice)
	liqPrice, _ := parseJSONNumber(e.LiquidationPrice)
	pnl, _ := parseJSONNumber(e.UnrealisedPnl)
	lev, _ := parseJSONNumber(e.Leverage)
	margin, _ := parseJSONNumber(e.PosMargin)

	marginMode := domain.MarginIsolated
	if e.CrossMode {
		marginMode = domain.MarginCross
	}

	return domain.Position{
		Symbol:           sym,
		Side:             side,
		ContractQty:      contractQty,
		BaseQty:          baseQty,
		EntryPrice:       entryPrice,
		MarkPrice:        markPrice,
		LiquidationPrice: liqPrice,
		UnrealizedPnL:    pnl,
		MarginMode:       marginMode,
		Leverage:         lev,
		Margin:           margin,
		ADLQueue:         nil, // TODO:VERIFY: нет публичного ADL у KuCoin
		Updated:          a.clock(),
	}, nil
}

// ============================================================
// GetOpenOrders — VERIFIED
// ============================================================

// GetOpenOrders возвращает активные ордера по символу.
// VERIFIED: GET /api/v1/orders?status=active&symbol=
func (a *Adapter) GetOpenOrders(ctx context.Context, symbol domain.ExchangeSymbol) ([]domain.Order, error) {
	params := map[string]string{"status": "active"}
	if symbol != "" {
		params["symbol"] = string(symbol)
	}
	query := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/api/v1/orders", query)
	if err != nil {
		return nil, fmt.Errorf("kucoin GetOpenOrders: %w", err)
	}

	var result kcOrderListResult
	if err := json.Unmarshal(env.Data, &result); err != nil {
		return nil, fmt.Errorf("kucoin GetOpenOrders: parse: %w", err)
	}

	orders := make([]domain.Order, 0, len(result.Items))
	for _, e := range result.Items {
		ord, err := a.parseOrder(e)
		if err != nil {
			continue
		}
		orders = append(orders, ord)
	}
	return orders, nil
}

// kcOrderListResult — обёртка списка ордеров.
type kcOrderListResult struct {
	CurrentPage int          `json:"currentPage"`
	PageSize    int          `json:"pageSize"`
	TotalNum    int          `json:"totalNum"`
	TotalPage   int          `json:"totalPage"`
	Items       []orderEntry `json:"items"`
}

// orderEntry — поля одного ордера KuCoin Futures.
// VERIFIED: orderId, clientOid, symbol, side (buy/sell), type, size (лоты), price,
// filledSize, avgDealPrice, fee, status, reduceOnly, createdAt.
type orderEntry struct {
	OrderId      string      `json:"orderId"`
	ClientOid    string      `json:"clientOid"`
	Symbol       string      `json:"symbol"`
	Side         string      `json:"side"` // "buy" / "sell"
	OrderType    string      `json:"type"` // "limit" / "market"
	Size         json.Number `json:"size"` // в лотах
	Price        string      `json:"price"`
	FilledSize   json.Number `json:"filledSize"`   // заполнено (лоты)
	AvgDealPrice json.Number `json:"avgDealPrice"` // средняя цена исполнения
	Fee          json.Number `json:"fee"`
	Status       string      `json:"status"` // "open" / "done" / "match" / "cancel"
	ReduceOnly   bool        `json:"reduceOnly"`
	CreatedAt    json.Number `json:"createdAt"` // ms
}

// parseOrder преобразует orderEntry в domain.Order.
func (a *Adapter) parseOrder(e orderEntry) (domain.Order, error) {
	sizeLots, err := e.Size.Int64()
	if err != nil {
		return domain.Order{}, fmt.Errorf("size: %w", err)
	}
	sym := domain.ExchangeSymbol(e.Symbol)

	// Конвертируем лоты → базовую qty
	baseQty := a.lotsToBaseQty(sym, sizeLots)

	filledLots, _ := e.FilledSize.Int64()
	filledQty := a.lotsToBaseQty(sym, filledLots)

	avgPrice, _ := parseJSONNumber(e.AvgDealPrice)
	fee, _ := parseJSONNumber(e.Fee)

	var side domain.Side
	switch strings.ToLower(e.Side) {
	case "buy":
		side = domain.SideLong
	case "sell":
		side = domain.SideShort
	default:
		return domain.Order{}, fmt.Errorf("unknown side: %s", e.Side)
	}

	var orderMode domain.OrderMode
	switch strings.ToLower(e.OrderType) {
	case "market":
		orderMode = domain.OrderMarket
	default:
		orderMode = domain.OrderMarketableLimitIOC
	}

	status := parseOrderStatus(e.Status)

	var ts time.Time
	if msN, err2 := e.CreatedAt.Int64(); err2 == nil {
		ts = time.UnixMilli(msN).UTC()
	}

	return domain.Order{
		ExchangeOrderID:   e.OrderId,
		ClientOrderID:     domain.ClientOrderID(e.ClientOid),
		Symbol:            sym,
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        e.ReduceOnly,
		RequestedQty:      baseQty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPrice,
		Fees:              fee,
		Status:            status,
		ExchangeTimestamp: ts,
		AckState:          domain.AckStateQueried,
	}, nil
}

// parseOrderStatus маппирует строковый статус KuCoin → domain.OrderStatus.
// VERIFIED: "open" = acknowledged, "done" = filled, "cancel" = cancelled.
func parseOrderStatus(s string) domain.OrderStatus {
	switch strings.ToLower(s) {
	case "open", "new", "active":
		return domain.OrderStatusAcknowledged
	case "match":
		return domain.OrderStatusPartiallyFilled
	case "done":
		return domain.OrderStatusFilled
	case "cancel", "cancelled":
		return domain.OrderStatusCancelled
	default:
		return domain.OrderStatusNew
	}
}

// ============================================================
// SetLeverage — TODO:VERIFY
// ============================================================

// SetLeverage.
// TODO:VERIFY: KuCoin Futures leverage передаётся в теле POST /api/v1/orders (per-order).
// Если есть отдельный endpoint — /api/v2/position/changeLeverage TODO:VERIFY.
// Для now — no-op с комментарием.
func (a *Adapter) SetLeverage(_ context.Context, _ domain.SetLeverageRequest) error {
	// TODO:VERIFY: KuCoin Futures v1 передаёт leverage в каждом ордере.
	// Если нужен отдельный endpoint: POST /api/v2/position/changeLeverage (TODO:VERIFY).
	// На данный момент метод адаптера — no-op.
	return nil
}

// ============================================================
// SetMarginMode — TODO:VERIFY
// ============================================================

// SetMarginMode переключает режим маржи.
// TODO:VERIFY: endpoint и поля для переключения ISOLATED/CROSS у KuCoin Futures.
func (a *Adapter) SetMarginMode(_ context.Context, _ domain.SetMarginModeRequest) error {
	// TODO:VERIFY: KuCoin Futures margin mode может передаваться в ордере (marginMode: "ISOLATED"/"CROSS").
	// Отдельного per-symbol endpoint в публичной документации не обнаружено.
	return nil
}

// ============================================================
// SetPositionMode — no-op (KuCoin one-way по умолчанию)
// ============================================================

// SetPositionMode — no-op.
// KuCoin Futures по умолчанию работает в one-way mode. Hedge mode TODO:VERIFY появился.
func (a *Adapter) SetPositionMode(_ context.Context, _ domain.SetPositionModeRequest) error {
	// TODO:VERIFY: hedge mode у KuCoin Futures появился, но endpoint не верифицирован.
	return nil
}

// ============================================================
// PlaceOrder — VERIFIED
// ============================================================

// placeOrderResponse — ответ POST /api/v1/orders.
// VERIFIED: orderId, clientOid.
type placeOrderResponse struct {
	OrderId   string `json:"orderId"`
	ClientOid string `json:"clientOid"`
}

// PlaceOrder размещает ордер. Safe=false обязательно.
// VERIFIED: POST /api/v1/orders
// size — в ЛОТАХ (целое число). BaseQty → лоты через multiplier.
// leverage, marginMode, positionSide передаются в теле.
// clientOid: до 40 символов, разрешены цифры, буквы, '-', '_' — VERIFIED.
func (a *Adapter) PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error) {
	// side: Long → "buy", Short → "sell"
	side := "buy"
	if req.Side == domain.SideShort {
		side = "sell"
	}

	// Тип ордера
	orderType := "limit"
	if req.OrderMode == domain.OrderMarket {
		orderType = "market"
	}

	// timeInForce
	tif := "GTC"
	switch req.TimeInForce {
	case domain.TIFIOC:
		tif = "IOC"
	case domain.TIFFOK:
		tif = "FOK"
	default:
		if req.OrderMode == domain.OrderMarketableLimitIOC || req.OrderMode == domain.OrderMarket {
			tif = "IOC"
		}
	}

	// Конвертируем BaseQty → лоты
	lots, err := a.baseQtyToLots(req.Symbol, req.BaseQty)
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("kucoin PlaceOrder: %w", err)
	}

	body := map[string]interface{}{
		"clientOid":    string(req.ClientOrderID),
		"symbol":       string(req.Symbol),
		"side":         side,
		"type":         orderType,
		"size":         lots, // ЦЕЛОЕ число лотов
		"timeInForce":  tif,
		"reduceOnly":   req.ReduceOnly,
		"positionSide": "BOTH", // one-way mode: всегда BOTH
		"leverage":     3,      // TODO: параметризовать из req или SetLeverage
		"marginMode":   "ISOLATED",
	}

	if orderType == "limit" && !req.Price.IsZero() {
		body["price"] = req.Price.String()
	}

	env, err := a.doSignedPOST(ctx, "/api/v1/orders", body)
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("kucoin PlaceOrder: %w", err)
	}

	var res placeOrderResponse
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return domain.OrderAck{}, fmt.Errorf("kucoin PlaceOrder: parse: %w", err)
	}

	return domain.OrderAck{
		ExchangeOrderID: res.OrderId,
		ClientOrderID:   req.ClientOrderID,
		Status:          domain.OrderStatusAcknowledged,
		Timestamp:       a.clock(),
	}, nil
}

// ============================================================
// CancelOrder — VERIFIED
// ============================================================

// CancelOrder отменяет ордер.
// VERIFIED: DELETE /api/v1/orders/{orderId}  (по биржевому id)
// Если передан ClientOrderID без ExchangeOrderID — GET /api/v1/orders/byClientOid для резолва.
func (a *Adapter) CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error {
	orderID := req.ExchangeOrderID

	// Если нет биржевого ID — резолвим по clientOid
	if orderID == "" && req.ClientOrderID != "" {
		resolvedID, err := a.resolveOrderIDByClientOid(ctx, string(req.ClientOrderID))
		if err != nil {
			return fmt.Errorf("kucoin CancelOrder: resolve clientOid: %w", err)
		}
		orderID = resolvedID
	}

	if orderID == "" {
		return fmt.Errorf("kucoin CancelOrder: ни orderId, ни clientOid не указаны")
	}

	_, err := a.doSignedDELETE(ctx, "/api/v1/orders/"+orderID, "")
	if err != nil {
		if errors.Is(err, exchange.ErrOrderNotFound) {
			return err
		}
		return fmt.Errorf("kucoin CancelOrder: %w", err)
	}
	return nil
}

// resolveOrderIDByClientOid получает биржевой orderId через clientOid.
// VERIFIED: GET /api/v1/orders/byClientOid?clientOid=
func (a *Adapter) resolveOrderIDByClientOid(ctx context.Context, clientOid string) (string, error) {
	query := BuildSortedQuery(map[string]string{"clientOid": clientOid})
	env, err := a.doSignedGET(ctx, "/api/v1/orders/byClientOid", query)
	if err != nil {
		return "", err
	}
	var entry orderEntry
	if err := json.Unmarshal(env.Data, &entry); err != nil {
		return "", fmt.Errorf("parse byClientOid: %w", err)
	}
	if entry.OrderId == "" {
		return "", fmt.Errorf("%w: clientOid=%s", exchange.ErrOrderNotFound, clientOid)
	}
	return entry.OrderId, nil
}

// ============================================================
// GetOrder — VERIFIED
// ============================================================

// GetOrder возвращает состояние ордера.
// VERIFIED: GET /api/v1/orders/{orderId} (по биржевому id)
// VERIFIED: GET /api/v1/orders/byClientOid?clientOid= (по client id)
func (a *Adapter) GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error) {
	if req.ExchangeOrderID != "" {
		return a.getOrderByExchangeID(ctx, req.ExchangeOrderID)
	}
	if req.ClientOrderID != "" {
		return a.getOrderByClientOid(ctx, string(req.ClientOrderID))
	}
	return domain.Order{}, fmt.Errorf("kucoin GetOrder: ни orderId, ни clientOid не указаны")
}

func (a *Adapter) getOrderByExchangeID(ctx context.Context, orderID string) (domain.Order, error) {
	env, err := a.doSignedGET(ctx, "/api/v1/orders/"+orderID, "")
	if err != nil {
		if errors.Is(err, exchange.ErrOrderNotFound) {
			return domain.Order{}, err
		}
		return domain.Order{}, fmt.Errorf("kucoin GetOrder (by orderId): %w", err)
	}
	var entry orderEntry
	if err := json.Unmarshal(env.Data, &entry); err != nil {
		return domain.Order{}, fmt.Errorf("kucoin GetOrder: parse: %w", err)
	}
	return a.parseOrder(entry)
}

func (a *Adapter) getOrderByClientOid(ctx context.Context, clientOid string) (domain.Order, error) {
	query := BuildSortedQuery(map[string]string{"clientOid": clientOid})
	env, err := a.doSignedGET(ctx, "/api/v1/orders/byClientOid", query)
	if err != nil {
		if errors.Is(err, exchange.ErrOrderNotFound) {
			return domain.Order{}, err
		}
		return domain.Order{}, fmt.Errorf("kucoin GetOrder (by clientOid): %w", err)
	}
	var entry orderEntry
	if err := json.Unmarshal(env.Data, &entry); err != nil {
		return domain.Order{}, fmt.Errorf("kucoin GetOrder (by clientOid): parse: %w", err)
	}
	return a.parseOrder(entry)
}

// ============================================================
// GetADLState — нулевой (нет публичного API у KuCoin)
// ============================================================

// GetADLState — ADL нет в публичном API KuCoin Futures.
// TODO:VERIFY: возможно есть в positions WS (delevPercentage).
func (a *Adapter) GetADLState(_ context.Context, symbol domain.ExchangeSymbol) (domain.ADLState, error) {
	// TODO:VERIFY: delevPercentage в position WS (positions) может быть ADL-индикатором.
	return domain.ADLState{
		Symbol:     symbol,
		LongQueue:  decimal.Zero,
		ShortQueue: decimal.Zero,
		Timestamp:  a.clock(),
	}, nil
}

// ============================================================
// InternalTransfer — TODO:VERIFY
// ============================================================

// InternalTransfer переводит средства между futures и main аккаунтами.
// TODO:VERIFY: POST /api/v3/transfer-out (futures→main) и /api/v3/transfer-in (main→futures).
func (a *Adapter) InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error) {
	// TODO:VERIFY: структура запроса и путь.
	from := strings.ToLower(req.From)
	var path string
	if from == "futures" {
		path = "/api/v3/transfer-out"
	} else {
		path = "/api/v3/transfer-in"
	}
	body := map[string]interface{}{
		"currency":       req.Asset,
		"amount":         req.Amount.String(),
		"recAccountType": mapKuCoinAccountType(req.To),
	}
	env, err := a.doSignedPOST(ctx, path, body)
	if err != nil {
		return domain.TransferResult{}, fmt.Errorf("kucoin InternalTransfer: %w", err)
	}
	// TODO:VERIFY: поля ответа
	var res struct {
		ApplyId string `json:"applyId"`
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return domain.TransferResult{}, fmt.Errorf("kucoin InternalTransfer: parse: %w", err)
	}
	return domain.TransferResult{TransferID: res.ApplyId, Status: "submitted"}, nil
}

func mapKuCoinAccountType(t string) string {
	switch strings.ToLower(t) {
	case "spot", "main":
		return "MAIN"
	case "futures", "contract":
		return "FUTURES"
	case "trade", "trading":
		return "TRADE"
	default:
		return strings.ToUpper(t)
	}
}

// ============================================================
// Withdraw — TODO:VERIFY (spot API)
// ============================================================

// Withdraw создаёт заявку на вывод через spot API (api.kucoin.com).
// TODO:VERIFY: POST /api/v1/withdrawals (spot API, не futures).
func (a *Adapter) Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	body := map[string]interface{}{
		"currency": req.Asset,
		"address":  req.Address,
		"amount":   req.Amount.String(),
		"chain":    req.Network,
	}
	if req.Memo != "" {
		body["memo"] = req.Memo
	}
	env, err := a.doSignedSpotPOST(ctx, "/api/v1/withdrawals", body)
	if err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("kucoin Withdraw: %w", err)
	}
	var res struct {
		WithdrawalId string `json:"withdrawalId"`
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("kucoin Withdraw: parse: %w", err)
	}
	return domain.WithdrawalResult{WithdrawalID: res.WithdrawalId, Status: "submitted"}, nil
}

// ============================================================
// GetWithdrawalHistory — TODO:VERIFY (spot API)
// ============================================================

// withdrawHistoryItem — одна запись истории вывода.
type withdrawHistoryItem struct {
	Id         string      `json:"id"`
	Currency   string      `json:"currency"`
	Chain      string      `json:"chain"`
	Amount     json.Number `json:"amount"`
	Fee        json.Number `json:"fee"`
	Status     string      `json:"status"`
	CreatedAt  json.Number `json:"createdAt"` // ms
	WalletTxId string      `json:"walletTxId"`
}

// GetWithdrawalHistory возвращает историю выводов.
// TODO:VERIFY: GET /api/v1/withdrawals (spot API).
func (a *Adapter) GetWithdrawalHistory(ctx context.Context, q domain.TransferQuery) ([]domain.Withdrawal, error) {
	params := map[string]string{}
	if q.Asset != "" {
		params["currency"] = q.Asset
	}
	if q.Limit > 0 {
		params["pageSize"] = fmt.Sprintf("%d", q.Limit)
	}
	query := BuildSortedQuery(params)
	env, err := a.doSignedSpotGET(ctx, "/api/v1/withdrawals", query)
	if err != nil {
		return nil, fmt.Errorf("kucoin GetWithdrawalHistory: %w", err)
	}
	var result struct {
		Items []withdrawHistoryItem `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		return nil, fmt.Errorf("kucoin GetWithdrawalHistory: parse: %w", err)
	}

	var out []domain.Withdrawal
	for _, r := range result.Items {
		amount, _ := parseJSONNumber(r.Amount)
		fee, _ := parseJSONNumber(r.Fee)
		var ts time.Time
		if ms, err2 := r.CreatedAt.Int64(); err2 == nil {
			ts = time.UnixMilli(ms).UTC()
		}
		out = append(out, domain.Withdrawal{
			WithdrawalID: r.Id,
			TxID:         r.WalletTxId,
			Asset:        r.Currency,
			Network:      r.Chain,
			Amount:       amount,
			Fee:          fee,
			Status:       r.Status,
			RequestedAt:  ts,
		})
	}
	return out, nil
}

// ============================================================
// GetDepositHistory — TODO:VERIFY (spot API)
// ============================================================

// depositHistoryItem — одна запись депозита.
type depositHistoryItem struct {
	Currency   string      `json:"currency"`
	Chain      string      `json:"chain"`
	Amount     json.Number `json:"amount"`
	Status     string      `json:"status"`
	CreatedAt  json.Number `json:"createdAt"` // ms
	WalletTxId string      `json:"walletTxId"`
}

// GetDepositHistory возвращает историю депозитов.
// TODO:VERIFY: GET /api/v1/deposits (spot API).
func (a *Adapter) GetDepositHistory(ctx context.Context, q domain.TransferQuery) ([]domain.Deposit, error) {
	params := map[string]string{}
	if q.Asset != "" {
		params["currency"] = q.Asset
	}
	if q.Limit > 0 {
		params["pageSize"] = fmt.Sprintf("%d", q.Limit)
	}
	query := BuildSortedQuery(params)
	env, err := a.doSignedSpotGET(ctx, "/api/v1/deposits", query)
	if err != nil {
		return nil, fmt.Errorf("kucoin GetDepositHistory: %w", err)
	}
	var result struct {
		Items []depositHistoryItem `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		return nil, fmt.Errorf("kucoin GetDepositHistory: parse: %w", err)
	}

	var out []domain.Deposit
	for _, r := range result.Items {
		amount, _ := parseJSONNumber(r.Amount)
		var ts time.Time
		if ms, err2 := r.CreatedAt.Int64(); err2 == nil {
			ts = time.UnixMilli(ms).UTC()
		}
		out = append(out, domain.Deposit{
			TxID:        r.WalletTxId,
			Asset:       r.Currency,
			Network:     r.Chain,
			Amount:      amount,
			Status:      r.Status,
			ConfirmedAt: ts,
		})
	}
	return out, nil
}

// ============================================================
// GetNetworkInfo — TODO:VERIFY (spot API)
// ============================================================

// currencyData — ответ GET /api/v3/currencies/{currency} (spot API).
// TODO:VERIFY: поля chain, withdrawalMinFee, withdrawalMinSize, isWithdrawEnabled, isDepositEnabled.
type currencyData struct {
	Currency string       `json:"currency"`
	Chains   []chainEntry `json:"chains"`
}

type chainEntry struct {
	ChainName         string      `json:"chainName"`
	ChainId           string      `json:"chainId"`
	IsWithdrawEnabled bool        `json:"isWithdrawEnabled"`
	IsDepositEnabled  bool        `json:"isDepositEnabled"`
	WithdrawalMinFee  json.Number `json:"withdrawalMinFee"`
	WithdrawalMinSize json.Number `json:"withdrawalMinSize"`
	DepositMinSize    json.Number `json:"depositMinSize"`
}

// GetNetworkInfo возвращает информацию о сетях для актива.
// TODO:VERIFY: GET /api/v3/currencies/{currency} (spot API).
func (a *Adapter) GetNetworkInfo(ctx context.Context, asset string) ([]domain.NetworkInfo, error) {
	env, err := a.doSignedSpotGET(ctx, "/api/v3/currencies/"+asset, "")
	if err != nil {
		return nil, fmt.Errorf("kucoin GetNetworkInfo %s: %w", asset, err)
	}
	var data currencyData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("kucoin GetNetworkInfo %s: parse: %w", asset, err)
	}

	var out []domain.NetworkInfo
	for _, ch := range data.Chains {
		fee, _ := parseJSONNumber(ch.WithdrawalMinFee)
		wMin, _ := parseJSONNumber(ch.WithdrawalMinSize)
		dMin, _ := parseJSONNumber(ch.DepositMinSize)
		name := ch.ChainName
		if name == "" {
			name = ch.ChainId
		}
		out = append(out, domain.NetworkInfo{
			Network:         name,
			WithdrawEnabled: ch.IsWithdrawEnabled,
			DepositEnabled:  ch.IsDepositEnabled,
			WithdrawFee:     fee,
			WithdrawMin:     wMin,
			DepositMin:      dMin,
		})
	}
	return out, nil
}

// ============================================================
// Вспомогательные функции
// ============================================================

// parseDecimalOrZero парсит строку в Decimal; при пустой строке → Zero.
func parseDecimalOrZero(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, nil
	}
	return decimal.FromString(s)
}

// parseJSONNumber парсит json.Number в Decimal.
// KuCoin присылает числовые поля как JSON numbers (не строки).
func parseJSONNumber(n json.Number) (decimal.Decimal, error) {
	s := n.String()
	if s == "" || s == "0" {
		return decimal.Zero, nil
	}
	return decimal.FromString(s)
}
