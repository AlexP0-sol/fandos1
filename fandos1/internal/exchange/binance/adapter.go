// Package binance реализует адаптер биржи Binance USDT-M Futures.
//
// Все REST-запросы подписываются через HMAC-SHA256 (signer.go).
// Подпись передаётся в query string (параметр signature), ключ — в заголовке X-MBX-APIKEY.
//
// VERIFIED (Binance USDT-M Futures official OpenAPI spec + docs, 2026-07):
//   - GET /fapi/v1/time
//   - GET /fapi/v1/exchangeInfo (PERPETUAL + USDT, фильтры LOT_SIZE / PRICE_FILTER)
//   - GET /fapi/v1/premiumIndex?symbol= (lastFundingRate / nextFundingTime / markPrice / indexPrice)
//   - GET /fapi/v1/ticker/bookTicker?symbol= (bestBid/bestAsk)
//   - GET /fapi/v1/ticker/24hr?symbol= (quoteVolume / lastPrice)
//   - GET /fapi/v1/depth?symbol=&limit= (bids / asks)
//   - GET /fapi/v2/balance (asset / balance / availableBalance)
//   - GET /fapi/v2/positionRisk (positionAmt / entryPrice / markPrice / liquidationPrice / ...)
//   - GET /fapi/v1/openOrders?symbol= (массив FuturesOrder)
//   - GET /fapi/v1/order?symbol=&origClientOrderId= (-2013 → ErrOrderNotFound)
//   - POST /fapi/v1/order (quantity / side / type / timeInForce / reduceOnly / newClientOrderId)
//   - DELETE /fapi/v1/order (symbol / origClientOrderId; -2011 → ErrOrderNotFound)
//   - POST /fapi/v1/leverage (symbol / leverage)
//   - POST /fapi/v1/marginType (symbol / marginType; -4046 → success)
//   - POST /fapi/v1/positionSide/dual (dualSidePosition; -4059 → success)
//   - GET /fapi/v1/adlQuantile?symbol= (массив [{symbol, adlQuantile:{LONG,SHORT,BOTH,...}}])
//
// TODO:VERIFY (SAPI, требует верификации на актуальной документации):
//   - POST /sapi/v1/capital/withdraw/apply
//   - GET  /sapi/v1/capital/withdraw/history
//   - GET  /sapi/v1/capital/deposit/hisrec
//   - GET  /sapi/v1/capital/config/getall
//   - POST /sapi/v1/asset/transfer (внутренний перевод futures↔spot)
//
// Confidence policy для GetFunding:
//   - HIGH   — до следующего funding < 30 мин
//   - MEDIUM — до следующего funding < 4 ч
//   - LOW    — иначе
package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// ============================================================
// Конфигурация и конструктор
// ============================================================

// Config — параметры адаптера Binance.
type Config struct {
	RESTBaseURL  string           // default: https://fapi.binance.com
	SAPIBaseURL  string           // default: https://api.binance.com (для SAPI: withdraw/transfer)
	WSBaseURL    string           // default: wss://fstream.binance.com
	APIKey       string           // X-MBX-APIKEY
	Signer       *Signer          // обязательно (HMAC-SHA256)
	HTTPDoer     HTTPDoer         // обязательно
	RecvWindowMs int64            // default: 5000
	Clock        func() time.Time // default: time.Now
}

// HTTPDoer — интерфейс HTTP-клиента (соответствует bybit.HTTPDoer).
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
	Safe    bool
}

// Константы по умолчанию.
const (
	defaultRESTBase     = "https://fapi.binance.com"
	defaultSAPIBase     = "https://api.binance.com"
	defaultWSBase       = "wss://fstream.binance.com"
	defaultRecvWindowMs = int64(5000)
)

// Adapter — реализация exchange.ExchangeAdapter для Binance USDT-M Futures.
type Adapter struct {
	restBase   string // fapi base (торговля)
	sapiBase   string // sapi base (вывод/перевод)
	wsBase     string // websocket base
	apiKey     string
	signer     *Signer
	http       HTTPDoer
	recvWindow int64
	clock      func() time.Time
}

// New создаёт адаптер Binance из конфига. Паникует при обязательных полях == nil.
func New(cfg Config) *Adapter {
	if cfg.Signer == nil {
		panic("binance: Signer обязателен")
	}
	if cfg.HTTPDoer == nil {
		panic("binance: HTTPDoer обязателен")
	}
	a := &Adapter{
		restBase:   cfg.RESTBaseURL,
		sapiBase:   cfg.SAPIBaseURL,
		wsBase:     cfg.WSBaseURL,
		apiKey:     cfg.APIKey,
		signer:     cfg.Signer,
		http:       cfg.HTTPDoer,
		recvWindow: cfg.RecvWindowMs,
		clock:      cfg.Clock,
	}
	if a.restBase == "" {
		a.restBase = defaultRESTBase
	}
	if a.sapiBase == "" {
		a.sapiBase = defaultSAPIBase
	}
	if a.wsBase == "" {
		a.wsBase = defaultWSBase
	}
	if a.recvWindow == 0 {
		a.recvWindow = defaultRecvWindowMs
	}
	if a.clock == nil {
		a.clock = time.Now
	}
	return a
}

// ID возвращает идентификатор биржи.
func (a *Adapter) ID() domain.ExchangeID { return domain.ExchangeBinance }

// ============================================================
// Вспомогательные HTTP-методы
// ============================================================

// nowMs — текущее время в миллисекундах.
func (a *Adapter) nowMs() int64 { return a.clock().UnixMilli() }

// apiKeyHeaders — заголовок аутентификации.
func (a *Adapter) apiKeyHeaders() map[string]string {
	return map[string]string{"X-MBX-APIKEY": a.apiKey}
}

// doPublicGET — GET-запрос к FAPI без аутентификации.
func (a *Adapter) doPublicGET(ctx context.Context, path, query string) ([]byte, int, error) {
	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method: http.MethodGet,
		Path:   path,
		Query:  query,
		Safe:   true,
	})
	return body, status, err
}

// doSignedGET — GET-запрос к FAPI с подписью (Safe=true — чтение).
func (a *Adapter) doSignedGET(ctx context.Context, path, query string) ([]byte, int, error) {
	signedQuery := a.signer.SignQuery(query, a.nowMs(), a.recvWindow)
	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodGet,
		Path:    path,
		Query:   signedQuery,
		Headers: a.apiKeyHeaders(),
		Safe:    true,
	})
	return body, status, err
}

// doSignedDELETE — DELETE-запрос к FAPI с подписью (Safe=false — отмена ордера).
func (a *Adapter) doSignedDELETE(ctx context.Context, path, query string) ([]byte, int, error) {
	signedQuery := a.signer.SignQuery(query, a.nowMs(), a.recvWindow)
	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodDelete,
		Path:    path,
		Query:   signedQuery,
		Headers: a.apiKeyHeaders(),
		Safe:    false,
	})
	return body, status, err
}

// doSignedPOST — POST-запрос к FAPI с подписью в query string (Safe=false).
// Тело пустое — параметры передаются в query string (стандарт Binance FAPI для POST).
func (a *Adapter) doSignedPOST(ctx context.Context, path string, params map[string]string) ([]byte, int, error) {
	query := buildQuery(params)
	signedQuery := a.signer.SignQuery(query, a.nowMs(), a.recvWindow)
	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Query:   signedQuery,
		Headers: a.apiKeyHeaders(),
		Safe:    false,
	})
	return body, status, err
}

// doSignedSAPIGET — GET-запрос к SAPI с подписью.
func (a *Adapter) doSignedSAPIGET(ctx context.Context, path, query string) ([]byte, int, error) {
	signedQuery := a.signer.SignQuery(query, a.nowMs(), a.recvWindow)
	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodGet,
		Path:    sapiPath(path),
		Query:   signedQuery,
		Headers: a.apiKeyHeaders(),
		Safe:    true,
	})
	return body, status, err
}

// doSignedSAPIPOST — POST-запрос к SAPI с подписью (Safe=false).
func (a *Adapter) doSignedSAPIPOST(ctx context.Context, path string, params map[string]string) ([]byte, int, error) {
	query := buildQuery(params)
	signedQuery := a.signer.SignQuery(query, a.nowMs(), a.recvWindow)
	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    sapiPath(path),
		Query:   signedQuery,
		Headers: a.apiKeyHeaders(),
		Safe:    false,
	})
	return body, status, err
}

// sapiPath маркирует путь как SAPI-запрос (используется в testHTTPDoer для маршрутизации).
// Префикс |SAPI| позволяет тестовому doer отличить fapi от sapi при едином baseURL.
func sapiPath(p string) string { return "|SAPI|" + p }

// wrapHTTPStatus маппирует HTTP-статус и JSON-код ошибки в sentinel.
// Binance возвращает {"code":<N>,"msg":"<text>"} при ошибках.
func wrapHTTPStatus(status int, body []byte) error {
	if status == 429 || status == 418 {
		return fmt.Errorf("%w: HTTP %d", exchange.ErrRateLimited, status)
	}
	if status == 401 {
		return fmt.Errorf("%w: HTTP 401", exchange.ErrUnauthorized)
	}
	if status >= 200 && status < 300 {
		return nil
	}
	// 4xx — читаем JSON-код ошибки.
	var apiErr struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Code != 0 {
		return mapAPIError(apiErr.Code, apiErr.Msg)
	}
	return fmt.Errorf("binance: HTTP %d: %s", status, string(body))
}

// mapAPIError маппирует числовой код Binance API в sentinel-ошибки.
// Документация: https://developers.binance.com/docs/derivatives/usds-margined-futures/error-code
func mapAPIError(code int64, msg string) error {
	switch code {
	case -2011:
		// Ордер уже отменён или не найден (CancelOrder)
		return fmt.Errorf("%w: code=%d %s", exchange.ErrOrderNotFound, code, msg)
	case -2013:
		// Ордер не существует (GetOrder)
		return fmt.Errorf("%w: code=%d %s", exchange.ErrOrderNotFound, code, msg)
	case -2014, -2015:
		// Невалидный API key или подпись
		return fmt.Errorf("%w: code=%d %s", exchange.ErrUnauthorized, code, msg)
	case -1121:
		// Невалидный символ
		return fmt.Errorf("%w: code=%d %s", exchange.ErrInvalidSymbol, code, msg)
	case -2019:
		// Недостаточно маржи
		return fmt.Errorf("%w: code=%d %s", exchange.ErrInsufficientMargin, code, msg)
	case -4046:
		// Режим маржи уже установлен — считаем успехом
		return errMarginModeNotChanged
	case -4059:
		// Режим позиций уже установлен — считаем успехом
		return errPositionModeNotChanged
	case -4028:
		// Плечо не изменено (уже установлено)
		return errLeverageNotChanged
	default:
		return fmt.Errorf("binance: API error code=%d: %s", code, msg)
	}
}

// Внутренние ошибки для идемпотентных операций.
var (
	errMarginModeNotChanged   = errors.New("binance: margin mode not changed (-4046)")
	errPositionModeNotChanged = errors.New("binance: position mode not changed (-4059)")
	errLeverageNotChanged     = errors.New("binance: leverage not changed (-4028)")
)

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

// parseDecimalOrZero парсит строку в Decimal; при пустой строке возвращает Zero.
func parseDecimalOrZero(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, nil
	}
	return decimal.FromString(s)
}

// buildQuery строит query string из карты параметров (отсортировано для детерминизма).
func buildQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(params[k]))
	}
	return b.String()
}

// ============================================================
// GetServerTime — VERIFIED
// ============================================================

// serverTimeResponse — ответ /fapi/v1/time.
type serverTimeResponse struct {
	ServerTime int64 `json:"serverTime"`
}

// GetServerTime возвращает серверное время Binance.
// VERIFIED: GET /fapi/v1/time → {"serverTime": <ms>}
func (a *Adapter) GetServerTime(ctx context.Context) (time.Time, error) {
	body, status, err := a.doPublicGET(ctx, "/fapi/v1/time", "")
	if err != nil {
		return time.Time{}, wrapNetErr(fmt.Errorf("binance GetServerTime: %w", err))
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return time.Time{}, fmt.Errorf("binance GetServerTime: %w", err)
	}
	var res serverTimeResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return time.Time{}, fmt.Errorf("binance GetServerTime: parse: %w", err)
	}
	return time.UnixMilli(res.ServerTime).UTC(), nil
}

// ============================================================
// GetInstruments — VERIFIED
// ============================================================

// exchangeInfoResponse — ответ /fapi/v1/exchangeInfo.
type exchangeInfoResponse struct {
	Symbols []exchangeInfoSymbol `json:"symbols"`
}

// exchangeInfoSymbol — один инструмент в exchangeInfo.
type exchangeInfoSymbol struct {
	Symbol       string               `json:"symbol"`
	ContractType string               `json:"contractType"`
	Status       string               `json:"status"`
	BaseAsset    string               `json:"baseAsset"`
	QuoteAsset   string               `json:"quoteAsset"`
	MarginAsset  string               `json:"marginAsset"`
	Filters      []exchangeInfoFilter `json:"filters"`
}

// exchangeInfoFilter — один фильтр символа.
type exchangeInfoFilter struct {
	FilterType string `json:"filterType"`
	// LOT_SIZE поля
	StepSize string `json:"stepSize"`
	MinQty   string `json:"minQty"`
	MaxQty   string `json:"maxQty"`
	// PRICE_FILTER поля
	TickSize string `json:"tickSize"`
	MinPrice string `json:"minPrice"`
	MaxPrice string `json:"maxPrice"`
	// MIN_NOTIONAL поля
	Notional string `json:"notional"`
}

// fundingInfoEntry — одна запись из /fapi/v1/fundingInfo.
// TODO:VERIFY: поля adjustedFundingRateCap/Floor/fundingIntervalHours
type fundingInfoEntry struct {
	Symbol               string `json:"symbol"`
	FundingIntervalHours int64  `json:"fundingIntervalHours"` // TODO:VERIFY
}

// GetInstruments возвращает все торгуемые PERPETUAL USDT-инструменты.
// VERIFIED: GET /fapi/v1/exchangeInfo → symbols[].contractType=="PERPETUAL" && quoteAsset=="USDT"
// Лучшее усилие: пытается смержить данные из /fapi/v1/fundingInfo для fundingIntervalSec.
// При ошибке fundingInfo используется дефолтный интервал 8 часов.
// TODO:VERIFY: формат ответа /fapi/v1/fundingInfo (fundingIntervalHours)
func (a *Adapter) GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error) {
	body, status, err := a.doPublicGET(ctx, "/fapi/v1/exchangeInfo", "")
	if err != nil {
		return nil, wrapNetErr(fmt.Errorf("binance GetInstruments: %w", err))
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return nil, fmt.Errorf("binance GetInstruments: %w", err)
	}
	var ei exchangeInfoResponse
	if err := json.Unmarshal(body, &ei); err != nil {
		return nil, fmt.Errorf("binance GetInstruments: parse exchangeInfo: %w", err)
	}

	// Лучшее усилие: получаем fundingInfo для fundingIntervalSec.
	fundingIntervals := make(map[string]int64) // symbol → интервал в секундах
	if fBody, fStatus, fErr := a.doPublicGET(ctx, "/fapi/v1/fundingInfo", ""); fErr == nil && fStatus == 200 {
		var fiList []fundingInfoEntry
		if json.Unmarshal(fBody, &fiList) == nil {
			for _, fi := range fiList {
				if fi.FundingIntervalHours > 0 {
					fundingIntervals[fi.Symbol] = fi.FundingIntervalHours * 3600
				}
			}
		}
		// TODO:VERIFY: поле fundingIntervalHours может называться иначе в актуальной документации
	}

	var result []domain.CanonicalInstrument
	for _, sym := range ei.Symbols {
		// Фильтруем: только PERPETUAL с USDT-квотой и статусом TRADING
		if sym.ContractType != "PERPETUAL" || sym.QuoteAsset != "USDT" {
			continue
		}
		if sym.Status != "TRADING" {
			continue
		}
		instr, err := parseExchangeInfoSymbol(sym)
		if err != nil {
			// Пропускаем инструменты с неполными данными
			continue
		}
		// Применяем fundingInterval из fundingInfo, если есть
		if interval, ok := fundingIntervals[sym.Symbol]; ok {
			instr.FundingIntervalSec = interval
		}
		result = append(result, instr)
	}
	return result, nil
}

// parseExchangeInfoSymbol преобразует exchangeInfoSymbol в domain.CanonicalInstrument.
func parseExchangeInfoSymbol(sym exchangeInfoSymbol) (domain.CanonicalInstrument, error) {
	var stepSize, minQty, tickSize decimal.Decimal
	var err error
	for _, f := range sym.Filters {
		switch f.FilterType {
		case "LOT_SIZE":
			stepSize, err = decimal.FromString(f.StepSize)
			if err != nil {
				return domain.CanonicalInstrument{}, fmt.Errorf("LOT_SIZE stepSize: %w", err)
			}
			minQty, err = decimal.FromString(f.MinQty)
			if err != nil {
				return domain.CanonicalInstrument{}, fmt.Errorf("LOT_SIZE minQty: %w", err)
			}
		case "PRICE_FILTER":
			tickSize, err = decimal.FromString(f.TickSize)
			if err != nil {
				return domain.CanonicalInstrument{}, fmt.Errorf("PRICE_FILTER tickSize: %w", err)
			}
		}
	}
	if stepSize.IsZero() || tickSize.IsZero() {
		return domain.CanonicalInstrument{}, fmt.Errorf("symbol %s: missing LOT_SIZE or PRICE_FILTER", sym.Symbol)
	}

	return domain.CanonicalInstrument{
		Exchange:           domain.ExchangeBinance,
		CanonicalBaseAsset: domain.AssetSymbol(sym.BaseAsset),
		ExchangeSymbol:     domain.ExchangeSymbol(sym.Symbol),
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		SettlementCurrency: sym.MarginAsset,
		ContractMultiplier: decimal.One,
		QtyStep:            stepSize,
		MinQty:             minQty,
		TickSize:           tickSize,
		MaxLeverage:        decimal.FromInt(125), // Binance USDT-M max leverage
		FundingIntervalSec: 8 * 3600,             // дефолт 8 ч; перезаписывается из fundingInfo
		FundingPriceType:   domain.FundingPriceMark,
		SupportsADL:        true,
		Status:             domain.InstrumentStatusActive,
	}, nil
}

// ============================================================
// GetFunding — VERIFIED
// ============================================================

// premiumIndexResponse — ответ /fapi/v1/premiumIndex.
// VERIFIED: поля lastFundingRate / nextFundingTime / markPrice / indexPrice.
type premiumIndexResponse struct {
	Symbol          string `json:"symbol"`
	MarkPrice       string `json:"markPrice"`
	IndexPrice      string `json:"indexPrice"`
	EstimateSettle  string `json:"estimatedSettlePrice"`
	LastFundingRate string `json:"lastFundingRate"`
	NextFundingTime int64  `json:"nextFundingTime"` // миллисекунды
	InterestRate    string `json:"interestRate"`
	Time            int64  `json:"time"`
}

// GetFunding возвращает нормализованную funding-информацию.
// VERIFIED: GET /fapi/v1/premiumIndex?symbol=
//
// Confidence policy:
//   - HIGH   — до nextFundingTime < 30 мин
//   - MEDIUM — до nextFundingTime < 4 ч
//   - LOW    — иначе
func (a *Adapter) GetFunding(ctx context.Context, symbol domain.ExchangeSymbol) (domain.FundingInfo, error) {
	query := buildQuery(map[string]string{"symbol": string(symbol)})
	body, status, err := a.doPublicGET(ctx, "/fapi/v1/premiumIndex", query)
	if err != nil {
		return domain.FundingInfo{}, wrapNetErr(fmt.Errorf("binance GetFunding: %w", err))
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return domain.FundingInfo{}, fmt.Errorf("binance GetFunding: %w", err)
	}
	var res premiumIndexResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return domain.FundingInfo{}, fmt.Errorf("binance GetFunding: parse: %w", err)
	}

	rate, err := decimal.FromString(res.LastFundingRate)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("binance GetFunding: lastFundingRate: %w", err)
	}

	nextFunding := time.UnixMilli(res.NextFundingTime).UTC()

	// Confidence policy
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
		PredictedFundingRate: rate, // Binance premiumIndex даёт только lastFundingRate
		RateType:             domain.FundingRatePredicted,
		FundingIntervalSec:   8 * 3600, // TODO: брать из GetInstruments
		NextFundingTime:      nextFunding,
		Confidence:           confidence,
		FundingPriceType:     domain.FundingPriceMark,
	}, nil
}

// ============================================================
// GetTicker — VERIFIED (два вызова: bookTicker + 24hr)
// ============================================================

// bookTickerResponse — ответ /fapi/v1/ticker/bookTicker.
// VERIFIED: поля bidPrice / bidQty / askPrice / askQty
type bookTickerResponse struct {
	Symbol   string `json:"symbol"`
	BidPrice string `json:"bidPrice"`
	BidQty   string `json:"bidQty"`
	AskPrice string `json:"askPrice"`
	AskQty   string `json:"askQty"`
	Time     int64  `json:"time"`
}

// ticker24hrResponse — ответ /fapi/v1/ticker/24hr.
// VERIFIED: поля lastPrice / quoteVolume
type ticker24hrResponse struct {
	Symbol      string `json:"symbol"`
	LastPrice   string `json:"lastPrice"`
	QuoteVolume string `json:"quoteVolume"`
	CloseTime   int64  `json:"closeTime"`
}

// GetTicker возвращает нормализованный тикер.
// Документация: два раздельных вызова:
//  1. GET /fapi/v1/ticker/bookTicker → bestBid/bestAsk
//  2. GET /fapi/v1/ticker/24hr      → lastPrice/quoteVolume
//
// Примечание: markPrice и indexPrice берутся из premiumIndex (отдельный вызов при необходимости).
// Здесь MarkPrice не заполняется — только BBO + lastPrice + volume. VERIFIED.
func (a *Adapter) GetTicker(ctx context.Context, symbol domain.ExchangeSymbol) (domain.Ticker, error) {
	sym := string(symbol)

	// Вызов 1: bookTicker для BBO
	btBody, btStatus, err := a.doPublicGET(ctx, "/fapi/v1/ticker/bookTicker",
		buildQuery(map[string]string{"symbol": sym}))
	if err != nil {
		return domain.Ticker{}, wrapNetErr(fmt.Errorf("binance GetTicker bookTicker: %w", err))
	}
	if err := wrapHTTPStatus(btStatus, btBody); err != nil {
		return domain.Ticker{}, fmt.Errorf("binance GetTicker bookTicker: %w", err)
	}
	var bt bookTickerResponse
	if err := json.Unmarshal(btBody, &bt); err != nil {
		return domain.Ticker{}, fmt.Errorf("binance GetTicker: parse bookTicker: %w", err)
	}

	// Вызов 2: 24hr ticker для lastPrice / quoteVolume
	t24Body, t24Status, err := a.doPublicGET(ctx, "/fapi/v1/ticker/24hr",
		buildQuery(map[string]string{"symbol": sym}))
	if err != nil {
		return domain.Ticker{}, wrapNetErr(fmt.Errorf("binance GetTicker 24hr: %w", err))
	}
	if err := wrapHTTPStatus(t24Status, t24Body); err != nil {
		return domain.Ticker{}, fmt.Errorf("binance GetTicker 24hr: %w", err)
	}
	var t24 ticker24hrResponse
	if err := json.Unmarshal(t24Body, &t24); err != nil {
		return domain.Ticker{}, fmt.Errorf("binance GetTicker: parse 24hr: %w", err)
	}

	lastPrice, err := parseDecimalOrZero(t24.LastPrice)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("binance GetTicker: lastPrice: %w", err)
	}
	volume, err := parseDecimalOrZero(t24.QuoteVolume)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("binance GetTicker: quoteVolume: %w", err)
	}

	ts := a.clock()
	if bt.Time > 0 {
		ts = time.UnixMilli(bt.Time).UTC()
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

// depthResponse — ответ /fapi/v1/depth.
// VERIFIED: поля lastUpdateId / bids / asks (arrays of [price, qty])
type depthResponse struct {
	LastUpdateID int64      `json:"lastUpdateId"`
	E            int64      `json:"E"` // время сообщения (мс)
	T            int64      `json:"T"` // время транзакции (мс)
	Bids         [][]string `json:"bids"`
	Asks         [][]string `json:"asks"`
}

// GetOrderBookSnapshot возвращает снимок стакана.
// VERIFIED: GET /fapi/v1/depth?symbol=&limit=
// Допустимые limit: 5 / 10 / 20 / 50 / 100 / 500 / 1000.
func (a *Adapter) GetOrderBookSnapshot(ctx context.Context, symbol domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error) {
	query := buildQuery(map[string]string{
		"symbol": string(symbol),
		"limit":  fmt.Sprintf("%d", depth),
	})
	body, status, err := a.doPublicGET(ctx, "/fapi/v1/depth", query)
	if err != nil {
		return domain.OrderBookSnapshot{}, wrapNetErr(fmt.Errorf("binance GetOrderBookSnapshot: %w", err))
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("binance GetOrderBookSnapshot: %w", err)
	}
	var res depthResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("binance GetOrderBookSnapshot: parse: %w", err)
	}

	bids, err := parsePriceLevels(res.Bids)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("binance GetOrderBookSnapshot: bids: %w", err)
	}
	asks, err := parsePriceLevels(res.Asks)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("binance GetOrderBookSnapshot: asks: %w", err)
	}

	ts := a.clock()
	if res.T > 0 {
		ts = time.UnixMilli(res.T).UTC()
	} else if res.E > 0 {
		ts = time.UnixMilli(res.E).UTC()
	}

	return domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeBinance,
		Symbol:     symbol,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  ts,
		Sequence:   res.LastUpdateID,
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
// GetBalances — VERIFIED (v2)
// ============================================================

// balanceEntry — одна запись в ответе /fapi/v2/balance.
// VERIFIED: поля asset / balance / availableBalance
// TODO:VERIFY: v2 vs v3 — актуальная документация рекомендует v2; v3 может существовать
type balanceEntry struct {
	AccountAlias     string `json:"accountAlias"`
	Asset            string `json:"asset"`
	Balance          string `json:"balance"`
	CrossWalletBal   string `json:"crossWalletBalance"`
	CrossUnPnl       string `json:"crossUnPnl"`
	AvailableBalance string `json:"availableBalance"`
	MaxWithdrawAmt   string `json:"maxWithdrawAmount"`
	MarginAvailable  bool   `json:"marginAvailable"`
	UpdateTime       int64  `json:"updateTime"`
}

// GetBalances возвращает балансы USDT-M аккаунта.
// VERIFIED: GET /fapi/v2/balance (массив balanceEntry)
func (a *Adapter) GetBalances(ctx context.Context) ([]domain.Balance, error) {
	body, status, err := a.doSignedGET(ctx, "/fapi/v2/balance", "")
	if err != nil {
		return nil, wrapNetErr(fmt.Errorf("binance GetBalances: %w", err))
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return nil, fmt.Errorf("binance GetBalances: %w", err)
	}
	var entries []balanceEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("binance GetBalances: parse: %w", err)
	}

	var balances []domain.Balance
	for _, e := range entries {
		wallet, err := parseDecimalOrZero(e.Balance)
		if err != nil {
			continue
		}
		avail, err := parseDecimalOrZero(e.AvailableBalance)
		if err != nil {
			continue
		}
		balances = append(balances, domain.Balance{
			Asset:            e.Asset,
			WalletBalance:    wallet,
			AvailableBalance: avail,
		})
	}
	return balances, nil
}

// ============================================================
// GetPositions — VERIFIED (v2)
// ============================================================

// positionRiskEntry — одна запись в ответе /fapi/v2/positionRisk.
// VERIFIED: поля symbol / positionAmt / entryPrice / markPrice / unRealizedProfit /
//
//	liquidationPrice / leverage / marginType / isolatedMargin / updateTime
//
// TODO:VERIFY: v2 vs v3 — актуальная документация рекомендует v2
type positionRiskEntry struct {
	Symbol           string `json:"symbol"`
	PositionAmt      string `json:"positionAmt"` // со знаком: + long, - short
	EntryPrice       string `json:"entryPrice"`
	BreakEvenPrice   string `json:"breakEvenPrice"`
	MarkPrice        string `json:"markPrice"`
	UnRealizedProfit string `json:"unRealizedProfit"`
	LiquidationPrice string `json:"liquidationPrice"`
	Leverage         string `json:"leverage"`
	MaxNotionalValue string `json:"maxNotionalValue"`
	MarginType       string `json:"marginType"` // "cross" или "isolated"
	IsolatedMargin   string `json:"isolatedMargin"`
	IsAutoAddMargin  string `json:"isAutoAddMargin"`
	PositionSide     string `json:"positionSide"` // "BOTH" / "LONG" / "SHORT"
	Notional         string `json:"notional"`
	IsolatedWallet   string `json:"isolatedWallet"`
	UpdateTime       int64  `json:"updateTime"`
}

// GetPositions возвращает все открытые позиции.
// VERIFIED: GET /fapi/v2/positionRisk
func (a *Adapter) GetPositions(ctx context.Context) ([]domain.Position, error) {
	body, status, err := a.doSignedGET(ctx, "/fapi/v2/positionRisk", "")
	if err != nil {
		return nil, wrapNetErr(fmt.Errorf("binance GetPositions: %w", err))
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return nil, fmt.Errorf("binance GetPositions: %w", err)
	}
	var entries []positionRiskEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("binance GetPositions: parse: %w", err)
	}

	var positions []domain.Position
	for _, e := range entries {
		amt, err := decimal.FromString(e.PositionAmt)
		if err != nil || amt.IsZero() {
			continue // пустые позиции пропускаем
		}
		pos, err := parsePositionRisk(e)
		if err != nil {
			continue
		}
		positions = append(positions, pos)
	}
	return positions, nil
}

// parsePositionRisk преобразует positionRiskEntry в domain.Position.
func parsePositionRisk(e positionRiskEntry) (domain.Position, error) {
	amt, err := decimal.FromString(e.PositionAmt)
	if err != nil {
		return domain.Position{}, fmt.Errorf("positionAmt: %w", err)
	}
	entryPrice, err := parseDecimalOrZero(e.EntryPrice)
	if err != nil {
		return domain.Position{}, fmt.Errorf("entryPrice: %w", err)
	}
	markPrice, err := parseDecimalOrZero(e.MarkPrice)
	if err != nil {
		return domain.Position{}, fmt.Errorf("markPrice: %w", err)
	}
	liqPrice, err := parseDecimalOrZero(e.LiquidationPrice)
	if err != nil {
		return domain.Position{}, fmt.Errorf("liquidationPrice: %w", err)
	}
	pnl, err := parseDecimalOrZero(e.UnRealizedProfit)
	if err != nil {
		return domain.Position{}, fmt.Errorf("unRealizedProfit: %w", err)
	}
	leverage, err := parseDecimalOrZero(e.Leverage)
	if err != nil {
		return domain.Position{}, fmt.Errorf("leverage: %w", err)
	}
	margin, err := parseDecimalOrZero(e.IsolatedMargin)
	if err != nil {
		return domain.Position{}, fmt.Errorf("isolatedMargin: %w", err)
	}

	// positionAmt > 0 → long, < 0 → short
	var side domain.Side
	if amt.IsPositive() {
		side = domain.SideLong
	} else {
		side = domain.SideShort
	}
	qty := amt.Abs()

	marginMode := domain.MarginCross
	if strings.ToLower(e.MarginType) == "isolated" {
		marginMode = domain.MarginIsolated
	}

	updatedTime := time.Time{}
	if e.UpdateTime > 0 {
		updatedTime = time.UnixMilli(e.UpdateTime).UTC()
	}

	return domain.Position{
		Symbol:           domain.ExchangeSymbol(e.Symbol),
		Side:             side,
		ContractQty:      qty,
		BaseQty:          qty,
		EntryPrice:       entryPrice,
		MarkPrice:        markPrice,
		LiquidationPrice: liqPrice,
		UnrealizedPnL:    pnl,
		MarginMode:       marginMode,
		Leverage:         leverage,
		Margin:           margin,
		Updated:          updatedTime,
	}, nil
}

// ============================================================
// GetOpenOrders — VERIFIED
// ============================================================

// openOrderEntry — один ордер в ответе /fapi/v1/openOrders.
// VERIFIED: поля orderId / clientOrderId / symbol / side / type / origQty / executedQty /
//
//	avgPrice / status / reduceOnly / time
type openOrderEntry struct {
	OrderID       int64  `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
	Symbol        string `json:"symbol"`
	Side          string `json:"side"` // "BUY" / "SELL"
	Type          string `json:"type"` // "MARKET" / "LIMIT" / ...
	OrigQty       string `json:"origQty"`
	ExecutedQty   string `json:"executedQty"`
	CumQty        string `json:"cumQty"`
	AvgPrice      string `json:"avgPrice"`
	Price         string `json:"price"`
	Status        string `json:"status"` // "NEW" / "PARTIALLY_FILLED" / "FILLED" / "CANCELED" / "REJECTED" / "EXPIRED"
	ReduceOnly    bool   `json:"reduceOnly"`
	TimeInForce   string `json:"timeInForce"`
	Time          int64  `json:"time"`
	UpdateTime    int64  `json:"updateTime"`
	CumQuote      string `json:"cumQuote"`
}

// GetOpenOrders возвращает открытые ордера по символу.
// VERIFIED: GET /fapi/v1/openOrders?symbol=
func (a *Adapter) GetOpenOrders(ctx context.Context, symbol domain.ExchangeSymbol) ([]domain.Order, error) {
	query := buildQuery(map[string]string{"symbol": string(symbol)})
	body, status, err := a.doSignedGET(ctx, "/fapi/v1/openOrders", query)
	if err != nil {
		return nil, wrapNetErr(fmt.Errorf("binance GetOpenOrders: %w", err))
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return nil, fmt.Errorf("binance GetOpenOrders: %w", err)
	}
	var entries []openOrderEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("binance GetOpenOrders: parse: %w", err)
	}

	orders := make([]domain.Order, 0, len(entries))
	for _, e := range entries {
		ord, err := parseOpenOrder(e)
		if err != nil {
			continue
		}
		orders = append(orders, ord)
	}
	return orders, nil
}

// parseOpenOrder преобразует openOrderEntry в domain.Order.
func parseOpenOrder(e openOrderEntry) (domain.Order, error) {
	qty, err := decimal.FromString(e.OrigQty)
	if err != nil {
		return domain.Order{}, fmt.Errorf("origQty: %w", err)
	}
	filledQty, err := parseDecimalOrZero(e.ExecutedQty)
	if err != nil {
		return domain.Order{}, fmt.Errorf("executedQty: %w", err)
	}
	avgPrice, err := parseDecimalOrZero(e.AvgPrice)
	if err != nil {
		return domain.Order{}, fmt.Errorf("avgPrice: %w", err)
	}

	side := parseSide(e.Side)
	status := parseOrderStatus(e.Status)
	orderMode := parseOrderMode(e.Type)

	ts := time.Time{}
	if e.Time > 0 {
		ts = time.UnixMilli(e.Time).UTC()
	}

	return domain.Order{
		ExchangeOrderID:   fmt.Sprintf("%d", e.OrderID),
		ClientOrderID:     domain.ClientOrderID(e.ClientOrderID),
		Symbol:            domain.ExchangeSymbol(e.Symbol),
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        e.ReduceOnly,
		RequestedQty:      qty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPrice,
		Status:            status,
		ExchangeTimestamp: ts,
		AckState:          domain.AckStateQueried,
	}, nil
}

// parseSide маппирует строку Binance → domain.Side.
func parseSide(s string) domain.Side {
	if strings.ToUpper(s) == "BUY" {
		return domain.SideLong
	}
	return domain.SideShort
}

// parseOrderStatus маппирует строковый статус Binance → domain.OrderStatus.
func parseOrderStatus(s string) domain.OrderStatus {
	switch strings.ToUpper(s) {
	case "NEW":
		return domain.OrderStatusAcknowledged
	case "PARTIALLY_FILLED":
		return domain.OrderStatusPartiallyFilled
	case "FILLED":
		return domain.OrderStatusFilled
	case "CANCELED":
		return domain.OrderStatusCancelled
	case "REJECTED":
		return domain.OrderStatusRejected
	case "EXPIRED":
		return domain.OrderStatusExpired
	default:
		return domain.OrderStatusNew
	}
}

// parseOrderMode маппирует тип ордера Binance → domain.OrderMode.
func parseOrderMode(t string) domain.OrderMode {
	if strings.ToUpper(t) == "MARKET" {
		return domain.OrderMarket
	}
	return domain.OrderMarketableLimitIOC
}

// ============================================================
// SetLeverage — VERIFIED
// ============================================================

// SetLeverage устанавливает плечо.
// VERIFIED: POST /fapi/v1/leverage (query params: symbol, leverage)
// -4028 "leverage not changed" → успех.
func (a *Adapter) SetLeverage(ctx context.Context, req domain.SetLeverageRequest) error {
	// Binance принимает leverage как целое число
	leverageInt := req.Leverage.Underlying().IntPart()
	params := map[string]string{
		"symbol":   string(req.Symbol),
		"leverage": fmt.Sprintf("%d", leverageInt),
	}
	body, status, err := a.doSignedPOST(ctx, "/fapi/v1/leverage", params)
	if err != nil {
		return wrapNetErr(fmt.Errorf("binance SetLeverage: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		if errors.Is(herr, errLeverageNotChanged) {
			return nil // уже установлено
		}
		return fmt.Errorf("binance SetLeverage: %w", herr)
	}
	return nil
}

// ============================================================
// SetMarginMode — VERIFIED
// ============================================================

// SetMarginMode переключает режим маржи.
// VERIFIED: POST /fapi/v1/marginType (query params: symbol, marginType)
// -4046 "No need to change margin type" → успех.
func (a *Adapter) SetMarginMode(ctx context.Context, req domain.SetMarginModeRequest) error {
	marginType := "CROSSED"
	if req.MarginMode == domain.MarginIsolated {
		marginType = "ISOLATED"
	}
	params := map[string]string{
		"symbol":     string(req.Symbol),
		"marginType": marginType,
	}
	body, status, err := a.doSignedPOST(ctx, "/fapi/v1/marginType", params)
	if err != nil {
		return wrapNetErr(fmt.Errorf("binance SetMarginMode: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		if errors.Is(herr, errMarginModeNotChanged) {
			return nil // уже в нужном режиме
		}
		return fmt.Errorf("binance SetMarginMode: %w", herr)
	}
	return nil
}

// ============================================================
// SetPositionMode — VERIFIED
// ============================================================

// SetPositionMode переключает режим позиций (one-way/hedge).
// VERIFIED: POST /fapi/v1/positionSide/dual (dualSidePosition=true → hedge, false → one-way)
// -4059 "No need to change position side" → успех.
func (a *Adapter) SetPositionMode(ctx context.Context, req domain.SetPositionModeRequest) error {
	dualSide := "false"
	if req.Mode == domain.PositionHedge {
		dualSide = "true"
	}
	params := map[string]string{
		"dualSidePosition": dualSide,
	}
	body, status, err := a.doSignedPOST(ctx, "/fapi/v1/positionSide/dual", params)
	if err != nil {
		return wrapNetErr(fmt.Errorf("binance SetPositionMode: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		if errors.Is(herr, errPositionModeNotChanged) {
			return nil // уже в нужном режиме
		}
		return fmt.Errorf("binance SetPositionMode: %w", herr)
	}
	return nil
}

// ============================================================
// PlaceOrder — VERIFIED (Safe=false обязательно)
// ============================================================

// placeOrderResponse — поля ответа на создание ордера.
// VERIFIED: поля orderId / clientOrderId
type placeOrderResponse struct {
	OrderID       int64  `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
	Symbol        string `json:"symbol"`
	Status        string `json:"status"`
	UpdateTime    int64  `json:"updateTime"`
}

// PlaceOrder размещает ордер. Safe=false обязательно (ордера не идемпотентны).
// VERIFIED: POST /fapi/v1/order
// Параметры передаются в query string (стандарт Binance).
func (a *Adapter) PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error) {
	// Маппинг side
	side := "BUY"
	if req.Side == domain.SideShort {
		side = "SELL"
	}

	// Тип ордера и timeInForce
	orderType := "LIMIT"
	tif := "IOC"
	if req.OrderMode == domain.OrderMarket {
		orderType = "MARKET"
		tif = ""
	} else if string(req.TimeInForce) != "" {
		tif = string(req.TimeInForce)
	}

	params := map[string]string{
		"symbol":           string(req.Symbol),
		"side":             side,
		"type":             orderType,
		"quantity":         req.BaseQty.String(),
		"newClientOrderId": string(req.ClientOrderID),
	}

	if tif != "" {
		params["timeInForce"] = tif
	}
	if orderType == "LIMIT" && !req.Price.IsZero() {
		params["price"] = req.Price.String()
	}
	// reduceOnly: Binance принимает строку "true"/"false"
	if req.ReduceOnly {
		params["reduceOnly"] = "true"
	}

	body, status, err := a.doSignedPOST(ctx, "/fapi/v1/order", params)
	if err != nil {
		return domain.OrderAck{}, wrapNetErr(fmt.Errorf("binance PlaceOrder: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		return domain.OrderAck{}, fmt.Errorf("binance PlaceOrder: %w", herr)
	}
	var res placeOrderResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return domain.OrderAck{}, fmt.Errorf("binance PlaceOrder: parse: %w", err)
	}

	return domain.OrderAck{
		ExchangeOrderID: fmt.Sprintf("%d", res.OrderID),
		ClientOrderID:   req.ClientOrderID,
		Status:          domain.OrderStatusAcknowledged,
		Timestamp:       a.clock(),
	}, nil
}

// ============================================================
// CancelOrder — VERIFIED
// ============================================================

// CancelOrder отменяет ордер.
// VERIFIED: DELETE /fapi/v1/order (query params: symbol + origClientOrderId или orderId)
// -2011 → ErrOrderNotFound (ордер не найден или уже отменён).
func (a *Adapter) CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error {
	params := map[string]string{
		"symbol": string(req.Symbol),
	}
	if req.ClientOrderID != "" {
		params["origClientOrderId"] = string(req.ClientOrderID)
	}
	if req.ExchangeOrderID != "" {
		params["orderId"] = req.ExchangeOrderID
	}

	query := buildQuery(params)
	body, status, err := a.doSignedDELETE(ctx, "/fapi/v1/order", query)
	if err != nil {
		return wrapNetErr(fmt.Errorf("binance CancelOrder: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		if errors.Is(herr, exchange.ErrOrderNotFound) {
			return herr
		}
		return fmt.Errorf("binance CancelOrder: %w", herr)
	}
	return nil
}

// ============================================================
// GetOrder — VERIFIED
// ============================================================

// GetOrder запрашивает состояние ордера по clientOrderID.
// VERIFIED: GET /fapi/v1/order?symbol=&origClientOrderId=
// -2013 → ErrOrderNotFound.
func (a *Adapter) GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error) {
	params := map[string]string{
		"symbol": string(req.Symbol),
	}
	if req.ClientOrderID != "" {
		params["origClientOrderId"] = string(req.ClientOrderID)
	}
	if req.ExchangeOrderID != "" {
		params["orderId"] = req.ExchangeOrderID
	}

	body, status, err := a.doSignedGET(ctx, "/fapi/v1/order", buildQuery(params))
	if err != nil {
		return domain.Order{}, wrapNetErr(fmt.Errorf("binance GetOrder: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		if errors.Is(herr, exchange.ErrOrderNotFound) {
			return domain.Order{}, herr
		}
		return domain.Order{}, fmt.Errorf("binance GetOrder: %w", herr)
	}
	var e openOrderEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return domain.Order{}, fmt.Errorf("binance GetOrder: parse: %w", err)
	}
	ord, err := parseOpenOrder(e)
	if err != nil {
		return domain.Order{}, fmt.Errorf("binance GetOrder: %w", err)
	}
	return ord, nil
}

// ============================================================
// GetADLState — VERIFIED
// ============================================================

// adlQuantileResponse — ответ /fapi/v1/adlQuantile.
// VERIFIED: массив объектов {symbol, adlQuantile:{LONG,SHORT,BOTH,HEDGE}}
type adlQuantileResponse struct {
	Symbol      string         `json:"symbol"`
	AdlQuantile adlQuantileMap `json:"adlQuantile"`
}

// adlQuantileMap — поля quantile по стороне позиции.
type adlQuantileMap struct {
	Long  int `json:"LONG"`
	Short int `json:"SHORT"`
	Both  int `json:"BOTH"`
	Hedge int `json:"HEDGE"`
}

// GetADLState возвращает ADL-состояние для символа.
// VERIFIED: GET /fapi/v1/adlQuantile?symbol=
// Quantile 0..4 ÷ 4 → [0,1] (Binance шкала 0..4, не 0..5 как Bybit).
func (a *Adapter) GetADLState(ctx context.Context, symbol domain.ExchangeSymbol) (domain.ADLState, error) {
	query := buildQuery(map[string]string{"symbol": string(symbol)})
	body, status, err := a.doSignedGET(ctx, "/fapi/v1/adlQuantile", query)
	if err != nil {
		return domain.ADLState{}, wrapNetErr(fmt.Errorf("binance GetADLState: %w", err))
	}
	if err := wrapHTTPStatus(status, body); err != nil {
		return domain.ADLState{}, fmt.Errorf("binance GetADLState: %w", err)
	}

	// Ответ — массив; ищем нужный символ
	var entries []adlQuantileResponse
	if err := json.Unmarshal(body, &entries); err != nil {
		// Попробуем как одиночный объект
		var single adlQuantileResponse
		if err2 := json.Unmarshal(body, &single); err2 != nil {
			return domain.ADLState{}, fmt.Errorf("binance GetADLState: parse: %w", err)
		}
		entries = []adlQuantileResponse{single}
	}

	now := a.clock()
	for _, e := range entries {
		if e.Symbol != string(symbol) {
			continue
		}
		// Binance ADL quantile: 0..4 → нормализуем в [0,1] делением на 4
		longQ := decimal.FromInt(int64(e.AdlQuantile.Long)).Div(decimal.FromInt(4))
		shortQ := decimal.FromInt(int64(e.AdlQuantile.Short)).Div(decimal.FromInt(4))
		// Для one-way mode используем Both
		if e.AdlQuantile.Both > 0 {
			q := decimal.FromInt(int64(e.AdlQuantile.Both)).Div(decimal.FromInt(4))
			longQ = q
			shortQ = q
		}
		return domain.ADLState{
			Symbol:     symbol,
			LongQueue:  longQ,
			ShortQueue: shortQ,
			Timestamp:  now,
		}, nil
	}

	// Не найдено — возвращаем нулевое состояние
	return domain.ADLState{
		Symbol:     symbol,
		LongQueue:  decimal.Zero,
		ShortQueue: decimal.Zero,
		Timestamp:  now,
	}, nil
}

// ============================================================
// InternalTransfer — TODO:VERIFY
// ============================================================

// internalTransferResponse — ответ SAPI /sapi/v1/asset/transfer.
// TODO:VERIFY: поля tranId, поддержка типов UMFUTURE_MAIN и MAIN_UMFUTURE
type internalTransferResponse struct {
	TranID int64 `json:"tranId"`
}

// InternalTransfer — внутренний перевод между аккаунтами.
// TODO:VERIFY: endpoint /sapi/v1/asset/transfer; параметры type (MAIN_UMFUTURE / UMFUTURE_MAIN)
func (a *Adapter) InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error) {
	transferType := mapTransferType(req.From, req.To)
	params := map[string]string{
		"type":   transferType,
		"asset":  req.Asset,
		"amount": req.Amount.String(),
	}
	body, status, err := a.doSignedSAPIPOST(ctx, "/sapi/v1/asset/transfer", params)
	if err != nil {
		return domain.TransferResult{}, wrapNetErr(fmt.Errorf("binance InternalTransfer: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		return domain.TransferResult{}, fmt.Errorf("binance InternalTransfer: %w", herr)
	}
	var res internalTransferResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return domain.TransferResult{}, fmt.Errorf("binance InternalTransfer: parse: %w", err)
	}
	return domain.TransferResult{
		TransferID: fmt.Sprintf("%d", res.TranID),
		Status:     "submitted",
	}, nil
}

// mapTransferType маппирует from/to в тип перевода Binance SAPI.
// TODO:VERIFY: точные значения типов
func mapTransferType(from, to string) string {
	f := strings.ToLower(from)
	t := strings.ToLower(to)
	if (f == "spot" || f == "main") && (t == "futures" || t == "umfuture") {
		return "MAIN_UMFUTURE"
	}
	if (f == "futures" || f == "umfuture") && (t == "spot" || t == "main") {
		return "UMFUTURE_MAIN"
	}
	return strings.ToUpper(from) + "_" + strings.ToUpper(to)
}

// ============================================================
// Withdraw — TODO:VERIFY
// ============================================================

// withdrawResponse — ответ SAPI /sapi/v1/capital/withdraw/apply.
// TODO:VERIFY: поле id
type withdrawResponse struct {
	ID string `json:"id"`
}

// Withdraw создаёт заявку на вывод через SAPI.
// TODO:VERIFY: поля coin/network/address/amount/addressTag актуальны для SAPI v1
func (a *Adapter) Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	params := map[string]string{
		"coin":    req.Asset,
		"network": req.Network,
		"address": req.Address,
		"amount":  req.Amount.String(),
	}
	if req.Memo != "" {
		params["addressTag"] = req.Memo
	}
	body, status, err := a.doSignedSAPIPOST(ctx, "/sapi/v1/capital/withdraw/apply", params)
	if err != nil {
		return domain.WithdrawalResult{}, wrapNetErr(fmt.Errorf("binance Withdraw: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("binance Withdraw: %w", herr)
	}
	var res withdrawResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("binance Withdraw: parse: %w", err)
	}
	return domain.WithdrawalResult{
		WithdrawalID: res.ID,
		Status:       "submitted",
	}, nil
}

// ============================================================
// GetWithdrawalHistory — TODO:VERIFY
// ============================================================

// withdrawHistoryEntry — одна запись истории вывода SAPI.
// TODO:VERIFY: поля id / txId / coin / network / amount / transactionFee / status / applyTime
type withdrawHistoryEntry struct {
	ID             string `json:"id"`
	TxID           string `json:"txId"`
	Coin           string `json:"coin"`
	Network        string `json:"network"`
	Amount         string `json:"amount"`
	TransactionFee string `json:"transactionFee"`
	Status         int    `json:"status"`    // 0=email sent, 1=cancelled, 2=awaiting, 3=rejected, 4=processing, 5=failure, 6=completed
	ApplyTime      string `json:"applyTime"` // "2019-10-12 11:12:02" UTC
}

// GetWithdrawalHistory возвращает историю выводов.
// TODO:VERIFY: GET /sapi/v1/capital/withdraw/history (параметры coin, limit, startTime, endTime)
func (a *Adapter) GetWithdrawalHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Withdrawal, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["coin"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", query.Limit)
	}
	body, status, err := a.doSignedSAPIGET(ctx, "/sapi/v1/capital/withdraw/history", buildQuery(params))
	if err != nil {
		return nil, wrapNetErr(fmt.Errorf("binance GetWithdrawalHistory: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		return nil, fmt.Errorf("binance GetWithdrawalHistory: %w", herr)
	}
	var entries []withdrawHistoryEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("binance GetWithdrawalHistory: parse: %w", err)
	}

	var result []domain.Withdrawal
	for _, e := range entries {
		amount, _ := parseDecimalOrZero(e.Amount)
		fee, _ := parseDecimalOrZero(e.TransactionFee)
		result = append(result, domain.Withdrawal{
			WithdrawalID: e.ID,
			TxID:         e.TxID,
			Asset:        e.Coin,
			Network:      e.Network,
			Amount:       amount,
			Fee:          fee,
			Status:       fmt.Sprintf("%d", e.Status),
			RequestedAt:  time.Time{}, // applyTime разбирается при необходимости
		})
	}
	return result, nil
}

// ============================================================
// GetDepositHistory — TODO:VERIFY
// ============================================================

// depositHistoryEntry — одна запись истории депозита.
// TODO:VERIFY: поля txId / coin / network / amount / status / insertTime
type depositHistoryEntry struct {
	TxID       string `json:"txId"`
	Coin       string `json:"coin"`
	Network    string `json:"network"`
	Amount     string `json:"amount"`
	Status     int    `json:"status"`     // 0=pending, 1=success, 6=credited but cannot withdraw
	InsertTime int64  `json:"insertTime"` // миллисекунды
}

// GetDepositHistory возвращает историю депозитов.
// TODO:VERIFY: GET /sapi/v1/capital/deposit/hisrec
func (a *Adapter) GetDepositHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Deposit, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["coin"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", query.Limit)
	}
	body, status, err := a.doSignedSAPIGET(ctx, "/sapi/v1/capital/deposit/hisrec", buildQuery(params))
	if err != nil {
		return nil, wrapNetErr(fmt.Errorf("binance GetDepositHistory: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		return nil, fmt.Errorf("binance GetDepositHistory: %w", herr)
	}
	var entries []depositHistoryEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("binance GetDepositHistory: parse: %w", err)
	}

	var result []domain.Deposit
	for _, e := range entries {
		amount, _ := parseDecimalOrZero(e.Amount)
		ts := time.Time{}
		if e.InsertTime > 0 {
			ts = time.UnixMilli(e.InsertTime).UTC()
		}
		result = append(result, domain.Deposit{
			TxID:        e.TxID,
			Asset:       e.Coin,
			Network:     e.Network,
			Amount:      amount,
			Status:      fmt.Sprintf("%d", e.Status),
			ConfirmedAt: ts,
		})
	}
	return result, nil
}

// ============================================================
// GetNetworkInfo — TODO:VERIFY
// ============================================================

// coinNetworkEntry — одна монета в ответе /sapi/v1/capital/config/getall.
// TODO:VERIFY: структура ответа (networkList, withdrawEnable, depositEnable, withdrawFee, withdrawMin)
type coinNetworkEntry struct {
	Coin        string              `json:"coin"`
	NetworkList []networkInfoDetail `json:"networkList"`
}

// networkInfoDetail — одна сеть вывода.
type networkInfoDetail struct {
	Network        string `json:"network"`
	WithdrawEnable bool   `json:"withdrawEnable"`
	DepositEnable  bool   `json:"depositEnable"`
	WithdrawFee    string `json:"withdrawFee"`
	WithdrawMin    string `json:"withdrawMin"`
	DepositDesc    string `json:"depositDesc"`
}

// GetNetworkInfo возвращает информацию о сетях для актива.
// TODO:VERIFY: GET /sapi/v1/capital/config/getall (параметр coin)
func (a *Adapter) GetNetworkInfo(ctx context.Context, asset string) ([]domain.NetworkInfo, error) {
	params := map[string]string{"coin": asset}
	body, status, err := a.doSignedSAPIGET(ctx, "/sapi/v1/capital/config/getall", buildQuery(params))
	if err != nil {
		return nil, wrapNetErr(fmt.Errorf("binance GetNetworkInfo: %w", err))
	}
	if herr := wrapHTTPStatus(status, body); herr != nil {
		return nil, fmt.Errorf("binance GetNetworkInfo: %w", herr)
	}
	var coins []coinNetworkEntry
	if err := json.Unmarshal(body, &coins); err != nil {
		return nil, fmt.Errorf("binance GetNetworkInfo: parse: %w", err)
	}

	var result []domain.NetworkInfo
	for _, coin := range coins {
		if coin.Coin != asset {
			continue
		}
		for _, n := range coin.NetworkList {
			fee, _ := parseDecimalOrZero(n.WithdrawFee)
			wMin, _ := parseDecimalOrZero(n.WithdrawMin)
			result = append(result, domain.NetworkInfo{
				Network:         n.Network,
				WithdrawEnabled: n.WithdrawEnable,
				DepositEnabled:  n.DepositEnable,
				WithdrawFee:     fee,
				WithdrawMin:     wMin,
			})
		}
	}
	return result, nil
}

// ============================================================
// SubscribePublic / SubscribePrivate — реализованы в ws.go
// ============================================================
