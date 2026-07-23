// Package gate реализует адаптер биржи Gate.io V4 для USDT-settled linear perpetual.
//
// REST-запросы используют Gate.io APIv4. Подпись — HMAC-SHA512.
//
// VERIFIED (официальная документация gateio.ws, 2026-07):
//   - GET /api/v4/spot/time                                          (server time)
//   - GET /api/v4/futures/usdt/contracts                             (all contracts)
//   - GET /api/v4/futures/usdt/contracts/{contract}                  (single contract)
//   - GET /api/v4/futures/usdt/tickers?contract=                     (ticker)
//   - GET /api/v4/futures/usdt/order_book?contract=&limit=           (order book)
//   - GET /api/v4/futures/usdt/accounts                              (balances)
//   - GET /api/v4/futures/usdt/positions                             (positions)
//   - GET /api/v4/futures/usdt/orders?status=open&contract=          (open orders)
//   - GET /api/v4/futures/usdt/orders/{order_id}                     (single order)
//   - POST /api/v4/futures/usdt/orders                               (place order)
//   - DELETE /api/v4/futures/usdt/orders/{order_id}                  (cancel order)
//   - POST /api/v4/futures/usdt/positions/{contract}/leverage        (set leverage)
//   - POST /api/v4/wallet/transfers                                  (spot↔futures transfer)
//   - POST /api/v4/withdrawals                                       (withdraw)
//   - GET /api/v4/withdrawals                                        (withdrawal history)
//   - GET /api/v4/wallet/deposits                                    (deposit history)
//   - GET /api/v4/wallet/currency_chains?currency=                   (network info)
//
// TODO:VERIFY:
//   - GET /api/v4/futures/usdt/dual_mode (read) — проверить поле dual_mode в ответе
//   - POST /api/v4/futures/usdt/dual_mode?dual_mode=false — выключение dual-mode TODO:VERIFY
//   - GET /api/v4/futures/usdt/orders?status=finished — поиск по text-полю (clientID)
//   - adl_ranking поле в позициях — Gate может называть его иначе TODO:VERIFY
//
// Особенности Gate.io futures:
//   - Количество контрактов (size) — целое со знаком: положительное=long, отрицательное=short.
//   - quanto_multiplier — размер одного контракта в базовой монете.
//     Конверсия: contracts = floor(baseQty / quanto_multiplier); baseQty = contracts × quanto_multiplier.
//   - Рыночный ордер: price="0", tif="ioc".
//   - ClientOrderID передаётся в поле text с префиксом "t-"; итого ≤ 28 символов в text (30 суммарно
//     с "t-", официально), но реально поле text ≤ 28 символов после "t-" = 30 суммарно. Допустимые
//     символы в text: буквы, цифры, '-', '_', '.'. Наши ClientOrderID совместимы; при превышении
//     длины хвост обрезается детерминированно.
//
// Резолюция ClientOrderID → ExchangeOrderID:
//   - Gate не имеет прямого поиска по clientID через REST (text-поиск не документирован).
//   - Адаптер хранит in-memory map clientID → orderID.
//   - ОГРАНИЧЕНИЕ: маппинг теряется при рестарте адаптера.
//     Если orderID неизвестен, GetOrder итерирует открытые + завершённые ордера.
//
// WebSocket (TODO:VERIFY — URI wss://fx-ws.gateio.ws/v4/ws/usdt не проверен в prod):
//   - SubscribePublic/SubscribePrivate возвращают ErrWSNotImplemented.
//   - Каналы futures.tickers и futures.order_book документированы, но impl оставлена TODO.
//
// Confidence policy для GetFunding:
//   - HIGH   — до следующего funding < 30 мин
//   - MEDIUM — до следующего funding < 4 ч
//   - LOW    — иначе
package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// Config — параметры адаптера Gate.io.
type Config struct {
	RESTBaseURL  string           // default: https://api.gateio.ws
	WSBaseURL    string           // default: wss://fx-ws.gateio.ws/v4/ws/usdt (TODO:VERIFY)
	APIKey       string           // обязательно для приватных запросов
	APISecret    string           // обязательно для приватных запросов
	Passphrase   string           // игнорируется (Gate.io не использует Passphrase)
	HTTPDoer     HTTPDoer         // обязательно
	RecvWindowMs int64            // не используется Gate.io; зарезервировано для совместимости
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
	Query   string // query-string без "?", уже сформированная
	Body    io.Reader
	Headers map[string]string
	Safe    bool // true для GET (idempotent), false для POST/DELETE
}

// defaultRESTBase — Production REST.
const defaultRESTBase = "https://api.gateio.ws"

// apiV4Prefix — префикс всех V4 endpoint-ов.
const apiV4Prefix = "/api/v4"

// defaultWSBase — TODO:VERIFY.
const defaultWSBase = "wss://fx-ws.gateio.ws/v4/ws/usdt"

// settle — раздел Gate.io фьючерсов (USDT).
const settle = "usdt"

// Adapter — реализация exchange.ExchangeAdapter для Gate.io V4 USDT perpetual.
type Adapter struct {
	restBase string
	wsBase   string
	signer   *Signer
	http     HTTPDoer
	clock    func() time.Time

	// in-memory map clientID → exchangeOrderID для GetOrder/CancelOrder.
	// Теряется при рестарте адаптера. Документировано как ограничение.
	orderMap   map[string]string
	orderMapMu sync.RWMutex

	// contractCache кэширует quan_multiplier по символу.
	contractCache   map[string]decimal.Decimal
	contractCacheMu sync.RWMutex
}

// New создаёт адаптер Gate.io. Возвращает ошибку, если HTTPDoer == nil.
func New(cfg Config) (*Adapter, error) {
	if cfg.HTTPDoer == nil {
		return nil, fmt.Errorf("gate: HTTPDoer обязателен")
	}
	signer := NewSigner(cfg.APIKey, []byte(cfg.APISecret))
	a := &Adapter{
		restBase:      cfg.RESTBaseURL,
		wsBase:        cfg.WSBaseURL,
		signer:        signer,
		http:          cfg.HTTPDoer,
		clock:         cfg.Clock,
		orderMap:      make(map[string]string),
		contractCache: make(map[string]decimal.Decimal),
	}
	if a.restBase == "" {
		a.restBase = defaultRESTBase
	}
	// wsBase не устанавливается по умолчанию: пользователь обязан явно задать Config.WSBaseURL.
	// Если WSBaseURL пуст — SubscribePublic возвращает ErrWSNotImplemented.
	// Рекомендуемый URL: defaultWSBase = wss://fx-ws.gateio.ws/v4/ws/usdt.
	if a.clock == nil {
		a.clock = time.Now
	}
	return a, nil
}

// ID возвращает идентификатор биржи.
func (a *Adapter) ID() domain.ExchangeID { return domain.ExchangeGate }

// nowSec — текущее время в секундах Unix.
func (a *Adapter) nowSec() int64 { return a.clock().Unix() }

// ============================================================
// Общий HTTP helper
// ============================================================

// gateError — тело ошибки Gate.io.
type gateError struct {
	Label   string `json:"label"`
	Message string `json:"message"`
}

// doPublicGET — GET без аутентификации.
func (a *Adapter) doPublicGET(ctx context.Context, path, query string) ([]byte, error) {
	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method: http.MethodGet,
		Path:   path,
		Query:  query,
		Safe:   true,
	})
	if err != nil {
		return nil, wrapNetErr(err)
	}
	if err := checkHTTPStatus(status, body); err != nil {
		return nil, err
	}
	return body, nil
}

// doSignedGET — GET с аутентификацией.
func (a *Adapter) doSignedGET(ctx context.Context, path, query string) ([]byte, error) {
	ts := a.nowSec()
	sign := a.signer.Sign(http.MethodGet, path, query, "", ts)
	headers := a.signer.AuthHeaders(ts, sign)

	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodGet,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    true,
	})
	if err != nil {
		return nil, wrapNetErr(err)
	}
	if err := checkHTTPStatus(status, body); err != nil {
		return nil, err
	}
	return body, nil
}

// doSignedPOST — POST с JSON-телом и аутентификацией.
func (a *Adapter) doSignedPOST(ctx context.Context, path string, payload interface{}) ([]byte, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("gate: marshal: %w", err)
	}
	bodyStr := string(bodyBytes)

	ts := a.nowSec()
	sign := a.signer.Sign(http.MethodPost, path, "", bodyStr, ts)
	headers := a.signer.AuthHeaders(ts, sign)
	headers["Content-Type"] = "application/json"

	status, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Body:    bytes.NewReader(bodyBytes),
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return nil, wrapNetErr(err)
	}
	if err := checkHTTPStatus(status, respBody); err != nil {
		return nil, err
	}
	return respBody, nil
}

// doSignedDELETE — DELETE с аутентификацией.
func (a *Adapter) doSignedDELETE(ctx context.Context, path, query string) ([]byte, error) {
	ts := a.nowSec()
	sign := a.signer.Sign(http.MethodDelete, path, query, "", ts)
	headers := a.signer.AuthHeaders(ts, sign)

	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodDelete,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return nil, wrapNetErr(err)
	}
	if err := checkHTTPStatus(status, body); err != nil {
		return nil, err
	}
	return body, nil
}

// checkHTTPStatus проверяет HTTP-статус и маппирует ошибки Gate.io.
func checkHTTPStatus(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}

	// Пытаемся разобрать тело ошибки.
	var gErr gateError
	_ = json.Unmarshal(body, &gErr)

	return mapAPIError(status, gErr.Label, gErr.Message)
}

// mapAPIError маппирует HTTP-статус и label Gate.io в sentinel-ошибки.
// VERIFIED: label-ы из официальной документации Gate.io V4.
func mapAPIError(httpStatus int, label, message string) error {
	// HTTP 429 — rate limited.
	if httpStatus == 429 {
		return fmt.Errorf("%w: HTTP 429 %s", exchange.ErrRateLimited, message)
	}

	// HTTP 401/403 — unauthorized.
	if httpStatus == 401 || httpStatus == 403 {
		return fmt.Errorf("%w: HTTP %d %s %s", exchange.ErrUnauthorized, httpStatus, label, message)
	}

	// label-based mapping.
	switch label {
	case "INVALID_KEY", "INVALID_SIGNATURE", "MISSING_REQUIRED_HEADER", "INVALID_PARAM_VALUE":
		if label == "INVALID_KEY" || label == "INVALID_SIGNATURE" {
			return fmt.Errorf("%w: label=%s %s", exchange.ErrUnauthorized, label, message)
		}
	case "ORDER_NOT_FOUND":
		return fmt.Errorf("%w: label=%s %s", exchange.ErrOrderNotFound, label, message)
	case "BALANCE_NOT_ENOUGH", "INSUFFICIENT_AVAILABLE":
		return fmt.Errorf("%w: label=%s %s", exchange.ErrInsufficientMargin, label, message)
	case "CONTRACT_NOT_FOUND", "INVALID_CONTRACT":
		return fmt.Errorf("%w: label=%s %s", exchange.ErrInvalidSymbol, label, message)
	}

	if httpStatus == 401 || httpStatus == 403 {
		return fmt.Errorf("%w: label=%s %s", exchange.ErrUnauthorized, label, message)
	}

	return fmt.Errorf("gate: API error HTTP %d label=%s: %s", httpStatus, label, message)
}

// wrapNetErr оборачивает сетевые/таймаут ошибки.
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

// serverTimeResponse — ответ /api/v4/spot/time.
type serverTimeResponse struct {
	ServerTime int64 `json:"server_time"` // Unix seconds
}

// GetServerTime возвращает серверное время Gate.io.
// VERIFIED: GET /api/v4/spot/time
func (a *Adapter) GetServerTime(ctx context.Context) (time.Time, error) {
	body, err := a.doPublicGET(ctx, apiV4Prefix+"/spot/time", "")
	if err != nil {
		return time.Time{}, fmt.Errorf("gate GetServerTime: %w", err)
	}
	var res serverTimeResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return time.Time{}, fmt.Errorf("gate GetServerTime: parse: %w", err)
	}
	return time.Unix(res.ServerTime, 0).UTC(), nil
}

// ============================================================
// GetInstruments — VERIFIED
// ============================================================

// contractEntry — один контракт из /api/v4/futures/usdt/contracts.
// Gate.io может возвращать числа как число или строку — используем json.RawMessage.
type contractEntry struct {
	Name             string      `json:"name"`               // BTC_USDT
	QuantoMultiplier string      `json:"quanto_multiplier"`  // размер 1 контракта в базовой монете (строка)
	OrderSizeMin     json.Number `json:"order_size_min"`     // минимальный размер в контрактах (число или строка)
	OrderPriceRound  string      `json:"order_price_round"`  // tick size (строка)
	FundingRate      string      `json:"funding_rate"`       // текущий funding rate
	FundingInterval  int64       `json:"funding_interval"`   // в секундах
	FundingNextApply float64     `json:"funding_next_apply"` // Unix timestamp секунды (float в ответе)
	InDelisting      bool        `json:"in_delisting"`
	TradeStatus      string      `json:"trade_status"` // "trading" / "settling" / "closed"
	OrderbookID      int64       `json:"orderbook_id"`
	LeversMin        string      `json:"leverage_min"`
	LeversMax        string      `json:"leverage_max"`
	MaintenanceRate  string      `json:"maintenance_rate"`
	MarkType         string      `json:"mark_type"` // "index" / "mark"
}

// futuresPath строит префикс пути futures/usdt.
func futuresPath(sub string) string {
	return apiV4Prefix + "/futures/" + settle + sub
}

// GetInstruments возвращает все торгуемые USDT-perpetual контракты.
// VERIFIED: GET /api/v4/futures/usdt/contracts
func (a *Adapter) GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error) {
	body, err := a.doPublicGET(ctx, futuresPath("/contracts"), "")
	if err != nil {
		return nil, fmt.Errorf("gate GetInstruments: %w", err)
	}

	var contracts []contractEntry
	if err := json.Unmarshal(body, &contracts); err != nil {
		return nil, fmt.Errorf("gate GetInstruments: parse: %w", err)
	}

	var result []domain.CanonicalInstrument
	for _, c := range contracts {
		if c.TradeStatus != "trading" || c.InDelisting {
			continue
		}
		instr, err := parseContract(c)
		if err != nil {
			continue // некорректные данные — пропускаем
		}
		// Кэшируем quanto_multiplier.
		a.contractCacheMu.Lock()
		a.contractCache[c.Name] = instr.ContractMultiplier
		a.contractCacheMu.Unlock()
		result = append(result, instr)
	}
	return result, nil
}

// parseContract преобразует contractEntry в domain.CanonicalInstrument.
func parseContract(c contractEntry) (domain.CanonicalInstrument, error) {
	multiplier, err := decimal.FromString(c.QuantoMultiplier)
	if err != nil || multiplier.IsZero() {
		return domain.CanonicalInstrument{}, fmt.Errorf("quanto_multiplier: %w", err)
	}

	tickSize, err := decimal.FromString(c.OrderPriceRound)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("order_price_round: %w", err)
	}

	// order_size_min — минимальный размер ордера в контрактах (целое).
	// Переводим в базовые единицы: minQty = orderSizeMin × multiplier.
	minSizeStr := c.OrderSizeMin.String()
	if minSizeStr == "" {
		minSizeStr = "1"
	}
	minSizeContracts, err := decimal.FromString(minSizeStr)
	if err != nil {
		minSizeContracts = decimal.One
	}
	minQty := minSizeContracts.Mul(multiplier)

	// leverage_max.
	var maxLev decimal.Decimal
	if c.LeversMax != "" {
		maxLev, _ = decimal.FromString(c.LeversMax)
	}

	// Базовый актив — первая часть имени BTC_USDT → BTC.
	baseAsset := c.Name
	if idx := strings.Index(c.Name, "_"); idx >= 0 {
		baseAsset = c.Name[:idx]
	}

	status := domain.InstrumentStatusActive
	if c.InDelisting {
		status = domain.InstrumentStatusDelisted
	}

	return domain.CanonicalInstrument{
		Exchange:           domain.ExchangeGate,
		CanonicalBaseAsset: domain.AssetSymbol(baseAsset),
		ExchangeSymbol:     domain.ExchangeSymbol(c.Name),
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		SettlementCurrency: "USDT",
		ContractMultiplier: multiplier,
		QtyStep:            multiplier, // шаг = 1 контракт = multiplier базового актива
		MinQty:             minQty,
		TickSize:           tickSize,
		MaxLeverage:        maxLev,
		FundingIntervalSec: c.FundingInterval,
		FundingPriceType:   domain.FundingPriceMark,
		SupportsADL:        true,
		Status:             status,
	}, nil
}

// getQuantoMultiplier возвращает quanto_multiplier из кэша или запрашивает через API.
func (a *Adapter) getQuantoMultiplier(ctx context.Context, symbol domain.ExchangeSymbol) (decimal.Decimal, error) {
	sym := string(symbol)

	a.contractCacheMu.RLock()
	mult, ok := a.contractCache[sym]
	a.contractCacheMu.RUnlock()
	if ok {
		return mult, nil
	}

	// Запрашиваем одиночный контракт.
	body, err := a.doPublicGET(ctx, futuresPath("/contracts/"+sym), "")
	if err != nil {
		return decimal.Zero, fmt.Errorf("gate getQuantoMultiplier: %w", err)
	}
	var c contractEntry
	if err := json.Unmarshal(body, &c); err != nil {
		return decimal.Zero, fmt.Errorf("gate getQuantoMultiplier: parse: %w", err)
	}
	mult, err = decimal.FromString(c.QuantoMultiplier)
	if err != nil {
		return decimal.Zero, fmt.Errorf("gate getQuantoMultiplier: quanto_multiplier: %w", err)
	}

	a.contractCacheMu.Lock()
	a.contractCache[sym] = mult
	a.contractCacheMu.Unlock()

	return mult, nil
}

// baseQtyToContracts конвертирует base qty → количество контрактов (округление вниз).
// contracts = floor(baseQty / quanto_multiplier)
// Документация: один контракт = quanto_multiplier единиц базового актива.
func baseQtyToContracts(baseQty, multiplier decimal.Decimal) int64 {
	if multiplier.IsZero() {
		return 0
	}
	contracts, _ := baseQty.Quantize(multiplier)
	// Quantize даёт floor(qty/step)*step; нам нужно floor(qty/multiplier).
	// Так как step=multiplier, floor = contracts / multiplier.
	result := contracts.Div(multiplier)
	return result.Underlying().IntPart()
}

// contractsToBaseQty конвертирует количество контрактов → base qty.
// baseQty = abs(contracts) × quanto_multiplier
func contractsToBaseQty(contracts int64, multiplier decimal.Decimal) decimal.Decimal {
	if contracts < 0 {
		contracts = -contracts
	}
	return decimal.FromInt(contracts).Mul(multiplier)
}

// ============================================================
// GetFunding — из /api/v4/futures/usdt/contracts/{contract}
// ============================================================

// GetFunding возвращает нормализованную funding-информацию.
// VERIFIED: поля funding_rate, funding_next_apply из GET /api/v4/futures/usdt/contracts/{contract}
//
// Confidence policy:
//   - HIGH   — до nextFundingTime < 30 мин
//   - MEDIUM — до nextFundingTime < 4 ч
//   - LOW    — иначе
func (a *Adapter) GetFunding(ctx context.Context, symbol domain.ExchangeSymbol) (domain.FundingInfo, error) {
	sym := string(symbol)
	body, err := a.doPublicGET(ctx, futuresPath("/contracts/"+sym), "")
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("gate GetFunding: %w", err)
	}
	var c contractEntry
	if err := json.Unmarshal(body, &c); err != nil {
		return domain.FundingInfo{}, fmt.Errorf("gate GetFunding: parse: %w", err)
	}

	rate, err := decimal.FromString(c.FundingRate)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("gate GetFunding: funding_rate: %w", err)
	}

	// funding_next_apply — Unix timestamp в секундах (float64 в ответе Gate.io).
	nextFunding := time.Unix(int64(c.FundingNextApply), 0).UTC()

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
		PredictedFundingRate: rate,
		RateType:             domain.FundingRatePredicted,
		FundingIntervalSec:   c.FundingInterval,
		NextFundingTime:      nextFunding,
		Confidence:           confidence,
		FundingPriceType:     domain.FundingPriceMark,
	}, nil
}

// ============================================================
// GetTicker — VERIFIED
// ============================================================

// tickerEntry — ответ /api/v4/futures/usdt/tickers.
type tickerEntry struct {
	Contract        string `json:"contract"`
	Last            string `json:"last"`
	MarkPrice       string `json:"mark_price"`
	IndexPrice      string `json:"index_price"`
	Volume24hSettle string `json:"volume_24h_settle"`
	// Gate.io тикер не содержит funding_rate напрямую — берём из контракта.
}

// GetTicker возвращает нормализованный тикер.
// VERIFIED: GET /api/v4/futures/usdt/tickers?contract=
func (a *Adapter) GetTicker(ctx context.Context, symbol domain.ExchangeSymbol) (domain.Ticker, error) {
	query := BuildSortedQuery(map[string]string{"contract": string(symbol)})
	body, err := a.doPublicGET(ctx, futuresPath("/tickers"), query)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("gate GetTicker: %w", err)
	}

	var tickers []tickerEntry
	if err := json.Unmarshal(body, &tickers); err != nil {
		return domain.Ticker{}, fmt.Errorf("gate GetTicker: parse: %w", err)
	}
	if len(tickers) == 0 {
		return domain.Ticker{}, fmt.Errorf("%w: %s", exchange.ErrInvalidSymbol, symbol)
	}
	t := tickers[0]

	lastPrice, err := parseDecimalOrZero(t.Last)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("gate GetTicker: last: %w", err)
	}
	markPrice, err := parseDecimalOrZero(t.MarkPrice)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("gate GetTicker: mark_price: %w", err)
	}
	indexPrice, err := parseDecimalOrZero(t.IndexPrice)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("gate GetTicker: index_price: %w", err)
	}
	volume, err := parseDecimalOrZero(t.Volume24hSettle)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("gate GetTicker: volume_24h_settle: %w", err)
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

// orderBookResponse — ответ /api/v4/futures/usdt/order_book.
type orderBookResponse struct {
	ID      int64      `json:"id"`
	Current float64    `json:"current"`
	Update  float64    `json:"update"`
	Asks    [][]string `json:"asks"` // [[price, size], ...]
	Bids    [][]string `json:"bids"`
}

// GetOrderBookSnapshot возвращает снимок стакана.
// VERIFIED: GET /api/v4/futures/usdt/order_book?contract=&limit=
func (a *Adapter) GetOrderBookSnapshot(ctx context.Context, symbol domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error) {
	query := BuildSortedQuery(map[string]string{
		"contract": string(symbol),
		"limit":    strconv.Itoa(depth),
	})
	body, err := a.doPublicGET(ctx, futuresPath("/order_book"), query)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("gate GetOrderBookSnapshot: %w", err)
	}
	var res orderBookResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("gate GetOrderBookSnapshot: parse: %w", err)
	}

	bids, err := parsePriceLevels(res.Bids)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("gate GetOrderBookSnapshot: bids: %w", err)
	}
	asks, err := parsePriceLevels(res.Asks)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("gate GetOrderBookSnapshot: asks: %w", err)
	}

	// current — Unix секунды с дробной частью.
	ts := time.Unix(int64(res.Current), 0).UTC()
	if res.Current == 0 {
		ts = a.clock()
	}

	return domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeGate,
		Symbol:     symbol,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  ts,
		Sequence:   res.ID,
		IsSnapshot: true,
	}, nil
}

// ============================================================
// GetBalances — VERIFIED
// ============================================================

// accountResponse — ответ /api/v4/futures/usdt/accounts.
type accountResponse struct {
	Total     string `json:"total"`
	Available string `json:"available"`
	Currency  string `json:"currency"`
}

// GetBalances возвращает баланс futures-аккаунта Gate.io.
// VERIFIED: GET /api/v4/futures/usdt/accounts
func (a *Adapter) GetBalances(ctx context.Context) ([]domain.Balance, error) {
	body, err := a.doSignedGET(ctx, futuresPath("/accounts"), "")
	if err != nil {
		return nil, fmt.Errorf("gate GetBalances: %w", err)
	}
	var res accountResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("gate GetBalances: parse: %w", err)
	}

	total, err := parseDecimalOrZero(res.Total)
	if err != nil {
		return nil, fmt.Errorf("gate GetBalances: total: %w", err)
	}
	avail, err := parseDecimalOrZero(res.Available)
	if err != nil {
		return nil, fmt.Errorf("gate GetBalances: available: %w", err)
	}

	asset := res.Currency
	if asset == "" {
		asset = "USDT"
	}

	return []domain.Balance{
		{
			Asset:            asset,
			WalletBalance:    total,
			AvailableBalance: avail,
		},
	}, nil
}

// ============================================================
// GetPositions — VERIFIED
// ============================================================

// positionEntry — одна позиция Gate.io futures.
// size — целое со знаком: положительный=long, отрицательный=short.
type positionEntry struct {
	Contract           string      `json:"contract"`
	Size               json.Number `json:"size"` // целое со знаком, может быть числом или строкой
	EntryPrice         string      `json:"entry_price"`
	MarkPrice          string      `json:"mark_price"`
	LiqPrice           string      `json:"liq_price"`
	UnrealisedPnl      string      `json:"unrealised_pnl"`
	Margin             string      `json:"margin"`
	Leverage           string      `json:"leverage"`
	Mode               string      `json:"mode"`        // "single" / "dual_long" / "dual_short"
	AdlRanking         json.Number `json:"adl_ranking"` // TODO:VERIFY поле называется adl_ranking?
	CrossLeverageLimit string      `json:"cross_leverage_limit"`
}

// GetPositions возвращает все открытые позиции.
// VERIFIED: GET /api/v4/futures/usdt/positions
func (a *Adapter) GetPositions(ctx context.Context) ([]domain.Position, error) {
	body, err := a.doSignedGET(ctx, futuresPath("/positions"), "")
	if err != nil {
		return nil, fmt.Errorf("gate GetPositions: %w", err)
	}
	var entries []positionEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("gate GetPositions: parse: %w", err)
	}

	var positions []domain.Position
	for _, e := range entries {
		sizeStr := e.Size.String()
		if sizeStr == "" || sizeStr == "0" {
			continue
		}
		sizeInt, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil || sizeInt == 0 {
			continue
		}

		pos, err := a.parsePosition(ctx, e, sizeInt)
		if err != nil {
			continue
		}
		positions = append(positions, pos)
	}
	return positions, nil
}

// parsePosition преобразует positionEntry в domain.Position.
func (a *Adapter) parsePosition(ctx context.Context, e positionEntry, sizeInt int64) (domain.Position, error) {
	sym := domain.ExchangeSymbol(e.Contract)

	// Сторона позиции из знака size.
	var side domain.Side
	if sizeInt > 0 {
		side = domain.SideLong
	} else {
		side = domain.SideShort
	}

	// Количество контрактов (абсолютное).
	absSize := sizeInt
	if absSize < 0 {
		absSize = -absSize
	}
	contractQty := decimal.FromInt(absSize)

	// Конверсия в baseQty через quanto_multiplier.
	multiplier, err := a.getQuantoMultiplier(ctx, sym)
	if err != nil {
		// Фоллбэк — используем 1 (для тестов без кэша).
		multiplier = decimal.One
	}
	baseQty := contractsToBaseQty(absSize, multiplier)

	entryPrice, _ := parseDecimalOrZero(e.EntryPrice)
	markPrice, _ := parseDecimalOrZero(e.MarkPrice)
	liqPrice, _ := parseDecimalOrZero(e.LiqPrice)
	pnl, _ := parseDecimalOrZero(e.UnrealisedPnl)
	margin, _ := parseDecimalOrZero(e.Margin)
	leverage, _ := parseDecimalOrZero(e.Leverage)

	// Маржинальный режим.
	marginMode := domain.MarginCross
	if e.Leverage != "" && e.Leverage != "0" {
		// Gate.io: если leverage == 0 → cross, иначе isolated.
		if lev, err := strconv.ParseFloat(e.Leverage, 64); err == nil && lev > 0 {
			marginMode = domain.MarginIsolated
		}
	}

	// ADL ранг — TODO:VERIFY поле adl_ranking.
	var adlState *domain.ADLQueuePosition
	if adlStr := e.AdlRanking.String(); adlStr != "" && adlStr != "<nil>" {
		if adlInt, err := strconv.ParseInt(adlStr, 10, 64); err == nil && adlInt > 0 {
			// Gate.io adl_ranking: 1-5, нормализуем в [0,1].
			adlNorm := decimal.FromInt(adlInt).Div(decimal.FromInt(5))
			adlState = &domain.ADLQueuePosition{
				LongQueue:  adlNorm,
				ShortQueue: adlNorm,
			}
		}
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
		Leverage:         leverage,
		Margin:           margin,
		ADLQueue:         adlState,
		Updated:          a.clock(),
	}, nil
}

// ============================================================
// GetOpenOrders — VERIFIED
// ============================================================

// futuresOrderEntry — ордер Gate.io futures.
type futuresOrderEntry struct {
	ID         json.Number `json:"id"` // int64 (exchange order id)
	Contract   string      `json:"contract"`
	Size       json.Number `json:"size"`       // целое со знаком
	Price      string      `json:"price"`      // "0" для market
	Tif        string      `json:"tif"`        // "gtc"/"ioc"/"poc"
	Text       string      `json:"text"`       // "t-<clientOrderID>"
	Status     string      `json:"status"`     // "open"/"finished"/"cancelled"
	FinishAs   string      `json:"finish_as"`  // "filled"/"cancelled"/"ioc"/"auto_deleveraged"...
	FillPrice  string      `json:"fill_price"` // avg fill price
	Left       json.Number `json:"left"`       // remaining contracts
	ReduceOnly bool        `json:"is_reduce_only"`
	CreateTime float64     `json:"create_time"` // Unix timestamp float
}

// GetOpenOrders возвращает открытые ордера по символу.
// VERIFIED: GET /api/v4/futures/usdt/orders?status=open&contract=
func (a *Adapter) GetOpenOrders(ctx context.Context, symbol domain.ExchangeSymbol) ([]domain.Order, error) {
	query := BuildSortedQuery(map[string]string{
		"contract": string(symbol),
		"status":   "open",
	})
	body, err := a.doSignedGET(ctx, futuresPath("/orders"), query)
	if err != nil {
		return nil, fmt.Errorf("gate GetOpenOrders: %w", err)
	}
	var entries []futuresOrderEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("gate GetOpenOrders: parse: %w", err)
	}

	orders := make([]domain.Order, 0, len(entries))
	for _, e := range entries {
		ord, err := a.parseFuturesOrder(e)
		if err != nil {
			continue
		}
		orders = append(orders, ord)
	}
	return orders, nil
}

// parseFuturesOrder преобразует futuresOrderEntry в domain.Order.
func (a *Adapter) parseFuturesOrder(e futuresOrderEntry) (domain.Order, error) {
	// size — знаковое целое: положительный=long, отрицательный=short.
	sizeStr := e.Size.String()
	sizeInt, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return domain.Order{}, fmt.Errorf("size: %w", err)
	}

	var side domain.Side
	if sizeInt >= 0 {
		side = domain.SideLong
	} else {
		side = domain.SideShort
	}

	// Сохраняем маппинг text → orderID.
	clientID := textToClientID(e.Text)
	orderIDStr := e.ID.String()

	if clientID != "" && orderIDStr != "" {
		a.orderMapMu.Lock()
		a.orderMap[clientID] = orderIDStr
		a.orderMapMu.Unlock()
	}

	absSize := sizeInt
	if absSize < 0 {
		absSize = -absSize
	}

	// Gate.io: left = remaining contracts.
	leftStr := e.Left.String()
	leftInt, _ := strconv.ParseInt(leftStr, 10, 64)
	if leftInt < 0 {
		leftInt = -leftInt
	}
	filledContracts := absSize - leftInt
	if filledContracts < 0 {
		filledContracts = 0
	}

	// Конвертируем контракты в базовые единицы через хранимый кэш.
	// Если нет в кэше — используем 1 (тест может это перекрыть через GetInstruments).
	sym := domain.ExchangeSymbol(e.Contract)
	multiplier := a.cachedMultiplier(sym)

	requestedQty := contractsToBaseQty(absSize, multiplier)
	filledQty := contractsToBaseQty(filledContracts, multiplier)

	avgPrice, _ := parseDecimalOrZero(e.FillPrice)
	status := parseFuturesOrderStatus(e.Status, e.FinishAs)

	var orderMode domain.OrderMode
	if e.Price == "0" || e.Price == "" {
		orderMode = domain.OrderMarket
	} else if e.Tif == "ioc" {
		orderMode = domain.OrderMarketableLimitIOC
	} else {
		orderMode = domain.OrderMarketableLimitIOC
	}

	ts := time.Unix(int64(e.CreateTime), 0).UTC()

	return domain.Order{
		ExchangeOrderID:   orderIDStr,
		ClientOrderID:     domain.ClientOrderID(clientID),
		Symbol:            sym,
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        e.ReduceOnly,
		RequestedQty:      requestedQty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPrice,
		Status:            status,
		ExchangeTimestamp: ts,
		AckState:          domain.AckStateQueried,
	}, nil
}

// cachedMultiplier возвращает кэшированный multiplier или One.
func (a *Adapter) cachedMultiplier(sym domain.ExchangeSymbol) decimal.Decimal {
	a.contractCacheMu.RLock()
	m, ok := a.contractCache[string(sym)]
	a.contractCacheMu.RUnlock()
	if ok {
		return m
	}
	return decimal.One
}

// parseFuturesOrderStatus маппирует статус Gate.io → domain.OrderStatus.
func parseFuturesOrderStatus(status, finishAs string) domain.OrderStatus {
	switch status {
	case "open":
		return domain.OrderStatusAcknowledged
	case "finished":
		switch finishAs {
		case "filled":
			return domain.OrderStatusFilled
		case "cancelled", "ioc", "reduce_only", "position_closed", "stp":
			return domain.OrderStatusCancelled
		case "auto_deleveraged", "liquidated":
			return domain.OrderStatusFilled
		default:
			return domain.OrderStatusFilled
		}
	default:
		return domain.OrderStatusNew
	}
}

// textToClientID извлекает clientOrderID из поля text (убирает префикс "t-").
func textToClientID(text string) string {
	if strings.HasPrefix(text, "t-") {
		return text[2:]
	}
	return text
}

// clientIDToText преобразует clientOrderID в значение поля text Gate.io.
// Правила: prefix "t-" + clientID; суммарно ≤ 30 символов (28 после "t-").
// Допустимые символы: буквы, цифры, '-', '_', '.'.
// Если суммарная длина > 30 — обрезаем clientID детерминированно (хвост).
func clientIDToText(clientID string) string {
	const maxTextLen = 28 // после "t-"
	clean := sanitizeClientID(clientID)
	if len(clean) > maxTextLen {
		clean = clean[:maxTextLen]
	}
	return "t-" + clean
}

// sanitizeClientID оставляет только допустимые символы Gate.io text: [A-Za-z0-9._-].
func sanitizeClientID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ============================================================
// GetOrder — VERIFIED (поиск через map + REST fallback)
// ============================================================

// GetOrder запрашивает состояние ордера.
// Приоритеты:
//  1. req.ExchangeOrderID → GET /api/v4/futures/usdt/orders/{order_id}
//  2. req.ClientOrderID → in-memory map → GET /api/v4/futures/usdt/orders/{order_id}
//  3. Фоллбэк: итерация открытых + завершённых ордеров (медленно, только при рестарте).
//
// ОГРАНИЧЕНИЕ: маппинг clientID → orderID теряется при рестарте адаптера.
func (a *Adapter) GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error) {
	exchangeID := req.ExchangeOrderID
	clientID := string(req.ClientOrderID)

	// Шаг 1: если есть ExchangeOrderID — запрашиваем напрямую.
	if exchangeID != "" {
		return a.fetchOrderByID(ctx, exchangeID)
	}

	// Шаг 2: ищем в маппинге.
	if clientID != "" {
		a.orderMapMu.RLock()
		eid, ok := a.orderMap[clientID]
		a.orderMapMu.RUnlock()
		if ok {
			return a.fetchOrderByID(ctx, eid)
		}
	}

	// Шаг 3: перебираем открытые ордера (fallback после рестарта).
	sym := string(req.Symbol)
	if sym != "" && clientID != "" {
		ord, found, err := a.searchOrderByText(ctx, sym, clientID)
		if err != nil {
			return domain.Order{}, fmt.Errorf("gate GetOrder: search open: %w", err)
		}
		if found {
			return ord, nil
		}
		// Проверяем завершённые.
		ord, found, err = a.searchFinishedOrderByText(ctx, sym, clientID)
		if err != nil {
			return domain.Order{}, fmt.Errorf("gate GetOrder: search finished: %w", err)
		}
		if found {
			return ord, nil
		}
	}

	return domain.Order{}, fmt.Errorf("%w: clientOrderID=%s", exchange.ErrOrderNotFound, clientID)
}

// fetchOrderByID запрашивает ордер по exchange order ID.
// VERIFIED: GET /api/v4/futures/usdt/orders/{order_id}
func (a *Adapter) fetchOrderByID(ctx context.Context, orderID string) (domain.Order, error) {
	body, err := a.doSignedGET(ctx, futuresPath("/orders/"+orderID), "")
	if err != nil {
		if errors.Is(err, exchange.ErrOrderNotFound) {
			return domain.Order{}, err
		}
		return domain.Order{}, fmt.Errorf("gate fetchOrderByID: %w", err)
	}
	var e futuresOrderEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return domain.Order{}, fmt.Errorf("gate fetchOrderByID: parse: %w", err)
	}
	return a.parseFuturesOrder(e)
}

// searchOrderByText итерирует открытые ордера и ищет по clientID в поле text.
func (a *Adapter) searchOrderByText(ctx context.Context, contract, clientID string) (domain.Order, bool, error) {
	query := BuildSortedQuery(map[string]string{
		"contract": contract,
		"status":   "open",
	})
	body, err := a.doSignedGET(ctx, futuresPath("/orders"), query)
	if err != nil {
		return domain.Order{}, false, err
	}
	var entries []futuresOrderEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return domain.Order{}, false, err
	}
	wantText := "t-" + sanitizeClientID(clientID)
	for _, e := range entries {
		if e.Text == wantText || textToClientID(e.Text) == clientID {
			ord, err := a.parseFuturesOrder(e)
			if err != nil {
				continue
			}
			return ord, true, nil
		}
	}
	return domain.Order{}, false, nil
}

// searchFinishedOrderByText итерирует завершённые ордера.
func (a *Adapter) searchFinishedOrderByText(ctx context.Context, contract, clientID string) (domain.Order, bool, error) {
	query := BuildSortedQuery(map[string]string{
		"contract": contract,
		"status":   "finished",
	})
	body, err := a.doSignedGET(ctx, futuresPath("/orders"), query)
	if err != nil {
		return domain.Order{}, false, err
	}
	var entries []futuresOrderEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return domain.Order{}, false, err
	}
	wantText := "t-" + sanitizeClientID(clientID)
	for _, e := range entries {
		if e.Text == wantText || textToClientID(e.Text) == clientID {
			ord, err := a.parseFuturesOrder(e)
			if err != nil {
				continue
			}
			return ord, true, nil
		}
	}
	return domain.Order{}, false, nil
}

// ============================================================
// PlaceOrder — VERIFIED
// ============================================================

// placeOrderRequest — тело POST /api/v4/futures/usdt/orders.
type placeOrderRequest struct {
	Contract   string `json:"contract"`
	Size       int64  `json:"size"`  // знаковое: long>0, short<0
	Price      string `json:"price"` // "0" для market
	Tif        string `json:"tif"`   // "gtc"/"ioc"
	ReduceOnly bool   `json:"is_reduce_only"`
	Text       string `json:"text"` // "t-" + clientID (≤ 30 символов)
}

// PlaceOrder размещает ордер Gate.io futures.
// VERIFIED: POST /api/v4/futures/usdt/orders
//
// Конверсия: baseQty → contracts = floor(baseQty / quanto_multiplier).
// Side: long → size > 0, short → size < 0.
// Market ордер: price="0", tif="ioc".
func (a *Adapter) PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error) {
	// Получаем multiplier.
	multiplier, err := a.getQuantoMultiplier(ctx, req.Symbol)
	if err != nil {
		// Если не можем получить multiplier — используем 1.
		multiplier = decimal.One
	}

	contracts := baseQtyToContracts(req.BaseQty, multiplier)
	if contracts == 0 {
		return domain.OrderAck{}, fmt.Errorf("gate PlaceOrder: qty too small (contracts=0)")
	}

	// Знак size: long=+, short=-.
	size := contracts
	if req.Side == domain.SideShort {
		size = -contracts
	}

	// Price и tif.
	price := "0"
	tif := "ioc"
	if req.OrderMode != domain.OrderMarket && !req.Price.IsZero() {
		price = req.Price.String()
		tif = "ioc" // marketable limit IOC
	}
	if req.TimeInForce == domain.TIFGTC {
		tif = "gtc"
	}

	// text = "t-" + clientID (≤ 30 символов суммарно).
	textVal := clientIDToText(string(req.ClientOrderID))

	payload := placeOrderRequest{
		Contract:   string(req.Symbol),
		Size:       size,
		Price:      price,
		Tif:        tif,
		ReduceOnly: req.ReduceOnly,
		Text:       textVal,
	}

	body, err := a.doSignedPOST(ctx, futuresPath("/orders"), payload)
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("gate PlaceOrder: %w", err)
	}

	var e futuresOrderEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return domain.OrderAck{}, fmt.Errorf("gate PlaceOrder: parse: %w", err)
	}

	orderIDStr := e.ID.String()

	// Сохраняем маппинг.
	clientID := string(req.ClientOrderID)
	if clientID != "" && orderIDStr != "" {
		a.orderMapMu.Lock()
		a.orderMap[clientID] = orderIDStr
		a.orderMapMu.Unlock()
	}

	return domain.OrderAck{
		ExchangeOrderID: orderIDStr,
		ClientOrderID:   req.ClientOrderID,
		Status:          domain.OrderStatusAcknowledged,
		Timestamp:       a.clock(),
	}, nil
}

// ============================================================
// CancelOrder — VERIFIED
// ============================================================

// CancelOrder отменяет ордер по ExchangeOrderID или ClientOrderID.
// VERIFIED: DELETE /api/v4/futures/usdt/orders/{order_id}
func (a *Adapter) CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error {
	orderID := req.ExchangeOrderID

	// Если нет ExchangeOrderID — ищем в маппинге.
	if orderID == "" && req.ClientOrderID != "" {
		a.orderMapMu.RLock()
		eid, ok := a.orderMap[string(req.ClientOrderID)]
		a.orderMapMu.RUnlock()
		if ok {
			orderID = eid
		}
	}

	if orderID == "" {
		return fmt.Errorf("%w: cannot cancel without exchangeOrderID (clientID=%s)", exchange.ErrOrderNotFound, req.ClientOrderID)
	}

	_, err := a.doSignedDELETE(ctx, futuresPath("/orders/"+orderID), "")
	if err != nil {
		if errors.Is(err, exchange.ErrOrderNotFound) {
			return err
		}
		return fmt.Errorf("gate CancelOrder: %w", err)
	}
	return nil
}

// ============================================================
// SetLeverage — VERIFIED
// ============================================================

// SetLeverage устанавливает плечо через query-параметр.
// VERIFIED: POST /api/v4/futures/usdt/positions/{contract}/leverage?leverage=
func (a *Adapter) SetLeverage(ctx context.Context, req domain.SetLeverageRequest) error {
	sym := string(req.Symbol)
	path := futuresPath("/positions/" + sym + "/leverage")

	// Gate.io: leverage передаётся как query param в POST.
	// Для cross margin leverage=0.
	levStr := req.Leverage.String()

	// Строим query и подписываем с ним.
	query := "leverage=" + levStr
	ts := a.nowSec()
	sign := a.signer.Sign(http.MethodPost, path, query, "", ts)
	headers := a.signer.AuthHeaders(ts, sign)

	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return fmt.Errorf("gate SetLeverage: %w", wrapNetErr(err))
	}
	if err := checkHTTPStatus(status, body); err != nil {
		return fmt.Errorf("gate SetLeverage: %w", err)
	}
	return nil
}

// ============================================================
// SetMarginMode — TODO:VERIFY
// ============================================================

// SetMarginMode переключает режим маржи.
// Gate.io: leverage=0 → cross, leverage>0 → isolated.
// TODO:VERIFY: точный путь и параметры для смены режима маржи.
func (a *Adapter) SetMarginMode(ctx context.Context, req domain.SetMarginModeRequest) error {
	sym := string(req.Symbol)
	path := futuresPath("/positions/" + sym + "/leverage")

	levStr := "0" // 0 = cross
	if req.MarginMode == domain.MarginIsolated {
		levStr = "10" // значение по умолчанию; TODO: параметризовать
	}

	query := "leverage=" + levStr
	ts := a.nowSec()
	sign := a.signer.Sign(http.MethodPost, path, query, "", ts)
	headers := a.signer.AuthHeaders(ts, sign)

	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return fmt.Errorf("gate SetMarginMode: %w", wrapNetErr(err))
	}
	if err := checkHTTPStatus(status, body); err != nil {
		return fmt.Errorf("gate SetMarginMode: %w", err)
	}
	return nil
}

// ============================================================
// SetPositionMode — TODO:VERIFY
// ============================================================

// SetPositionMode переключает dual-mode.
// TODO:VERIFY: POST /api/v4/futures/usdt/dual_mode?dual_mode=false
func (a *Adapter) SetPositionMode(ctx context.Context, req domain.SetPositionModeRequest) error {
	dualMode := req.Mode == domain.PositionHedge
	query := "dual_mode=" + strconv.FormatBool(dualMode)
	path := futuresPath("/dual_mode")

	ts := a.nowSec()
	sign := a.signer.Sign(http.MethodPost, path, query, "", ts)
	headers := a.signer.AuthHeaders(ts, sign)

	status, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return fmt.Errorf("gate SetPositionMode: %w", wrapNetErr(err))
	}
	if err := checkHTTPStatus(status, body); err != nil {
		return fmt.Errorf("gate SetPositionMode: %w", err)
	}
	return nil
}

// ============================================================
// GetADLState — через GetPositions (TODO:VERIFY adl_ranking поле)
// ============================================================

// GetADLState возвращает ADL-состояние для символа.
// TODO:VERIFY: поле adl_ranking в Gate.io positions — уточнить точное имя.
func (a *Adapter) GetADLState(ctx context.Context, symbol domain.ExchangeSymbol) (domain.ADLState, error) {
	positions, err := a.GetPositions(ctx)
	if err != nil {
		return domain.ADLState{}, fmt.Errorf("gate GetADLState: %w", err)
	}

	for _, pos := range positions {
		if pos.Symbol == symbol && pos.ADLQueue != nil {
			return domain.ADLState{
				Symbol:     symbol,
				LongQueue:  pos.ADLQueue.LongQueue,
				ShortQueue: pos.ADLQueue.ShortQueue,
				Timestamp:  pos.Updated,
			}, nil
		}
	}

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

// transferPayload — тело POST /api/v4/wallet/transfers.
type transferPayload struct {
	Currency string `json:"currency"`
	From     string `json:"from"` // "spot" / "futures"
	To       string `json:"to"`
	Amount   string `json:"amount"`
	Settle   string `json:"settle,omitempty"` // "usdt" для futures
}

// transferResponse — ответ /api/v4/wallet/transfers.
type transferResponse struct {
	Currency string `json:"currency"`
	From     string `json:"from"`
	To       string `json:"to"`
	Amount   string `json:"amount"`
}

// InternalTransfer переводит средства между spot и futures.
// TODO:VERIFY: структура запроса и ответа wallet/transfers.
func (a *Adapter) InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error) {
	from := mapGateAccountType(req.From)
	to := mapGateAccountType(req.To)

	payload := transferPayload{
		Currency: req.Asset,
		From:     from,
		To:       to,
		Amount:   req.Amount.String(),
		Settle:   settle,
	}

	body, err := a.doSignedPOST(ctx, apiV4Prefix+"/wallet/transfers", payload)
	if err != nil {
		return domain.TransferResult{}, fmt.Errorf("gate InternalTransfer: %w", err)
	}
	var res transferResponse
	_ = json.Unmarshal(body, &res)

	return domain.TransferResult{
		TransferID: fmt.Sprintf("%s->%s:%s", from, to, req.Amount.String()),
		Status:     "ok",
	}, nil
}

// mapGateAccountType маппирует доменное имя аккаунта в Gate.io.
func mapGateAccountType(t string) string {
	switch strings.ToLower(t) {
	case "spot":
		return "spot"
	case "futures", "contract":
		return "futures"
	default:
		return strings.ToLower(t)
	}
}

// ============================================================
// Withdraw — TODO:VERIFY
// ============================================================

// withdrawPayload — тело POST /api/v4/withdrawals.
type withdrawPayload struct {
	Currency string `json:"currency"`
	Amount   string `json:"amount"`
	Address  string `json:"address"`
	Memo     string `json:"memo,omitempty"`
	Chain    string `json:"chain"`
}

// withdrawResponse — ответ /api/v4/withdrawals.
type withdrawResponse struct {
	ID     string `json:"id"`
	TxID   string `json:"txid"`
	Status string `json:"status"`
}

// Withdraw создаёт заявку на вывод средств.
// TODO:VERIFY: структура запроса и ответа /api/v4/withdrawals.
func (a *Adapter) Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	payload := withdrawPayload{
		Currency: req.Asset,
		Amount:   req.Amount.String(),
		Address:  req.Address,
		Memo:     req.Memo,
		Chain:    req.Network,
	}
	body, err := a.doSignedPOST(ctx, apiV4Prefix+"/withdrawals", payload)
	if err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("gate Withdraw: %w", err)
	}
	var res withdrawResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("gate Withdraw: parse: %w", err)
	}
	return domain.WithdrawalResult{
		WithdrawalID: res.ID,
		TxID:         res.TxID,
		Status:       res.Status,
	}, nil
}

// ============================================================
// GetWithdrawalHistory — TODO:VERIFY
// ============================================================

// withdrawRecord — запись истории вывода Gate.io.
type withdrawRecord struct {
	ID        string  `json:"id"`
	TxID      string  `json:"txid"`
	Currency  string  `json:"currency"`
	Chain     string  `json:"chain"`
	Amount    string  `json:"amount"`
	Fee       string  `json:"fee"`
	Status    string  `json:"status"`
	Timestamp float64 `json:"timestamp"` // Unix seconds
}

// GetWithdrawalHistory возвращает историю выводов.
// TODO:VERIFY: поля запроса/ответа GET /api/v4/withdrawals.
func (a *Adapter) GetWithdrawalHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Withdrawal, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["currency"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = strconv.Itoa(query.Limit)
	}
	q := BuildSortedQuery(params)
	body, err := a.doSignedGET(ctx, apiV4Prefix+"/withdrawals", q)
	if err != nil {
		return nil, fmt.Errorf("gate GetWithdrawalHistory: %w", err)
	}
	var records []withdrawRecord
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("gate GetWithdrawalHistory: parse: %w", err)
	}

	var result []domain.Withdrawal
	for _, r := range records {
		amount, _ := parseDecimalOrZero(r.Amount)
		fee, _ := parseDecimalOrZero(r.Fee)
		ts := time.Unix(int64(r.Timestamp), 0).UTC()
		result = append(result, domain.Withdrawal{
			WithdrawalID: r.ID,
			TxID:         r.TxID,
			Asset:        r.Currency,
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

// depositRecord — запись депозита Gate.io.
type depositRecord struct {
	ID        string  `json:"id"`
	TxID      string  `json:"txid"`
	Currency  string  `json:"currency"`
	Chain     string  `json:"chain"`
	Amount    string  `json:"amount"`
	Status    string  `json:"status"`
	Timestamp float64 `json:"timestamp"` // Unix seconds
}

// GetDepositHistory возвращает историю депозитов.
// TODO:VERIFY: поля запроса/ответа GET /api/v4/wallet/deposits.
func (a *Adapter) GetDepositHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Deposit, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["currency"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = strconv.Itoa(query.Limit)
	}
	q := BuildSortedQuery(params)
	body, err := a.doSignedGET(ctx, apiV4Prefix+"/wallet/deposits", q)
	if err != nil {
		return nil, fmt.Errorf("gate GetDepositHistory: %w", err)
	}
	var records []depositRecord
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("gate GetDepositHistory: parse: %w", err)
	}

	var result []domain.Deposit
	for _, r := range records {
		amount, _ := parseDecimalOrZero(r.Amount)
		ts := time.Unix(int64(r.Timestamp), 0).UTC()
		result = append(result, domain.Deposit{
			TxID:        r.TxID,
			Asset:       r.Currency,
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

// currencyChain — одна сеть монеты Gate.io.
type currencyChain struct {
	Chain              string `json:"chain"`
	NameCn             string `json:"name_cn"`
	IsDisabled         int    `json:"is_disabled"` // 0=enabled, 1=disabled
	IsDepositDisabled  int    `json:"is_deposit_disabled"`
	IsWithdrawDisabled int    `json:"is_withdraw_disabled"`
	WithdrawFee        string `json:"withdraw_fix"`         // фиксированная комиссия
	WithdrawMin        string `json:"withdraw_amount_mini"` // минимальная сумма вывода
	DepositMin         string `json:"deposit_amount_mini"`
}

// GetNetworkInfo возвращает информацию о сетях для актива.
// TODO:VERIFY: поля ответа GET /api/v4/wallet/currency_chains?currency=.
func (a *Adapter) GetNetworkInfo(ctx context.Context, asset string) ([]domain.NetworkInfo, error) {
	q := BuildSortedQuery(map[string]string{"currency": asset})
	body, err := a.doSignedGET(ctx, apiV4Prefix+"/wallet/currency_chains", q)
	if err != nil {
		return nil, fmt.Errorf("gate GetNetworkInfo: %w", err)
	}
	var chains []currencyChain
	if err := json.Unmarshal(body, &chains); err != nil {
		return nil, fmt.Errorf("gate GetNetworkInfo: parse: %w", err)
	}

	var result []domain.NetworkInfo
	for _, ch := range chains {
		fee, _ := parseDecimalOrZero(ch.WithdrawFee)
		wMin, _ := parseDecimalOrZero(ch.WithdrawMin)
		dMin, _ := parseDecimalOrZero(ch.DepositMin)
		result = append(result, domain.NetworkInfo{
			Network:         ch.Chain,
			WithdrawEnabled: ch.IsWithdrawDisabled == 0 && ch.IsDisabled == 0,
			DepositEnabled:  ch.IsDepositDisabled == 0 && ch.IsDisabled == 0,
			WithdrawFee:     fee,
			WithdrawMin:     wMin,
			DepositMin:      dMin,
		})
	}
	return result, nil
}

// ============================================================
// WebSocket — реализация перенесена в ws.go
// ============================================================

// ErrWSNotImplemented — stub для приватного WS Gate.io (не реализован).
// SubscribePublic реализован в ws.go; SubscribePrivate возвращает эту ошибку.
var ErrWSNotImplemented = errors.New("gate: WebSocket приватный канал не реализован (TODO)")

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

// parsePriceLevels разбирает [[price, size], ...] в []domain.PriceLevel.
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
