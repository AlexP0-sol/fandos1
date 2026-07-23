// Package mexc реализует адаптер биржи MEXC для линейных USDT-perpetual фьючерсов
// через Contract API v1 (https://mexcdevelop.github.io/apidocs/contract_v1_en/).
//
// Envelope ответа:
//
//	{"success":true,"code":0,"data":...}
//
// success=false / code≠0 → ошибка; коды маппируются в sentinel-ошибки через mapAPIError.
//
// Особенность: торговый объём задаётся в контрактах (поле vol).
// Конверсия: vol = floor(baseQty / contractSize).
// contractSize кешируется при первом вызове GetInstruments.
//
// VERIFIED (официальная документация MEXC Contract API v1, official Go SDK mexcdevelop/mexc-api-demo):
//   - GET /api/v1/contract/ping
//   - GET /api/v1/contract/detail
//   - GET /api/v1/contract/funding_rate/{symbol}
//   - GET /api/v1/contract/ticker?symbol=
//   - GET /api/v1/contract/depth/{symbol}
//   - GET /api/v1/private/account/assets
//   - GET /api/v1/private/position/open_positions
//   - GET /api/v1/private/order/list/open_orders
//   - POST /api/v1/private/order/submit
//   - POST /api/v1/private/order/cancel
//   - GET /api/v1/private/order/external/{symbol}/{externalOid}
//   - POST /api/v1/private/position/change_leverage
//
// TODO:VERIFY:
//   - POST /api/v1/private/position/change_margin_type (margin mode)
//   - POST /api/v1/private/position/position_mode/change (position mode)
//   - POST /api/v1/private/order/cancel_with_external (cancel by externalOid)
//   - Spot API для переводов/выводов: https://api.mexc.com (SpotBaseURL в Config)
//   - Spot API auth для переводов использует тот же HMAC, но иной формат (TODO:VERIFY)
//   - ADL индикатор отсутствует в публичных endpoint-ах MEXC (TODO:VERIFY)
//
// Подпись: VERIFIED (official Go SDK signer.go):
//   - signature = hex(HMAC-SHA256(apiSecret, accessKey + requestTime + parameterString))
//   - GET: parameterString = sorted key=value&... (без URL-кодирования)
//   - POST: parameterString = raw JSON body
//   - Заголовки: ApiKey, Request-Time (мс), Signature, Content-Type: application/json
//
// Маппинг side (VERIFIED, официальная документация):
//
//	1 = open long  (SideLong,  ReduceOnly=false)
//	2 = close short (SideShort, ReduceOnly=true)
//	3 = open short (SideShort, ReduceOnly=false)
//	4 = close long  (SideLong,  ReduceOnly=true)
//
// Confidence policy для GetFunding:
//   - HIGH   — до nextSettleTime < 30 мин
//   - MEDIUM — до nextSettleTime < 4 ч
//   - LOW    — иначе
package mexc

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

// Config — параметры адаптера MEXC.
type Config struct {
	RESTBaseURL  string           // default: https://contract.mexc.com
	SpotBaseURL  string           // default: https://api.mexc.com (для переводов/выводов) TODO:VERIFY
	WSBaseURL    string           // default: wss://contract.mexc.com/edge
	APIKey       string           // обязателен для приватных запросов
	APISecret    string           // обязателен для приватных запросов
	Passphrase   string           // не используется MEXC; оставлен для совместимости интерфейса
	HTTPDoer     HTTPDoer         // обязателен
	RecvWindowMs int64            // не используется MEXC Contract API; оставлен для совместимости
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
	Query   string
	Body    io.Reader
	Headers map[string]string
	Safe    bool // true = только GET; false = мутирующий запрос (POST)
}

// Дефолтные URL-ы.
const (
	defaultRESTBase = "https://contract.mexc.com"
	defaultSpotBase = "https://api.mexc.com"
	defaultWSBase   = "wss://contract.mexc.com/edge"
)

// Adapter — реализация exchange.ExchangeAdapter для MEXC Contract API v1.
type Adapter struct {
	restBase string
	spotBase string
	wsBase   string
	signer   *Signer
	http     HTTPDoer
	clock    func() time.Time

	// Кеш инструментов: symbol → contractSize (USDT perpetual = размер контракта в base coin).
	// Используется для конверсии baseQty ↔ vol (contracts).
	mu              sync.RWMutex
	contractSizeMap map[domain.ExchangeSymbol]decimal.Decimal
}

// New создаёт адаптер MEXC из конфига. Возвращает ошибку при отсутствии обязательных полей.
func New(cfg Config) (*Adapter, error) {
	if cfg.HTTPDoer == nil {
		return nil, fmt.Errorf("mexc: HTTPDoer обязателен")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("mexc: APIKey обязателен")
	}
	if cfg.APISecret == "" {
		return nil, fmt.Errorf("mexc: APISecret обязателен")
	}
	a := &Adapter{
		restBase:        cfg.RESTBaseURL,
		spotBase:        cfg.SpotBaseURL,
		wsBase:          cfg.WSBaseURL,
		signer:          NewSigner(cfg.APIKey, []byte(cfg.APISecret)),
		http:            cfg.HTTPDoer,
		clock:           cfg.Clock,
		contractSizeMap: make(map[domain.ExchangeSymbol]decimal.Decimal),
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
func (a *Adapter) ID() domain.ExchangeID { return domain.ExchangeMEXC }

// ============================================================
// Envelope
// ============================================================

// mexcEnvelope — универсальная обёртка ответов MEXC Contract API v1.
// VERIFIED: {"success":bool,"code":int,"data":...,"message":string}.
type mexcEnvelope struct {
	Success bool            `json:"success"`
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// decodeEnvelope декодирует MEXC-конверт и проверяет success/code.
func decodeEnvelope(body []byte) (mexcEnvelope, error) {
	var env mexcEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("mexc: не удалось разобрать конверт: %w", err)
	}
	if !env.Success || env.Code != 0 {
		return env, mapAPIError(env.Code, env.Message)
	}
	return env, nil
}

// mapAPIError маппирует код ошибки MEXC в sentinel-ошибки.
//
// Коды взяты из официальной документации и общих паттернов:
// TODO:VERIFY: точные коды для rate limit, margin, invalid symbol.
func mapAPIError(code int64, msg string) error {
	switch code {
	case 401, 1002:
		// 1002 — типичный код неверной подписи/ключа в MEXC Contract API
		return fmt.Errorf("%w: code=%d %s", exchange.ErrUnauthorized, code, msg)
	case 429, 510:
		// 429 — HTTP rate limit; 510 — TODO:VERIFY внутренний код rate limit MEXC
		return fmt.Errorf("%w: code=%d %s", exchange.ErrRateLimited, code, msg)
	case 2011:
		// TODO:VERIFY: код "order not found"
		return fmt.Errorf("%w: code=%d %s", exchange.ErrOrderNotFound, code, msg)
	case 2013:
		// TODO:VERIFY: код "insufficient margin"
		return fmt.Errorf("%w: code=%d %s", exchange.ErrInsufficientMargin, code, msg)
	case 2001:
		// TODO:VERIFY: код "contract/symbol not found"
		return fmt.Errorf("%w: code=%d %s", exchange.ErrInvalidSymbol, code, msg)
	}
	// Дополнительные эвристики по тексту ошибки.
	msgLower := strings.ToLower(msg)
	switch {
	case strings.Contains(msgLower, "not found") && strings.Contains(msgLower, "order"):
		return fmt.Errorf("%w: code=%d %s", exchange.ErrOrderNotFound, code, msg)
	case strings.Contains(msgLower, "symbol") && (strings.Contains(msgLower, "not found") || strings.Contains(msgLower, "invalid")):
		return fmt.Errorf("%w: code=%d %s", exchange.ErrInvalidSymbol, code, msg)
	case strings.Contains(msgLower, "margin") && strings.Contains(msgLower, "insufficient"):
		return fmt.Errorf("%w: code=%d %s", exchange.ErrInsufficientMargin, code, msg)
	case strings.Contains(msgLower, "unauthorized") || strings.Contains(msgLower, "signature"):
		return fmt.Errorf("%w: code=%d %s", exchange.ErrUnauthorized, code, msg)
	}
	return fmt.Errorf("mexc: API error code=%d: %s", code, msg)
}

// ============================================================
// HTTP helpers
// ============================================================

// nowMs — текущее время в миллисекундах.
func (a *Adapter) nowMs() int64 { return a.clock().UnixMilli() }

// doPublicGET — GET без аутентификации.
func (a *Adapter) doPublicGET(ctx context.Context, path, query string) (mexcEnvelope, error) {
	_, body, err := a.http.Do(ctx, HTTPRequest{
		Method: http.MethodGet,
		Path:   path,
		Query:  query,
		Safe:   true,
	})
	if err != nil {
		return mexcEnvelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(body)
}

// doSignedGET — GET с аутентификацией.
func (a *Adapter) doSignedGET(ctx context.Context, path, query string) (mexcEnvelope, error) {
	tsStr, sig := a.signer.SignGET(a.nowMs(), query)
	headers := a.signer.AuthHeaders(tsStr, sig)

	_, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodGet,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    true,
	})
	if err != nil {
		return mexcEnvelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(body)
}

// doSignedPOST — POST с JSON-телом и аутентификацией. Safe=false.
func (a *Adapter) doSignedPOST(ctx context.Context, path string, payload interface{}) (mexcEnvelope, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return mexcEnvelope{}, fmt.Errorf("mexc: marshal: %w", err)
	}
	bodyStr := string(bodyBytes)

	tsStr, sig := a.signer.SignPOST(a.nowMs(), bodyStr)
	headers := a.signer.AuthHeaders(tsStr, sig)

	_, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Body:    bytes.NewReader(bodyBytes),
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return mexcEnvelope{}, wrapNetErr(err)
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
// Конверсия qty ↔ vol (контракты)
// ============================================================

// baseQtyToVol конвертирует базовый объём в количество контрактов (округление вниз).
//
// Конверсия: vol = floor(baseQty / contractSize).
// contractSize хранится в кеше contractSizeMap (заполняется через GetInstruments).
// Если contractSize не найден — возвращает ошибку, пока кеш не заполнен.
//
// Документация: MEXC использует поле vol для объёма в контрактах (lot),
// а contractSize из /api/v1/contract/detail задаёт размер одного контракта
// в базовой монете (например, BTC_USDT: contractSize=0.0001 BTC).
func (a *Adapter) baseQtyToVol(sym domain.ExchangeSymbol, baseQty decimal.Decimal) (int64, error) {
	a.mu.RLock()
	cs, ok := a.contractSizeMap[sym]
	a.mu.RUnlock()

	if !ok {
		return 0, fmt.Errorf("mexc: contractSize for %s not cached; call GetInstruments first", sym)
	}
	if cs.IsZero() {
		return 0, fmt.Errorf("mexc: contractSize for %s is zero", sym)
	}
	// floor(baseQty / contractSize) → количество контрактов.
	volDec, _ := baseQty.Quantize(cs)
	// vol = volDec / cs = количество шагов.
	volCount := volDec.Div(cs)
	return volCount.Underlying().IntPart(), nil
}

// volToBaseQty конвертирует количество контрактов обратно в базовый объём.
func (a *Adapter) volToBaseQty(sym domain.ExchangeSymbol, vol int64) (decimal.Decimal, bool) {
	a.mu.RLock()
	cs, ok := a.contractSizeMap[sym]
	a.mu.RUnlock()
	if !ok {
		return decimal.Zero, false
	}
	return decimal.FromInt(vol).Mul(cs), true
}

// ============================================================
// GetServerTime — VERIFIED
// ============================================================

// pingResponse — ответ /api/v1/contract/ping.
// VERIFIED: возвращает {"success":true,"code":0,"data":{"serverTime":1234567890}}.
// Фактически data — timestamp в мс.
type pingData struct {
	ServerTime int64 `json:"serverTime"`
}

// GetServerTime возвращает серверное время MEXC.
// VERIFIED: GET /api/v1/contract/ping.
func (a *Adapter) GetServerTime(ctx context.Context) (time.Time, error) {
	env, err := a.doPublicGET(ctx, "/api/v1/contract/ping", "")
	if err != nil {
		return time.Time{}, fmt.Errorf("mexc GetServerTime: %w", err)
	}
	var pd pingData
	if err := json.Unmarshal(env.Data, &pd); err != nil {
		// Некоторые реализации возвращают просто число, не объект.
		// Пробуем распарсить как int64.
		var ms int64
		if err2 := json.Unmarshal(env.Data, &ms); err2 != nil {
			return time.Time{}, fmt.Errorf("mexc GetServerTime: parse: %w", err)
		}
		return time.UnixMilli(ms).UTC(), nil
	}
	return time.UnixMilli(pd.ServerTime).UTC(), nil
}

// ============================================================
// GetInstruments — VERIFIED
// ============================================================

// contractDetailEntry — один контракт из /api/v1/contract/detail.
// VERIFIED: официальная документация MEXC Contract API v1.
type contractDetailEntry struct {
	Symbol       string      `json:"symbol"`
	ContractSize json.Number `json:"contractSize"` // размер одного контракта в base coin
	PriceUnit    json.Number `json:"priceUnit"`    // шаг цены (tickSize)
	VolUnit      json.Number `json:"volUnit"`      // шаг объёма в контрактах (qtyStep contracts)
	MinVol       json.Number `json:"minVol"`       // минимальный объём в контрактах
	MaxVol       json.Number `json:"maxVol"`
	SettleCoin   string      `json:"settleCoin"`
	BaseCoin     string      `json:"baseCoin"`
	QuoteCoin    string      `json:"quoteCoin"`
	MaxLeverage  json.Number `json:"maxLeverage"`
	State        int         `json:"state"` // 0=enabled, 1=delivery, 2=completed TODO:VERIFY
	APIAllowed   bool        `json:"apiAllowed"`
}

// GetInstruments возвращает все USDT-perpetual инструменты.
// Побочный эффект: заполняет кеш contractSizeMap.
// VERIFIED: GET /api/v1/contract/detail (без параметров возвращает все контракты).
func (a *Adapter) GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error) {
	env, err := a.doPublicGET(ctx, "/api/v1/contract/detail", "")
	if err != nil {
		return nil, fmt.Errorf("mexc GetInstruments: %w", err)
	}

	var entries []contractDetailEntry
	if err := json.Unmarshal(env.Data, &entries); err != nil {
		return nil, fmt.Errorf("mexc GetInstruments: parse: %w", err)
	}

	newCache := make(map[domain.ExchangeSymbol]decimal.Decimal, len(entries))
	var result []domain.CanonicalInstrument

	for _, e := range entries {
		// Фильтр: только USDT-settled и разрешён API.
		if !strings.EqualFold(e.SettleCoin, "USDT") {
			continue
		}
		// state=0 → активный. Остальные пропускаем для IsTradable.
		instr, cs, err := parseContractDetail(e)
		if err != nil {
			continue
		}
		sym := domain.ExchangeSymbol(e.Symbol)
		newCache[sym] = cs
		result = append(result, instr)
	}

	// Обновляем кеш атомарно.
	a.mu.Lock()
	a.contractSizeMap = newCache
	a.mu.Unlock()

	return result, nil
}

// parseContractDetail преобразует contractDetailEntry → (domain.CanonicalInstrument, contractSize).
func parseContractDetail(e contractDetailEntry) (domain.CanonicalInstrument, decimal.Decimal, error) {
	cs, err := decimal.FromString(e.ContractSize.String())
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("contractSize: %w", err)
	}
	tickSize, err := decimal.FromString(e.PriceUnit.String())
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("priceUnit: %w", err)
	}
	// volUnit — шаг объёма в контрактах. Переводим в base qty: volUnit × contractSize.
	volUnitDec, err := decimal.FromString(e.VolUnit.String())
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("volUnit: %w", err)
	}
	qtyStep := volUnitDec.Mul(cs) // шаг в базовых монетах

	minVol, err := decimal.FromString(e.MinVol.String())
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("minVol: %w", err)
	}
	minQty := minVol.Mul(cs) // минимальный объём в базовых монетах

	maxLev, err := parseDecimalOrZero(e.MaxLeverage.String())
	if err != nil {
		return domain.CanonicalInstrument{}, decimal.Zero, fmt.Errorf("maxLeverage: %w", err)
	}

	status := domain.InstrumentStatusActive
	if e.State != 0 {
		status = domain.InstrumentStatusDelisted
	}

	instr := domain.CanonicalInstrument{
		Exchange:           domain.ExchangeMEXC,
		CanonicalBaseAsset: domain.AssetSymbol(e.BaseCoin),
		ExchangeSymbol:     domain.ExchangeSymbol(e.Symbol),
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		SettlementCurrency: e.SettleCoin,
		ContractMultiplier: cs, // contractSize = мультипликатор контракта
		QtyStep:            qtyStep,
		MinQty:             minQty,
		TickSize:           tickSize,
		MaxLeverage:        maxLev,
		FundingIntervalSec: 8 * 3600, // TODO:VERIFY: MEXC использует 8h интервал по умолчанию
		FundingPriceType:   domain.FundingPriceMark,
		SupportsADL:        false, // TODO:VERIFY: нет публичного ADL индикатора у MEXC
		Status:             status,
	}
	return instr, cs, nil
}

// ============================================================
// GetFunding — VERIFIED
// ============================================================

// fundingRateData — ответ /api/v1/contract/funding_rate/{symbol}.
// VERIFIED: поля fundingRate, nextSettleTime.
type fundingRateData struct {
	Symbol         string      `json:"symbol"`
	FundingRate    json.Number `json:"fundingRate"`
	MaxFundingRate json.Number `json:"maxFundingRate"`
	MinFundingRate json.Number `json:"minFundingRate"`
	CollectCycle   int64       `json:"collectCycle"`   // интервал в часах TODO:VERIFY
	NextSettleTime int64       `json:"nextSettleTime"` // следующее время расчёта (мс)
	Timestamp      int64       `json:"timestamp"`
}

// GetFunding возвращает нормализованную funding-информацию.
// VERIFIED: GET /api/v1/contract/funding_rate/{symbol}.
//
// Confidence policy:
//   - HIGH   — до nextSettleTime < 30 мин
//   - MEDIUM — до nextSettleTime < 4 ч
//   - LOW    — иначе
func (a *Adapter) GetFunding(ctx context.Context, symbol domain.ExchangeSymbol) (domain.FundingInfo, error) {
	path := "/api/v1/contract/funding_rate/" + string(symbol)
	env, err := a.doPublicGET(ctx, path, "")
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("mexc GetFunding %s: %w", symbol, err)
	}

	var d fundingRateData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return domain.FundingInfo{}, fmt.Errorf("mexc GetFunding %s: parse: %w", symbol, err)
	}

	rate, err := decimal.FromString(d.FundingRate.String())
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("mexc GetFunding %s: fundingRate: %w", symbol, err)
	}

	nextFunding := time.UnixMilli(d.NextSettleTime).UTC()
	untilFunding := nextFunding.Sub(a.clock())

	var confidence domain.ConfidenceLevel
	switch {
	case untilFunding < 30*time.Minute:
		confidence = domain.ConfidenceHigh
	case untilFunding < 4*time.Hour:
		confidence = domain.ConfidenceMedium
	default:
		confidence = domain.ConfidenceLow
	}

	// collectCycle — интервал в часах (TODO:VERIFY: может быть 0 или в других единицах).
	fundingIntervalSec := int64(8 * 3600)
	if d.CollectCycle > 0 {
		fundingIntervalSec = d.CollectCycle * 3600
	}

	return domain.FundingInfo{
		ExchangeSymbol:       symbol,
		RealizedFundingRate:  rate,
		PredictedFundingRate: rate, // MEXC выдаёт текущий rate (realized=predicted)
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

// tickerData — ответ /api/v1/contract/ticker?symbol=.
// VERIFIED: поля lastPrice, bid1, ask1, volume24, fundingRate.
type tickerData struct {
	Symbol      string      `json:"symbol"`
	LastPrice   json.Number `json:"lastPrice"`
	Bid1        json.Number `json:"bid1"`
	Ask1        json.Number `json:"ask1"`
	Volume24    json.Number `json:"volume24"` // объём за 24ч в quote (USDT)
	FundingRate json.Number `json:"fundingRate"`
	Timestamp   int64       `json:"timestamp"`
}

// GetTicker возвращает нормализованный тикер (Level 2).
// VERIFIED: GET /api/v1/contract/ticker?symbol=.
func (a *Adapter) GetTicker(ctx context.Context, symbol domain.ExchangeSymbol) (domain.Ticker, error) {
	query := "symbol=" + string(symbol)
	env, err := a.doPublicGET(ctx, "/api/v1/contract/ticker", query)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("mexc GetTicker %s: %w", symbol, err)
	}

	var d tickerData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return domain.Ticker{}, fmt.Errorf("mexc GetTicker %s: parse: %w", symbol, err)
	}

	lastPrice, err := parseDecimalOrZero(d.LastPrice.String())
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("mexc GetTicker %s: lastPrice: %w", symbol, err)
	}
	volume, err := parseDecimalOrZero(d.Volume24.String())
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("mexc GetTicker %s: volume24: %w", symbol, err)
	}

	ts := a.clock()
	if d.Timestamp > 0 {
		ts = time.UnixMilli(d.Timestamp).UTC()
	}

	return domain.Ticker{
		Symbol:         symbol,
		LastPrice:      lastPrice,
		QuoteVolume24h: volume,
		Timestamp:      ts,
	}, nil
}

// ============================================================
// GetOrderBookSnapshot — VERIFIED
// ============================================================

// depthData — ответ /api/v1/contract/depth/{symbol}.
// VERIFIED: поля asks, bids как массивы [price, qty, ...].
type depthData struct {
	Asks      [][]json.Number `json:"asks"` // [[price, qty], ...]
	Bids      [][]json.Number `json:"bids"`
	Timestamp int64           `json:"timestamp"`
	Version   int64           `json:"version"` // sequence
}

// GetOrderBookSnapshot возвращает снимок стакана.
// VERIFIED: GET /api/v1/contract/depth/{symbol}.
func (a *Adapter) GetOrderBookSnapshot(ctx context.Context, symbol domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error) {
	path := "/api/v1/contract/depth/" + string(symbol)
	env, err := a.doPublicGET(ctx, path, "")
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("mexc GetOrderBookSnapshot %s: %w", symbol, err)
	}

	var d depthData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("mexc GetOrderBookSnapshot %s: parse: %w", symbol, err)
	}

	bids, err := parseDepthLevels(d.Bids, depth)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("mexc GetOrderBookSnapshot %s: bids: %w", symbol, err)
	}
	asks, err := parseDepthLevels(d.Asks, depth)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("mexc GetOrderBookSnapshot %s: asks: %w", symbol, err)
	}

	return domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeMEXC,
		Symbol:     symbol,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  time.UnixMilli(d.Timestamp).UTC(),
		Sequence:   d.Version,
		IsSnapshot: true,
	}, nil
}

// parseDepthLevels разбирает [[price, qty], ...] в []domain.PriceLevel.
func parseDepthLevels(raw [][]json.Number, maxDepth int) ([]domain.PriceLevel, error) {
	n := len(raw)
	if maxDepth > 0 && n > maxDepth {
		n = maxDepth
	}
	levels := make([]domain.PriceLevel, 0, n)
	for i := 0; i < n; i++ {
		pair := raw[i]
		if len(pair) < 2 {
			continue
		}
		price, err := decimal.FromString(pair[0].String())
		if err != nil {
			return nil, err
		}
		qty, err := decimal.FromString(pair[1].String())
		if err != nil {
			return nil, err
		}
		levels = append(levels, domain.PriceLevel{Price: price, Qty: qty})
	}
	return levels, nil
}

// ============================================================
// GetBalances — VERIFIED
// ============================================================

// assetEntry — один актив из /api/v1/private/account/assets.
// VERIFIED: поля currency, availableBalance, frozenBalance.
type assetEntry struct {
	Currency         string      `json:"currency"`
	PositionMargin   json.Number `json:"positionMargin"`
	FrozenBalance    json.Number `json:"frozenBalance"`
	AvailableBalance json.Number `json:"availableBalance"`
	CashBalance      json.Number `json:"cashBalance"`
	Equity           json.Number `json:"equity"`
}

// GetBalances возвращает балансы по всем активам фьючерсного аккаунта.
// VERIFIED: GET /api/v1/private/account/assets.
func (a *Adapter) GetBalances(ctx context.Context) ([]domain.Balance, error) {
	env, err := a.doSignedGET(ctx, "/api/v1/private/account/assets", "")
	if err != nil {
		return nil, fmt.Errorf("mexc GetBalances: %w", err)
	}

	var entries []assetEntry
	if err := json.Unmarshal(env.Data, &entries); err != nil {
		return nil, fmt.Errorf("mexc GetBalances: parse: %w", err)
	}

	balances := make([]domain.Balance, 0, len(entries))
	for _, e := range entries {
		equity, err := parseDecimalOrZero(e.Equity.String())
		if err != nil {
			continue
		}
		avail, err := parseDecimalOrZero(e.AvailableBalance.String())
		if err != nil {
			continue
		}
		balances = append(balances, domain.Balance{
			Asset:            e.Currency,
			WalletBalance:    equity,
			AvailableBalance: avail,
		})
	}
	return balances, nil
}

// ============================================================
// GetPositions — VERIFIED
// ============================================================

// positionEntry — одна позиция из /api/v1/private/position/open_positions.
// VERIFIED: поля symbol, positionType (1=long, 2=short), openVol, openAvgPrice, etc.
type positionEntry struct {
	PositionID     int64       `json:"positionId"`
	Symbol         string      `json:"symbol"`
	PositionType   int         `json:"positionType"` // 1=long, 2=short
	OpenVol        json.Number `json:"openVol"`      // открытый объём в контрактах
	OpenAvgPrice   json.Number `json:"openAvgPrice"`
	CloseAvgPrice  json.Number `json:"closeAvgPrice"`
	HoldVol        json.Number `json:"holdVol"`      // текущий удерживаемый объём
	HoldAvgPrice   json.Number `json:"holdAvgPrice"` // средняя цена
	FreezeVol      json.Number `json:"freezeVol"`
	CloseVol       json.Number `json:"closeVol"`
	LiquidatePrice json.Number `json:"liquidatePrice"`
	MarginType     int         `json:"marginType"` // 1=isolated, 2=cross
	Leverage       json.Number `json:"leverage"`
	Margin         json.Number `json:"im"` // initial margin
	UnrealizedPnL  json.Number `json:"unrealizedPnl"`
	UpdateTime     int64       `json:"updateTime"`
}

// GetPositions возвращает все открытые позиции.
// VERIFIED: GET /api/v1/private/position/open_positions.
func (a *Adapter) GetPositions(ctx context.Context) ([]domain.Position, error) {
	env, err := a.doSignedGET(ctx, "/api/v1/private/position/open_positions", "")
	if err != nil {
		return nil, fmt.Errorf("mexc GetPositions: %w", err)
	}

	var entries []positionEntry
	if err := json.Unmarshal(env.Data, &entries); err != nil {
		return nil, fmt.Errorf("mexc GetPositions: parse: %w", err)
	}

	positions := make([]domain.Position, 0, len(entries))
	for _, e := range entries {
		holdVol, err := decimal.FromString(e.HoldVol.String())
		if err != nil || holdVol.IsZero() {
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
	sym := domain.ExchangeSymbol(e.Symbol)

	holdVol, err := decimal.FromString(e.HoldVol.String())
	if err != nil {
		return domain.Position{}, fmt.Errorf("holdVol: %w", err)
	}
	entryPrice, err := parseDecimalOrZero(e.HoldAvgPrice.String())
	if err != nil {
		return domain.Position{}, fmt.Errorf("holdAvgPrice: %w", err)
	}
	liqPrice, err := parseDecimalOrZero(e.LiquidatePrice.String())
	if err != nil {
		return domain.Position{}, fmt.Errorf("liquidatePrice: %w", err)
	}
	pnl, err := parseDecimalOrZero(e.UnrealizedPnL.String())
	if err != nil {
		return domain.Position{}, fmt.Errorf("unrealizedPnl: %w", err)
	}
	leverage, err := parseDecimalOrZero(e.Leverage.String())
	if err != nil {
		return domain.Position{}, fmt.Errorf("leverage: %w", err)
	}
	margin, err := parseDecimalOrZero(e.Margin.String())
	if err != nil {
		return domain.Position{}, fmt.Errorf("im: %w", err)
	}

	// positionType: 1=long, 2=short.
	var side domain.Side
	switch e.PositionType {
	case 1:
		side = domain.SideLong
	case 2:
		side = domain.SideShort
	default:
		return domain.Position{}, fmt.Errorf("unknown positionType: %d", e.PositionType)
	}

	// marginType: 1=isolated, 2=cross.
	marginMode := domain.MarginCross
	if e.MarginType == 1 {
		marginMode = domain.MarginIsolated
	}

	// Конвертируем vol → baseQty.
	volInt := holdVol.Underlying().IntPart()
	baseQty, ok := a.volToBaseQty(sym, volInt)
	if !ok {
		// Кеш не заполнен — используем holdVol как contractQty.
		baseQty = holdVol
	}

	return domain.Position{
		Symbol:           sym,
		Side:             side,
		ContractQty:      holdVol,
		BaseQty:          baseQty,
		EntryPrice:       entryPrice,
		LiquidationPrice: liqPrice,
		UnrealizedPnL:    pnl,
		MarginMode:       marginMode,
		Leverage:         leverage,
		Margin:           margin,
		Updated:          time.UnixMilli(e.UpdateTime).UTC(),
	}, nil
}

// ============================================================
// GetOpenOrders — VERIFIED
// ============================================================

// openOrderEntry — один открытый ордер из /api/v1/private/order/list/open_orders.
// VERIFIED: поля orderId, symbol, side, price, vol, dealVol, orderType, etc.
type openOrderEntry struct {
	OrderID     int64       `json:"orderId"`
	Symbol      string      `json:"symbol"`
	ExternalOid string      `json:"externalOid"` // наш clientOrderID
	Side        int         `json:"side"`        // 1=open long, 2=close short, 3=open short, 4=close long
	Price       json.Number `json:"price"`
	Vol         json.Number `json:"vol"`     // запрошенный объём в контрактах
	DealVol     json.Number `json:"dealVol"` // исполненный объём в контрактах
	AvgPrice    json.Number `json:"dealAvgPrice"`
	OrderType   int         `json:"type"`  // 1=limit, 5=market
	State       int         `json:"state"` // 1=pending, 2=partial, 3=filled, 4=cancelled, etc.
	CreateTime  int64       `json:"createTime"`
}

// GetOpenOrders возвращает открытые ордера по символу.
// VERIFIED: GET /api/v1/private/order/list/open_orders.
func (a *Adapter) GetOpenOrders(ctx context.Context, symbol domain.ExchangeSymbol) ([]domain.Order, error) {
	params := map[string]string{"symbol": string(symbol)}
	query := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/api/v1/private/order/list/open_orders", query)
	if err != nil {
		return nil, fmt.Errorf("mexc GetOpenOrders %s: %w", symbol, err)
	}

	var result struct {
		ResultList []openOrderEntry `json:"resultList"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		// Попробуем парсить как прямой массив (API может возвращать оба формата).
		var list []openOrderEntry
		if err2 := json.Unmarshal(env.Data, &list); err2 != nil {
			return nil, fmt.Errorf("mexc GetOpenOrders %s: parse: %w", symbol, err)
		}
		result.ResultList = list
	}

	orders := make([]domain.Order, 0, len(result.ResultList))
	for _, e := range result.ResultList {
		ord, err := a.parseOpenOrder(e)
		if err != nil {
			continue
		}
		orders = append(orders, ord)
	}
	return orders, nil
}

// parseOpenOrder преобразует openOrderEntry в domain.Order.
func (a *Adapter) parseOpenOrder(e openOrderEntry) (domain.Order, error) {
	sym := domain.ExchangeSymbol(e.Symbol)
	vol, err := decimal.FromString(e.Vol.String())
	if err != nil {
		return domain.Order{}, fmt.Errorf("vol: %w", err)
	}
	dealVol, err := parseDecimalOrZero(e.DealVol.String())
	if err != nil {
		return domain.Order{}, fmt.Errorf("dealVol: %w", err)
	}
	avgPrice, err := parseDecimalOrZero(e.AvgPrice.String())
	if err != nil {
		return domain.Order{}, fmt.Errorf("dealAvgPrice: %w", err)
	}

	side, reduceOnly := parseSideCode(e.Side)
	status := parseMEXCOrderState(e.State)

	var orderMode domain.OrderMode
	if e.OrderType == 5 {
		orderMode = domain.OrderMarket
	} else {
		orderMode = domain.OrderMarketableLimitIOC
	}

	// Конвертируем vol → baseQty.
	volInt := vol.Underlying().IntPart()
	requestedQty, ok := a.volToBaseQty(sym, volInt)
	if !ok {
		requestedQty = vol
	}
	filledQty, ok2 := a.volToBaseQty(sym, dealVol.Underlying().IntPart())
	if !ok2 {
		filledQty = dealVol
	}

	return domain.Order{
		ExchangeOrderID:   fmt.Sprintf("%d", e.OrderID),
		ClientOrderID:     domain.ClientOrderID(e.ExternalOid),
		Symbol:            sym,
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        reduceOnly,
		RequestedQty:      requestedQty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPrice,
		Status:            status,
		ExchangeTimestamp: time.UnixMilli(e.CreateTime).UTC(),
		AckState:          domain.AckStateQueried,
	}, nil
}

// parseSideCode маппирует MEXC side code в (domain.Side, reduceOnly).
//
// VERIFIED (официальная документация MEXC Contract API):
//
//	1 = open long  → SideLong,  reduceOnly=false
//	2 = close short → SideShort, reduceOnly=true
//	3 = open short → SideShort, reduceOnly=false
//	4 = close long  → SideLong,  reduceOnly=true
func parseSideCode(code int) (domain.Side, bool) {
	switch code {
	case 1:
		return domain.SideLong, false
	case 2:
		return domain.SideShort, true
	case 3:
		return domain.SideShort, false
	case 4:
		return domain.SideLong, true
	default:
		return domain.SideLong, false
	}
}

// domainSideToCode маппирует domain.Side + reduceOnly → MEXC side code.
//
// VERIFIED (официальная документация MEXC Contract API):
//
//	SideLong  + reduceOnly=false → 1 (open long)
//	SideLong  + reduceOnly=true  → 4 (close long)
//	SideShort + reduceOnly=false → 3 (open short)
//	SideShort + reduceOnly=true  → 2 (close short)
func domainSideToCode(side domain.Side, reduceOnly bool) int {
	switch {
	case side == domain.SideLong && !reduceOnly:
		return 1
	case side == domain.SideLong && reduceOnly:
		return 4
	case side == domain.SideShort && !reduceOnly:
		return 3
	case side == domain.SideShort && reduceOnly:
		return 2
	default:
		return 1
	}
}

// parseMEXCOrderState маппирует числовой статус ордера MEXC → domain.OrderStatus.
// TODO:VERIFY: точные коды состояний из документации.
func parseMEXCOrderState(state int) domain.OrderStatus {
	switch state {
	case 1:
		return domain.OrderStatusAcknowledged // pending
	case 2:
		return domain.OrderStatusPartiallyFilled
	case 3:
		return domain.OrderStatusFilled
	case 4:
		return domain.OrderStatusCancelled
	case 5:
		return domain.OrderStatusCancelled
	default:
		return domain.OrderStatusNew
	}
}

// ============================================================
// PlaceOrder — VERIFIED (submit endpoint)
// ============================================================

// placeOrderRequest — тело запроса POST /api/v1/private/order/submit.
// VERIFIED: обязательные поля symbol, vol, side, type, openType, externalOid.
type placeOrderRequest struct {
	Symbol      string `json:"symbol"`
	Price       string `json:"price,omitempty"`
	Vol         int64  `json:"vol"`         // объём в контрактах
	Side        int    `json:"side"`        // 1=open long, 2=close short, 3=open short, 4=close long
	Type        int    `json:"type"`        // 1=limit, 5=market
	OpenType    int    `json:"openType"`    // 1=isolated, 2=cross
	ExternalOid string `json:"externalOid"` // наш clientOrderID [a-zA-Z0-9_-] ≤32 символов
}

// placeOrderResponse — ответ POST /api/v1/private/order/submit.
type placeOrderResponse struct {
	OrderID int64 `json:"orderId"`
}

// PlaceOrder размещает ордер. Safe=false обязательно.
// VERIFIED: POST /api/v1/private/order/submit.
//
// Конверсия: baseQty → vol = floor(baseQty / contractSize).
// Требует предварительного вызова GetInstruments для заполнения кеша contractSize.
func (a *Adapter) PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error) {
	sideCode := domainSideToCode(req.Side, req.ReduceOnly)

	orderType := 5 // market
	if req.OrderMode != domain.OrderMarket {
		orderType = 1 // limit
	}

	vol, err := a.baseQtyToVol(req.Symbol, req.BaseQty)
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("mexc PlaceOrder: baseQtyToVol: %w", err)
	}
	if vol <= 0 {
		return domain.OrderAck{}, fmt.Errorf("mexc PlaceOrder: vol=0 for baseQty=%s symbol=%s", req.BaseQty, req.Symbol)
	}

	body := placeOrderRequest{
		Symbol:      string(req.Symbol),
		Vol:         vol,
		Side:        sideCode,
		Type:        orderType,
		OpenType:    2, // cross margin (по умолчанию)
		ExternalOid: string(req.ClientOrderID),
	}
	if orderType == 1 && !req.Price.IsZero() {
		body.Price = req.Price.String()
	}

	env, err := a.doSignedPOST(ctx, "/api/v1/private/order/submit", body)
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("mexc PlaceOrder: %w", err)
	}

	var resp placeOrderResponse
	if err := json.Unmarshal(env.Data, &resp); err != nil {
		return domain.OrderAck{}, fmt.Errorf("mexc PlaceOrder: parse: %w", err)
	}

	return domain.OrderAck{
		ExchangeOrderID: fmt.Sprintf("%d", resp.OrderID),
		ClientOrderID:   req.ClientOrderID,
		Status:          domain.OrderStatusAcknowledged,
		Timestamp:       a.clock(),
	}, nil
}

// ============================================================
// CancelOrder — VERIFIED
// ============================================================

// CancelOrder отменяет ордер по ID.
// VERIFIED: POST /api/v1/private/order/cancel (тело: массив ID).
func (a *Adapter) CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error {
	if req.ExchangeOrderID == "" {
		return fmt.Errorf("mexc CancelOrder: ExchangeOrderID обязателен")
	}

	// Тело — массив числовых ID ордеров.
	// VERIFIED: официальный Go SDK использует JSON-массив для cancel.
	body := []string{req.ExchangeOrderID}

	_, err := a.doSignedPOST(ctx, "/api/v1/private/order/cancel", body)
	if errors.Is(err, exchange.ErrOrderNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("mexc CancelOrder: %w", err)
	}
	return nil
}

// ============================================================
// GetOrder — VERIFIED (by externalOid)
// ============================================================

// externalOrderData — ответ /api/v1/private/order/external/{symbol}/{externalOid}.
// VERIFIED: endpoint существует в официальном MEXC SDK.
type externalOrderData struct {
	OrderID     int64       `json:"orderId"`
	Symbol      string      `json:"symbol"`
	ExternalOid string      `json:"externalOid"`
	Side        int         `json:"side"`
	Price       json.Number `json:"price"`
	Vol         json.Number `json:"vol"`
	DealVol     json.Number `json:"dealVol"`
	AvgPrice    json.Number `json:"dealAvgPrice"`
	Type        int         `json:"type"`
	State       int         `json:"state"`
	CreateTime  int64       `json:"createTime"`
	Fee         json.Number `json:"closeFee"`
}

// GetOrder запрашивает состояние ордера по clientOrderID через external endpoint.
// VERIFIED: GET /api/v1/private/order/external/{symbol}/{externalOid}.
func (a *Adapter) GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error) {
	if req.ClientOrderID == "" {
		return domain.Order{}, fmt.Errorf("mexc GetOrder: ClientOrderID обязателен")
	}
	sym := req.Symbol
	if sym == "" {
		return domain.Order{}, fmt.Errorf("mexc GetOrder: Symbol обязателен")
	}

	path := "/api/v1/private/order/external/" + string(sym) + "/" + string(req.ClientOrderID)
	env, err := a.doSignedGET(ctx, path, "")
	if err != nil {
		if errors.Is(err, exchange.ErrOrderNotFound) {
			return domain.Order{}, fmt.Errorf("%w: clientOrderID=%s", exchange.ErrOrderNotFound, req.ClientOrderID)
		}
		return domain.Order{}, fmt.Errorf("mexc GetOrder: %w", err)
	}

	var d externalOrderData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return domain.Order{}, fmt.Errorf("mexc GetOrder: parse: %w", err)
	}

	// Если data == null (ордер не найден).
	if d.OrderID == 0 && d.ExternalOid == "" {
		return domain.Order{}, fmt.Errorf("%w: clientOrderID=%s", exchange.ErrOrderNotFound, req.ClientOrderID)
	}

	return a.parseExternalOrder(d)
}

// parseExternalOrder преобразует externalOrderData в domain.Order.
func (a *Adapter) parseExternalOrder(d externalOrderData) (domain.Order, error) {
	sym := domain.ExchangeSymbol(d.Symbol)

	vol, err := decimal.FromString(d.Vol.String())
	if err != nil {
		return domain.Order{}, fmt.Errorf("vol: %w", err)
	}
	dealVol, err := parseDecimalOrZero(d.DealVol.String())
	if err != nil {
		return domain.Order{}, fmt.Errorf("dealVol: %w", err)
	}
	avgPrice, err := parseDecimalOrZero(d.AvgPrice.String())
	if err != nil {
		return domain.Order{}, fmt.Errorf("dealAvgPrice: %w", err)
	}
	fee, err := parseDecimalOrZero(d.Fee.String())
	if err != nil {
		return domain.Order{}, fmt.Errorf("closeFee: %w", err)
	}

	side, reduceOnly := parseSideCode(d.Side)
	status := parseMEXCOrderState(d.State)

	var orderMode domain.OrderMode
	if d.Type == 5 {
		orderMode = domain.OrderMarket
	} else {
		orderMode = domain.OrderMarketableLimitIOC
	}

	volInt := vol.Underlying().IntPart()
	requestedQty, ok := a.volToBaseQty(sym, volInt)
	if !ok {
		requestedQty = vol
	}
	dealVolInt := dealVol.Underlying().IntPart()
	filledQty, ok2 := a.volToBaseQty(sym, dealVolInt)
	if !ok2 {
		filledQty = dealVol
	}

	return domain.Order{
		ExchangeOrderID:   fmt.Sprintf("%d", d.OrderID),
		ClientOrderID:     domain.ClientOrderID(d.ExternalOid),
		Symbol:            sym,
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        reduceOnly,
		RequestedQty:      requestedQty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPrice,
		Fees:              fee,
		Status:            status,
		ExchangeTimestamp: time.UnixMilli(d.CreateTime).UTC(),
		AckState:          domain.AckStateQueried,
	}, nil
}

// ============================================================
// SetLeverage — VERIFIED
// ============================================================

// SetLeverage устанавливает плечо.
// VERIFIED: POST /api/v1/private/position/change_leverage.
func (a *Adapter) SetLeverage(ctx context.Context, req domain.SetLeverageRequest) error {
	body := map[string]interface{}{
		"symbol":   string(req.Symbol),
		"leverage": req.Leverage.Underlying().IntPart(),
		"openType": 2, // cross (TODO:VERIFY — может потребоваться параметризация)
	}
	_, err := a.doSignedPOST(ctx, "/api/v1/private/position/change_leverage", body)
	if err != nil {
		return fmt.Errorf("mexc SetLeverage: %w", err)
	}
	return nil
}

// ============================================================
// SetMarginMode — TODO:VERIFY
// ============================================================

// SetMarginMode переключает режим маржи.
// TODO:VERIFY: endpoint и поля запроса.
func (a *Adapter) SetMarginMode(ctx context.Context, req domain.SetMarginModeRequest) error {
	// TODO:VERIFY endpoint для смены margin mode в MEXC Contract API.
	openType := 2 // cross
	if req.MarginMode == domain.MarginIsolated {
		openType = 1
	}
	body := map[string]interface{}{
		"symbol":   string(req.Symbol),
		"openType": openType,
	}
	_, err := a.doSignedPOST(ctx, "/api/v1/private/position/change_margin_type", body)
	if err != nil {
		return fmt.Errorf("mexc SetMarginMode: %w", err)
	}
	return nil
}

// ============================================================
// SetPositionMode — TODO:VERIFY
// ============================================================

// SetPositionMode переключает режим позиций (one-way/hedge).
// TODO:VERIFY: endpoint и поля. MEXC может не поддерживать hedge mode для USDT perpetual.
func (a *Adapter) SetPositionMode(ctx context.Context, req domain.SetPositionModeRequest) error {
	// TODO:VERIFY: MEXC contract API поддержка position mode.
	positionMode := 1 // one-way
	if req.Mode == domain.PositionHedge {
		positionMode = 2
	}
	body := map[string]interface{}{
		"positionMode": positionMode,
	}
	_, err := a.doSignedPOST(ctx, "/api/v1/private/position/position_mode/change", body)
	if err != nil {
		return fmt.Errorf("mexc SetPositionMode: %w", err)
	}
	return nil
}

// ============================================================
// GetADLState — TODO:VERIFY (нет публичного индикатора)
// ============================================================

// GetADLState возвращает ADL-состояние.
// TODO:VERIFY: MEXC не публикует ADL-индикатор в публичных endpoint-ах.
// Возвращает нулевое состояние с пометкой.
func (a *Adapter) GetADLState(_ context.Context, symbol domain.ExchangeSymbol) (domain.ADLState, error) {
	// TODO:VERIFY: нет публичного ADL индикатора у MEXC Contract API.
	// Возвращаем нулевые значения как документировано в контракте интерфейса.
	return domain.ADLState{
		Symbol:     symbol,
		LongQueue:  decimal.Zero,
		ShortQueue: decimal.Zero,
		Timestamp:  a.clock(),
	}, nil
}

// ============================================================
// WebSocket — TODO:VERIFY (типизированная заглушка)
// ============================================================

// errWSNotImplemented — WS не реализован в этой версии.
var errWSNotImplemented = errors.New("mexc: WebSocket не реализован; используйте REST polling")

// SubscribePublic — TODO:VERIFY WS endpoint wss://contract.mexc.com/edge.
func (a *Adapter) SubscribePublic(_ context.Context, _ []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	// TODO:VERIFY: WS base URL wss://contract.mexc.com/edge (подтверждено в docs 2024-01-31).
	// Каналы: sub.ticker, sub.depth (TODO:VERIFY формат сообщений).
	return nil, errWSNotImplemented
}

// SubscribePrivate — TODO:VERIFY приватные WS каналы MEXC.
func (a *Adapter) SubscribePrivate(_ context.Context, _ domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	// TODO:VERIFY: приватная WS аутентификация и каналы MEXC.
	return nil, errWSNotImplemented
}

// ============================================================
// Transfers — TODO:VERIFY (Spot API)
// ============================================================

// InternalTransfer — перевод между futures и spot.
// TODO:VERIFY: MEXC использует Spot API (api.mexc.com) для переводов.
// Отдельный SpotBaseURL в Config с дефолтом https://api.mexc.com.
func (a *Adapter) InternalTransfer(_ context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error) {
	// TODO:VERIFY: endpoint и параметры перевода через Spot API MEXC.
	// Предположительно POST https://api.mexc.com/api/v3/capital/transfer.
	// Формат подписи для Spot API может отличаться от Contract API (TODO:VERIFY).
	return domain.TransferResult{}, fmt.Errorf("mexc InternalTransfer: TODO:VERIFY (Spot API endpoint)")
}

// Withdraw — вывод средств.
// TODO:VERIFY: MEXC Spot API для вывода.
func (a *Adapter) Withdraw(_ context.Context, _ domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	return domain.WithdrawalResult{}, fmt.Errorf("mexc Withdraw: TODO:VERIFY (Spot API endpoint)")
}

// GetWithdrawalHistory — история выводов.
// TODO:VERIFY: MEXC Spot API для истории выводов.
func (a *Adapter) GetWithdrawalHistory(_ context.Context, _ domain.TransferQuery) ([]domain.Withdrawal, error) {
	return nil, fmt.Errorf("mexc GetWithdrawalHistory: TODO:VERIFY (Spot API endpoint)")
}

// GetDepositHistory — история депозитов.
// TODO:VERIFY: MEXC Spot API для истории депозитов.
func (a *Adapter) GetDepositHistory(_ context.Context, _ domain.TransferQuery) ([]domain.Deposit, error) {
	return nil, fmt.Errorf("mexc GetDepositHistory: TODO:VERIFY (Spot API endpoint)")
}

// GetNetworkInfo — информация о сетях.
// TODO:VERIFY: MEXC Spot API для информации о сетях.
func (a *Adapter) GetNetworkInfo(_ context.Context, _ string) ([]domain.NetworkInfo, error) {
	return nil, fmt.Errorf("mexc GetNetworkInfo: TODO:VERIFY (Spot API endpoint)")
}

// ============================================================
// Вспомогательные функции
// ============================================================

// parseDecimalOrZero парсит строку в Decimal; при пустой строке или "0" возвращает Zero.
func parseDecimalOrZero(s string) (decimal.Decimal, error) {
	if s == "" || s == "0" {
		return decimal.Zero, nil
	}
	return decimal.FromString(s)
}
