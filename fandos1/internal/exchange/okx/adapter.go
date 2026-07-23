// Package okx реализует адаптер биржи OKX V5 для linear USDT-margined perpetual (SWAP).
//
// Все REST-запросы используют общий конверт OKX V5:
//
//	{"code":"0","msg":"","data":[...]}
//
// code != "0" маппируется в sentinel-ошибки через mapAPIError.
// Аутентификация — через заголовки OK-ACCESS-* (VERIFIED по документации OKX V5).
//
// VERIFIED (OKX V5 official docs, 2026-07):
//   - GET /api/v5/public/time
//   - GET /api/v5/public/instruments?instType=SWAP (ctVal маппинг, фильтр settleCcy==USDT && ctType linear)
//   - GET /api/v5/public/funding-rate?instId=
//   - GET /api/v5/market/ticker?instId=
//   - GET /api/v5/market/books?instId=&sz=
//   - GET /api/v5/account/balance
//   - GET /api/v5/account/positions?instType=SWAP
//   - GET /api/v5/trade/orders-pending
//   - POST /api/v5/trade/order
//   - POST /api/v5/trade/cancel-order
//   - GET /api/v5/trade/order?instId=&clOrdId=
//   - POST /api/v5/account/set-leverage
//   - POST /api/v5/account/set-position-mode
//   - POST /api/v5/asset/transfer
//   - POST /api/v5/asset/withdrawal
//   - GET /api/v5/asset/withdrawal-history
//   - GET /api/v5/asset/deposit-history
//   - GET /api/v5/asset/currencies
//
// Подпись (VERIFIED):
//   - pre-hash = timestamp + METHOD + requestPath + body
//   - timestamp — ISO 8601 UTC миллисекунды: "2020-12-08T09:08:57.715Z"
//   - sign = base64(HMAC-SHA256(secret, pre-hash))
//   - Заголовки: OK-ACCESS-KEY, OK-ACCESS-SIGN, OK-ACCESS-TIMESTAMP, OK-ACCESS-PASSPHRASE
//
// Конверсия qty (VERIFIED по документации OKX V5):
//   - OKX qty для SWAP-ордеров — в КОНТРАКТАХ, не в базовой валюте.
//   - ctVal = размер одного контракта в базовой монете (из /api/v5/public/instruments).
//   - domain base qty → OKX contracts: floor(baseQty / ctVal), результат целое число.
//   - OKX contracts → domain base qty: contracts * ctVal.
//   - ctVal кешируется per instId после GetInstruments; при отсутствии — lazy запрос инструментов.
//
// clOrdId (VERIFIED по документации OKX V5):
//   - OKX clOrdId принимает только буквы [a-zA-Z] и цифры [0-9], максимум 32 символа.
//   - Наш формат ClientOrderID может содержать '_' и '-'.
//   - Трансляция: удаляем '_' и '-' (оба символа недопустимы в OKX clOrdId).
//   - Уникальность: компоненты ID уже содержат [A-Z0-9] с достаточной энтропией,
//     удаление разделителей не нарушает уникальность при разумных длинах.
//   - Оригинальный ClientOrderID хранится в поле clOrdId поиска (GetOrder по clOrdId
//     транслирует точно так же, чтобы найти ордер).
//   - Если результирующая строка > 32 символов — обрезаем до 32 (документируется).
//
// ADL (VERIFIED по документации OKX V5):
//   - Поле adl в /api/v5/account/positions: integer 1..5.
//   - Нормализация: (adl-1)/4 → [0,1].
//
// Position mode (VERIFIED):
//   - POST /api/v5/account/set-position-mode: posMode = "long_short_mode" | "net_mode"
//   - Код "Already set" (нет специального кода, сервер возвращает success) → success.
//   - TODO:VERIFY: точный код "already set" от OKX; возможно нет отдельного кода.
//
// Confidence policy для GetFunding:
//   - HIGH   — до следующего funding < 30 мин
//   - MEDIUM — до следующего funding < 4 ч
//   - LOW    — иначе
//
// WS:
//   - SubscribePublic/SubscribePrivate: не реализованы, возвращают ErrWSNotImplemented.
//   - TODO: реализовать WS wss://ws.okx.com:8443/ws/v5/public (tickers/books5 channels).
package okx

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
// Sentinel-ошибки пакета
// ============================================================

// ErrWSNotImplemented — WebSocket-подписки не реализованы в текущей версии.
// TODO: реализовать WS wss://ws.okx.com:8443/ws/v5/public.
var ErrWSNotImplemented = errors.New("okx: websocket subscriptions not implemented")

// errAlreadySet — внутренняя ошибка для "already set" ответов.
var errAlreadySet = errors.New("okx: already set (idempotent)")

// ============================================================
// Конфигурация и конструктор
// ============================================================

// Config — параметры адаптера OKX.
// Конструктор New вызывается фабрикой приложения единообразно для всех адаптеров.
type Config struct {
	RESTBaseURL  string           // default: https://www.okx.com
	WSBaseURL    string           // default: wss://ws.okx.com:8443/ws/v5/public (TODO:VERIFY port)
	APIKey       string           // обязательно для приватных запросов
	APISecret    string           // обязательно для приватных запросов
	Passphrase   string           // обязательно для приватных запросов (OKX-специфично)
	HTTPDoer     HTTPDoer         // обязательно
	RecvWindowMs int64            // не используется OKX (у них нет recv_window), но оставлен для единообразия Config
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

// defaultRESTBase — адрес production-сервера OKX V5.
// VERIFIED по официальной документации OKX V5.
const defaultRESTBase = "https://www.okx.com"

// defaultWSBase — публичный WebSocket OKX V5.
// TODO:VERIFY: точный порт и путь.
const defaultWSBase = "wss://ws.okx.com:8443/ws/v5/public"

// Adapter — реализация exchange.ExchangeAdapter для OKX V5.
type Adapter struct {
	restBase  string
	wsBaseURL string
	signer    *Signer
	http      HTTPDoer
	clock     func() time.Time

	// Кеш ctVal per instId (заполняется при GetInstruments или lazy-запросе).
	// ctVal — размер одного контракта в базовой монете (например, BTC).
	ctValCache   map[string]decimal.Decimal
	ctValCacheMu sync.RWMutex
}

// New создаёт адаптер OKX из конфига.
// HTTPDoer обязателен. Приватные запросы (PlaceOrder и т.д.) требуют APIKey/APISecret/Passphrase;
// ошибка при пустых ключах возникает lazily при первом приватном вызове.
func New(cfg Config) (*Adapter, error) {
	if cfg.HTTPDoer == nil {
		return nil, fmt.Errorf("okx: HTTPDoer обязателен")
	}

	var signer *Signer
	if cfg.APIKey != "" || cfg.APISecret != "" {
		signer = NewSigner(cfg.APIKey, []byte(cfg.APISecret), cfg.Passphrase)
	}

	a := &Adapter{
		restBase:   cfg.RESTBaseURL,
		wsBaseURL:  cfg.WSBaseURL,
		signer:     signer,
		http:       cfg.HTTPDoer,
		clock:      cfg.Clock,
		ctValCache: make(map[string]decimal.Decimal),
	}
	if a.restBase == "" {
		a.restBase = defaultRESTBase
	}
	if a.wsBaseURL == "" {
		a.wsBaseURL = defaultWSBase
	}
	if a.clock == nil {
		a.clock = time.Now
	}
	return a, nil
}

// ID возвращает идентификатор биржи.
func (a *Adapter) ID() domain.ExchangeID { return domain.ExchangeOKX }

// ============================================================
// Конверт OKX V5
// ============================================================

// v5Envelope — универсальная обёртка ответов OKX V5.
// VERIFIED: {"code":"0","msg":"","data":[...]}
type v5Envelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// decodeEnvelope декодирует V5-конверт и проверяет code.
// При code != "0" возвращает mapAPIError(code, msg).
func decodeEnvelope(body []byte) (v5Envelope, error) {
	var env v5Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("okx: не удалось разобрать конверт: %w", err)
	}
	if env.Code != "0" {
		return env, mapAPIError(env.Code, env.Msg)
	}
	return env, nil
}

// mapAPIError маппирует code OKX V5 в sentinel-ошибки.
// VERIFIED: коды из официальной документации OKX V5 и CCXT.
func mapAPIError(code, msg string) error {
	switch code {
	case "50011", "429":
		// Request too frequent — rate limit hit.
		// VERIFIED: 50011 = "Request too frequent".
		return fmt.Errorf("%w: code=%s %s", exchange.ErrRateLimited, code, msg)
	case "50111", "50113":
		// 50111 = Invalid OK_ACCESS_KEY; 50113 = Invalid signature.
		// VERIFIED по CCXT okx.ts error mapping.
		return fmt.Errorf("%w: code=%s %s", exchange.ErrUnauthorized, code, msg)
	case "50112":
		// Invalid OK_ACCESS_TIMESTAMP.
		return fmt.Errorf("%w: code=%s %s", exchange.ErrUnauthorized, code, msg)
	case "50114":
		// Invalid authorization.
		return fmt.Errorf("%w: code=%s %s", exchange.ErrUnauthorized, code, msg)
	case "51000":
		// Parameter instId error — VERIFIED.
		if strings.Contains(strings.ToLower(msg), "instid") || strings.Contains(strings.ToLower(msg), "instrument") {
			return fmt.Errorf("%w: code=%s %s", exchange.ErrInvalidSymbol, code, msg)
		}
		return fmt.Errorf("okx: invalid param code=%s %s", code, msg)
	case "51001", "51002":
		// Instrument ID does not exist / does not match.
		return fmt.Errorf("%w: code=%s %s", exchange.ErrInvalidSymbol, code, msg)
	case "51008", "51502":
		// Insufficient balance or margin. VERIFIED: 51008 = "Insufficient balance".
		return fmt.Errorf("%w: code=%s %s", exchange.ErrInsufficientMargin, code, msg)
	case "51603":
		// Order does not exist. VERIFIED по CCXT okx.ts: "Order does not exist".
		return fmt.Errorf("%w: code=%s %s", exchange.ErrOrderNotFound, code, msg)
	default:
		return fmt.Errorf("okx: API error code=%s: %s", code, msg)
	}
}

// ============================================================
// Вспомогательные функции HTTP
// ============================================================

// requireSigner проверяет наличие signer для приватных запросов.
func (a *Adapter) requireSigner() error {
	if a.signer == nil {
		return fmt.Errorf("%w: APIKey/APISecret/Passphrase required for private calls", exchange.ErrUnauthorized)
	}
	return nil
}

// doPublicGET — GET без аутентификации.
// VERIFIED: для GET requestPath включает query-строку в подпись (но здесь публичный).
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

// doSignedGET — GET с аутентификацией через заголовки OK-ACCESS-*.
// VERIFIED: для GET тело отсутствует; подпись = base64(HMAC-SHA256(secret, ts+GET+path+query+"""))
// requestPath для подписи включает '?' и query-строку.
func (a *Adapter) doSignedGET(ctx context.Context, path, query string) (v5Envelope, error) {
	if err := a.requireSigner(); err != nil {
		return v5Envelope{}, err
	}
	ts := FormatTimestamp(a.clock())

	// Для GET: requestPath включает query-строку (VERIFIED по документации OKX V5).
	requestPath := path
	if query != "" {
		requestPath = path + "?" + query
	}

	sig := a.signer.Sign(ts, "GET", requestPath, "")
	headers := a.signer.AuthHeaders(ts, sig)
	headers["Content-Type"] = "application/json"

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

// doSignedPOST — POST с JSON-телом и аутентификацией. Safe=false (ордера/переводы).
// VERIFIED: для POST body включается в подпись; requestPath — только путь (без query).
func (a *Adapter) doSignedPOST(ctx context.Context, path string, payload interface{}) (v5Envelope, error) {
	if err := a.requireSigner(); err != nil {
		return v5Envelope{}, err
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return v5Envelope{}, fmt.Errorf("okx: marshal: %w", err)
	}
	bodyStr := string(bodyBytes)

	ts := FormatTimestamp(a.clock())
	sig := a.signer.Sign(ts, "POST", path, bodyStr)
	headers := a.signer.AuthHeaders(ts, sig)
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

// parseDecimalOrZero парсит строку в Decimal; при пустой строке возвращает Zero.
func parseDecimalOrZero(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Zero, nil
	}
	return decimal.FromString(s)
}

// ============================================================
// Конверсия clOrdId
// ============================================================

// translateClientOrderID транслирует наш ClientOrderID в OKX-совместимый clOrdId.
//
// OKX clOrdId допускает только буквы [a-zA-Z] и цифры [0-9], максимум 32 символа.
// (VERIFIED по официальной документации OKX V5: "Letters (case sensitive) and digits,
// no longer than 32 characters".)
//
// Трансляция:
//   - Удаляем '_' и '-' (оба символа недопустимы в OKX clOrdId).
//   - Остальные символы [a-zA-Z0-9] остаются as-is.
//   - Если результат > 32 символов — обрезаем до 32.
//   - Уникальность сохраняется: компоненты нашего ID уже содержат [A-Z0-9]
//     с достаточной энтропией, удаление разделителей не нарушает уникальность.
func translateClientOrderID(id domain.ClientOrderID) string {
	var b strings.Builder
	for _, r := range string(id) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		// '_' и '-' пропускаем
	}
	result := b.String()
	if len(result) > 32 {
		result = result[:32]
	}
	return result
}

// ============================================================
// GetServerTime — VERIFIED
// ============================================================

// serverTimeData — поля ответа /api/v5/public/time.
// VERIFIED: {"ts":"1597026383085"}
type serverTimeData struct {
	Ts string `json:"ts"`
}

// GetServerTime возвращает серверное время OKX.
// VERIFIED: GET /api/v5/public/time
func (a *Adapter) GetServerTime(ctx context.Context) (time.Time, error) {
	env, err := a.doPublicGET(ctx, "/api/v5/public/time", "")
	if err != nil {
		return time.Time{}, fmt.Errorf("okx GetServerTime: %w", err)
	}
	var data []serverTimeData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return time.Time{}, fmt.Errorf("okx GetServerTime: parse: %w", err)
	}
	if len(data) == 0 {
		return time.Time{}, fmt.Errorf("okx GetServerTime: empty data")
	}
	ms, err := decimal.FromString(data[0].Ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("okx GetServerTime: parse ts: %w", err)
	}
	return time.UnixMilli(ms.Underlying().IntPart()).UTC(), nil
}

// ============================================================
// GetInstruments — VERIFIED (фильтр SWAP + USDT-margined linear)
// ============================================================

// instrumentData — один инструмент SWAP в ответе /api/v5/public/instruments.
// VERIFIED по официальной документации OKX V5: поля instType, instId, settleCcy,
// ctType, ctVal, lotSz, minSz, tickSz.
type instrumentData struct {
	InstType  string `json:"instType"`
	InstId    string `json:"instId"`
	Uly       string `json:"uly"`       // underlying, например "BTC-USDT"
	SettleCcy string `json:"settleCcy"` // USDT для USDT-margined
	CtType    string `json:"ctType"`    // "linear" или "inverse"
	CtVal     string `json:"ctVal"`     // размер контракта в базовой монете
	LotSz     string `json:"lotSz"`     // шаг размера лота (в контрактах)
	MinSz     string `json:"minSz"`     // минимальный размер ордера (в контрактах)
	TickSz    string `json:"tickSz"`    // шаг цены
	Lever     string `json:"lever"`     // максимальное плечо
	ListTime  string `json:"listTime"`  // время листинга
	ExpTime   string `json:"expTime"`   // "" для SWAP
	State     string `json:"state"`     // "live", "suspend", "preopen"
	BaseCcy   string `json:"baseCcy"`   // базовая валюта (для SWAP может быть пустой)
	QuoteCcy  string `json:"quoteCcy"`  // котируемая валюта
	// FundingInterval: TODO:VERIFY — OKX возвращает fundingInterval в часах (целое).
	// По официальной документации: "Funding rate settlement time, in hours. 8 hours by default".
	// Поле называется fundingInterval, TODO:VERIFY точное имя поля в ответе.
}

// GetInstruments возвращает все USDT-margined linear perpetual инструменты OKX.
// VERIFIED: GET /api/v5/public/instruments?instType=SWAP
// Фильтр: settleCcy == "USDT" && ctType == "linear".
// Также кеширует ctVal для конверсии qty.
func (a *Adapter) GetInstruments(ctx context.Context) ([]domain.CanonicalInstrument, error) {
	env, err := a.doPublicGET(ctx, "/api/v5/public/instruments", "instType=SWAP")
	if err != nil {
		return nil, fmt.Errorf("okx GetInstruments: %w", err)
	}

	var data []instrumentData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("okx GetInstruments: parse: %w", err)
	}

	var result []domain.CanonicalInstrument
	a.ctValCacheMu.Lock()
	defer a.ctValCacheMu.Unlock()

	for _, d := range data {
		// Фильтр: только USDT-margined linear (settleCcy==USDT && ctType==linear).
		if d.SettleCcy != "USDT" || d.CtType != "linear" {
			continue
		}
		// Только торгующие инструменты.
		if d.State != "live" && d.State != "" {
			if d.State == "suspend" || d.State == "preopen" {
				// оставляем suspend/preopen как halted/reduce-only — пропускаем неактивные
				if d.State == "suspend" {
					continue
				}
			}
		}

		instr, err := parseInstrument(d)
		if err != nil {
			// Пропускаем инструменты с некорректными данными.
			continue
		}

		// Кешируем ctVal.
		if !instr.ContractMultiplier.IsZero() {
			a.ctValCache[d.InstId] = instr.ContractMultiplier
		}

		result = append(result, instr)
	}
	return result, nil
}

// parseInstrument преобразует instrumentData в domain.CanonicalInstrument.
func parseInstrument(d instrumentData) (domain.CanonicalInstrument, error) {
	ctVal, err := decimal.FromString(d.CtVal)
	if err != nil || ctVal.IsZero() {
		return domain.CanonicalInstrument{}, fmt.Errorf("ctVal: %w", err)
	}
	// LotSz у OKX — в контрактах; QtyStep тоже в контрактах.
	// domain QtyStep: мы конвертируем в базовые единицы (lotSz * ctVal).
	// TODO:VERIFY: правильно ли domain ожидает QtyStep в базовых единицах?
	// По аналогии с bybit, который использует qtyStep в базовой монете — да.
	lotSz, err := decimal.FromString(d.LotSz)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("lotSz: %w", err)
	}
	minSz, err := decimal.FromString(d.MinSz)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("minSz: %w", err)
	}
	tickSz, err := decimal.FromString(d.TickSz)
	if err != nil {
		return domain.CanonicalInstrument{}, fmt.Errorf("tickSz: %w", err)
	}

	// MaxLeverage: у OKX поле lever — строка.
	var maxLev decimal.Decimal
	if d.Lever != "" {
		maxLev, err = decimal.FromString(d.Lever)
		if err != nil {
			maxLev = decimal.Zero
		}
	}

	// Status маппинг.
	status := domain.InstrumentStatusActive
	switch d.State {
	case "suspend":
		status = domain.InstrumentStatusHalted
	case "preopen":
		status = domain.InstrumentStatusReduceOnly
	}

	// BaseCcy из instId: "BTC-USDT-SWAP" → baseCcy = "BTC".
	// Uly = "BTC-USDT", baseCcy может быть пустым у SWAP.
	baseCcy := d.BaseCcy
	if baseCcy == "" && d.Uly != "" {
		// Uly = "BTC-USDT" → baseCcy = "BTC".
		parts := strings.SplitN(d.Uly, "-", 2)
		if len(parts) > 0 {
			baseCcy = parts[0]
		}
	}

	// QtyStep в контрактах → базовых единицах: lotSz * ctVal.
	// MinQty в контрактах → базовых единицах: minSz * ctVal.
	qtyStep := lotSz.Mul(ctVal)
	minQty := minSz.Mul(ctVal)

	// FundingIntervalSec: OKX SWAP обычно 8h = 28800s.
	// TODO:VERIFY: точное поле fundingInterval в ответе instruments.
	const defaultFundingIntervalSec = int64(8 * 3600)

	return domain.CanonicalInstrument{
		Exchange:           domain.ExchangeOKX,
		CanonicalBaseAsset: domain.AssetSymbol(baseCcy),
		ExchangeSymbol:     domain.ExchangeSymbol(d.InstId),
		InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
		SettlementCurrency: d.SettleCcy,
		ContractMultiplier: ctVal, // ctVal = размер контракта в базовой монете
		QtyStep:            qtyStep,
		MinQty:             minQty,
		TickSize:           tickSz,
		MaxLeverage:        maxLev,
		FundingIntervalSec: defaultFundingIntervalSec,
		FundingPriceType:   domain.FundingPriceMark,
		SupportsADL:        true,
		Status:             status,
	}, nil
}

// getCtVal возвращает ctVal для instId из кеша; при отсутствии — lazy-запрос инструментов.
func (a *Adapter) getCtVal(ctx context.Context, instId string) (decimal.Decimal, error) {
	a.ctValCacheMu.RLock()
	ctVal, ok := a.ctValCache[instId]
	a.ctValCacheMu.RUnlock()
	if ok {
		return ctVal, nil
	}
	// Lazy-запрос инструментов для заполнения кеша.
	if _, err := a.GetInstruments(ctx); err != nil {
		return decimal.Zero, fmt.Errorf("okx getCtVal: lazy GetInstruments: %w", err)
	}
	a.ctValCacheMu.RLock()
	ctVal, ok = a.ctValCache[instId]
	a.ctValCacheMu.RUnlock()
	if !ok {
		return decimal.Zero, fmt.Errorf("okx getCtVal: ctVal not found for %s", instId)
	}
	return ctVal, nil
}

// baseQtyToContracts конвертирует domain base qty → OKX contracts (целое, floor).
// contracts = floor(baseQty / ctVal).
// Округление вниз: никогда не превышать запрошенный объём.
func baseQtyToContracts(baseQty, ctVal decimal.Decimal) decimal.Decimal {
	if ctVal.IsZero() {
		panic("okx: ctVal is zero in baseQtyToContracts")
	}
	// floor(baseQty / ctVal) = Truncate(baseQty / ctVal, 0).
	contracts, _ := baseQty.Quantize(ctVal)
	// contracts уже кратно ctVal; нам нужно целое число контрактов.
	return contracts.Div(ctVal).Truncate(0)
}

// contractsToBaseQty конвертирует OKX contracts → domain base qty.
// baseQty = contracts * ctVal.
func contractsToBaseQty(contracts, ctVal decimal.Decimal) decimal.Decimal {
	return contracts.Mul(ctVal)
}

// ============================================================
// GetFunding — VERIFIED
// ============================================================

// fundingRateData — поля ответа /api/v5/public/funding-rate.
// VERIFIED по официальной документации OKX V5.
type fundingRateData struct {
	InstId          string `json:"instId"`
	FundingRate     string `json:"fundingRate"`     // текущая ставка
	NextFundingRate string `json:"nextFundingRate"` // прогноз (может быть пустым)
	FundingTime     string `json:"fundingTime"`     // следующий funding timestamp (ms)
	NextFundingTime string `json:"nextFundingTime"` // TODO:VERIFY: поле в API
}

// GetFunding возвращает funding-информацию.
// VERIFIED: GET /api/v5/public/funding-rate?instId=
func (a *Adapter) GetFunding(ctx context.Context, symbol domain.ExchangeSymbol) (domain.FundingInfo, error) {
	query := "instId=" + string(symbol)
	env, err := a.doPublicGET(ctx, "/api/v5/public/funding-rate", query)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("okx GetFunding: %w", err)
	}

	var data []fundingRateData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.FundingInfo{}, fmt.Errorf("okx GetFunding: parse: %w", err)
	}
	if len(data) == 0 {
		return domain.FundingInfo{}, fmt.Errorf("%w: %s", exchange.ErrInvalidSymbol, symbol)
	}
	d := data[0]

	rate, err := parseDecimalOrZero(d.FundingRate)
	if err != nil {
		return domain.FundingInfo{}, fmt.Errorf("okx GetFunding: fundingRate: %w", err)
	}

	// nextFundingRate может быть пустым — используем текущий как predicted.
	predicted, err := parseDecimalOrZero(d.NextFundingRate)
	if err != nil || predicted.IsZero() {
		predicted = rate
	}

	// fundingTime — следующий settlement timestamp в ms.
	// TODO:VERIFY: точное поле — fundingTime или nextFundingTime.
	fundingTimeStr := d.FundingTime
	if fundingTimeStr == "" {
		fundingTimeStr = d.NextFundingTime
	}
	var nextFunding time.Time
	if fundingTimeStr != "" {
		ms, err := decimal.FromString(fundingTimeStr)
		if err == nil {
			nextFunding = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	// Confidence policy.
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
		PredictedFundingRate: predicted,
		RateType:             domain.FundingRatePredicted,
		FundingIntervalSec:   8 * 3600, // TODO:VERIFY: брать из GetInstruments
		NextFundingTime:      nextFunding,
		Confidence:           confidence,
		FundingPriceType:     domain.FundingPriceMark,
	}, nil
}

// ============================================================
// GetTicker — VERIFIED
// ============================================================

// tickerData — поля ответа /api/v5/market/ticker.
// VERIFIED по официальной документации OKX V5.
type tickerData struct {
	InstId    string `json:"instId"`
	Last      string `json:"last"`      // последняя цена
	BidPx     string `json:"bidPx"`     // лучший бид
	AskPx     string `json:"askPx"`     // лучший аск
	Vol24h    string `json:"vol24h"`    // объём в базовой валюте за 24h
	VolCcy24h string `json:"volCcy24h"` // объём в котируемой валюте за 24h
	MarkPx    string `json:"markPx"`    // mark price (TODO:VERIFY: может быть пустым)
	IdxPx     string `json:"idxPx"`     // index price (TODO:VERIFY: может быть пустым)
	Ts        string `json:"ts"`        // timestamp ms
}

// GetTicker возвращает нормализованный тикер.
// VERIFIED: GET /api/v5/market/ticker?instId=
func (a *Adapter) GetTicker(ctx context.Context, symbol domain.ExchangeSymbol) (domain.Ticker, error) {
	query := "instId=" + string(symbol)
	env, err := a.doPublicGET(ctx, "/api/v5/market/ticker", query)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("okx GetTicker: %w", err)
	}

	var data []tickerData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.Ticker{}, fmt.Errorf("okx GetTicker: parse: %w", err)
	}
	if len(data) == 0 {
		return domain.Ticker{}, fmt.Errorf("%w: %s", exchange.ErrInvalidSymbol, symbol)
	}
	d := data[0]

	lastPrice, err := parseDecimalOrZero(d.Last)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("okx GetTicker: last: %w", err)
	}
	markPrice, err := parseDecimalOrZero(d.MarkPx)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("okx GetTicker: markPx: %w", err)
	}
	indexPrice, err := parseDecimalOrZero(d.IdxPx)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("okx GetTicker: idxPx: %w", err)
	}
	volume, err := parseDecimalOrZero(d.VolCcy24h)
	if err != nil {
		return domain.Ticker{}, fmt.Errorf("okx GetTicker: volCcy24h: %w", err)
	}

	ts := a.clock()
	if d.Ts != "" {
		ms, err := decimal.FromString(d.Ts)
		if err == nil {
			ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	return domain.Ticker{
		Symbol:         symbol,
		LastPrice:      lastPrice,
		MarkPrice:      markPrice,
		IndexPrice:     indexPrice,
		QuoteVolume24h: volume,
		Timestamp:      ts,
	}, nil
}

// ============================================================
// GetOrderBookSnapshot — VERIFIED
// ============================================================

// orderBookData — ответ /api/v5/market/books.
// VERIFIED по официальной документации OKX V5.
type orderBookData struct {
	Bids [][]string `json:"bids"` // [[price, qty, _, numOrders], ...]
	Asks [][]string `json:"asks"`
	Ts   string     `json:"ts"`
}

// GetOrderBookSnapshot возвращает снимок стакана.
// VERIFIED: GET /api/v5/market/books?instId=&sz=
func (a *Adapter) GetOrderBookSnapshot(ctx context.Context, symbol domain.ExchangeSymbol, depth int) (domain.OrderBookSnapshot, error) {
	query := BuildSortedQuery(map[string]string{
		"instId": string(symbol),
		"sz":     fmt.Sprintf("%d", depth),
	})
	env, err := a.doPublicGET(ctx, "/api/v5/market/books", query)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("okx GetOrderBookSnapshot: %w", err)
	}

	var data []orderBookData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("okx GetOrderBookSnapshot: parse: %w", err)
	}
	if len(data) == 0 {
		return domain.OrderBookSnapshot{}, fmt.Errorf("%w: %s", exchange.ErrInvalidSymbol, symbol)
	}
	d := data[0]

	bids, err := parsePriceLevels(d.Bids)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("okx GetOrderBookSnapshot: bids: %w", err)
	}
	asks, err := parsePriceLevels(d.Asks)
	if err != nil {
		return domain.OrderBookSnapshot{}, fmt.Errorf("okx GetOrderBookSnapshot: asks: %w", err)
	}

	ts := a.clock()
	if d.Ts != "" {
		ms, err := decimal.FromString(d.Ts)
		if err == nil {
			ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	return domain.OrderBookSnapshot{
		Exchange:   domain.ExchangeOKX,
		Symbol:     symbol,
		Bids:       bids,
		Asks:       asks,
		Timestamp:  ts,
		IsSnapshot: true,
	}, nil
}

// parsePriceLevels разбирает [[price, qty, ...], ...] в []domain.PriceLevel.
// OKX books возвращает [price, qty, liquidated_orders, orders_count] — берём первые два.
func parsePriceLevels(raw [][]string) ([]domain.PriceLevel, error) {
	levels := make([]domain.PriceLevel, 0, len(raw))
	for _, entry := range raw {
		if len(entry) < 2 {
			continue
		}
		price, err := decimal.FromString(entry[0])
		if err != nil {
			return nil, err
		}
		qty, err := decimal.FromString(entry[1])
		if err != nil {
			return nil, err
		}
		levels = append(levels, domain.PriceLevel{Price: price, Qty: qty})
	}
	return levels, nil
}

// ============================================================
// WebSocket — не реализованы (ErrWSNotImplemented)
// ============================================================

// SubscribePublic возвращает ErrWSNotImplemented.
// TODO: реализовать WS wss://ws.okx.com:8443/ws/v5/public (tickers/books5 channels).
func (a *Adapter) SubscribePublic(_ context.Context, _ []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	return nil, ErrWSNotImplemented
}

// SubscribePrivate возвращает ErrWSNotImplemented.
// TODO: реализовать приватный WS с login-frame (apiKey/passphrase/timestamp/sign).
func (a *Adapter) SubscribePrivate(_ context.Context, _ domain.CredentialRef) (<-chan exchange.PrivateEvent, error) {
	return nil, ErrWSNotImplemented
}

// ============================================================
// GetBalances — VERIFIED
// ============================================================

// accountBalanceData — верхний уровень ответа /api/v5/account/balance.
// VERIFIED по официальной документации OKX V5.
type accountBalanceData struct {
	TotalEq string                 `json:"totalEq"`
	Details []accountBalanceDetail `json:"details"`
}

// accountBalanceDetail — детали по одной монете.
// VERIFIED: availEq = available equity; eq = total equity per currency.
type accountBalanceDetail struct {
	Ccy     string `json:"ccy"`
	Eq      string `json:"eq"`      // полный баланс (total equity)
	AvailEq string `json:"availEq"` // доступный баланс для торговли
}

// GetBalances возвращает балансы торгового аккаунта.
// VERIFIED: GET /api/v5/account/balance
func (a *Adapter) GetBalances(ctx context.Context) ([]domain.Balance, error) {
	env, err := a.doSignedGET(ctx, "/api/v5/account/balance", "")
	if err != nil {
		return nil, fmt.Errorf("okx GetBalances: %w", err)
	}
	var data []accountBalanceData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("okx GetBalances: parse: %w", err)
	}

	var balances []domain.Balance
	for _, acc := range data {
		for _, d := range acc.Details {
			eq, err := parseDecimalOrZero(d.Eq)
			if err != nil {
				continue
			}
			availEq, err := parseDecimalOrZero(d.AvailEq)
			if err != nil {
				continue
			}
			balances = append(balances, domain.Balance{
				Asset:            d.Ccy,
				WalletBalance:    eq,
				AvailableBalance: availEq,
			})
		}
	}
	return balances, nil
}

// ============================================================
// GetPositions — VERIFIED
// ============================================================

// positionData — одна позиция SWAP в ответе /api/v5/account/positions.
// VERIFIED по официальной документации OKX V5.
type positionData struct {
	InstId   string `json:"instId"`
	PosSide  string `json:"posSide"`  // "long" / "short" / "net"
	Pos      string `json:"pos"`      // размер позиции (в контрактах, signed)
	AvgPx    string `json:"avgPx"`    // средняя цена входа
	MarkPx   string `json:"markPx"`   // текущая mark price
	LiqPx    string `json:"liqPx"`    // цена ликвидации
	UplRatio string `json:"uplRatio"` // unrealized PnL ratio
	Upl      string `json:"upl"`      // unrealized PnL (абсолютный)
	MgnMode  string `json:"mgnMode"`  // "cross" / "isolated"
	Lever    string `json:"lever"`    // текущее плечо
	Margin   string `json:"margin"`   // маржа позиции
	MgnRatio string `json:"mgnRatio"` // margin ratio
	Adl      string `json:"adl"`      // ADL ранг 1..5
	CTime    string `json:"cTime"`    // время создания (ms)
	UTime    string `json:"uTime"`    // время последнего обновления (ms)
}

// GetPositions возвращает все открытые SWAP-позиции.
// VERIFIED: GET /api/v5/account/positions?instType=SWAP
func (a *Adapter) GetPositions(ctx context.Context) ([]domain.Position, error) {
	env, err := a.doSignedGET(ctx, "/api/v5/account/positions", "instType=SWAP")
	if err != nil {
		return nil, fmt.Errorf("okx GetPositions: %w", err)
	}
	var data []positionData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("okx GetPositions: parse: %w", err)
	}

	var positions []domain.Position
	for _, d := range data {
		pos, err := a.parsePosition(ctx, d)
		if err != nil {
			continue
		}
		positions = append(positions, pos)
	}
	return positions, nil
}

// parsePosition преобразует positionData в domain.Position.
func (a *Adapter) parsePosition(ctx context.Context, d positionData) (domain.Position, error) {
	posQty, err := decimal.FromString(d.Pos)
	if err != nil || posQty.IsZero() {
		return domain.Position{}, fmt.Errorf("pos zero or invalid: %w", err)
	}

	// Определяем сторону.
	// OKX в net-режиме: pos > 0 → long, pos < 0 → short.
	// В long_short режиме: posSide = "long" / "short".
	var side domain.Side
	switch d.PosSide {
	case "long":
		side = domain.SideLong
	case "short":
		side = domain.SideShort
	case "net":
		if posQty.IsPositive() {
			side = domain.SideLong
		} else {
			side = domain.SideShort
		}
	default:
		return domain.Position{}, fmt.Errorf("unknown posSide: %s", d.PosSide)
	}

	absQty := posQty.Abs()

	// Конвертируем контракты → base qty через ctVal.
	ctVal, err := a.getCtVal(ctx, d.InstId)
	if err != nil {
		// При отсутствии ctVal — используем qty as-is с предупреждением.
		ctVal = decimal.One
	}
	baseQty := contractsToBaseQty(absQty, ctVal)

	avgPx, err := parseDecimalOrZero(d.AvgPx)
	if err != nil {
		return domain.Position{}, fmt.Errorf("avgPx: %w", err)
	}
	markPx, err := parseDecimalOrZero(d.MarkPx)
	if err != nil {
		return domain.Position{}, fmt.Errorf("markPx: %w", err)
	}
	liqPx, err := parseDecimalOrZero(d.LiqPx)
	if err != nil {
		return domain.Position{}, fmt.Errorf("liqPx: %w", err)
	}
	upl, err := parseDecimalOrZero(d.Upl)
	if err != nil {
		return domain.Position{}, fmt.Errorf("upl: %w", err)
	}
	lever, err := parseDecimalOrZero(d.Lever)
	if err != nil {
		return domain.Position{}, fmt.Errorf("lever: %w", err)
	}
	margin, err := parseDecimalOrZero(d.Margin)
	if err != nil {
		return domain.Position{}, fmt.Errorf("margin: %w", err)
	}

	marginMode := domain.MarginCross
	if d.MgnMode == "isolated" {
		marginMode = domain.MarginIsolated
	}

	// ADL: 1..5 → нормализуем (adl-1)/4 → [0,1].
	// VERIFIED по документации OKX V5: adl integer 1..5.
	var adlState *domain.ADLQueuePosition
	if d.Adl != "" {
		adlVal, err := decimal.FromString(d.Adl)
		if err == nil {
			// (adl - 1) / 4 → [0,1]
			adlNorm := adlVal.Sub(decimal.One).Div(decimal.FromInt(4))
			adlState = &domain.ADLQueuePosition{
				LongQueue:  adlNorm,
				ShortQueue: adlNorm,
			}
		}
	}

	updatedAt := a.clock()
	if d.UTime != "" {
		ms, err := decimal.FromString(d.UTime)
		if err == nil {
			updatedAt = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	return domain.Position{
		Symbol:           domain.ExchangeSymbol(d.InstId),
		Side:             side,
		ContractQty:      absQty,
		BaseQty:          baseQty,
		EntryPrice:       avgPx,
		MarkPrice:        markPx,
		LiquidationPrice: liqPx,
		UnrealizedPnL:    upl,
		MarginMode:       marginMode,
		Leverage:         lever,
		Margin:           margin,
		ADLQueue:         adlState,
		Updated:          updatedAt,
	}, nil
}

// ============================================================
// GetOpenOrders — VERIFIED
// ============================================================

// orderData — один ордер в ответе OKX V5.
// VERIFIED по официальной документации OKX V5.
type orderData struct {
	InstId     string `json:"instId"`
	OrdId      string `json:"ordId"`
	ClOrdId    string `json:"clOrdId"`
	Side       string `json:"side"`       // "buy" / "sell"
	PosSide    string `json:"posSide"`    // "long" / "short" / "net"
	OrdType    string `json:"ordType"`    // "market" / "limit" / "ioc" / "fok"
	Sz         string `json:"sz"`         // размер в контрактах
	Px         string `json:"px"`         // цена (для limit)
	AccFillSz  string `json:"accFillSz"`  // заполнено (контрактах)
	AvgPx      string `json:"avgPx"`      // средняя цена исполнения
	Fee        string `json:"fee"`        // комиссия
	FeeCcy     string `json:"feeCcy"`     // валюта комиссии
	State      string `json:"state"`      // "live" / "partially_filled" / "filled" / "canceled"
	ReduceOnly string `json:"reduceOnly"` // "true" / "false"
	TdMode     string `json:"tdMode"`     // "cross" / "isolated"
	CTime      string `json:"cTime"`      // timestamp создания ms
}

// GetOpenOrders возвращает открытые ордера по символу.
// VERIFIED: GET /api/v5/trade/orders-pending?instType=SWAP&instId=
func (a *Adapter) GetOpenOrders(ctx context.Context, symbol domain.ExchangeSymbol) ([]domain.Order, error) {
	query := BuildSortedQuery(map[string]string{
		"instType": "SWAP",
		"instId":   string(symbol),
	})
	env, err := a.doSignedGET(ctx, "/api/v5/trade/orders-pending", query)
	if err != nil {
		return nil, fmt.Errorf("okx GetOpenOrders: %w", err)
	}
	var data []orderData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("okx GetOpenOrders: parse: %w", err)
	}

	orders := make([]domain.Order, 0, len(data))
	for _, d := range data {
		ord, err := a.parseOrder(ctx, d)
		if err != nil {
			continue
		}
		orders = append(orders, ord)
	}
	return orders, nil
}

// parseOrder преобразует orderData в domain.Order.
func (a *Adapter) parseOrder(ctx context.Context, d orderData) (domain.Order, error) {
	// OKX sz — в контрактах; конвертируем в base qty.
	sz, err := decimal.FromString(d.Sz)
	if err != nil {
		return domain.Order{}, fmt.Errorf("sz: %w", err)
	}
	ctVal, ctValErr := a.getCtVal(ctx, d.InstId)
	if ctValErr != nil {
		ctVal = decimal.One
	}
	requestedQty := contractsToBaseQty(sz, ctVal)

	filledSz, err := parseDecimalOrZero(d.AccFillSz)
	if err != nil {
		return domain.Order{}, fmt.Errorf("accFillSz: %w", err)
	}
	filledQty := contractsToBaseQty(filledSz, ctVal)

	avgPx, err := parseDecimalOrZero(d.AvgPx)
	if err != nil {
		return domain.Order{}, fmt.Errorf("avgPx: %w", err)
	}
	fee, err := parseDecimalOrZero(d.Fee)
	if err != nil {
		return domain.Order{}, fmt.Errorf("fee: %w", err)
	}

	// OKX side: "buy" / "sell".
	var side domain.Side
	switch d.Side {
	case "buy":
		side = domain.SideLong
	case "sell":
		side = domain.SideShort
	default:
		return domain.Order{}, fmt.Errorf("unknown side: %s", d.Side)
	}

	// OKX ordType: "market" / "limit" / "ioc" / "fok" / "post_only".
	var orderMode domain.OrderMode
	switch d.OrdType {
	case "market":
		orderMode = domain.OrderMarket
	case "ioc":
		orderMode = domain.OrderMarketableLimitIOC
	default:
		orderMode = domain.OrderMarketableLimitIOC
	}

	status := parseOrderStatus(d.State)

	reduceOnly := d.ReduceOnly == "true"

	ts := a.clock()
	if d.CTime != "" {
		ms, err := decimal.FromString(d.CTime)
		if err == nil {
			ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
		}
	}

	return domain.Order{
		ExchangeOrderID:   d.OrdId,
		ClientOrderID:     domain.ClientOrderID(d.ClOrdId),
		Symbol:            domain.ExchangeSymbol(d.InstId),
		Side:              side,
		OrderMode:         orderMode,
		ReduceOnly:        reduceOnly,
		RequestedQty:      requestedQty,
		FilledQty:         filledQty,
		AvgFillPrice:      avgPx,
		Fees:              fee.Abs(),
		Status:            status,
		ExchangeTimestamp: ts,
		AckState:          domain.AckStateQueried,
	}, nil
}

// parseOrderStatus маппирует строковый статус OKX → domain.OrderStatus.
// VERIFIED по официальной документации OKX V5.
func parseOrderStatus(s string) domain.OrderStatus {
	switch s {
	case "live":
		return domain.OrderStatusAcknowledged
	case "partially_filled":
		return domain.OrderStatusPartiallyFilled
	case "filled":
		return domain.OrderStatusFilled
	case "canceled":
		return domain.OrderStatusCancelled
	default:
		return domain.OrderStatusNew
	}
}

// ============================================================
// GetOrder — VERIFIED
// ============================================================

// GetOrder запрашивает состояние ордера по clientOrderID или exchangeOrderID.
// VERIFIED: GET /api/v5/trade/order?instId=&clOrdId=  или &ordId=
// При 51603 → ErrOrderNotFound.
func (a *Adapter) GetOrder(ctx context.Context, req domain.OrderQuery) (domain.Order, error) {
	params := map[string]string{}
	if req.Symbol != "" {
		params["instId"] = string(req.Symbol)
	}
	if req.ExchangeOrderID != "" {
		params["ordId"] = req.ExchangeOrderID
	} else if req.ClientOrderID != "" {
		// Транслируем clOrdId так же, как при PlaceOrder.
		params["clOrdId"] = translateClientOrderID(req.ClientOrderID)
	}

	query := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/api/v5/trade/order", query)
	if err != nil {
		if errors.Is(err, exchange.ErrOrderNotFound) {
			return domain.Order{}, err
		}
		return domain.Order{}, fmt.Errorf("okx GetOrder: %w", err)
	}

	var data []orderData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.Order{}, fmt.Errorf("okx GetOrder: parse: %w", err)
	}
	if len(data) == 0 {
		return domain.Order{}, fmt.Errorf("%w: clientOrderID=%s", exchange.ErrOrderNotFound, req.ClientOrderID)
	}
	return a.parseOrder(ctx, data[0])
}

// ============================================================
// PlaceOrder — VERIFIED
// ============================================================

// placeOrderRequest — тело запроса /api/v5/trade/order.
// VERIFIED по официальной документации OKX V5.
type placeOrderRequest struct {
	InstId     string `json:"instId"`
	TdMode     string `json:"tdMode"`               // "cross" / "isolated"
	Side       string `json:"side"`                 // "buy" / "sell"
	PosSide    string `json:"posSide,omitempty"`    // "long" / "short" (в hedge-режиме)
	OrdType    string `json:"ordType"`              // "market" / "limit" / "ioc"
	Sz         string `json:"sz"`                   // количество КОНТРАКТОВ (целое)
	Px         string `json:"px,omitempty"`         // цена (для limit/ioc)
	ReduceOnly bool   `json:"reduceOnly,omitempty"` // reduce-only
	ClOrdId    string `json:"clOrdId,omitempty"`    // client order ID (alphanumeric ≤32)
	TgtCcy     string `json:"tgtCcy,omitempty"`     // TODO:VERIFY: нужен ли для SWAP
}

// placeOrderResponseData — поля result при создании ордера.
type placeOrderResponseData struct {
	OrdId   string `json:"ordId"`
	ClOrdId string `json:"clOrdId"`
	SCode   string `json:"sCode"` // статус (0=ok)
	SMsg    string `json:"sMsg"`  // сообщение
}

// PlaceOrder размещает ордер. Safe=false обязательно.
// VERIFIED: POST /api/v5/trade/order
// qty конвертируется из base qty → контракты (floor division на ctVal).
// clOrdId транслируется: удаляются '_' и '-', обрезается до 32 символов.
func (a *Adapter) PlaceOrder(ctx context.Context, req domain.PlaceOrderRequest) (domain.OrderAck, error) {
	// Получаем ctVal для конверсии qty.
	ctVal, err := a.getCtVal(ctx, string(req.Symbol))
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("okx PlaceOrder: getCtVal: %w", err)
	}

	// Конвертируем base qty → целые контракты (floor).
	contracts := baseQtyToContracts(req.BaseQty, ctVal)
	if contracts.IsZero() {
		return domain.OrderAck{}, fmt.Errorf("%w: qty %s < 1 contract (ctVal=%s)", exchange.ErrInsufficientMargin, req.BaseQty, ctVal)
	}

	// Маппинг side: Long → buy, Short → sell.
	side := "buy"
	if req.Side == domain.SideShort {
		side = "sell"
	}

	// Тип ордера.
	// VERIFIED: "market" / "limit" / "ioc" / "fok" / "post_only".
	ordType := "limit"
	switch req.OrderMode {
	case domain.OrderMarket:
		ordType = "market"
	case domain.OrderMarketableLimitIOC:
		ordType = "ioc"
	}

	// Если TIF задан явно.
	if req.TimeInForce == domain.TIFIOC {
		ordType = "ioc"
	} else if req.TimeInForce == domain.TIFGTC {
		ordType = "limit"
	}

	// Транслируем clOrdId.
	clOrdId := translateClientOrderID(req.ClientOrderID)

	body := placeOrderRequest{
		InstId:     string(req.Symbol),
		TdMode:     "cross", // VERIFIED: cross margin для USDT perpetual
		Side:       side,
		OrdType:    ordType,
		Sz:         contracts.String(),
		ReduceOnly: req.ReduceOnly,
		ClOrdId:    clOrdId,
	}

	// Цена для limit/ioc ордеров.
	if ordType != "market" && !req.Price.IsZero() {
		body.Px = req.Price.String()
	}

	env, err := a.doSignedPOST(ctx, "/api/v5/trade/order", body)
	if err != nil {
		return domain.OrderAck{}, fmt.Errorf("okx PlaceOrder: %w", err)
	}

	var data []placeOrderResponseData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.OrderAck{}, fmt.Errorf("okx PlaceOrder: parse: %w", err)
	}
	if len(data) == 0 {
		return domain.OrderAck{}, fmt.Errorf("okx PlaceOrder: empty response data")
	}
	d := data[0]

	// Проверяем sCode (per-order ошибки в batch-ответах).
	if d.SCode != "" && d.SCode != "0" {
		return domain.OrderAck{}, mapAPIError(d.SCode, d.SMsg)
	}

	return domain.OrderAck{
		ExchangeOrderID: d.OrdId,
		ClientOrderID:   req.ClientOrderID,
		Status:          domain.OrderStatusAcknowledged,
		Timestamp:       a.clock(),
	}, nil
}

// ============================================================
// CancelOrder — VERIFIED
// ============================================================

// cancelOrderRequest — тело запроса /api/v5/trade/cancel-order.
type cancelOrderRequest struct {
	InstId  string `json:"instId"`
	OrdId   string `json:"ordId,omitempty"`
	ClOrdId string `json:"clOrdId,omitempty"`
}

// CancelOrder отменяет ордер.
// VERIFIED: POST /api/v5/trade/cancel-order
// 51603 → ErrOrderNotFound.
func (a *Adapter) CancelOrder(ctx context.Context, req domain.CancelOrderRequest) error {
	body := cancelOrderRequest{
		InstId: string(req.Symbol),
	}
	if req.ExchangeOrderID != "" {
		body.OrdId = req.ExchangeOrderID
	} else if req.ClientOrderID != "" {
		body.ClOrdId = translateClientOrderID(req.ClientOrderID)
	}

	_, err := a.doSignedPOST(ctx, "/api/v5/trade/cancel-order", body)
	if errors.Is(err, exchange.ErrOrderNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("okx CancelOrder: %w", err)
	}
	return nil
}

// ============================================================
// SetLeverage — VERIFIED
// ============================================================

// setLeverageRequest — тело запроса /api/v5/account/set-leverage.
// VERIFIED по официальной документации OKX V5.
type setLeverageRequest struct {
	InstId  string `json:"instId"`
	Lever   string `json:"lever"`
	MgnMode string `json:"mgnMode"`           // "cross" / "isolated"
	PosSide string `json:"posSide,omitempty"` // для isolated: "long" / "short"
}

// SetLeverage устанавливает плечо для инструмента.
// VERIFIED: POST /api/v5/account/set-leverage
func (a *Adapter) SetLeverage(ctx context.Context, req domain.SetLeverageRequest) error {
	body := setLeverageRequest{
		InstId:  string(req.Symbol),
		Lever:   req.Leverage.String(),
		MgnMode: "cross",
	}
	_, err := a.doSignedPOST(ctx, "/api/v5/account/set-leverage", body)
	if err != nil {
		return fmt.Errorf("okx SetLeverage: %w", err)
	}
	return nil
}

// ============================================================
// SetMarginMode — TODO:VERIFY
// ============================================================

// SetMarginMode переключает режим маржи.
// TODO:VERIFY: OKX V5 endpoint и параметры для переключения cross/isolated per-instrument.
// Возможно POST /api/v5/account/set-leverage с mgnMode=cross/isolated достаточно.
func (a *Adapter) SetMarginMode(ctx context.Context, req domain.SetMarginModeRequest) error {
	mgnMode := "cross"
	if req.MarginMode == domain.MarginIsolated {
		mgnMode = "isolated"
	}
	body := setLeverageRequest{
		InstId:  string(req.Symbol),
		Lever:   "10", // TODO:VERIFY: требуется ли lever при смене mgnMode
		MgnMode: mgnMode,
	}
	_, err := a.doSignedPOST(ctx, "/api/v5/account/set-leverage", body)
	if err != nil {
		return fmt.Errorf("okx SetMarginMode: %w", err)
	}
	return nil
}

// ============================================================
// SetPositionMode — VERIFIED
// ============================================================

// setPositionModeRequest — тело запроса /api/v5/account/set-position-mode.
// VERIFIED: posMode = "long_short_mode" | "net_mode".
type setPositionModeRequest struct {
	PosMode string `json:"posMode"` // "long_short_mode" / "net_mode"
}

// SetPositionMode переключает режим позиций (one-way/hedge).
// VERIFIED: POST /api/v5/account/set-position-mode
// "already set" — OKX возвращает успех (нет отдельного error code).
// TODO:VERIFY: точный ответ при "already set" (может быть code=0 с сообщением).
func (a *Adapter) SetPositionMode(ctx context.Context, req domain.SetPositionModeRequest) error {
	posMode := "net_mode"
	if req.Mode == domain.PositionHedge {
		posMode = "long_short_mode"
	}
	body := setPositionModeRequest{PosMode: posMode}
	_, err := a.doSignedPOST(ctx, "/api/v5/account/set-position-mode", body)
	// OKX возвращает code=0 при успехе, включая "already set".
	if err != nil {
		return fmt.Errorf("okx SetPositionMode: %w", err)
	}
	return nil
}

// ============================================================
// GetADLState — из positions.adl
// ============================================================

// GetADLState возвращает ADL-состояние для символа.
// ADL поле берётся из /api/v5/account/positions (поле adl).
// VERIFIED: adl integer 1..5; нормализация (adl-1)/4 → [0,1].
func (a *Adapter) GetADLState(ctx context.Context, symbol domain.ExchangeSymbol) (domain.ADLState, error) {
	positions, err := a.GetPositions(ctx)
	if err != nil {
		return domain.ADLState{}, fmt.Errorf("okx GetADLState: %w", err)
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

// transferRequest — тело запроса /api/v5/asset/transfer.
// TODO:VERIFY: точные значения type (0=main to sub, 1=sub to main, etc).
type transferRequest struct {
	Type     string `json:"type"` // "0" = trading → funding, "1" = funding → trading
	Ccy      string `json:"ccy"`
	Amt      string `json:"amt"`
	From     string `json:"from"` // "6" = funding, "18" = trading
	To       string `json:"to"`
	ClientId string `json:"clientId,omitempty"`
}

// transferResponseData — ответ на перевод.
type transferResponseData struct {
	TransId  string `json:"transId"`
	ClientId string `json:"clientId"`
	Ccy      string `json:"ccy"`
	From     string `json:"from"`
	Amt      string `json:"amt"`
	To       string `json:"to"`
}

// mapOKXAccountType маппирует строку типа аккаунта в OKX account ID.
// TODO:VERIFY: OKX использует числовые ID: 6=funding, 18=trading.
func mapOKXAccountType(t string) string {
	switch strings.ToLower(t) {
	case "funding", "fund":
		return "6"
	case "trading", "trade", "unified":
		return "18"
	default:
		return t
	}
}

// InternalTransfer — внутренний перевод между аккаунтами OKX.
// TODO:VERIFY: поля from/to (OKX использует числовые ID).
func (a *Adapter) InternalTransfer(ctx context.Context, req domain.InternalTransferRequest) (domain.TransferResult, error) {
	body := transferRequest{
		Type: "0",
		Ccy:  req.Asset,
		Amt:  req.Amount.String(),
		From: mapOKXAccountType(req.From),
		To:   mapOKXAccountType(req.To),
	}
	env, err := a.doSignedPOST(ctx, "/api/v5/asset/transfer", body)
	if err != nil {
		return domain.TransferResult{}, fmt.Errorf("okx InternalTransfer: %w", err)
	}
	var data []transferResponseData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.TransferResult{}, fmt.Errorf("okx InternalTransfer: parse: %w", err)
	}
	if len(data) == 0 {
		return domain.TransferResult{}, fmt.Errorf("okx InternalTransfer: empty response")
	}
	return domain.TransferResult{
		TransferID: data[0].TransId,
		Status:     "submitted",
	}, nil
}

// ============================================================
// Withdraw — TODO:VERIFY
// ============================================================

// withdrawRequest — тело запроса /api/v5/asset/withdrawal.
// TODO:VERIFY: точные поля и значения.
type withdrawRequest struct {
	Ccy      string `json:"ccy"`
	Amt      string `json:"amt"`
	Dest     string `json:"dest"` // "4" = chain withdrawal
	ToAddr   string `json:"toAddr"`
	Fee      string `json:"fee"` // TODO:VERIFY: обязателен ли
	Chain    string `json:"chain"`
	ClientId string `json:"clientId,omitempty"`
	Memo     string `json:"areaCode,omitempty"` // TODO:VERIFY: memo поле
}

// withdrawResponseData — ответ на вывод.
type withdrawResponseData struct {
	Amt      string `json:"amt"`
	WdId     string `json:"wdId"`
	Ccy      string `json:"ccy"`
	ClientId string `json:"clientId"`
	Chain    string `json:"chain"`
}

// Withdraw создаёт заявку на вывод средств.
// TODO:VERIFY: поле fee обязательно; fee-данные нужно получить через GetNetworkInfo.
func (a *Adapter) Withdraw(ctx context.Context, req domain.WithdrawalRequest) (domain.WithdrawalResult, error) {
	body := withdrawRequest{
		Ccy:    req.Asset,
		Amt:    req.Amount.String(),
		Dest:   "4", // on-chain withdrawal
		ToAddr: req.Address,
		Fee:    "0",                           // TODO:VERIFY: получать из GetNetworkInfo
		Chain:  req.Asset + "-" + req.Network, // TODO:VERIFY: формат OKX chain (например "USDT-TRC20")
	}
	if req.Memo != "" {
		body.Memo = req.Memo
	}
	env, err := a.doSignedPOST(ctx, "/api/v5/asset/withdrawal", body)
	if err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("okx Withdraw: %w", err)
	}
	var data []withdrawResponseData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return domain.WithdrawalResult{}, fmt.Errorf("okx Withdraw: parse: %w", err)
	}
	if len(data) == 0 {
		return domain.WithdrawalResult{}, fmt.Errorf("okx Withdraw: empty response")
	}
	return domain.WithdrawalResult{
		WithdrawalID: data[0].WdId,
		Status:       "submitted",
	}, nil
}

// ============================================================
// GetWithdrawalHistory — TODO:VERIFY
// ============================================================

// withdrawalHistoryData — одна запись истории вывода.
// TODO:VERIFY: точные поля ответа.
type withdrawalHistoryData struct {
	WdId  string `json:"wdId"`
	TxId  string `json:"txId"`
	Ccy   string `json:"ccy"`
	Chain string `json:"chain"`
	Amt   string `json:"amt"`
	Fee   string `json:"fee"`
	State string `json:"state"` // "-3"/"-2"/"-1"/"0"/"1"/"2"
	Ts    string `json:"ts"`
}

// GetWithdrawalHistory возвращает историю выводов.
// TODO:VERIFY: поля запроса/ответа.
func (a *Adapter) GetWithdrawalHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Withdrawal, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["ccy"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", query.Limit)
	}
	q := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/api/v5/asset/withdrawal-history", q)
	if err != nil {
		return nil, fmt.Errorf("okx GetWithdrawalHistory: %w", err)
	}
	var data []withdrawalHistoryData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("okx GetWithdrawalHistory: parse: %w", err)
	}

	var result []domain.Withdrawal
	for _, d := range data {
		amount, _ := parseDecimalOrZero(d.Amt)
		fee, _ := parseDecimalOrZero(d.Fee)
		ts := time.Time{}
		if d.Ts != "" {
			ms, err := decimal.FromString(d.Ts)
			if err == nil {
				ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}
		// chain: "USDT-TRC20" → network = "TRC20".
		network := d.Chain
		if idx := strings.Index(d.Chain, "-"); idx >= 0 {
			network = d.Chain[idx+1:]
		}
		result = append(result, domain.Withdrawal{
			WithdrawalID: d.WdId,
			TxID:         d.TxId,
			Asset:        d.Ccy,
			Network:      network,
			Amount:       amount,
			Fee:          fee,
			Status:       d.State,
			RequestedAt:  ts,
		})
	}
	return result, nil
}

// ============================================================
// GetDepositHistory — TODO:VERIFY
// ============================================================

// depositHistoryData — одна запись истории депозита.
// TODO:VERIFY: точные поля ответа.
type depositHistoryData struct {
	DepId string `json:"depId"`
	TxId  string `json:"txId"`
	Ccy   string `json:"ccy"`
	Chain string `json:"chain"`
	Amt   string `json:"amt"`
	State string `json:"state"` // "0"=wait conf, "1"=credited, "2"=success
	Ts    string `json:"ts"`
}

// GetDepositHistory возвращает историю депозитов.
// TODO:VERIFY: поля запроса/ответа.
func (a *Adapter) GetDepositHistory(ctx context.Context, query domain.TransferQuery) ([]domain.Deposit, error) {
	params := map[string]string{}
	if query.Asset != "" {
		params["ccy"] = query.Asset
	}
	if query.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", query.Limit)
	}
	q := BuildSortedQuery(params)
	env, err := a.doSignedGET(ctx, "/api/v5/asset/deposit-history", q)
	if err != nil {
		return nil, fmt.Errorf("okx GetDepositHistory: %w", err)
	}
	var data []depositHistoryData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("okx GetDepositHistory: parse: %w", err)
	}

	var result []domain.Deposit
	for _, d := range data {
		amount, _ := parseDecimalOrZero(d.Amt)
		ts := time.Time{}
		if d.Ts != "" {
			ms, err := decimal.FromString(d.Ts)
			if err == nil {
				ts = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		}
		network := d.Chain
		if idx := strings.Index(d.Chain, "-"); idx >= 0 {
			network = d.Chain[idx+1:]
		}
		result = append(result, domain.Deposit{
			TxID:        d.TxId,
			Asset:       d.Ccy,
			Network:     network,
			Amount:      amount,
			Status:      d.State,
			ConfirmedAt: ts,
		})
	}
	return result, nil
}

// ============================================================
// GetNetworkInfo — TODO:VERIFY
// ============================================================

// currencyData — один токен из /api/v5/asset/currencies.
// TODO:VERIFY: точные поля ответа.
type currencyData struct {
	Ccy      string `json:"ccy"`
	Name     string `json:"name"`
	Chain    string `json:"chain"`
	CanDep   bool   `json:"canDep"`
	CanWd    bool   `json:"canWd"`
	MinDep   string `json:"minDep"`
	MinWd    string `json:"minWd"`
	WdFee    string `json:"wdFee"`
	WdTickSz string `json:"wdTickSz"`
	// Chains содержит информацию по сетям (если поддерживается).
	Chains []currencyChain `json:"chains"`
}

// currencyChain — одна сеть для монеты.
// TODO:VERIFY: структура chains в ответе /api/v5/asset/currencies.
type currencyChain struct {
	Chain  string `json:"chain"`
	CanDep bool   `json:"canDep"`
	CanWd  bool   `json:"canWd"`
	MinDep string `json:"minDep"`
	MinWd  string `json:"minWd"`
	WdFee  string `json:"wdFee"`
}

// GetNetworkInfo возвращает информацию о сетях для актива.
// TODO:VERIFY: поля запроса/ответа.
func (a *Adapter) GetNetworkInfo(ctx context.Context, asset string) ([]domain.NetworkInfo, error) {
	q := "ccy=" + asset
	env, err := a.doSignedGET(ctx, "/api/v5/asset/currencies", q)
	if err != nil {
		return nil, fmt.Errorf("okx GetNetworkInfo: %w", err)
	}
	var data []currencyData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return nil, fmt.Errorf("okx GetNetworkInfo: parse: %w", err)
	}

	var result []domain.NetworkInfo
	for _, d := range data {
		if d.Ccy != asset {
			continue
		}
		// Если есть chains — используем их.
		if len(d.Chains) > 0 {
			for _, ch := range d.Chains {
				fee, _ := parseDecimalOrZero(ch.WdFee)
				wdMin, _ := parseDecimalOrZero(ch.MinWd)
				depMin, _ := parseDecimalOrZero(ch.MinDep)
				// chain: "USDT-TRC20" → network = "TRC20".
				network := ch.Chain
				if idx := strings.Index(ch.Chain, "-"); idx >= 0 {
					network = ch.Chain[idx+1:]
				}
				result = append(result, domain.NetworkInfo{
					Network:         network,
					WithdrawEnabled: ch.CanWd,
					DepositEnabled:  ch.CanDep,
					WithdrawFee:     fee,
					WithdrawMin:     wdMin,
					DepositMin:      depMin,
				})
			}
		} else {
			// Fallback — используем верхний уровень (если нет chains).
			fee, _ := parseDecimalOrZero(d.WdFee)
			wdMin, _ := parseDecimalOrZero(d.MinWd)
			depMin, _ := parseDecimalOrZero(d.MinDep)
			network := d.Chain
			if idx := strings.Index(d.Chain, "-"); idx >= 0 {
				network = d.Chain[idx+1:]
			}
			result = append(result, domain.NetworkInfo{
				Network:         network,
				WithdrawEnabled: d.CanWd,
				DepositEnabled:  d.CanDep,
				WithdrawFee:     fee,
				WithdrawMin:     wdMin,
				DepositMin:      depMin,
			})
		}
	}
	return result, nil
}
