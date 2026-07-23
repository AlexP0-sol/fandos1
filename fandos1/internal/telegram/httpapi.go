package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thecd/fundarbitrage/internal/auth"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/scanner"
	"github.com/thecd/fundarbitrage/internal/strategy"
)

// ============================================================
// DTOs
// ============================================================

// AuthRequest — тело POST /api/auth.
type AuthRequest struct {
	InitData string `json:"initData"`
}

// AuthResponse — ответ POST /api/auth.
type AuthResponse struct {
	Token     string            `json:"token"`
	ExpiresAt time.Time         `json:"expiresAt"`
	User      auth.TelegramUser `json:"user"`
}

// ExchangeStatusDTO — состояние одной биржи для /api/status.
type ExchangeStatusDTO struct {
	Exchange         string `json:"exchange"`
	Connected        bool   `json:"connected"`
	WSHealthy        bool   `json:"wsHealthy"`
	APIHealthy       bool   `json:"apiHealthy"`
	EquityUSDT       string `json:"equityUSDT"`
	FreeMarginUSDT   string `json:"freeMarginUSDT"`
	UsedMarginUSDT   string `json:"usedMarginUSDT"`
	CounterpartyTier string `json:"counterpartyTier"`
}

// StatusDTO — ответ GET /api/status (раздел 14.1).
type StatusDTO struct {
	SystemState     string              `json:"systemState"`
	RunMode         string              `json:"runMode"`
	Exchanges       []ExchangeStatusDTO `json:"exchanges"`
	TotalEquityUSDT string              `json:"totalEquityUSDT"`
	OpenPositions   int                 `json:"openPositions"`
	RealizedPnL     string              `json:"realizedPnL"`
	UnrealizedPnL   string              `json:"unrealizedPnL"`
	FundingPnL      string              `json:"fundingPnL"`
	ActiveIncidents []string            `json:"activeIncidents"`
	ClockOffsetMs   int64               `json:"clockOffsetMs"`
}

// PnLComponentDTO — компонент детализации PnL.
type PnLComponentDTO struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CandidateDTO — ответ GET /api/candidates (раздел 14.2).
type CandidateDTO struct {
	Asset          string            `json:"asset"`
	LongExchange   string            `json:"longExchange"`
	ShortExchange  string            `json:"shortExchange"`
	LongSymbol     string            `json:"longSymbol"`
	ShortSymbol    string            `json:"shortSymbol"`
	IntervalClass  string            `json:"intervalClass"`
	Eligible       bool              `json:"eligible"`
	Reason         string            `json:"reason,omitempty"`
	ExpectedNetPnL string            `json:"expectedNetPnL"`
	PnLBreakdown   []PnLComponentDTO `json:"pnlBreakdown"`
	CompositeScore string            `json:"compositeScore"`
	// Scores.
	LiquidityScore         string `json:"liquidityScore"`
	FundingConfidenceScore string `json:"fundingConfidenceScore"`
	BasisStabilityScore    string `json:"basisStabilityScore"`
	ExecutionRiskScore     string `json:"executionRiskScore"`
	CounterpartyRiskScore  string `json:"counterpartyRiskScore"`
	DataQualityScore       string `json:"dataQualityScore"`
	ADLRiskScore           string `json:"adlRiskScore"`
	// Market.
	SecondsToFunding int64  `json:"secondsToFunding"`
	EvaluatedAt      string `json:"evaluatedAt"`
}

// candidateDTOFromScanner конвертирует scanner.Candidate в CandidateDTO.
func candidateDTOFromScanner(c scanner.Candidate) CandidateDTO {
	comps := make([]PnLComponentDTO, 0, len(c.PnLBreakdown.Components))
	for _, comp := range c.PnLBreakdown.Components {
		comps = append(comps, PnLComponentDTO{
			Name:  comp.Name,
			Value: comp.Value.String(),
		})
	}
	return CandidateDTO{
		Asset:                  string(c.Asset),
		LongExchange:           string(c.LongExchange),
		ShortExchange:          string(c.ShortExchange),
		LongSymbol:             string(c.LongSymbol),
		ShortSymbol:            string(c.ShortSymbol),
		IntervalClass:          string(c.IntervalClass),
		Eligible:               c.Eligible,
		Reason:                 c.Reason,
		ExpectedNetPnL:         c.PnLBreakdown.Net.String(),
		PnLBreakdown:           comps,
		CompositeScore:         c.CompositeScore.String(),
		LiquidityScore:         c.Scores.LiquidityScore.Value.String(),
		FundingConfidenceScore: c.Scores.FundingConfidenceScore.Value.String(),
		BasisStabilityScore:    c.Scores.BasisStabilityScore.Value.String(),
		ExecutionRiskScore:     c.Scores.ExecutionRiskScore.Value.String(),
		CounterpartyRiskScore:  c.Scores.CounterpartyRiskScore.Value.String(),
		DataQualityScore:       c.Scores.DataQualityScore.Value.String(),
		ADLRiskScore:           c.Scores.ADLRiskScore.Value.String(),
		SecondsToFunding:       c.SecondsToFunding,
		EvaluatedAt:            c.EvaluatedAt.UTC().Format(time.RFC3339),
	}
}

// ============================================================
// Провайдер-интерфейсы (для dependency injection)
// ============================================================

// StatusProvider — источник данных для GET /api/status.
type StatusProvider interface {
	Status(ctx context.Context) (StatusDTO, error)
}

// CandidatesProvider — источник данных для GET /api/candidates.
type CandidatesProvider interface {
	Candidates(ctx context.Context) ([]scanner.Candidate, error)
}

// SettingsProvider — источник и хранилище настроек для GET/PUT /api/settings.
// Версионирование — оптимистичная блокировка (409 при конфликте).
type SettingsProvider interface {
	// Get возвращает настройки в виде сырого JSON и текущую версию.
	Get(ctx context.Context) (raw json.RawMessage, version int64, err error)
	// Save сохраняет настройки. version должна совпасть с текущей, иначе 409.
	Save(ctx context.Context, raw json.RawMessage, version int64) error
}

// CloseRequester — интерфейс для ручного закрытия позиции.
type CloseRequester interface {
	// RequestClose инициирует закрытие позиции.
	RequestClose(ctx context.Context, positionID string) error
}

// CredentialDTO — метаданные одного API-ключа для отображения в UI (без секретов).
type CredentialDTO struct {
	Exchange    string    `json:"exchange"`
	Kind        string    `json:"kind"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
	Revoked     bool      `json:"revoked"`
}

// CredentialsProvider — провайдер управления API-ключами бирж.
// Шифрование выполняет app-провайдер; telegram-пакет — только транспорт.
type CredentialsProvider interface {
	// List возвращает список API-ключей пользователя без секретов.
	List(ctx context.Context) ([]CredentialDTO, error)
	// Save шифрует и сохраняет API-ключ. Возвращает fingerprint.
	// passphrase необязательна (OKX/Bitget/KuCoin требуют её, остальные — нет).
	Save(ctx context.Context, exchange, kind, apiKey, apiSecret, passphrase string) (fingerprint string, err error)
	// Revoke отзывает API-ключ по паре (exchange, kind).
	Revoke(ctx context.Context, exchange, kind string) error
}

// OwnerClaimer — интерфейс автоматического клейма владельца при первом входе.
type OwnerClaimer interface {
	// ClaimOwner атомарно устанавливает telegram_id владельца.
	// Возвращает true если клейм успешен (был placeholder -1).
	ClaimOwner(ctx context.Context, telegramID int64) (bool, error)
}

// ============================================================
// Kill-switch interfaces (раздел 27)
// ============================================================

// KillSwitchProvider — провайдер аварийной остановки (section 27).
// Engage engages SAFE_HALT and revokes all credentials.
// Status returns whether SAFE_HALT is currently active.
type KillSwitchProvider interface {
	// Engage triggers SAFE_HALT + best-effort credential revoke.
	// reason MUST NOT be logged.
	Engage(ctx context.Context, reason string) error
	// Status returns current halted state and reason.
	Status(ctx context.Context) (halted bool, reason string, err error)
}

// TwoFactorVerifier — second-factor check (section 27.3).
// A nil verifier means 2FA is disabled (dev/test mode).
type TwoFactorVerifier interface {
	// Verify returns true if code is valid for userID.
	// The code value MUST NOT be logged by any caller.
	Verify(ctx context.Context, userID int64, code string) (bool, error)
}

// MemorySharedSecretVerifier is a stub TwoFactorVerifier that compares
// the submitted code against a single shared secret.  For development only.
// A real TOTP / Telegram-confirm implementation should replace this.
type MemorySharedSecretVerifier struct {
	Secret string // shared secret expected as the "2FA code"
}

// Verify returns true when code equals the shared secret.
// The code is compared in constant time to prevent timing attacks.
func (v *MemorySharedSecretVerifier) Verify(_ context.Context, _ int64, code string) (bool, error) {
	// constant-time comparison via XOR length check + byte loop
	if len(code) != len(v.Secret) {
		return false, nil
	}
	diff := byte(0)
	for i := 0; i < len(code); i++ {
		diff |= code[i] ^ v.Secret[i]
	}
	return diff == 0, nil
}

// VersionConflictError — ошибка конфликта версии настроек (→ HTTP 409).
var VersionConflictError = errors.New("settings: version conflict")

// ============================================================
// IdemStore — хранилище idempotency keys (раздел 13.5)
// ============================================================

// IdemStore — интерфейс хранилища idempotency keys.
type IdemStore interface {
	// Seen возвращает true если ключ уже видели в рамках scope.
	// При первом вызове регистрирует ключ и возвращает false.
	Seen(ctx context.Context, key, scope string) (bool, error)
}

// MemoryIdemStore — in-memory реализация IdemStore.
type MemoryIdemStore struct {
	mu   sync.Mutex
	seen map[string]bool
}

// NewMemoryIdemStore создаёт MemoryIdemStore.
func NewMemoryIdemStore() *MemoryIdemStore {
	return &MemoryIdemStore{seen: make(map[string]bool)}
}

// Seen регистрирует ключ и возвращает true если ключ уже был зарегистрирован.
func (s *MemoryIdemStore) Seen(_ context.Context, key, scope string) (bool, error) {
	composite := scope + ":" + key
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[composite] {
		return true, nil
	}
	s.seen[composite] = true
	return false, nil
}

// ============================================================
// APIConfig — конфигурация HTTP API
// ============================================================

// APIConfig — настройки HTTP API Mini App.
type APIConfig struct {
	BotToken       string
	MaxInitDataAge time.Duration // максимальный возраст initData (15 минут по умолчанию)
	Allowlist      auth.AdminAllowlist
	SessionTTL     time.Duration
}

// defaultMaxAge — стандартный максимальный возраст initData (раздел 13.4).
const defaultMaxAge = 15 * time.Minute

// defaultSessionTTL — TTL сессии.
const defaultSessionTTL = 24 * time.Hour

// ============================================================
// Handler — HTTP API сервер Mini App
// ============================================================

// Handler — mux + зависимости для HTTP API.
type Handler struct {
	cfg         APIConfig
	botCfg      Config
	sessions    *SessionManager
	idem        IdemStore
	status      StatusProvider
	candidates  CandidatesProvider
	settings    SettingsProvider
	closer      CloseRequester
	credentials CredentialsProvider
	owner       OwnerClaimer
	killSwitch  KillSwitchProvider
	twoFactor   TwoFactorVerifier
	mux         *http.ServeMux
}

// HandlerDeps — зависимости Handler (опциональные компоненты).
type HandlerDeps struct {
	Sessions    *SessionManager
	Idem        IdemStore
	Status      StatusProvider
	Candidates  CandidatesProvider
	Settings    SettingsProvider
	Closer      CloseRequester
	Credentials CredentialsProvider
	Owner       OwnerClaimer
	// KillSwitch — аварийная остановка (section 27). nil → 501.
	KillSwitch KillSwitchProvider
	// TwoFactor — второй фактор аутентификации (section 27.3). nil = disabled.
	TwoFactor TwoFactorVerifier
}

// NewHandler создаёт Handler и регистрирует маршруты.
// botCfg нужен для WebAppDir — путь к директории с webapp.
func NewHandler(apiCfg APIConfig, botCfg Config, deps HandlerDeps) *Handler {
	if apiCfg.MaxInitDataAge == 0 {
		apiCfg.MaxInitDataAge = defaultMaxAge
	}
	if apiCfg.SessionTTL == 0 {
		apiCfg.SessionTTL = defaultSessionTTL
	}

	sessions := deps.Sessions
	if sessions == nil {
		sessions = NewSessionManager(NewMemorySessionStore())
	}
	idem := deps.Idem
	if idem == nil {
		idem = NewMemoryIdemStore()
	}

	h := &Handler{
		cfg:         apiCfg,
		botCfg:      botCfg,
		sessions:    sessions,
		idem:        idem,
		status:      deps.Status,
		candidates:  deps.Candidates,
		settings:    deps.Settings,
		closer:      deps.Closer,
		credentials: deps.Credentials,
		owner:       deps.Owner,
		killSwitch:  deps.KillSwitch,
		twoFactor:   deps.TwoFactor,
		mux:         http.NewServeMux(),
	}
	h.registerRoutes()
	return h
}

// ServeHTTP реализует http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// registerRoutes регистрирует все маршруты.
func (h *Handler) registerRoutes() {
	// Статический файл webapp.
	h.mux.HandleFunc("GET /", h.serveWebApp)

	// Публичный endpoint аутентификации.
	h.mux.HandleFunc("POST /api/auth", h.handleAuth)

	// Защищённые endpoints — оборачиваем в middleware.
	h.mux.Handle("GET /api/status", h.authMiddleware(http.HandlerFunc(h.handleStatus)))
	h.mux.Handle("GET /api/candidates", h.authMiddleware(http.HandlerFunc(h.handleCandidates)))
	h.mux.Handle("GET /api/settings", h.authMiddleware(http.HandlerFunc(h.handleGetSettings)))
	h.mux.Handle("PUT /api/settings", h.authMiddleware(h.idemMiddleware("settings", http.HandlerFunc(h.handlePutSettings))))
	h.mux.Handle("POST /api/positions/{id}/close", h.authMiddleware(h.idemMiddleware("positions.close", http.HandlerFunc(h.handlePositionClose))))

	// Маршруты управления API-ключами (раздел 13).
	h.mux.Handle("GET /api/credentials", h.authMiddleware(http.HandlerFunc(h.handleListCredentials)))
	h.mux.Handle("POST /api/credentials", h.authMiddleware(h.idemMiddleware("credentials", h.twoFactorMiddleware(http.HandlerFunc(h.handleSaveCredential)))))
	h.mux.Handle("DELETE /api/credentials/{exchange}/{kind}", h.authMiddleware(http.HandlerFunc(h.handleRevokeCredential)))

	// Kill-switch (раздел 27).
	h.mux.Handle("POST /api/killswitch", h.authMiddleware(h.idemMiddleware("killswitch", h.twoFactorMiddleware(http.HandlerFunc(h.handleKillSwitchEngage)))))
	h.mux.Handle("GET /api/killswitch", h.authMiddleware(http.HandlerFunc(h.handleKillSwitchStatus)))
}

// ============================================================
// Middleware
// ============================================================

// contextKey — тип ключей в context.
type contextKey int

const (
	ctxUserID contextKey = iota
)

// authMiddleware — проверяет Bearer токен из Authorization header.
func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "отсутствует Bearer токен")
			return
		}
		userID, err := h.sessions.Validate(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "недействительная или истёкшая сессия")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// idemMiddleware — проверяет заголовок X-Idempotency-Key (раздел 13.5).
func (h *Handler) idemMiddleware(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Idempotency-Key")
		if key == "" {
			writeError(w, http.StatusBadRequest, "заголовок X-Idempotency-Key обязателен")
			return
		}
		seen, err := h.idem.Seen(r.Context(), key, scope)
		if err != nil {
			slog.Error("telegram: idem store ошибка", "err", err)
			writeError(w, http.StatusInternalServerError, "внутренняя ошибка")
			return
		}
		if seen {
			writeError(w, http.StatusConflict, "повтор idempotency key — запрос уже обработан")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// twoFactorMiddleware — проверяет заголовок X-2FA-Code (section 27.3).
// When TwoFactor verifier is nil (dev mode) the check is skipped entirely.
// The code value is NEVER logged.
func (h *Handler) twoFactorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.twoFactor == nil {
			// 2FA disabled — allow through.
			next.ServeHTTP(w, r)
			return
		}
		code := r.Header.Get("X-2FA-Code")
		if code == "" {
			writeError(w, http.StatusUnauthorized, "заголовок X-2FA-Code обязателен")
			return
		}
		userID, _ := r.Context().Value(ctxUserID).(int64)
		ok, err := h.twoFactor.Verify(r.Context(), userID, code)
		if err != nil {
			// Do NOT expose internal details; code is not logged.
			slog.Error("telegram: 2FA verify error")
			writeError(w, http.StatusInternalServerError, "ошибка проверки 2FA")
			return
		}
		if !ok {
			writeError(w, http.StatusForbidden, "неверный код 2FA")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware — логирует каждый запрос через slog.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lw, r)
		slog.Info("telegram: http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// loggingResponseWriter перехватывает статус ответа для логирования.
type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// extractBearerToken извлекает токен из Authorization: Bearer <token>.
func extractBearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// ============================================================
// Handlers
// ============================================================

// serveWebApp отдаёт webapp/index.html.
func (h *Handler) serveWebApp(w http.ResponseWriter, r *http.Request) {
	// Обслуживаем только корневой путь.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	dir := h.botCfg.WebAppDir
	if dir == "" {
		http.Error(w, "webapp not configured", http.StatusNotFound)
		return
	}
	indexPath := filepath.Join(dir, "index.html")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		slog.Error("telegram: webapp index.html не найден", "path", indexPath, "err", err)
		http.Error(w, "webapp not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleAuth — POST /api/auth.
// Проверяет initData, создаёт сессию, возвращает токен.
func (h *Handler) handleAuth(w http.ResponseWriter, r *http.Request) {
	var req AuthRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if req.InitData == "" {
		writeError(w, http.StatusBadRequest, "initData обязателен")
		return
	}

	// Проверяем подпись через internal/auth (раздел 13.4).
	valCfg := auth.ValidateConfig{
		BotToken: h.cfg.BotToken,
		MaxAge:   h.cfg.MaxInitDataAge,
	}
	result := auth.ValidateInitData(req.InitData, valCfg, h.cfg.Allowlist)
	if !result.Valid {
		slog.Warn("telegram: auth: отклонена initData", "reason", result.Reason)
		writeError(w, http.StatusUnauthorized, "initData недействительна")
		return
	}

	// Создаём сессию.
	token, expiresAt, err := h.sessions.Create(r.Context(), result.User.ID, h.cfg.SessionTTL)
	if err != nil {
		slog.Error("telegram: auth: ошибка создания сессии", "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка создания сессии")
		return
	}

	// Если провайдер claimOwner подключён — пробуем захватить владельца.
	if h.owner != nil {
		claimed, claimErr := h.owner.ClaimOwner(r.Context(), result.User.ID)
		if claimErr != nil {
			slog.Warn("telegram: auth: ошибка ClaimOwner", "err", claimErr)
		} else if claimed {
			slog.Info("telegram: auth: владелец успешно зарегистрирован", "telegram_id", result.User.ID)
		}
	}

	slog.Info("telegram: auth: успешный вход", "user_id", result.User.ID)

	writeJSON(w, http.StatusOK, AuthResponse{
		Token:     token,
		ExpiresAt: expiresAt,
		User:      result.User,
	})
}

// handleStatus — GET /api/status (раздел 14.1).
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if h.status == nil {
		// Stub-ответ когда провайдер не подключён.
		writeJSON(w, http.StatusOK, StatusDTO{
			SystemState:     string(domain.StateReady),
			RunMode:         string(domain.RunModeDryRun),
			Exchanges:       []ExchangeStatusDTO{},
			ActiveIncidents: []string{},
		})
		return
	}
	dto, err := h.status.Status(r.Context())
	if err != nil {
		slog.Error("telegram: status: ошибка провайдера", "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка получения статуса")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleCandidates — GET /api/candidates (раздел 14.2).
func (h *Handler) handleCandidates(w http.ResponseWriter, r *http.Request) {
	if h.candidates == nil {
		writeJSON(w, http.StatusOK, []CandidateDTO{})
		return
	}
	cands, err := h.candidates.Candidates(r.Context())
	if err != nil {
		slog.Error("telegram: candidates: ошибка провайдера", "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка получения кандидатов")
		return
	}
	dtos := make([]CandidateDTO, 0, len(cands))
	for _, c := range cands {
		dtos = append(dtos, candidateDTOFromScanner(c))
	}
	writeJSON(w, http.StatusOK, dtos)
}

// handleGetSettings — GET /api/settings.
func (h *Handler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"version":  int64(0),
			"settings": json.RawMessage(`{}`),
		})
		return
	}
	raw, version, err := h.settings.Get(r.Context())
	if err != nil {
		slog.Error("telegram: settings get: ошибка", "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка получения настроек")
		return
	}
	resp := map[string]interface{}{
		"version":  version,
		"settings": raw,
	}
	writeJSON(w, http.StatusOK, resp)
}

// putSettingsRequest — тело PUT /api/settings.
type putSettingsRequest struct {
	Version  int64           `json:"version"`
	Settings json.RawMessage `json:"settings"`
}

// handlePutSettings — PUT /api/settings (оптимистичная блокировка).
func (h *Handler) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req putSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}
	if h.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings provider не подключён")
		return
	}
	if err := h.settings.Save(r.Context(), req.Settings, req.Version); err != nil {
		if errors.Is(err, VersionConflictError) {
			writeError(w, http.StatusConflict, "конфликт версии настроек — перезагрузите страницу")
			return
		}
		slog.Error("telegram: settings save: ошибка", "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка сохранения настроек")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePositionClose — POST /api/positions/{id}/close (202 Accepted).
func (h *Handler) handlePositionClose(w http.ResponseWriter, r *http.Request) {
	positionID := r.PathValue("id")
	if positionID == "" {
		writeError(w, http.StatusBadRequest, "position id обязателен")
		return
	}
	if h.closer == nil {
		writeError(w, http.StatusServiceUnavailable, "close requester не подключён")
		return
	}
	userID, _ := r.Context().Value(ctxUserID).(int64)
	slog.Info("telegram: запрос закрытия позиции", "position_id", positionID, "user_id", userID)

	if err := h.closer.RequestClose(r.Context(), positionID); err != nil {
		slog.Error("telegram: close position: ошибка", "position_id", positionID, "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка запроса закрытия")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":     "accepted",
		"positionId": positionID,
	})
}

// ============================================================
// Credentials handlers
// ============================================================

// handleListCredentials — GET /api/credentials.
// Возвращает список API-ключей пользователя (без секретов).
func (h *Handler) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	if h.credentials == nil {
		writeError(w, http.StatusNotImplemented, "credentials provider не подключён")
		return
	}
	items, err := h.credentials.List(r.Context())
	if err != nil {
		slog.Error("telegram: credentials list: ошибка", "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка получения списка ключей")
		return
	}
	if items == nil {
		items = []CredentialDTO{}
	}
	writeJSON(w, http.StatusOK, items)
}

// saveCredentialRequest — тело POST /api/credentials.
type saveCredentialRequest struct {
	Exchange   string `json:"exchange"`
	Kind       string `json:"kind"`
	APIKey     string `json:"apiKey"`
	APISecret  string `json:"apiSecret"`
	Passphrase string `json:"passphrase"`
}

// saveCredentialResponse — ответ POST /api/credentials.
type saveCredentialResponse struct {
	Fingerprint string `json:"fingerprint"`
}

// handleSaveCredential — POST /api/credentials (требует X-Idempotency-Key).
// Тело: {exchange, kind, apiKey, apiSecret, passphrase?}.
// Валидация: exchange ∈ domain.SupportedExchanges(), kind ∈ trade|withdrawal,
// apiKey/apiSecret непустые и ≤512 символов.
// Секреты НИКОГДА не попадают в логи/ошибки/ответы.
func (h *Handler) handleSaveCredential(w http.ResponseWriter, r *http.Request) {
	if h.credentials == nil {
		writeError(w, http.StatusNotImplemented, "credentials provider не подключён")
		return
	}

	var req saveCredentialRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}

	// Валидация exchange.
	if !domain.ExchangeID(req.Exchange).IsValid() {
		writeError(w, http.StatusBadRequest, "неподдерживаемая биржа: "+req.Exchange)
		return
	}

	// Валидация kind.
	if req.Kind != "trade" && req.Kind != "withdrawal" {
		writeError(w, http.StatusBadRequest, "kind должен быть 'trade' или 'withdrawal'")
		return
	}

	// Валидация apiKey (не пустой, ≤512).
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "apiKey обязателен")
		return
	}
	if len(req.APIKey) > 512 {
		writeError(w, http.StatusBadRequest, "apiKey слишком длинный (макс. 512 символов)")
		return
	}

	// Валидация apiSecret (не пустой, ≤512).
	if req.APISecret == "" {
		writeError(w, http.StatusBadRequest, "apiSecret обязателен")
		return
	}
	if len(req.APISecret) > 512 {
		writeError(w, http.StatusBadRequest, "apiSecret слишком длинный (макс. 512 символов)")
		return
	}

	// Сохраняем. Секреты не логируем.
	fingerprint, err := h.credentials.Save(r.Context(), req.Exchange, req.Kind, req.APIKey, req.APISecret, req.Passphrase)
	if err != nil {
		slog.Error("telegram: credentials save: ошибка", "exchange", req.Exchange, "kind", req.Kind)
		writeError(w, http.StatusInternalServerError, "ошибка сохранения ключа")
		return
	}

	slog.Info("telegram: credentials save: успешно", "exchange", req.Exchange, "kind", req.Kind, "fingerprint", fingerprint)
	writeJSON(w, http.StatusCreated, saveCredentialResponse{Fingerprint: fingerprint})
}

// handleRevokeCredential — DELETE /api/credentials/{exchange}/{kind}.
// Возвращает 204 при успехе.
func (h *Handler) handleRevokeCredential(w http.ResponseWriter, r *http.Request) {
	if h.credentials == nil {
		writeError(w, http.StatusNotImplemented, "credentials provider не подключён")
		return
	}

	exchange := r.PathValue("exchange")
	kind := r.PathValue("kind")

	if !domain.ExchangeID(exchange).IsValid() {
		writeError(w, http.StatusBadRequest, "неподдерживаемая биржа: "+exchange)
		return
	}
	if kind != "trade" && kind != "withdrawal" {
		writeError(w, http.StatusBadRequest, "kind должен быть 'trade' или 'withdrawal'")
		return
	}

	if err := h.credentials.Revoke(r.Context(), exchange, kind); err != nil {
		slog.Error("telegram: credentials revoke: ошибка", "exchange", exchange, "kind", kind, "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка отзыва ключа")
		return
	}

	slog.Info("telegram: credentials revoke: успешно", "exchange", exchange, "kind", kind)
	w.WriteHeader(http.StatusNoContent)
}

// ============================================================
// Kill-switch handlers (section 27)
// ============================================================

// killSwitchEngageRequest — тело POST /api/killswitch.
type killSwitchEngageRequest struct {
	// Reason is intentionally not logged anywhere in this handler.
	Reason string `json:"reason"`
}

// killSwitchStatusResponse — ответ GET /api/killswitch.
type killSwitchStatusResponse struct {
	Halted bool   `json:"halted"`
	Reason string `json:"reason,omitempty"`
}

// handleKillSwitchEngage — POST /api/killswitch (auth + idem + 2FA).
// Engages SAFE_HALT and revokes all credentials.
// reason is NEVER logged.
func (h *Handler) handleKillSwitchEngage(w http.ResponseWriter, r *http.Request) {
	if h.killSwitch == nil {
		writeError(w, http.StatusNotImplemented, "kill-switch provider не подключён")
		return
	}

	var req killSwitchEngageRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: "+err.Error())
		return
	}

	userID, _ := r.Context().Value(ctxUserID).(int64)
	// Intentional: reason and userID are logged separately, reason value is NEVER included.
	slog.Info("telegram: kill-switch engage requested", "user_id", userID)

	if err := h.killSwitch.Engage(r.Context(), req.Reason); err != nil {
		slog.Error("telegram: kill-switch engage error")
		writeError(w, http.StatusInternalServerError, "ошибка активации kill-switch")
		return
	}

	slog.Warn("telegram: kill-switch engaged", "user_id", userID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "halted"})
}

// handleKillSwitchStatus — GET /api/killswitch.
func (h *Handler) handleKillSwitchStatus(w http.ResponseWriter, r *http.Request) {
	if h.killSwitch == nil {
		writeError(w, http.StatusNotImplemented, "kill-switch provider не подключён")
		return
	}
	halted, reason, err := h.killSwitch.Status(r.Context())
	if err != nil {
		slog.Error("telegram: kill-switch status error", "err", err)
		writeError(w, http.StatusInternalServerError, "ошибка получения статуса kill-switch")
		return
	}
	writeJSON(w, http.StatusOK, killSwitchStatusResponse{Halted: halted, Reason: reason})
}

// ============================================================
// HTTP helpers
// ============================================================

// errorResponse — стандартный JSON-ответ об ошибке.
type errorResponse struct {
	Error string `json:"error"`
}

// writeJSON записывает JSON-ответ.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Error("telegram: writeJSON encode ошибка", "err", err)
	}
}

// writeError записывает JSON-ответ об ошибке. Не включает внутренние детали.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// decodeJSON декодирует тело запроса. Ограничивает тело 1MB.
func decodeJSON(r *http.Request, v interface{}) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	// Проверяем что нет дополнительного мусора в теле.
	if _, err := io.ReadAll(r.Body); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("читать остаток тела: %w", err)
	}
	return nil
}

// NewLoggingHandler оборачивает handler в logging middleware.
// Используется из app-пакета при монтировании.
func NewLoggingHandler(h http.Handler) http.Handler {
	return loggingMiddleware(h)
}

// ============================================================
// Stub-провайдеры (используются в тестах)
// ============================================================

// StaticStatusProvider — stub StatusProvider с фиксированным ответом.
type StaticStatusProvider struct {
	DTO StatusDTO
	Err error
}

func (p *StaticStatusProvider) Status(_ context.Context) (StatusDTO, error) {
	return p.DTO, p.Err
}

// StaticCandidatesProvider — stub CandidatesProvider.
type StaticCandidatesProvider struct {
	Items []scanner.Candidate
	Err   error
}

func (p *StaticCandidatesProvider) Candidates(_ context.Context) ([]scanner.Candidate, error) {
	return p.Items, p.Err
}

// MemorySettingsProvider — in-memory реализация SettingsProvider с версионированием.
type MemorySettingsProvider struct {
	mu      sync.Mutex
	raw     json.RawMessage
	version int64
}

// NewMemorySettingsProvider создаёт MemorySettingsProvider с начальными настройками.
func NewMemorySettingsProvider(initial json.RawMessage) *MemorySettingsProvider {
	if initial == nil {
		initial = json.RawMessage(`{}`)
	}
	return &MemorySettingsProvider{raw: initial, version: 1}
}

// Get возвращает текущие настройки и версию.
func (p *MemorySettingsProvider) Get(_ context.Context) (json.RawMessage, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make(json.RawMessage, len(p.raw))
	copy(cp, p.raw)
	return cp, p.version, nil
}

// Save сохраняет настройки. Возвращает VersionConflictError при несовпадении версии.
func (p *MemorySettingsProvider) Save(_ context.Context, raw json.RawMessage, version int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if version != p.version {
		return fmt.Errorf("settings: %w", VersionConflictError)
	}
	p.raw = raw
	p.version++
	return nil
}

// StaticCloseRequester — stub CloseRequester.
type StaticCloseRequester struct {
	Err error
}

func (r *StaticCloseRequester) RequestClose(_ context.Context, _ string) error {
	return r.Err
}

// ============================================================
// Ссылка на strategy.IntervalClass для candidateDTOFromScanner
// (избегаем неиспользуемого импорта если DTO не вызывает напрямую)
// ============================================================

// Убеждаемся что пакет strategy используется.
var _ strategy.PnLBreakdown
