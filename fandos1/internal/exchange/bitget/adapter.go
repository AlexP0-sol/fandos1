// Package bitget реализует адаптер биржи Bitget V2 для USDT-M Perpetual Futures.
//
// REST envelope (VERIFIED по официальной документации):
//
//	{"code":"00000","msg":"success","data":...,"requestTime":...}
//
// code != "00000" → mapAPIError.
//
// Аутентификация (VERIFIED): заголовки ACCESS-KEY, ACCESS-SIGN, ACCESS-TIMESTAMP,
// ACCESS-PASSPHRASE, Content-Type: application/json, locale: en-US.
//
// VERIFIED endpoints:
//   - GET  /api/v2/public/time
//   - GET  /api/v2/mix/market/contracts?productType=USDT-FUTURES
//   - GET  /api/v2/mix/market/current-fund-rate?symbol=&productType=
//   - GET  /api/v2/mix/market/funding-time?symbol=&productType=
//   - GET  /api/v2/mix/market/ticker?symbol=&productType=
//   - GET  /api/v2/mix/market/merge-depth?symbol=&productType=&limit=
//   - GET  /api/v2/mix/account/accounts?productType=
//   - GET  /api/v2/mix/position/all-position?productType=
//   - GET  /api/v2/mix/order/orders-pending?productType=
//   - POST /api/v2/mix/order/place-order
//   - POST /api/v2/mix/order/cancel-order
//   - GET  /api/v2/mix/order/detail?symbol=&clientOid=
//   - POST /api/v2/mix/account/set-leverage
//   - POST /api/v2/mix/account/set-margin-mode
//   - POST /api/v2/mix/account/set-position-mode
//   - POST /api/v2/spot/wallet/transfer
//   - POST /api/v2/spot/wallet/withdrawal
//   - GET  /api/v2/spot/wallet/withdrawal-records
//   - GET  /api/v2/spot/wallet/deposit-records
//   - GET  /api/v2/spot/public/coins
//
// TODO:VERIFY endpoints (структура ответа не верифицирована по реальным данным):
//   - Точные коды ошибок: rate limit (429/"40429"), margin (40754), order not found (40109),
//     symbol not exist (40034), unauthorized (40001/40009)
//   - ADL fields в /api/v2/mix/position/all-position (поле не документировано явно)
//   - clientOid constraints для Bitget V2 Mix (проверить regex)
//   - Qty единицы: base coin vs contracts (документация говорит base coin для USDT-FUTURES)
//   - pricePlace/priceEndStep → tickSize mapping (vs volumePlace → qtyStep)
//
// Confidence policy для GetFunding:
//   - HIGH   — до nextFundingTime < 30 мин
//   - MEDIUM — до nextFundingTime < 4 ч
//   - LOW    — иначе
package bitget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// ============================================================
// Конфигурация и конструктор
// ============================================================

// Config — параметры адаптера Bitget.
type Config struct {
	RESTBaseURL  string           // default: https://api.bitget.com
	WSBaseURL    string           // default: wss://ws.bitget.com/v2/ws/public (TODO:VERIFY private URL)
	APIKey       string           // обязательно
	APISecret    string           // обязательно
	Passphrase   string           // ОБЯЗАТЕЛЬНО для приватных вызовов (VERIFIED)
	HTTPDoer     HTTPDoer         // обязательно
	RecvWindowMs int64            // default: 5000 (не используется Bitget, но резерв)
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
	Safe    bool // true=GET (идемпотентный), false=POST
}

// defaultRESTBase — production REST endpoint Bitget.
// VERIFIED: https://www.bitget.com/api-doc/classic/quickStart/intro
const defaultRESTBase = "https://api.bitget.com"

// defaultWSPublic — публичный WebSocket Bitget V2.
// VERIFIED: wss://ws.bitget.com/v2/ws/public
const defaultWSPublic = "wss://ws.bitget.com/v2/ws/public"

// defaultWSPrivate — приватный WebSocket Bitget V2.
// TODO:VERIFY: точный URL; документация указывает wss://ws.bitget.com/v2/ws/private
const defaultWSPrivate = "wss://ws.bitget.com/v2/ws/private"

// productType — тип продукта USDT-M Futures (VERIFIED).
const productType = "USDT-FUTURES"

// Adapter — реализация exchange.ExchangeAdapter для Bitget V2.
type Adapter struct {
	restBase string
	wsURL    string
	signer   *Signer
	http     HTTPDoer
	clock    func() time.Time
}

// New создаёт адаптер Bitget из конфига. Возвращает ошибку при обязательных полях == "".
func New(cfg Config) (*Adapter, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("bitget: APIKey обязателен")
	}
	if cfg.APISecret == "" {
		return nil, fmt.Errorf("bitget: APISecret обязателен")
	}
	if cfg.Passphrase == "" {
		return nil, fmt.Errorf("bitget: Passphrase обязательна для приватных вызовов")
	}
	if cfg.HTTPDoer == nil {
		return nil, fmt.Errorf("bitget: HTTPDoer обязателен")
	}

	restBase := cfg.RESTBaseURL
	if restBase == "" {
		restBase = defaultRESTBase
	}
	wsURL := cfg.WSBaseURL
	if wsURL == "" {
		wsURL = defaultWSPublic
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	signer := NewSigner(cfg.APIKey, []byte(cfg.APISecret), cfg.Passphrase)

	return &Adapter{
		restBase: restBase,
		wsURL:    wsURL,
		signer:   signer,
		http:     cfg.HTTPDoer,
		clock:    clock,
	}, nil
}

// ID возвращает идентификатор биржи.
func (a *Adapter) ID() domain.ExchangeID { return domain.ExchangeBitget }

// ============================================================
// Общий конверт Bitget V2
// ============================================================

// v2Envelope — универсальная обёртка ответов Bitget V2.
// VERIFIED: {"code":"00000","msg":"success","data":...,"requestTime":...}
type v2Envelope struct {
	Code        string          `json:"code"`
	Msg         string          `json:"msg"`
	Data        json.RawMessage `json:"data"`
	RequestTime int64           `json:"requestTime"`
}

// decodeEnvelope декодирует V2-конверт и проверяет code.
// При code != "00000" возвращает mapAPIError(code, msg).
func decodeEnvelope(body []byte) (v2Envelope, error) {
	var env v2Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("bitget: не удалось разобрать конверт: %w", err)
	}
	if env.Code != "00000" {
		return env, mapAPIError(env.Code, env.Msg)
	}
	return env, nil
}

// mapAPIError маппирует code Bitget V2 в sentinel-ошибки.
//
// Коды верифицированы частично (TODO:VERIFY точные коды):
//   - 429 HTTP status → ErrRateLimited (VERIFIED по HTTP docs)
//   - "40429" → ErrRateLimited (TODO:VERIFY: это строковый код rate limit в envelope)
//   - "40001"/"40009" → ErrUnauthorized (TODO:VERIFY: API key invalid / signature error)
//   - "40034" → ErrInvalidSymbol (TODO:VERIFY: param/symbol not exist)
//   - "40754" → ErrInsufficientMargin (TODO:VERIFY: insufficient margin code)
//   - "40109" → ErrOrderNotFound (TODO:VERIFY: order not found code)
func mapAPIError(code, msg string) error {
	switch code {
	case "429", "40429":
		// Rate limit exceeded (TODO:VERIFY: точный string code в envelope)
		return fmt.Errorf("%w: code=%s %s", exchange.ErrRateLimited, code, msg)
	case "40001", "40009":
		// Invalid API key / wrong signature (TODO:VERIFY)
		return fmt.Errorf("%w: code=%s %s", exchange.ErrUnauthorized, code, msg)
	case "40034":
		// Symbol not exist / invalid param (TODO:VERIFY)
		return fmt.Errorf("%w: code=%s %s", exchange.ErrInvalidSymbol, code, msg)
	case "40754":
		// Insufficient margin (TODO:VERIFY)
		return fmt.Errorf("%w: code=%s %s", exchange.ErrInsufficientMargin, code, msg)
	case "40109":
		// Order not found (TODO:VERIFY)
		return fmt.Errorf("%w: code=%s %s", exchange.ErrOrderNotFound, code, msg)
	default:
		// Проверяем по содержимому msg для потенциальных неотображённых кодов
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many") {
			return fmt.Errorf("%w: code=%s %s", exchange.ErrRateLimited, code, msg)
		}
		if strings.Contains(lower, "sign") || strings.Contains(lower, "api key") ||
			strings.Contains(lower, "unauthorized") || strings.Contains(lower, "passphrase") {
			return fmt.Errorf("%w: code=%s %s", exchange.ErrUnauthorized, code, msg)
		}
		if strings.Contains(lower, "symbol") || strings.Contains(lower, "invalid param") {
			return fmt.Errorf("%w: code=%s %s", exchange.ErrInvalidSymbol, code, msg)
		}
		if strings.Contains(lower, "margin") || strings.Contains(lower, "balance") {
			return fmt.Errorf("%w: code=%s %s", exchange.ErrInsufficientMargin, code, msg)
		}
		if strings.Contains(lower, "order not") || strings.Contains(lower, "not found") {
			return fmt.Errorf("%w: code=%s %s", exchange.ErrOrderNotFound, code, msg)
		}
		return fmt.Errorf("bitget: API error code=%s: %s", code, msg)
	}
}

// ============================================================
// Вспомогательные HTTP-функции
// ============================================================

// nowMs — текущее время в миллисекундах.
func (a *Adapter) nowMs() int64 { return a.clock().UnixMilli() }

// doPublicGET — GET без аутентификации (публичные endpoints).
func (a *Adapter) doPublicGET(ctx context.Context, path, query string) (v2Envelope, error) {
	_, body, err := a.http.Do(ctx, HTTPRequest{
		Method: http.MethodGet,
		Path:   path,
		Query:  query,
		Safe:   true,
	})
	if err != nil {
		return v2Envelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(body)
}

// doSignedGET — GET с аутентификацией через заголовки ACCESS-*.
func (a *Adapter) doSignedGET(ctx context.Context, path, query string) (v2Envelope, error) {
	ts := a.nowMs()
	_, sig := a.signer.SignGET(ts, path, query)
	headers := a.signer.AuthHeaders(ts, sig)

	_, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodGet,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    true,
	})
	if err != nil {
		return v2Envelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(body)
}

// doSignedPOST — POST с JSON-телом и аутентификацией. Safe=false.
func (a *Adapter) doSignedPOST(ctx context.Context, path string, payload interface{}) (v2Envelope, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return v2Envelope{}, fmt.Errorf("bitget: marshal: %w", err)
	}
	bodyStr := string(bodyBytes)

	ts := a.nowMs()
	_, sig := a.signer.SignPOST(ts, path, bodyStr)
	headers := a.signer.AuthHeaders(ts, sig)

	_, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Body:    bytes.NewReader(bodyBytes),
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return v2Envelope{}, wrapNetErr(err)
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

// serverTimeData — данные ответа /api/v2/public/time.
// VERIFIED: поле serverTime в ms (TODO:VERIFY точное имя поля).
type serverTimeData struct {
	ServerTime string `json:"serverTime"`
}

// GetServerTime возвращает серверное время Bitget.
// VERIFIED: GET /api/v2/public/time
func (a *Adapter) GetServerTime(ctx context.Context) (time.Time, error) {
	env, err := a.doPublicGET(ctx, "/api/v2/public/time", "")
	if err != nil {
		return time.Time{}, fmt.Errorf("bitget GetServerTime: %w", err)
	}
	var res serverTimeData
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return time.Time{}, fmt.Errorf("bitget GetServerTime: parse: %w", err)
	}
	ms, err := decimal.FromString(res.ServerTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("bitget GetServerTime: parse serverTime: %w", err)
	}
	return time.UnixMilli(ms.Underlying().IntPart()).UTC(), nil
}

// ============================================================
// GetInstruments — VERIFIED (endpoint + basic fields)
// ============================================================

// contractEntry — один инструмент из /api/v2/mix/market/contracts.
// VERIFIED: поля symbol, symbolStatus; TODO:VERIFY: pricePlace, priceEndStep, volumePlace.
type contractEntry struct {
	Symbol          string `json:"symbol"`
	BaseCoin        string `json:"baseCoin"`
	QuoteCoin       string `json:"quoteCoin"`
	SettleCoin      string `json:"settleCoin"`
	SymbolStatus    string `json:"symbolStatus"`    // "normal" = active (VERIFIED)
	MinTradeNum     string `json:"minTradeNum"`     // минимальный размер ордера (TODO:VERIFY base coin?)
	PricePlace      int    `json:"pricePlace"`      // количество знаков цены (TODO:VERIFY)
	PriceEndStep    int    `json:"priceEndStep"`    // шаг цены в единицах 10^(-pricePlace) (TODO:VERIFY)
	VolumePlace     int    `json:"volumePlace"`     // кол-во знаков объёма (qtyStep) (TODO:VERIFY)
	SizeMultiplier  string `json:"sizeMultiplier"`  // TODO:VERIFY
	MaxLeverageOver string `json:"maxLeverageOver"` // TODO:VERIFY поле maxLeverage
	FundingInterval string `json:"fundingInterval"` // TODO:VERIFY (в часах?)
}

// contractsListData — список контрактов.
type contractsListData struct {
	Contracts []contractEntry `json:"contracts"`
}

// GetInstruments возвращает все USDT-FUTURES инструменты.
// VERIFIED: GET /api/v2/mix/market/contracts?productType=USDT-FUTURES
func (a *Adapter) GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error) {
	query := BuildSortedQuery(map[string]string{"productType": productType})
	env, err := a.doPublicGET(ctx, "/api/v2/mix/market/contracts", query)
	if err != nil {
		return nil, fmt.Errorf("bitget GetInstruments: %w", err)
	}

	// Bitget возвращает данные как массив напрямую (не обёрнутый объект).
	// TODO:VERIFY: точная структура ответа data (массив или объект с полем contracts).
	var list []contractEntry
	if err := json.Unmarshal(env.Data, &list); err != nil {
		// Попробуем обёрнутый формат
		var wrapped contractsListData
		if err2 := json.Unmarshal(env.Data, &wrapped); err2 != nil {
			return nil, fmt.Errorf("bitget GetInstruments: parse: %w", err)
		}
		list = wrapped.Contracts
	}

	result := make([]domain.CanonicalInstrument, 0, len(list))
	for _, e := range list {
		if e.SymbolStatus != "normal" {
			continue
		}
		instr, err := parseContractEntry(e)
		if err != nil {
			continue // пропускаем некорректные инструменты
		}
		result = append(result, instr)
	}
	return result, nil
}

// parseContractEntry преобразует contractEntry в domain.CanonicalInstrument.
// TODO:VERIFY: точный маппинг pricePlace/priceEndStep → tickSize, volumePlace → qtyStep.
func parseContractEntry(e contractEntry) (domain.CanonicalInstrument, error) {
	// qtyStep = 10^(-volumePlace) (TODO:VERIFY)
	qtyStep := powTen(-e.VolumePlace)

	// minQty из minTradeNum
	minQty, err := parseDecimalOrZero(e.MinTradeNum)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("minTradeNum: %w", err)
	}
	if minQty.IsZero() && e.VolumePlace >= 0 {
		minQty = qtyStep
	}

	// tickSize = priceEndStep * 10^(-pricePlace) (TODO:VERIFY)
	tickSize := decimal.FromInt(int64(e.PriceEndStep)).Mul(powTen(-e.PricePlace))
	if tickSize.IsZero() {
		tickSize = powTen(-e.PricePlace)
	}

	// maxLeverage (TODO:VERIFY поле)
	maxLev, err := parseDecimalOrZero(e.MaxLeverageOver)
	if err != nil {
		maxLev = decimal.Zero
	}

	// fundingIntervalSec — TODO:VERIFY единицы (часы? минуты?)
	// Bitget USDT-M Futures стандарт: 8 часов
	fundingIntervalSec := int64(8 * 3600)
	if e.FundingInterval != "" {
		fi, err := decimal.FromString(e.FundingInterval)
		if err == nil && !fi.IsZero() {
			// Предполагаем часы (TODO:VERIFY)
			fundingIntervalSec = fi.Underlying().IntPart() * 3600
		}
	}

	status := domain.InstrumentStatusActive
	if e.SymbolStatus != "normal" {
		status = domain.InstrumentStatusDelisted
	}

	return domain.CanonicalInstrument{
		Exchange:           domain.ExchangeBitget,
		CanonicalBaseAsset: domain.AssetSymbol(e.BaseCoin),
		ExchangeSymbol:     domain.ExchangeSymbol(e.Symbol),
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		SettlementCurrency: e.SettleCoin,
		ContractMultiplier: decimal.One,
		QtyStep:            qtyStep,
		MinQty:             minQty,
		TickSize:           tickSize,
		MaxLeverage:        maxLev,
		FundingIntervalSec: fundingIntervalSec,
		FundingPriceType:   domain.FundingPriceMark,
		SupportsADL:        false, // TODO:VERIFY ADL support in Bitget
		Status:             status,
	}, nil
}

// powTen возвращает 10^exp как decimal.Decimal.
func powTen(exp int) decimal.Decimal {
	if exp >= 0 {
		v := decimal.One
		for i := 0; i < exp; i++ {
			v = v.Mul(decimal.FromInt(10))
		}
		return v
	}
	// Отрицательный показатель: 1/10^(-exp)
	s := "0."
	for i := 0; i < -exp-1; i++ {
		s += "0"
	}
	s += "1"
	d, _ := decimal.FromString(s)
	return d
}

// ============================================================
// GetFunding — VERIFIED (endpoints)
// ============================================================

// fundingRateData — поля текущего funding rate.
// VERIFIED: GET /api/v2/mix/market/current-fund-rate
type fundingRateData struct {
	Symbol      string `json:"symbol"`
	FundingRate string `json:"fundingRate"`
}

// fundingTimeData — время следующего funding.
// VERIFIED: GET /api/v2/mix/market/funding-time
type fundingTimeData struct {
	Symbol          string `json:"symbol"`
	NextFundingTime string `json:"nextFundingTime"` // ms UTC
	FundingTime     string `json:"fundingTime"`     // TODO:VERIFY поле
}

// GetFunding возвращает нормализованную funding-информацию.
// VERIFIED: GET /api/v2/mix/market/current-fund-rate + /api/v2/mix/market/funding-time
func (a *Adapter) GetFunding(ctx context.Context, symbol domain.ExchangeSymbol) (domain.FundingInfo, error) {
	symStr := string(symbol)

	// Запрашиваем текущий funding rate
	rateQuery := BuildSortedQuery(map[string]string{
		"symbol":      symStr,
		"productType": productType,
	})
	rateEnv, err := a.doPublicGET(ctx, "/api/v2/mix/market/current-fund-rate", rateQuery)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("bitget GetFunding rate: %w", err)
	}
	var rateData fundingRateData
	if err := json.Unmarshal(rateEnv.Data, &rateData); err != nil {
		return domain.FundingInfo{}, fmt.Errorf("bitget GetFunding rate: parse: %w", err)
	}

	rate, err := decimal.FromString(rateData.FundingRate)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("bitget GetFunding: fundingRate: %w", err)
	}

	// Запрашиваем время следующего funding
	timeQuery := BuildSortedQuery(map[string]string{
		"symbol":      symStr,
		"productType": productType,
	})
	timeEnv, err := a.doPublicGET(ctx, "/api/v2/mix/market/funding-time", timeQuery)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("bitget GetFunding time: %w", err)
	}
	var timeData fundingTimeData
	if err := json.Unmarshal(timeEnv.Data, &timeData); err != nil {
		return domain.FundingInfo{}, fmt.Errorf("bitget GetFunding time: parse: %w", err)
	}

	var nextFunding time.Time
	if timeData.NextFundingTime != "" {
		ms, err := decimal.FromString(timeData.NextFundingTime)
		if err == nil {
			nextFunding = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

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
		RealizedFundingRate:  rate,
		PredictedFundingRate: rate, // Bitget V2 даёт только текущий rate
		RateType:             domain.FundingRatePredicted,
		FundingIntervalSec:   8 * 3600, // по умолчанию 8h; TODO: брать из GetInstruments
		NextFundingTime:      nextFunding,
		Confidence:           confidence,
		FundingPriceType:     domain.FundingPriceMark,
	}, nil
}

// ============================================================
// GetTicker — VERIFIED
// ============================================================

// tickerData — поля тикера Bitget V2.
// VERIFIED: GET /api/v2/mix/market/ticker
type tickerData struct {
	Symbol     string `json:"symbol"`
	LastPr     string `json:"lastPr"`
	BidPr      string `json:"bidPr"`
	AskPr      string `json:"askPr"`
	MarkPrice  string `json:"markPrice"`
	IndexPrice string `json:"indexPrice"`
	UsdtVolume string `json:"usdtVolume"` // TODO:VERIFY точное поле объёма
}

// GetTicker возвращает нормализованный тикер.
// VERIFIED: GET /api/v2/mix/market/ticker?symbol=&productType=
func (a *Adapter) GetTicker(ctx context.Context, symbol domain.ExchangeSymbol) (domain.Ticker, error) {
	query := BuildSortedQuery(map[string]string{
		"symbol":      string(symbol),
		"productType": productType,
	})
	env, err := a.doPublicGET(ctx, "/api/v2/mix/market/ticker", query)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bitget GetTicker: %w", err)
	}
	var data tickerData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.Ticker{}, fmt.Errorf("bitget GetTicker: parse: %w", err)
	}

	lastPrice, err := parseDecimalOrZero(data.LastPr)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bitget GetTicker: lastPr: %w", err)
	}
	markPrice, err := parseDecimalOrZero(data.MarkPrice)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bitget GetTicker: markPrice: %w", err)
	}
	indexPrice, err := parseDecimalOrZero(data.IndexPrice)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bitget GetTicker: indexPrice: %w", err)
	}
	volume, err := parseDecimalOrZero(data.UsdtVolume)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bitget GetTicker: usdtVolume: %w", err)
	}

	return domain.Ticker{
		Symbol:         symbol,
		LastPrice:      lastPrice,
		MarkPrice:      markPrice,
		IndexPrice:     indexPrice,
		QuoteVolume24h: volume,
		Timestamp:      a.clock(),
	}, nil
}

// ============================================================
// GetOrderBookSnapshot — VERIFIED
// ============================================================

// mergeDepthData — данные стакана из merge-depth.
// VERIFIED: GET /api/v2/mix/market/merge-depth
type mergeDepthData struct {
	Ts   string     `json:"ts"`
	Bids [][]string `json:"bids"` // [[price, qty], ...]
	Asks [][]string `json:"asks"`
}

// GetOrderBookSnapshot возвращает снимок стакана.
// VERIFIED: GET /api/v2/mix/market/merge-depth?symbol=&productType=&limit=
func (a *Adapter) GetOrderBookSnapshot(ctx context.Context, symbol domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error) {
	query := BuildSortedQuery(map[string]string{
		"symbol":      string(symbol),
		"productType": productType,
		"limit":       fmt.Sprintf("%d", depth),
	})
	env, err := a.doPublicGET(ctx, "/api/v2/mix/market/merge-depth", query)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("bitget GetOrderBookSnapshot: %w", err)
	}
	var data mergeDepthData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("bitget GetOrderBookSnapshot: parse: %w", err)
	}

	bids, err := parsePriceLevels(data.Bids)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("bitget GetOrderBookSnapshot: bids: %w", err)
	}
	asks, err := parsePriceLevels(data.Asks)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("bitget GetOrderBookSnapshot: asks: %w", err)
	}

	ts := a.clock()
	if data.Ts != "" {
		ms, err := decimal.FromString(data.Ts)
		if err == nil {
			ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	return domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeBitget,
		Symbol:     symbol,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  ts,
		Sequence:   0, // TODO:VERIFY Bitget sequence field
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
// GetBalances — VERIFIED (endpoint)
// ============================================================

// accountEntry — запись аккаунта из /api/v2/mix/account/accounts.
// VERIFIED: GET /api/v2/mix/account/accounts?productType=USDT-FUTURES
// TODO:VERIFY: точные поля available и equity
type accountEntry struct {
	MarginCoin string `json:"marginCoin"`
	Available  string `json:"available"` // доступный баланс
	Equity     string `json:"equity"`    // общий equity
}

// GetBalances возвращает балансы аккаунта USDT-FUTURES.
// VERIFIED: GET /api/v2/mix/account/accounts?productType=USDT-FUTURES
func (a *Adapter) GetBalances(ctx context.Context) ([]domain.Balance, error) {
	query := BuildSortedQuery(map[string]string{"productType": productType})
	env, err := a.doSignedGET(ctx, "/api/v2/mix/account/accounts", query)
	if err != nil {
		return nil, fmt.Errorf("bitget GetBalances: %w", err)
	}

	var list []accountEntry
	if err := json.Unmarshal(env.Data, &list); err != nil {
		return nil, fmt.Errorf("bitget GetBalances: parse: %w", err)
	}

	balances := make([]domain.Balance, 0, len(list))
	for _, acc := range list {
		available, err := parseDecimalOrZero(acc.Available)
		if err != nil {
			continue
		}
		equity, err := parseDecimalOrZero(acc.Equity)
		if err != nil {
			continue
		}
		balances = append(balances, domain.Balance{
			Asset:            acc.MarginCoin,
			WalletBalance:    equity,
			AvailableBalance: available,
		})
	}
	return balances, nil
}

// ============================================================
// GetPositions — VERIFIED (endpoint)
// ============================================================

// positionEntry — одна позиция из /api/v2/mix/position/all-position.
// VERIFIED: endpoint; TODO:VERIFY: точные поля (особенно total vs size, adl).
type positionEntry struct {
	Symbol           string `json:"symbol"`
	HoldSide         string `json:"holdSide"`         // "long"/"short" (VERIFIED)
	Total            string `json:"total"`            // size (TODO:VERIFY: total или size?)
	OpenPriceAvg     string `json:"openPriceAvg"`     // entry price (VERIFIED)
	LiquidationPrice string `json:"liquidationPrice"` // (VERIFIED поле)
	MarginMode       string `json:"marginMode"`       // "crossed"/"isolated" (TODO:VERIFY)
	Leverage         string `json:"leverage"`         // TODO:VERIFY
	UnrealizedPL     string `json:"unrealizedPL"`     // TODO:VERIFY поле
	Margin           string `json:"margin"`           // TODO:VERIFY
	MarginRatio      string `json:"marginRatio"`      // TODO:VERIFY
	MarkPrice        string `json:"markPrice"`        // TODO:VERIFY
}

// GetPositions возвращает все открытые позиции.
// VERIFIED: GET /api/v2/mix/position/all-position?productType=USDT-FUTURES
func (a *Adapter) GetPositions(ctx context.Context) ([]domain.Position, error) {
	query := BuildSortedQuery(map[string]string{"productType": productType})
	env, err := a.doSignedGET(ctx, "/api/v2/mix/position/all-position", query)
	if err != nil {
		return nil, fmt.Errorf("bitget GetPositions: %w", err)
	}

	var list []positionEntry
	if err := json.Unmarshal(env.Data, &list); err != nil {
		return nil, fmt.Errorf("bitget GetPositions: parse: %w", err)
	}

	positions := make([]domain.Position, 0, len(list))
	for _, e := range list {
		qty, err := decimal.FromString(e.Total)
		if err != nil || qty.IsZero() {
			continue // пустые позиции пропускаем
		}
		pos, err := parsePosition(e)
		if err != nil {
			continue
		}
		positions = append(positions, pos)
	}
	return positions, nil
}

// parsePosition преобразует positionEntry в domain.Position.
func parsePosition(e positionEntry) (domain.Position, error) {
	qty, err := decimal.FromString(e.Total)
	if err != nil {
		return domain.Position{}, fmt.Errorf("total: %w", err)
	}
	entryPrice, err := parseDecimalOrZero(e.OpenPriceAvg)
	if err != nil {
		return domain.Position{}, fmt.Errorf("openPriceAvg: %w", err)
	}
	liqPrice, err := parseDecimalOrZero(e.LiquidationPrice)
	if err != nil {
		return domain.Position{}, fmt.Errorf("liquidationPrice: %w", err)
	}
	markPrice, err := parseDecimalOrZero(e.MarkPrice)
	if err != nil {
		return domain.Position{}, fmt.Errorf("markPrice: %w", err)
	}
	pnl, err := parseDecimalOrZero(e.UnrealizedPL)
	if err != nil {
		return domain.Position{}, fmt.Errorf("unrealizedPL: %w", err)
	}
	leverage, err := parseDecimalOrZero(e.Leverage)
	if err != nil {
		return domain.Position{}, fmt.Errorf("leverage: %w", err)
	}
	margin, err := parseDecimalOrZero(e.Margin)
	if err != nil {
		return domain.Position{}, fmt.Errorf("margin: %w", err)
	}

	var side domain.Side
	switch strings.ToLower(e.HoldSide) {
	case "long":
		side = domain.SideLong
	case "short":
		side = domain.SideShort
	default:
		return domain.Position{}, fmt.Errorf("unknown holdSide: %s", e.HoldSide)
	}

	marginMode := domain.MarginCross
	if strings.ToLower(e.MarginMode) == "isolated" {
		marginMode = domain.MarginIsolated
	}

	return domain.Position{
		Symbol:           domain.ExchangeSymbol(e.Symbol),
		Side:             side,
		ContractQty:      qty,
		BaseQty:          qty, // TODO:VERIFY: qty в base coin для USDT-FUTURES
		EntryPrice:       entryPrice,
		MarkPrice:        markPrice,
		LiquidationPrice: liqPrice,
		UnrealizedPnL:    pnl,
		MarginMode:       marginMode,
		Leverage:         leverage,
		Margin:           margin,
		ADLQueue:         nil, // TODO:VERIFY: ADL field не найдено в документации
		Updated:          time.Time{},
	}, nil
}

// ============================================================
// GetOpenOrders — VERIFIED (endpoint)
// ============================================================

// orderEntry — запись ордера из /api/v2/mix/order/orders-pending.
// VERIFIED: endpoint; TODO:VERIFY: точные поля
type orderEntry struct {
	OrderId    string `json:"orderId"`
	ClientOid  string `json:"clientOid"`
	Symbol     string `json:"symbol"`
	Side       string `json:"side"`      // "buy"/"sell" (VERIFIED)
	TradeSide  string `json:"tradeSide"` // "open"/"close" in hedge mode (VERIFIED)
	OrderType  string `json:"orderType"` // "market"/"limit" (VERIFIED)
	Price      string `json:"price"`
	Size       string `json:"size"`       // base qty (TODO:VERIFY)
	BaseVolume string `json:"baseVolume"` // filled qty (TODO:VERIFY поле)
	FillPrice  string `json:"fillPrice"`  // avg fill price (TODO:VERIFY)
	Fee        string `json:"fee"`        // TODO:VERIFY
	Status     string `json:"status"`     // TODO:VERIFY возможные значения
	Utime      string `json:"uTime"`      // update time ms
	Ctime      string `json:"cTime"`      // create time ms
}

// GetOpenOrders возвращает открытые ордера по символу.
// VERIFIED: GET /api/v2/mix/order/orders-pending?productType=USDT-FUTURES
func (a *Adapter) GetOpenOrders(ctx context.Context, symbol domain.ExchangeSymbol) ([]domain.Order, error) {
	query := BuildSortedQuery(map[string]string{
		"productType": productType,
		"symbol":      string(symbol),
	})
	env, err := a.doSignedGET(ctx, "/api/v2/mix/order/orders-pending", query)
	if err != nil {
		return nil, fmt.Errorf("bitget GetOpenOrders: %w", err)
	}

	// TODO:VERIFY: структура ответа (массив или объект с полем orderList)
	var list []orderEntry
	if err := json.Unmarshal(env.Data, &list); err != nil {
		// Попробуем объект с полем orderList
		var wrapped struct {
			OrderList []orderEntry `json:"orderList"`
		}
		if err2 := json.Unmarshal(env.Data, &wrapped); err2 != nil {
			return nil, fmt.Errorf("bitget GetOpenOrders: parse: %w", err)
		}
		list = wrapped.OrderList
	}

	orders := make([]domain.Order, 0, len(list))
	for _, e := range list {
		ord, err := parseOrder(e)
		if err != nil {
			continue
		}
		orders = append(orders, ord)
	}
	return orders, nil
}

// ============================================================
// GetOrder — VERIFIED (endpoint)
// ============================================================

// GetOrder запрашивает состояние ордера по clientOid.
// VERIFIED: GET /api/v2/mix/order/detail?symbol=&clientOid=
func (a *Adapter) GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error) {
	params := map[string]string{
		"productType": productType,
		"symbol":      string(req.Symbol),
	}
	if req.ClientOrderID != "" {
		params["clientOid"] = string(req.ClientOrderID)
	}
	if req.ExchangeOrderID != "" {
		params["orderId"] = req.ExchangeOrderID
	}
	query := BuildSortedQuery(params)

	env, err := a.doSignedGET(ctx, "/api/v2/mix/order/detail", query)
	if err != nil {
		if errors.Is(err, exchange.ErrOrderNotFound) {
			return domain.Order{}, err
		}
		return domain.Order{}, fmt.Errorf("bitget GetOrder: %w", err)
	}

	var e orderEntry
	if err := json.Unmarshal(env.Data, &e); err != nil {
		return domain.Order{}, fmt.Errorf("bitget GetOrder: parse: %w", err)
	}
	if e.OrderId == "" && e.ClientOid == "" {
		return domain.Order{}, fmt.Errorf("%w: clientOid=%s", exchange.ErrOrderNotFound, req.ClientOrderID)
	}

	return parseOrder(e)
}

// parseOrder преобразует orderEntry в domain.Order.
func parseOrder(e orderEntry) (domain.Order, error) {
	qty, err := parseDecimalOrZero(e.Size)
	if err != nil {
		return domain.Order{}, fmt.Errorf("size: %w", err)
	}
	filledQty, err := parseDecimalOrZero(e.BaseVolume)
	if err != nil {
		return domain.Order{}, fmt.Errorf("baseVolume: %w", err)
	}
	avgPrice, err := parseDecimalOrZero(e.FillPrice)
	if err != nil {
		return domain.Order{}, fmt.Errorf("fillPrice: %w", err)
	}
	fees, err := parseDecimalOrZero(e.Fee)
	if err != nil {
		return domain.Order{}, fmt.Errorf("fee: %w", err)
	}

	var side domain.Side
	switch strings.ToLower(e.Side) {
	case "buy":
		side = domain.SideLong
	case "sell":
		side = domain.SideShort
	default:
		return domain.Order{}, fmt.Errorf("unknown side: %s", e.Side)
	}

	// reduceOnly: tradeSide=="close" или если поле явно установлено
	reduceOnly := strings.ToLower(e.TradeSide) == "close"

	status := parseOrderStatus(e.Status)

	var orderMode domain.OrderMode
	switch strings.ToLower(e.OrderType) {
	case "market":
		orderMode = domain.OrderMarket
	default:
		orderMode = domain.OrderMarketableLimitIOC
	}

	ts := time.Time{}
	if e.Ctime != "" {
		ms, err := decimal.FromString(e.Ctime)
		if err == nil {
			ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	return domain.Order{
		ExchangeOrderID:   e.OrderId,
		ClientOrderID:     domain.ClientOrderID(e.ClientOid),
		Symbol:            domain.ExchangeSymbol(e.Symbol),
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        reduceOnly,
		RequestedQty:      qty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPrice,
		Fees:              fees,
		Status:            status,
		ExchangeTimestamp: ts,
		AckState:          domain.AckStateQueried,
	}, nil
}

// parseOrderStatus маппирует строковый статус Bitget → domain.OrderStatus.
// TODO:VERIFY: точные строки статусов Bitget V2
func parseOrderStatus(s string) domain.OrderStatus {
	switch strings.ToLower(s) {
	case "live", "new", "init":
		return domain.OrderStatusAcknowledged
	case "partially_fill":
		return domain.OrderStatusPartiallyFilled
	case "full_fill", "filled":
		return domain.OrderStatusFilled
	case "cancelled", "canceled":
		return domain.OrderStatusCancelled
	case "rejected":
		return domain.OrderStatusRejected
	default:
		return domain.OrderStatusNew
	}
}

// ============================================================
// SetLeverage — VERIFIED (endpoint)
// ============================================================

// SetLeverage устанавливает плечо для символа.
// VERIFIED: POST /api/v2/mix/account/set-leverage
func (a *Adapter) SetLeverage(ctx context.Context, req domain.SetLeverageRequest) error {
	body := map[string]interface{}{
		"symbol":      string(req.Symbol),
		"productType": productType,
		"marginCoin":  "USDT",
		"leverage":    req.Leverage.String(),
	}
	_, err := a.doSignedPOST(ctx, "/api/v2/mix/account/set-leverage", body)
	if err != nil {
		return fmt.Errorf("bitget SetLeverage: %w", err)
	}
	return nil
}

// ============================================================
// SetMarginMode — VERIFIED (endpoint)
// ============================================================

// SetMarginMode переключает режим маржи.
// VERIFIED: POST /api/v2/mix/account/set-margin-mode
func (a *Adapter) SetMarginMode(ctx context.Context, req domain.SetMarginModeRequest) error {
	marginMode := "crossed"
	if req.MarginMode == domain.MarginIsolated {
		marginMode = "isolated"
	}
	body := map[string]interface{}{
		"symbol":      string(req.Symbol),
		"productType": productType,
		"marginCoin":  "USDT",
		"marginMode":  marginMode,
	}
	_, err := a.doSignedPOST(ctx, "/api/v2/mix/account/set-margin-mode", body)
	if err != nil {
		return fmt.Errorf("bitget SetMarginMode: %w", err)
	}
	return nil
}

// ============================================================
// SetPositionMode — VERIFIED (endpoint)
// ============================================================

// SetPositionMode переключает режим позиций (one-way/hedge).
// VERIFIED: POST /api/v2/mix/account/set-position-mode
// posMode: "one_way_mode" | "hedge_mode" (VERIFIED поля)
func (a *Adapter) SetPositionMode(ctx context.Context, req domain.SetPositionModeRequest) error {
	posMode := "one_way_mode"
	if req.Mode == domain.PositionHedge {
		posMode = "hedge_mode"
	}
	body := map[string]interface{}{
		"productType": productType,
		"posMode":     posMode,
	}
	_, err := a.doSignedPOST(ctx, "/api/v2/mix/account/set-position-mode", body)
	if err != nil {
		return fmt.Errorf("bitget SetPositionMode: %w", err)
	}
	return nil
}

// ============================================================
// PlaceOrder — VERIFIED (endpoint + основные поля)
// ============================================================

// placeOrderResponse — поля result при создании ордера.
// VERIFIED: {"orderId":"...","clientOid":"..."}
type placeOrderResponse struct {
	OrderId   string `json:"orderId"`
	ClientOid string `json:"clientOid"`
}

// PlaceOrder размещает ордер. Safe=false обязательно.
// VERIFIED: POST /api/v2/mix/order/place-order
//
// Маппинг side (VERIFIED):
//   - Long open  → side=buy,  tradeSide=open
//   - Long close → side=buy,  tradeSide=close (hedge mode)
//   - Short open → side=sell, tradeSide=open
//   - Short close→ side=sell, tradeSide=close (hedge mode)
//
// Для one-way mode используем reduceOnly=YES/NO вместо tradeSide.
// TODO:VERIFY: это поведение для Bitget V2 Mix (hedge vs one-way).
func (a *Adapter) PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error) {
	side := "buy"
	if req.Side == domain.SideShort {
		side = "sell"
	}

	orderType := "limit"
	if req.OrderMode == domain.OrderMarket {
		orderType = "market"
	}

	// force (TimeInForce)
	force := "gtc"
	tif := string(req.TimeInForce)
	switch tif {
	case "IOC":
		force = "ioc"
	case "FOK":
		force = "fok"
	case "GTC":
		force = "gtc"
	default:
		if req.OrderMode == domain.OrderMarketableLimitIOC {
			force = "ioc"
		} else if req.OrderMode == domain.OrderMarket {
			force = "ioc"
		}
	}

	body := map[string]interface{}{
		"symbol":      string(req.Symbol),
		"productType": productType,
		"marginMode":  "crossed", // default cross; TODO: параметризировать
		"marginCoin":  "USDT",
		"size":        req.BaseQty.String(), // base coin qty (VERIFIED для USDT-FUTURES)
		"side":        side,
		"orderType":   orderType,
		"force":       force,
		"clientOid":   string(req.ClientOrderID),
	}

	// Цена для limit ордеров
	if orderType == "limit" && !req.Price.IsZero() {
		body["price"] = req.Price.String()
	}

	// tradeSide для hedge mode / reduceOnly для one-way
	if req.ReduceOnly {
		body["tradeSide"] = "close"
		body["reduceOnly"] = "YES"
	} else {
		body["tradeSide"] = "open"
		body["reduceOnly"] = "NO"
	}

	env, err := a.doSignedPOST(ctx, "/api/v2/mix/order/place-order", body)
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("bitget PlaceOrder: %w", err)
	}

	var res placeOrderResponse
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return domain.OrderAck{}, fmt.Errorf("bitget PlaceOrder: parse: %w", err)
	}

	return domain.OrderAck{
		ExchangeOrderID: res.OrderId,
		ClientOrderID:   req.ClientOrderID,
		Status:          domain.OrderStatusAcknowledged,
		Timestamp:       a.clock(),
	}, nil
}

// ============================================================
// CancelOrder — VERIFIED (endpoint)
// ============================================================

// CancelOrder отменяет ордер.
// VERIFIED: POST /api/v2/mix/order/cancel-order
// TODO:VERIFY: code для order not found при cancel
func (a *Adapter) CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error {
	body := map[string]interface{}{
		"symbol":      string(req.Symbol),
		"productType": productType,
		"marginCoin":  "USDT",
	}
	if req.ClientOrderID != "" {
		body["clientOid"] = string(req.ClientOrderID)
	}
	if req.ExchangeOrderID != "" {
		body["orderId"] = req.ExchangeOrderID
	}

	_, err := a.doSignedPOST(ctx, "/api/v2/mix/order/cancel-order", body)
	if errors.Is(err, exchange.ErrOrderNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("bitget CancelOrder: %w", err)
	}
	return nil
}

// ============================================================
// GetADLState — TODO:VERIFY (ADL field отсутствует явно в документации)
// ============================================================

// GetADLState возвращает ADL-состояние.
// TODO:VERIFY: Bitget не документирует явного ADL-поля в positions API.
// Возвращаем нулевое состояние с комментарием.
func (a *Adapter) GetADLState(ctx context.Context, symbol domain.ExchangeSymbol) (domain.ADLState, error) {
	// TODO:VERIFY: ADL field в /api/v2/mix/position/all-position не найдено в документации.
	// Возвращаем нулевое состояние.
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

// InternalTransfer — внутренний перевод между аккаунтами Bitget.
// VERIFIED endpoint: POST /api/v2/spot/wallet/transfer
// TODO:VERIFY: fromType/toType для cross-продуктов
func (a *Adapter) InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error) {
	body := map[string]interface{}{
		"fromType": mapBitgetAccountType(req.From),
		"toType":   mapBitgetAccountType(req.To),
		"amount":   req.Amount.String(),
		"coin":     req.Asset,
		"symbol":   "", // optional
	}
	env, err := a.doSignedPOST(ctx, "/api/v2/spot/wallet/transfer", body)
	if err != nil {
		return domain.TransferResult{}, fmt.Errorf("bitget InternalTransfer: %w", err)
	}
	var res struct {
		TransferID string `json:"transferId"`
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return domain.TransferResult{}, fmt.Errorf("bitget InternalTransfer: parse: %w", err)
	}
	return domain.TransferResult{TransferID: res.TransferID, Status: "submitted"}, nil
}

// mapBitgetAccountType маппирует доменное название аккаунта в Bitget V2.
func mapBitgetAccountType(t string) string {
	switch strings.ToLower(t) {
	case "spot":
		return "spot"
	case "futures", "usdt-futures", "contract":
		return "usdt-futures"
	default:
		return strings.ToLower(t)
	}
}

// ============================================================
// Withdraw — TODO:VERIFY
// ============================================================

// Withdraw создаёт заявку на вывод средств.
// VERIFIED endpoint: POST /api/v2/spot/wallet/withdrawal
// TODO:VERIFY: поля chainName, transferType
func (a *Adapter) Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	body := map[string]interface{}{
		"coin":         req.Asset,
		"address":      req.Address,
		"chain":        req.Network,
		"size":         req.Amount.String(),
		"transferType": "on_chain",
	}
	if req.Memo != "" {
		body["tag"] = req.Memo
	}
	env, err := a.doSignedPOST(ctx, "/api/v2/spot/wallet/withdrawal", body)
	if err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("bitget Withdraw: %w", err)
	}
	var res struct {
		OrderId string `json:"orderId"`
	}
	if err := json.Unmarshal(env.Data, &res); err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("bitget Withdraw: parse: %w", err)
	}
	return domain.WithdrawalResult{WithdrawalID: res.OrderId, Status: "submitted"}, nil
}

// ============================================================
// GetWithdrawalHistory — TODO:VERIFY
// ============================================================

// GetWithdrawalHistory возвращает историю выводов.
// VERIFIED endpoint: GET /api/v2/spot/wallet/withdrawal-records
// TODO:VERIFY: поля ответа
func (a *Adapter) GetWithdrawalHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Withdrawal, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["coin"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", query.Limit)
	}
	q := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/api/v2/spot/wallet/withdrawal-records", q)
	if err != nil {
		return nil, fmt.Errorf("bitget GetWithdrawalHistory: %w", err)
	}

	var list []struct {
		OrderId string `json:"orderId"`
		TxId    string `json:"txId"`
		Coin    string `json:"coin"`
		Chain   string `json:"chain"`
		Size    string `json:"size"`
		Fee     string `json:"fee"`
		Status  string `json:"status"`
		Ctime   string `json:"cTime"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil {
		return nil, fmt.Errorf("bitget GetWithdrawalHistory: parse: %w", err)
	}

	result := make([]domain.Withdrawal, 0, len(list))
	for _, r := range list {
		amount, _ := parseDecimalOrZero(r.Size)
		fee, _ := parseDecimalOrZero(r.Fee)
		ts := time.Time{}
		if r.Ctime != "" {
			ms, err := decimal.FromString(r.Ctime)
			if err == nil {
				ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}
		result = append(result, domain.Withdrawal{
			WithdrawalID: r.OrderId,
			TxID:         r.TxId,
			Asset:        r.Coin,
			Network:      r.Chain,
			Amount:       amount,
			Fee:          fee,
			Status:       r.Status,
			RequestedAt:  ts,
		})
	}
	return result, nil
}

// ============================================================
// GetDepositHistory — TODO:VERIFY
// ============================================================

// GetDepositHistory возвращает историю депозитов.
// VERIFIED endpoint: GET /api/v2/spot/wallet/deposit-records
// TODO:VERIFY: поля ответа
func (a *Adapter) GetDepositHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Deposit, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["coin"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", query.Limit)
	}
	q := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/api/v2/spot/wallet/deposit-records", q)
	if err != nil {
		return nil, fmt.Errorf("bitget GetDepositHistory: %w", err)
	}

	var list []struct {
		TxId   string `json:"txId"`
		Coin   string `json:"coin"`
		Chain  string `json:"chain"`
		Amount string `json:"amount"`
		Status string `json:"status"`
		Utime  string `json:"uTime"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil {
		return nil, fmt.Errorf("bitget GetDepositHistory: parse: %w", err)
	}

	result := make([]domain.Deposit, 0, len(list))
	for _, r := range list {
		amount, _ := parseDecimalOrZero(r.Amount)
		ts := time.Time{}
		if r.Utime != "" {
			ms, err := decimal.FromString(r.Utime)
			if err == nil {
				ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}
		result = append(result, domain.Deposit{
			TxID:        r.TxId,
			Asset:       r.Coin,
			Network:     r.Chain,
			Amount:      amount,
			Status:      r.Status,
			ConfirmedAt: ts,
		})
	}
	return result, nil
}

// ============================================================
// GetNetworkInfo — TODO:VERIFY
// ============================================================

// GetNetworkInfo возвращает информацию о сетях для актива.
// VERIFIED endpoint: GET /api/v2/spot/public/coins
// TODO:VERIFY: структура ответа
func (a *Adapter) GetNetworkInfo(ctx context.Context, asset string) ([]domain.NetworkInfo, error) {
	q := BuildSortedQuery(map[string]string{"coin": asset})
	env, err := a.doPublicGET(ctx, "/api/v2/spot/public/coins", q)
	if err != nil {
		return nil, fmt.Errorf("bitget GetNetworkInfo: %w", err)
	}

	var list []struct {
		Coin   string `json:"coin"`
		Chains []struct {
			Chain             string `json:"chain"`
			WithdrawEnable    string `json:"withdrawEnable"` // "true"/"false" (TODO:VERIFY)
			DepositEnable     string `json:"depositEnable"`
			WithdrawFee       string `json:"withdrawFee"`
			MinWithdrawAmount string `json:"minWithdrawAmount"`
			MinDepositAmount  string `json:"minDepositAmount"`
		} `json:"chains"`
	}
	if err := json.Unmarshal(env.Data, &list); err != nil {
		return nil, fmt.Errorf("bitget GetNetworkInfo: parse: %w", err)
	}

	var result []domain.NetworkInfo
	for _, coin := range list {
		if coin.Coin != asset {
			continue
		}
		for _, ch := range coin.Chains {
			fee, _ := parseDecimalOrZero(ch.WithdrawFee)
			wMin, _ := parseDecimalOrZero(ch.MinWithdrawAmount)
			dMin, _ := parseDecimalOrZero(ch.MinDepositAmount)
			result = append(result, domain.NetworkInfo{
				Network:         ch.Chain,
				WithdrawEnabled: ch.WithdrawEnable == "true" || ch.WithdrawEnable == "1",
				DepositEnabled:  ch.DepositEnable == "true" || ch.DepositEnable == "1",
				WithdrawFee:     fee,
				WithdrawMin:     wMin,
				DepositMin:      dMin,
			})
		}
	}
	return result, nil
}

// ============================================================
// WS — TODO:VERIFY полная реализация
// ============================================================

// SubscribePublic — TODO:VERIFY: Bitget V2 WS channels format.
// Возвращает ErrWSNotImplemented. REST полный, WS — следующий этап.
func (a *Adapter) SubscribePublic(ctx context.Context, subscriptions []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	// TODO:VERIFY: Bitget V2 WS public URL wss://ws.bitget.com/v2/ws/public
	// TODO: реализовать полный WS subscriber по образцу bybit/ws.go
	return nil, fmt.Errorf("bitget SubscribePublic: %w", errWSNotImplemented)
}

// SubscribePrivate — TODO:VERIFY: Bitget V2 WS private auth format.
func (a *Adapter) SubscribePrivate(ctx context.Context, credentials domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	// TODO:VERIFY: Bitget V2 WS private URL wss://ws.bitget.com/v2/ws/private
	// TODO: реализовать приватный WS subscriber
	return nil, fmt.Errorf("bitget SubscribePrivate: %w", errWSNotImplemented)
}

// errWSNotImplemented — WS ещё не реализован.
var errWSNotImplemented = errors.New("bitget: WebSocket not implemented")

// ============================================================
// Вспомогательные функции
// ============================================================

// parseDecimalOrZero парсит строку в Decimal; при пустой строке возвращает Zero.
func parseDecimalOrZero(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, nil
	}
	return decimal.FromString(s)
}
