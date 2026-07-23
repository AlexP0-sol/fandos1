// Package bybit реализует адаптер биржи Bybit V5 для linear USDT perpetual.
//
// Все REST-запросы используют общий конверт V5:
//
//	{"retCode":0,"retMsg":"OK","result":{...},"time":1234567890}
//
// retCode != 0 маппируется в sentinel-ошибки через mapAPIError.
// Аутентификация — исключительно через заголовки X-BAPI-* (V5 не использует query-параметры для подписи).
//
// VERIFIED (Bybit V5 official docs, 2026-07):
//   - GET /v5/market/time
//   - GET /v5/market/instruments-info?category=linear  (pagination via nextPageCursor)
//   - GET /v5/market/tickers?category=linear&symbol=
//   - GET /v5/market/orderbook?category=linear&symbol=&limit=
//   - GET /v5/account/wallet-balance?accountType=UNIFIED
//   - GET /v5/position/list?category=linear&settleCoin=USDT
//   - GET /v5/order/realtime?category=linear&symbol=
//   - POST /v5/order/create
//   - POST /v5/order/cancel
//   - POST /v5/position/set-leverage  (retCode 110043 → success)
//   - POST /v5/position/switch-mode   (retCode 110025 → success)
//   - POST /v5/position/switch-isolated (TODO:VERIFY — unified accounts may differ)
//   - POST /v5/asset/transfer/inter-transfer  (TODO:VERIFY)
//   - POST /v5/asset/withdraw/create          (TODO:VERIFY)
//   - GET  /v5/asset/withdraw/query-record    (TODO:VERIFY)
//   - GET  /v5/asset/deposit/query-record     (TODO:VERIFY)
//   - GET  /v5/asset/coin/query-info          (TODO:VERIFY)
//
// Confidence policy для GetFunding:
//   - HIGH   — до следующего funding < 30 мин
//   - MEDIUM — до следующего funding < 4 ч
//   - LOW    — иначе
package bybit

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

// Config — параметры адаптера Bybit.
type Config struct {
	RESTBaseURL  string           // default: https://api.bybit.com
	WSPublicURL  string           // default: wss://stream.bybit.com/v5/public/linear
	WSPrivateURL string           // default: wss://stream.bybit.com/v5/private
	Signer       *Signer          // обязательно (для приватных запросов)
	HTTPDoer     HTTPDoer         // обязательно
	RecvWindowMs int64            // default: 5000
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
	Safe    bool
}

// defaultRESTBase — адрес production-сервера Bybit V5.
const defaultRESTBase = "https://api.bybit.com"

// defaultWSPublic — публичный WebSocket.
const defaultWSPublic = "wss://stream.bybit.com/v5/public/linear"

// defaultWSPrivate — приватный WebSocket.
const defaultWSPrivate = "wss://stream.bybit.com/v5/private"

// defaultRecvWindowMs — окно приёма (мс) по умолчанию.
const defaultRecvWindowMs = int64(5000)

// Adapter — реализация exchange.ExchangeAdapter для Bybit V5.
type Adapter struct {
	restBase     string
	wsPublicURL  string
	wsPrivateURL string
	signer       *Signer
	http         HTTPDoer
	recvWindow   int64
	clock        func() time.Time
}

// New создаёт адаптер Bybit из конфига. Паникует при обязательных полях == nil.
func New(cfg Config) *Adapter {
	if cfg.Signer == nil {
		panic("bybit: Signer обязателен")
	}
	if cfg.HTTPDoer == nil {
		panic("bybit: HTTPDoer обязателен")
	}
	a := &Adapter{
		restBase:     cfg.RESTBaseURL,
		wsPublicURL:  cfg.WSPublicURL,
		wsPrivateURL: cfg.WSPrivateURL,
		signer:       cfg.Signer,
		http:         cfg.HTTPDoer,
		recvWindow:   cfg.RecvWindowMs,
		clock:        cfg.Clock,
	}
	if a.restBase == "" {
		a.restBase = defaultRESTBase
	}
	if a.wsPublicURL == "" {
		a.wsPublicURL = defaultWSPublic
	}
	if a.wsPrivateURL == "" {
		a.wsPrivateURL = defaultWSPrivate
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
func (a *Adapter) ID() domain.ExchangeID { return domain.ExchangeBybit }

// ============================================================
// Общий конверт V5
// ============================================================

// v5Envelope — универсальная обёртка ответов Bybit V5.
type v5Envelope struct {
	RetCode int64           `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
	Time    int64           `json:"time"`
}

// decodeEnvelope декодирует V5-конверт и проверяет retCode.
// При retCode != 0 возвращает mapAPIError(retCode, retMsg).
func decodeEnvelope(body []byte) (v5Envelope, error) {
	var env v5Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("bybit: не удалось разобрать конверт: %w", err)
	}
	if env.RetCode != 0 {
		return env, mapAPIError(env.RetCode, env.RetMsg)
	}
	return env, nil
}

// mapAPIError маппирует retCode Bybit V5 в sentinel-ошибки.
// Документация: https://bybit-exchange.github.io/docs/v5/error
func mapAPIError(code int64, msg string) error {
	switch code {
	case 10003, 10004, 33004:
		// Ключ недействителен / неверная подпись / IP не в белом списке
		return fmt.Errorf("%w: retCode=%d %s", exchange.ErrUnauthorized, code, msg)
	case 10006:
		// Превышен rate limit
		return fmt.Errorf("%w: retCode=%d %s", exchange.ErrRateLimited, code, msg)
	case 110007:
		// Insufficient margin
		return fmt.Errorf("%w: retCode=%d %s", exchange.ErrInsufficientMargin, code, msg)
	case 10001:
		// Параметр ошибочен; может содержать symbol
		if strings.Contains(strings.ToLower(msg), "symbol") {
			return fmt.Errorf("%w: retCode=%d %s", exchange.ErrInvalidSymbol, code, msg)
		}
		return fmt.Errorf("bybit: invalid param retCode=%d %s", code, msg)
	case 110001:
		// Ордер не найден
		return fmt.Errorf("%w: retCode=%d %s", exchange.ErrOrderNotFound, code, msg)
	case 110043:
		// "leverage not modified" — ставим как успех (caller игнорирует ошибку)
		return errLeverageNotModified
	case 110025:
		// "position mode is not modified"
		return errPositionModeNotModified
	default:
		return fmt.Errorf("bybit: API error retCode=%d: %s", code, msg)
	}
}

// errLeverageNotModified — внутренняя ошибка, обозначающая retCode=110043.
var errLeverageNotModified = errors.New("bybit: leverage not modified (110043)")

// errPositionModeNotModified — внутренняя ошибка, обозначающая retCode=110025.
var errPositionModeNotModified = errors.New("bybit: position mode not modified (110025)")

// ============================================================
// Вспомогательные функции HTTP
// ============================================================

// nowMs — текущее время в миллисекундах (использует clock адаптера).
func (a *Adapter) nowMs() int64 { return a.clock().UnixMilli() }

// doPublicGET — GET без аутентификации.
func (a *Adapter) doPublicGET(ctx context.Context, path, query string) (v5Envelope, error) {
	_, body, err := a.http.Do(ctx, HTTPRequest{
		Method: http.MethodGet,
		Path:   path,
		Query:  query,
		Safe:   true,
	})
	if err != nil {
		return v5Envelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(body)
}

// doSignedGET — GET с аутентификацией через заголовки X-BAPI-*.
func (a *Adapter) doSignedGET(ctx context.Context, path, query string) (v5Envelope, error) {
	ts := a.nowMs()
	_, sig := a.signer.SignGet(ts, a.recvWindow, query)
	headers := a.signer.AuthHeaders(ts, a.recvWindow, sig)

	_, body, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodGet,
		Path:    path,
		Query:   query,
		Headers: headers,
		Safe:    true,
	})
	if err != nil {
		return v5Envelope{}, wrapNetErr(err)
	}
	return decodeEnvelope(body)
}

// doSignedPOST — POST с JSON-телом и аутентификацией. Safe=false (ордера).
func (a *Adapter) doSignedPOST(ctx context.Context, path string, payload interface{}) (v5Envelope, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return v5Envelope{}, fmt.Errorf("bybit: marshal: %w", err)
	}
	bodyStr := string(bodyBytes)

	ts := a.nowMs()
	_, sig := a.signer.SignPost(ts, a.recvWindow, bodyStr)
	headers := a.signer.AuthHeaders(ts, a.recvWindow, sig)
	headers["Content-Type"] = "application/json"

	_, respBody, err := a.http.Do(ctx, HTTPRequest{
		Method:  http.MethodPost,
		Path:    path,
		Body:    bytes.NewReader(bodyBytes),
		Headers: headers,
		Safe:    false,
	})
	if err != nil {
		return v5Envelope{}, wrapNetErr(err)
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

// serverTimeResult — результат /v5/market/time.
type serverTimeResult struct {
	TimeSecond string `json:"timeSecond"`
	TimeNano   string `json:"timeNano"`
}

// GetServerTime возвращает серверное время Bybit.
// VERIFIED: GET /v5/market/time
func (a *Adapter) GetServerTime(ctx context.Context) (time.Time, error) {
	env, err := a.doPublicGET(ctx, "/v5/market/time", "")
	if err != nil {
		return time.Time{}, fmt.Errorf("bybit GetServerTime: %w", err)
	}
	var res serverTimeResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return time.Time{}, fmt.Errorf("bybit GetServerTime: parse result: %w", err)
	}
	// Используем timeNano для максимальной точности.
	nanos, err := decimal.FromString(res.TimeNano)
	if err != nil {
		return time.Time{}, fmt.Errorf("bybit GetServerTime: parse timeNano: %w", err)
	}
	// timeNano — наносекунды Unix.
	nsInt := nanos.Underlying().IntPart()
	return time.Unix(nsInt/1e9, nsInt%1e9).UTC(), nil
}

// ============================================================
// GetInstruments — VERIFIED (pagination via nextPageCursor)
// ============================================================

// instrumentsResult — ответ /v5/market/instruments-info.
type instrumentsResult struct {
	Category       string            `json:"category"`
	NextPageCursor string            `json:"nextPageCursor"`
	List           []instrumentEntry `json:"list"`
}

// instrumentEntry — один инструмент в ответе.
type instrumentEntry struct {
	Symbol             string         `json:"symbol"`
	ContractType       string         `json:"contractType"`
	Status             string         `json:"status"`
	BaseCoin           string         `json:"baseCoin"`
	QuoteCoin          string         `json:"quoteCoin"`
	SettleCoin         string         `json:"settleCoin"`
	FundingInterval    int64          `json:"fundingInterval"` // в минутах
	LotSizeFilter      lotSizeFilter  `json:"lotSizeFilter"`
	PriceFilter        priceFilter    `json:"priceFilter"`
	LeverageFilter     leverageFilter `json:"leverageFilter"`
	UnifiedMarginTrade bool           `json:"unifiedMarginTrade"`
}

type lotSizeFilter struct {
	QtyStep     string `json:"qtyStep"`
	MinOrderQty string `json:"minOrderQty"`
	MaxOrderQty string `json:"maxOrderQty"`
}

type priceFilter struct {
	TickSize string `json:"tickSize"`
	MinPrice string `json:"minPrice"`
	MaxPrice string `json:"maxPrice"`
}

type leverageFilter struct {
	MinLeverage  string `json:"minLeverage"`
	MaxLeverage  string `json:"maxLeverage"`
	LeverageStep string `json:"leverageStep"`
}

// GetInstruments возвращает все торгуемые USDT-perpetual инструменты.
// VERIFIED: GET /v5/market/instruments-info?category=linear (pagination via nextPageCursor)
func (a *Adapter) GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error) {
	var result []domain.CanonicalInstrument
	cursor := ""

	for {
		params := map[string]string{"category": "linear", "limit": "1000"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		query := BuildSortedQuery(params)

		env, err := a.doPublicGET(ctx, "/v5/market/instruments-info", query)
		if err != nil {
			return nil, fmt.Errorf("bybit GetInstruments: %w", err)
		}

		var res instrumentsResult
		if err := json.Unmarshal(env.Result, &res); err != nil {
			return nil, fmt.Errorf("bybit GetInstruments: parse: %w", err)
		}

		for _, e := range res.List {
			// Фильтруем: только USDT quoteCoin и статус Trading
			if e.QuoteCoin != "USDT" || e.Status != "Trading" {
				continue
			}
			instr, err := parseInstrument(e)
			if err != nil {
				// Логируем и пропускаем инструменты с некорректными данными
				continue
			}
			result = append(result, instr)
		}

		if res.NextPageCursor == "" {
			break
		}
		cursor = res.NextPageCursor
	}

	return result, nil
}

// parseInstrument преобразует instrumentEntry в domain.CanonicalInstrument.
func parseInstrument(e instrumentEntry) (domain.CanonicalInstrument, error) {
	qtyStep, err := decimal.FromString(e.LotSizeFilter.QtyStep)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("qtyStep: %w", err)
	}
	minQty, err := decimal.FromString(e.LotSizeFilter.MinOrderQty)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("minOrderQty: %w", err)
	}
	tickSize, err := decimal.FromString(e.PriceFilter.TickSize)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("tickSize: %w", err)
	}
	maxLev, err := decimal.FromString(e.LeverageFilter.MaxLeverage)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("maxLeverage: %w", err)
	}

	// fundingInterval в минутах → в секундах
	fundingIntervalSec := e.FundingInterval * 60

	status := domain.InstrumentStatusActive
	if e.Status != "Trading" {
		status = domain.InstrumentStatusDelisted
	}

	return domain.CanonicalInstrument{
		Exchange:           domain.ExchangeBybit,
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
		SupportsADL:        true,
		Status:             status,
	}, nil
}

// ============================================================
// GetFunding / GetTicker — одна точка вызова tickers
// ============================================================

// tickerEntry — поля тикера в ответе /v5/market/tickers.
type tickerEntry struct {
	Symbol          string `json:"symbol"`
	Bid1Price       string `json:"bid1Price"`
	Ask1Price       string `json:"ask1Price"`
	LastPrice       string `json:"lastPrice"`
	MarkPrice       string `json:"markPrice"`
	IndexPrice      string `json:"indexPrice"`
	Turnover24h     string `json:"turnover24h"`
	FundingRate     string `json:"fundingRate"`
	NextFundingTime string `json:"nextFundingTime"` // в миллисекундах
}

// tickersResult — список тикеров.
type tickersResult struct {
	Category string        `json:"category"`
	List     []tickerEntry `json:"list"`
}

// fetchTicker — общий метод получения тикера символа.
// VERIFIED: GET /v5/market/tickers?category=linear&symbol=
func (a *Adapter) fetchTicker(ctx context.Context, symbol domain.ExchangeSymbol) (tickerEntry, error) {
	query := BuildSortedQuery(map[string]string{
		"category": "linear",
		"symbol":   string(symbol),
	})
	env, err := a.doPublicGET(ctx, "/v5/market/tickers", query)
	if err != nil {
		return tickerEntry{}, fmt.Errorf("bybit fetchTicker %s: %w", symbol, err)
	}
	var res tickersResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return tickerEntry{}, fmt.Errorf("bybit fetchTicker %s: parse: %w", symbol, err)
	}
	if len(res.List) == 0 {
		return tickerEntry{}, fmt.Errorf("%w: %s", exchange.ErrInvalidSymbol, symbol)
	}
	return res.List[0], nil
}

// GetFunding возвращает нормализованную funding-информацию.
// VERIFIED: GET /v5/market/tickers?category=linear&symbol=
//
// Confidence policy:
//   - HIGH   — до nextFundingTime < 30 мин
//   - MEDIUM — до nextFundingTime < 4 ч
//   - LOW    — иначе
func (a *Adapter) GetFunding(ctx context.Context, symbol domain.ExchangeSymbol) (domain.FundingInfo, error) {
	entry, err := a.fetchTicker(ctx, symbol)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("bybit GetFunding: %w", err)
	}

	rate, err := decimal.FromString(entry.FundingRate)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("bybit GetFunding: fundingRate: %w", err)
	}

	// nextFundingTime — миллисекунды UTC
	nextMs, err := decimal.FromString(entry.NextFundingTime)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("bybit GetFunding: nextFundingTime: %w", err)
	}
	nextFunding := time.UnixMilli(nextMs.Underlying().IntPart()).UTC()

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
		PredictedFundingRate: rate, // Bybit V5 tickers даёт только текущий rate
		RateType:             domain.FundingRatePredicted,
		FundingIntervalSec:   8 * 3600, // по умолчанию 8 ч; TODO: брать из GetInstruments
		NextFundingTime:      nextFunding,
		Confidence:           confidence,
		FundingPriceType:     domain.FundingPriceMark,
	}, nil
}

// GetTicker возвращает нормализованный тикер (Level 2).
// VERIFIED: GET /v5/market/tickers?category=linear&symbol=
func (a *Adapter) GetTicker(ctx context.Context, symbol domain.ExchangeSymbol) (domain.Ticker, error) {
	entry, err := a.fetchTicker(ctx, symbol)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bybit GetTicker: %w", err)
	}

	lastPrice, err := parseDecimalOrZero(entry.LastPrice)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bybit GetTicker: lastPrice: %w", err)
	}
	markPrice, err := parseDecimalOrZero(entry.MarkPrice)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bybit GetTicker: markPrice: %w", err)
	}
	indexPrice, err := parseDecimalOrZero(entry.IndexPrice)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bybit GetTicker: indexPrice: %w", err)
	}
	volume, err := parseDecimalOrZero(entry.Turnover24h)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("bybit GetTicker: turnover24h: %w", err)
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

// orderbookResult — ответ /v5/market/orderbook.
type orderbookResult struct {
	Symbol string     `json:"s"`
	Bids   [][]string `json:"b"` // [[price, qty], ...]
	Asks   [][]string `json:"a"`
	Ts     int64      `json:"ts"`
	Seq    int64      `json:"seq"`
}

// GetOrderBookSnapshot возвращает снимок стакана.
// VERIFIED: GET /v5/market/orderbook?category=linear&symbol=&limit=
func (a *Adapter) GetOrderBookSnapshot(ctx context.Context, symbol domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error) {
	query := BuildSortedQuery(map[string]string{
		"category": "linear",
		"symbol":   string(symbol),
		"limit":    fmt.Sprintf("%d", depth),
	})
	env, err := a.doPublicGET(ctx, "/v5/market/orderbook", query)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("bybit GetOrderBookSnapshot: %w", err)
	}
	var res orderbookResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("bybit GetOrderBookSnapshot: parse: %w", err)
	}

	bids, err := parsePriceLevels(res.Bids)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("bybit GetOrderBookSnapshot: bids: %w", err)
	}
	asks, err := parsePriceLevels(res.Asks)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("bybit GetOrderBookSnapshot: asks: %w", err)
	}

	return domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeBybit,
		Symbol:     symbol,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  time.UnixMilli(res.Ts).UTC(),
		Sequence:   res.Seq,
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
// GetBalances — VERIFIED
// ============================================================

// walletBalanceResult — ответ /v5/account/wallet-balance.
type walletBalanceResult struct {
	List []walletAccount `json:"list"`
}

type walletAccount struct {
	AccountType string       `json:"accountType"`
	Coin        []walletCoin `json:"coin"`
}

type walletCoin struct {
	Coin                string `json:"coin"`
	WalletBalance       string `json:"walletBalance"`
	AvailableToWithdraw string `json:"availableToWithdraw"`
	// Для UNIFIED также присутствует availableBalance
	AvailableBalance string `json:"availableBalance"`
}

// GetBalances возвращает балансы UNIFIED-аккаунта.
// VERIFIED: GET /v5/account/wallet-balance?accountType=UNIFIED
func (a *Adapter) GetBalances(ctx context.Context) ([]domain.Balance, error) {
	query := BuildSortedQuery(map[string]string{"accountType": "UNIFIED"})
	env, err := a.doSignedGET(ctx, "/v5/account/wallet-balance", query)
	if err != nil {
		return nil, fmt.Errorf("bybit GetBalances: %w", err)
	}
	var res walletBalanceResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return nil, fmt.Errorf("bybit GetBalances: parse: %w", err)
	}

	var balances []domain.Balance
	for _, acc := range res.List {
		for _, c := range acc.Coin {
			wallet, err := parseDecimalOrZero(c.WalletBalance)
			if err != nil {
				continue
			}
			// Для UNIFIED preferAvailableBalance, fallback → availableToWithdraw
			avail, err := parseDecimalOrZero(c.AvailableBalance)
			if err != nil || avail.IsZero() {
				avail, _ = parseDecimalOrZero(c.AvailableToWithdraw)
			}
			balances = append(balances, domain.Balance{
				Asset:            c.Coin,
				WalletBalance:    wallet,
				AvailableBalance: avail,
			})
		}
	}
	return balances, nil
}

// ============================================================
// GetPositions — VERIFIED
// ============================================================

// positionListResult — ответ /v5/position/list.
type positionListResult struct {
	Category       string          `json:"category"`
	List           []positionEntry `json:"list"`
	NextPageCursor string          `json:"nextPageCursor"`
}

type positionEntry struct {
	Symbol           string `json:"symbol"`
	Side             string `json:"side"`
	Size             string `json:"size"`
	EntryPrice       string `json:"entryPrice"`
	MarkPrice        string `json:"markPrice"`
	LiqPrice         string `json:"liqPrice"`
	UnrealisedPnl    string `json:"unrealisedPnl"`
	TradeMode        int    `json:"tradeMode"` // 0=cross, 1=isolated
	Leverage         string `json:"leverage"`
	PositionIM       string `json:"positionIM"`
	AdlRankIndicator int    `json:"adlRankIndicator"` // 0..5
	UpdatedTime      string `json:"updatedTime"`
}

// GetPositions возвращает все открытые позиции.
// VERIFIED: GET /v5/position/list?category=linear&settleCoin=USDT
func (a *Adapter) GetPositions(ctx context.Context) ([]domain.Position, error) {
	query := BuildSortedQuery(map[string]string{
		"category":   "linear",
		"settleCoin": "USDT",
	})
	env, err := a.doSignedGET(ctx, "/v5/position/list", query)
	if err != nil {
		return nil, fmt.Errorf("bybit GetPositions: %w", err)
	}
	var res positionListResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return nil, fmt.Errorf("bybit GetPositions: parse: %w", err)
	}

	var positions []domain.Position
	for _, e := range res.List {
		size, err := decimal.FromString(e.Size)
		if err != nil || size.IsZero() {
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
	qty, err := decimal.FromString(e.Size)
	if err != nil {
		return domain.Position{}, fmt.Errorf("size: %w", err)
	}
	entryPrice, err := parseDecimalOrZero(e.EntryPrice)
	if err != nil {
		return domain.Position{}, fmt.Errorf("entryPrice: %w", err)
	}
	markPrice, err := parseDecimalOrZero(e.MarkPrice)
	if err != nil {
		return domain.Position{}, fmt.Errorf("markPrice: %w", err)
	}
	liqPrice, err := parseDecimalOrZero(e.LiqPrice)
	if err != nil {
		return domain.Position{}, fmt.Errorf("liqPrice: %w", err)
	}
	pnl, err := parseDecimalOrZero(e.UnrealisedPnl)
	if err != nil {
		return domain.Position{}, fmt.Errorf("unrealisedPnl: %w", err)
	}
	leverage, err := parseDecimalOrZero(e.Leverage)
	if err != nil {
		return domain.Position{}, fmt.Errorf("leverage: %w", err)
	}
	margin, err := parseDecimalOrZero(e.PositionIM)
	if err != nil {
		return domain.Position{}, fmt.Errorf("positionIM: %w", err)
	}

	var side domain.Side
	switch e.Side {
	case "Buy":
		side = domain.SideLong
	case "Sell":
		side = domain.SideShort
	default:
		return domain.Position{}, fmt.Errorf("unknown side: %s", e.Side)
	}

	marginMode := domain.MarginCross
	if e.TradeMode == 1 {
		marginMode = domain.MarginIsolated
	}

	// adlRankIndicator [0..5] → нормализуем в [0,1] делением на 5
	adlNorm := decimal.FromInt(int64(e.AdlRankIndicator)).Div(decimal.FromInt(5))
	adlState := &domain.ADLQueuePosition{
		LongQueue:  adlNorm,
		ShortQueue: adlNorm,
	}

	updatedTime := time.Time{}
	if e.UpdatedTime != "" {
		ms, err := decimal.FromString(e.UpdatedTime)
		if err == nil {
			updatedTime = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
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
		ADLQueue:         adlState,
		Updated:          updatedTime,
	}, nil
}

// ============================================================
// GetOpenOrders — VERIFIED
// ============================================================

// GetOpenOrders возвращает открытые ордера по символу.
// VERIFIED: GET /v5/order/realtime?category=linear&symbol=
func (a *Adapter) GetOpenOrders(ctx context.Context, symbol domain.ExchangeSymbol) ([]domain.Order, error) {
	query := BuildSortedQuery(map[string]string{
		"category": "linear",
		"symbol":   string(symbol),
	})
	env, err := a.doSignedGET(ctx, "/v5/order/realtime", query)
	if err != nil {
		return nil, fmt.Errorf("bybit GetOpenOrders: %w", err)
	}
	var res orderListResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return nil, fmt.Errorf("bybit GetOpenOrders: parse: %w", err)
	}

	orders := make([]domain.Order, 0, len(res.List))
	for _, e := range res.List {
		ord, err := parseOrder(e)
		if err != nil {
			continue
		}
		orders = append(orders, ord)
	}
	return orders, nil
}

// ============================================================
// GetOrder — VERIFIED (realtime fallback to history)
// ============================================================

// orderListResult — список ордеров (realtime или history).
type orderListResult struct {
	Category       string       `json:"category"`
	List           []orderEntry `json:"list"`
	NextPageCursor string       `json:"nextPageCursor"`
}

// orderEntry — поля одного ордера.
type orderEntry struct {
	OrderId     string `json:"orderId"`
	OrderLinkId string `json:"orderLinkId"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	OrderType   string `json:"orderType"`
	Qty         string `json:"qty"`
	CumExecQty  string `json:"cumExecQty"`
	AvgPrice    string `json:"avgPrice"`
	CumExecFee  string `json:"cumExecFee"`
	OrderStatus string `json:"orderStatus"`
	ReduceOnly  bool   `json:"reduceOnly"`
	CreatedTime string `json:"createdTime"`
	TimeInForce string `json:"timeInForce"`
}

// parseOrder преобразует orderEntry в domain.Order.
func parseOrder(e orderEntry) (domain.Order, error) {
	qty, err := decimal.FromString(e.Qty)
	if err != nil {
		return domain.Order{}, fmt.Errorf("qty: %w", err)
	}
	filledQty, err := parseDecimalOrZero(e.CumExecQty)
	if err != nil {
		return domain.Order{}, fmt.Errorf("cumExecQty: %w", err)
	}
	avgPrice, err := parseDecimalOrZero(e.AvgPrice)
	if err != nil {
		return domain.Order{}, fmt.Errorf("avgPrice: %w", err)
	}
	fees, err := parseDecimalOrZero(e.CumExecFee)
	if err != nil {
		return domain.Order{}, fmt.Errorf("cumExecFee: %w", err)
	}

	var side domain.Side
	switch e.Side {
	case "Buy":
		side = domain.SideLong
	case "Sell":
		side = domain.SideShort
	default:
		return domain.Order{}, fmt.Errorf("unknown side: %s", e.Side)
	}

	status := parseOrderStatus(e.OrderStatus)

	var orderMode domain.OrderMode
	switch e.OrderType {
	case "Market":
		orderMode = domain.OrderMarket
	default:
		orderMode = domain.OrderMarketableLimitIOC
	}

	ts := time.Time{}
	if e.CreatedTime != "" {
		ms, err := decimal.FromString(e.CreatedTime)
		if err == nil {
			ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	return domain.Order{
		ExchangeOrderID:   e.OrderId,
		ClientOrderID:     domain.ClientOrderID(e.OrderLinkId),
		Symbol:            domain.ExchangeSymbol(e.Symbol),
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        e.ReduceOnly,
		RequestedQty:      qty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPrice,
		Fees:              fees,
		Status:            status,
		ExchangeTimestamp: ts,
		AckState:          domain.AckStateQueried,
	}, nil
}

// parseOrderStatus маппирует строковый статус Bybit → domain.OrderStatus.
func parseOrderStatus(s string) domain.OrderStatus {
	switch s {
	case "New":
		return domain.OrderStatusAcknowledged
	case "PartiallyFilled":
		return domain.OrderStatusPartiallyFilled
	case "Filled":
		return domain.OrderStatusFilled
	case "Cancelled":
		return domain.OrderStatusCancelled
	case "Rejected":
		return domain.OrderStatusRejected
	case "Deactivated":
		return domain.OrderStatusExpired
	default:
		return domain.OrderStatusNew
	}
}

// GetOrder запрашивает состояние ордера по clientOrderID.
// Сначала смотрит /v5/order/realtime, при пустом списке — /v5/order/history.
// VERIFIED: GET /v5/order/realtime?category=linear&orderLinkId=
// VERIFIED: GET /v5/order/history?category=linear&orderLinkId=
func (a *Adapter) GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error) {
	clientID := string(req.ClientOrderID)
	orderID := req.ExchangeOrderID
	sym := string(req.Symbol)

	// Шаг 1 — realtime (открытые/недавно закрытые ордера).
	params := map[string]string{"category": "linear"}
	if clientID != "" {
		params["orderLinkId"] = clientID
	}
	if orderID != "" {
		params["orderId"] = orderID
	}
	if sym != "" {
		params["symbol"] = sym
	}
	query := BuildSortedQuery(params)

	env, err := a.doSignedGET(ctx, "/v5/order/realtime", query)
	if err != nil {
		return domain.Order{}, fmt.Errorf("bybit GetOrder (realtime): %w", err)
	}
	var res orderListResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return domain.Order{}, fmt.Errorf("bybit GetOrder (realtime): parse: %w", err)
	}
	if len(res.List) > 0 {
		return parseOrder(res.List[0])
	}

	// Шаг 2 — history (завершённые/отменённые ордера).
	env, err = a.doSignedGET(ctx, "/v5/order/history", query)
	if err != nil {
		return domain.Order{}, fmt.Errorf("bybit GetOrder (history): %w", err)
	}
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return domain.Order{}, fmt.Errorf("bybit GetOrder (history): parse: %w", err)
	}
	if len(res.List) > 0 {
		return parseOrder(res.List[0])
	}

	return domain.Order{}, fmt.Errorf("%w: clientOrderID=%s", exchange.ErrOrderNotFound, clientID)
}

// ============================================================
// SetLeverage — VERIFIED
// ============================================================

// SetLeverage устанавливает плечо для символа.
// VERIFIED: POST /v5/position/set-leverage
// retCode 110043 "leverage not modified" → успех (уже установлено нужное плечо).
func (a *Adapter) SetLeverage(ctx context.Context, req domain.SetLeverageRequest) error {
	body := map[string]interface{}{
		"category":     "linear",
		"symbol":       string(req.Symbol),
		"buyLeverage":  req.Leverage.String(),
		"sellLeverage": req.Leverage.String(),
	}
	_, err := a.doSignedPOST(ctx, "/v5/position/set-leverage", body)
	if errors.Is(err, errLeverageNotModified) {
		return nil // уже установлено
	}
	if err != nil {
		return fmt.Errorf("bybit SetLeverage: %w", err)
	}
	return nil
}

// ============================================================
// SetMarginMode — TODO:VERIFY
// ============================================================

// SetMarginMode переключает режим маржи.
// TODO:VERIFY — для UNIFIED-аккаунтов путь может быть /v5/account/set-margin-mode.
// Текущая реализация использует /v5/position/switch-isolated (isolated per-symbol для линейного).
// Документация Bybit V5 указывает на /v5/account/set-margin-mode для UNIFIED.
func (a *Adapter) SetMarginMode(ctx context.Context, req domain.SetMarginModeRequest) error {
	// TODO:VERIFY — UNIFIED аккаунт использует /v5/account/set-margin-mode с tradeMode
	// linear perpetual используют /v5/position/switch-isolated (0=cross, 1=isolated)
	tradeMode := 0
	if req.MarginMode == domain.MarginIsolated {
		tradeMode = 1
	}
	body := map[string]interface{}{
		"category":     "linear",
		"symbol":       string(req.Symbol),
		"tradeMode":    tradeMode,
		"buyLeverage":  "10", // требуется при переключении в isolated; TODO: параметризовать
		"sellLeverage": "10",
	}
	_, err := a.doSignedPOST(ctx, "/v5/position/switch-isolated", body)
	if err != nil {
		return fmt.Errorf("bybit SetMarginMode: %w", err)
	}
	return nil
}

// ============================================================
// SetPositionMode — VERIFIED
// ============================================================

// SetPositionMode переключает режим позиций (one-way/hedge).
// VERIFIED: POST /v5/position/switch-mode
// retCode 110025 "position mode not modified" → успех.
func (a *Adapter) SetPositionMode(ctx context.Context, req domain.SetPositionModeRequest) error {
	mode := 0 // one-way
	if req.Mode == domain.PositionHedge {
		mode = 3 // hedge (both sides)
	}
	body := map[string]interface{}{
		"category": "linear",
		"mode":     mode,
	}
	_, err := a.doSignedPOST(ctx, "/v5/position/switch-mode", body)
	if errors.Is(err, errPositionModeNotModified) {
		return nil // уже в нужном режиме
	}
	if err != nil {
		return fmt.Errorf("bybit SetPositionMode: %w", err)
	}
	return nil
}

// ============================================================
// PlaceOrder — VERIFIED
// ============================================================

// placeOrderResponse — поля result при создании ордера.
type placeOrderResponse struct {
	OrderId     string `json:"orderId"`
	OrderLinkId string `json:"orderLinkId"`
}

// PlaceOrder размещает ордер. Safe=false обязательно.
// VERIFIED: POST /v5/order/create
func (a *Adapter) PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error) {
	// Маппинг side: Long → Buy, Short → Sell
	side := "Buy"
	if req.Side == domain.SideShort {
		side = "Sell"
	}

	// Тип ордера
	orderType := "Limit"
	if req.OrderMode == domain.OrderMarket {
		orderType = "Market"
	}

	// timeInForce
	tif := string(req.TimeInForce)
	if tif == "" {
		if req.OrderMode == domain.OrderMarketableLimitIOC {
			tif = "IOC"
		} else if req.OrderMode == domain.OrderMarket {
			tif = "IOC"
		} else {
			tif = "GTC"
		}
	}

	body := map[string]interface{}{
		"category":    "linear",
		"symbol":      string(req.Symbol),
		"side":        side,
		"orderType":   orderType,
		"qty":         req.BaseQty.String(),
		"timeInForce": tif,
		"reduceOnly":  req.ReduceOnly,
		"orderLinkId": string(req.ClientOrderID),
	}

	// Цена для limit-ордеров
	if orderType == "Limit" && !req.Price.IsZero() {
		body["price"] = req.Price.String()
	}

	env, err := a.doSignedPOST(ctx, "/v5/order/create", body)
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("bybit PlaceOrder: %w", err)
	}

	var res placeOrderResponse
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return domain.OrderAck{}, fmt.Errorf("bybit PlaceOrder: parse: %w", err)
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
// VERIFIED: POST /v5/order/cancel
// retCode 110001 "order not exists" → ErrOrderNotFound.
func (a *Adapter) CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error {
	body := map[string]interface{}{
		"category": "linear",
		"symbol":   string(req.Symbol),
	}
	if req.ExchangeOrderID != "" {
		body["orderId"] = req.ExchangeOrderID
	}
	if req.ClientOrderID != "" {
		body["orderLinkId"] = string(req.ClientOrderID)
	}

	_, err := a.doSignedPOST(ctx, "/v5/order/cancel", body)
	if errors.Is(err, exchange.ErrOrderNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("bybit CancelOrder: %w", err)
	}
	return nil
}

// ============================================================
// GetADLState — через GetPositions (VERIFIED)
// ============================================================

// GetADLState возвращает ADL-состояние для символа на основе GetPositions.
// VERIFIED: adlRankIndicator приходит в /v5/position/list (docs corrected).
func (a *Adapter) GetADLState(ctx context.Context, symbol domain.ExchangeSymbol) (domain.ADLState, error) {
	positions, err := a.GetPositions(ctx)
	if err != nil {
		return domain.ADLState{}, fmt.Errorf("bybit GetADLState: %w", err)
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

	// Позиция не найдена — ADL не актуален
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

// transferResult — поля ответа на перевод.
type transferResult struct {
	TransferId string `json:"transferId"`
	Status     string `json:"status"`
}

// InternalTransfer — внутренний перевод между аккаунтами.
// TODO:VERIFY: структура полей запроса и ответа.
func (a *Adapter) InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error) {
	transferID := generateUUID()
	body := map[string]interface{}{
		"transferId":      transferID,
		"coin":            req.Asset,
		"amount":          req.Amount.String(),
		"fromAccountType": mapAccountType(req.From),
		"toAccountType":   mapAccountType(req.To),
	}
	env, err := a.doSignedPOST(ctx, "/v5/asset/transfer/inter-transfer", body)
	if err != nil {
		return domain.TransferResult{}, fmt.Errorf("bybit InternalTransfer: %w", err)
	}
	var res transferResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return domain.TransferResult{}, fmt.Errorf("bybit InternalTransfer: parse: %w", err)
	}
	return domain.TransferResult{TransferID: res.TransferId, Status: res.Status}, nil
}

// mapAccountType маппирует доменное название аккаунта в Bybit V5.
func mapAccountType(t string) string {
	switch strings.ToLower(t) {
	case "spot":
		return "SPOT"
	case "futures", "contract":
		return "CONTRACT"
	case "unified":
		return "UNIFIED"
	default:
		return strings.ToUpper(t)
	}
}

// ============================================================
// Withdraw — TODO:VERIFY
// ============================================================

// withdrawResult — ответ на создание вывода.
type withdrawResult struct {
	Id string `json:"id"`
}

// Withdraw создаёт заявку на вывод средств.
// TODO:VERIFY: поля forceChain, accountType актуальны для V5.
func (a *Adapter) Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	body := map[string]interface{}{
		"coin":        req.Asset,
		"chain":       req.Network,
		"address":     req.Address,
		"amount":      req.Amount.String(),
		"timestamp":   a.nowMs(),
		"accountType": "FUND",
	}
	if req.Memo != "" {
		body["tag"] = req.Memo
	}
	env, err := a.doSignedPOST(ctx, "/v5/asset/withdraw/create", body)
	if err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("bybit Withdraw: %w", err)
	}
	var res withdrawResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("bybit Withdraw: parse: %w", err)
	}
	return domain.WithdrawalResult{WithdrawalID: res.Id, Status: "submitted"}, nil
}

// ============================================================
// GetWithdrawalHistory — TODO:VERIFY
// ============================================================

// withdrawRecord — одна запись истории вывода.
type withdrawRecord struct {
	WithdrawId  string `json:"withdrawId"`
	TxID        string `json:"txID"`
	Coin        string `json:"coin"`
	Chain       string `json:"chain"`
	Amount      string `json:"amount"`
	WithdrawFee string `json:"withdrawFee"`
	Status      string `json:"status"`
	CreateTime  string `json:"createTime"`
}

// withdrawHistoryResult — результат запроса истории выводов.
type withdrawHistoryResult struct {
	Rows           []withdrawRecord `json:"rows"`
	NextPageCursor string           `json:"nextPageCursor"`
}

// GetWithdrawalHistory возвращает историю выводов.
// TODO:VERIFY: поля запроса/ответа.
func (a *Adapter) GetWithdrawalHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Withdrawal, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["coin"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", query.Limit)
	}
	q := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/v5/asset/withdraw/query-record", q)
	if err != nil {
		return nil, fmt.Errorf("bybit GetWithdrawalHistory: %w", err)
	}
	var res withdrawHistoryResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return nil, fmt.Errorf("bybit GetWithdrawalHistory: parse: %w", err)
	}

	var result []domain.Withdrawal
	for _, r := range res.Rows {
		amount, _ := parseDecimalOrZero(r.Amount)
		fee, _ := parseDecimalOrZero(r.WithdrawFee)
		ts := time.Time{}
		if r.CreateTime != "" {
			ms, err := decimal.FromString(r.CreateTime)
			if err == nil {
				ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}
		result = append(result, domain.Withdrawal{
			WithdrawalID: r.WithdrawId,
			TxID:         r.TxID,
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

// depositRecord — одна запись депозита.
type depositRecord struct {
	TxID      string `json:"txID"`
	Coin      string `json:"coin"`
	Chain     string `json:"chain"`
	Amount    string `json:"amount"`
	Status    int    `json:"status"`
	SuccessAt string `json:"successAt"`
}

// depositHistoryResult — результат запроса истории депозитов.
type depositHistoryResult struct {
	Rows []depositRecord `json:"rows"`
}

// GetDepositHistory возвращает историю депозитов.
// TODO:VERIFY: поля запроса/ответа.
func (a *Adapter) GetDepositHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Deposit, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["coin"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", query.Limit)
	}
	q := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/v5/asset/deposit/query-record", q)
	if err != nil {
		return nil, fmt.Errorf("bybit GetDepositHistory: %w", err)
	}
	var res depositHistoryResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return nil, fmt.Errorf("bybit GetDepositHistory: parse: %w", err)
	}

	var result []domain.Deposit
	for _, r := range res.Rows {
		amount, _ := parseDecimalOrZero(r.Amount)
		ts := time.Time{}
		if r.SuccessAt != "" {
			ms, err := decimal.FromString(r.SuccessAt)
			if err == nil {
				ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}
		result = append(result, domain.Deposit{
			TxID:        r.TxID,
			Asset:       r.Coin,
			Network:     r.Chain,
			Amount:      amount,
			Status:      fmt.Sprintf("%d", r.Status),
			ConfirmedAt: ts,
		})
	}
	return result, nil
}

// ============================================================
// GetNetworkInfo — TODO:VERIFY
// ============================================================

// coinQueryResult — ответ /v5/asset/coin/query-info.
type coinQueryResult struct {
	Rows []coinInfoEntry `json:"rows"`
}

// coinInfoEntry — информация по монете.
type coinInfoEntry struct {
	Coin   string       `json:"coin"`
	Chains []chainEntry `json:"chains"`
}

// chainEntry — информация по одной сети.
type chainEntry struct {
	ChainType         string `json:"chainType"`
	WithdrawEnable    int    `json:"withdrawEnable"` // 1=enabled
	DepositEnable     int    `json:"depositEnable"`  // 1=enabled
	WithdrawFee       string `json:"withdrawFee"`
	WithdrawMinAmount string `json:"withdrawMinAmount"`
	DepositMin        string `json:"depositMin"`
}

// GetNetworkInfo возвращает информацию о сетях для актива.
// TODO:VERIFY: поля запроса/ответа.
func (a *Adapter) GetNetworkInfo(ctx context.Context, asset string) ([]domain.NetworkInfo, error) {
	q := BuildSortedQuery(map[string]string{"coin": asset})
	env, err := a.doSignedGET(ctx, "/v5/asset/coin/query-info", q)
	if err != nil {
		return nil, fmt.Errorf("bybit GetNetworkInfo: %w", err)
	}
	var res coinQueryResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		return nil, fmt.Errorf("bybit GetNetworkInfo: parse: %w", err)
	}

	var result []domain.NetworkInfo
	for _, coin := range res.Rows {
		if coin.Coin != asset {
			continue
		}
		for _, ch := range coin.Chains {
			fee, _ := parseDecimalOrZero(ch.WithdrawFee)
			wMin, _ := parseDecimalOrZero(ch.WithdrawMinAmount)
			dMin, _ := parseDecimalOrZero(ch.DepositMin)
			result = append(result, domain.NetworkInfo{
				Network:         ch.ChainType,
				WithdrawEnabled: ch.WithdrawEnable == 1,
				DepositEnabled:  ch.DepositEnable == 1,
				WithdrawFee:     fee,
				WithdrawMin:     wMin,
				DepositMin:      dMin,
			})
		}
	}
	return result, nil
}

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

// generateUUID генерирует простой UUID v4 (без crypto/rand для простоты).
// TODO: в production использовать google/uuid или crypto/rand.
func generateUUID() string {
	// Используем clock-based timestamp + random suffix
	ts := time.Now().UnixNano()
	return fmt.Sprintf("%016x-0000-4000-8000-%012x", ts, ts&0xffffffffffff)
}
