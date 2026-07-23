package telegram

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/auth"
	"github.com/thecd/fundarbitrage/internal/scanner"
)

// ============================================================
// Helpers для генерации корректного initData (повторяем паттерн из auth пакета)
// ============================================================

const testBotToken = "5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"
const testUserID = int64(279058397)

// signInitData — HMAC-SHA256 подпись по алгоритму Telegram WebApp.
func signInitData(botToken string, decoded map[string]string) string {
	keys := make([]string, 0, len(decoded))
	for k := range decoded {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var dcs strings.Builder
	for i, k := range keys {
		if i > 0 {
			dcs.WriteByte('\n')
		}
		dcs.WriteString(k)
		dcs.WriteByte('=')
		dcs.WriteString(decoded[k])
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	h := hmac.New(sha256.New, secret.Sum(nil))
	h.Write([]byte(dcs.String()))
	return hex.EncodeToString(h.Sum(nil))
}

// buildInitData — URL-encoded initData из декодированных пар + hash.
func buildInitData(decoded map[string]string, hash string) string {
	keys := make([]string, 0, len(decoded))
	for k := range decoded {
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
		b.WriteString(url.QueryEscape(decoded[k]))
	}
	b.WriteString("&hash=")
	b.WriteString(hash)
	return b.String()
}

const testUserJSON = `{"id":279058397,"first_name":"dev","last_name":"","username":"devuser","language_code":"en"}`

// validPairs возвращает корректные декодированные поля initData.
func validPairs() map[string]string {
	return map[string]string{
		"query_id":  "AAHdF6IQAAAAAN0XohDhrOrc",
		"user":      testUserJSON,
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
	}
}

// buildValidInitData генерирует подписанный initData с текущим временем.
func buildValidInitData() string {
	pairs := validPairs()
	hash := signInitData(testBotToken, pairs)
	return buildInitData(pairs, hash)
}

// ============================================================
// Test setup
// ============================================================

// newTestHandler создаёт Handler для тестов с нужными зависимостями.
func newTestHandler(deps HandlerDeps) (*Handler, *httptest.Server) {
	apiCfg := APIConfig{
		BotToken:       testBotToken,
		MaxInitDataAge: 15 * time.Minute,
		Allowlist:      auth.StaticAllowlist{testUserID: true},
		SessionTTL:     time.Hour,
	}
	botCfg := Config{}
	h := NewHandler(apiCfg, botCfg, deps)
	srv := httptest.NewServer(h)
	return h, srv
}

// doRequest выполняет HTTP запрос к тестовому серверу.
func doRequest(t *testing.T, srv *httptest.Server, method, path, token, idemKey string, body interface{}) *http.Response {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, srv.URL+path, reqBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("X-Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// decodeResp декодирует JSON тело ответа в v.
func decodeResp(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// ============================================================
// Auth tests
// ============================================================

// TestAuth_HappyPath — корректный initData → токен в ответе.
func TestAuth_HappyPath(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	initData := buildValidInitData()
	resp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, хотим 200; body: %s", resp.StatusCode, body)
	}

	var ar AuthResponse
	decodeResp(t, resp, &ar)
	if ar.Token == "" {
		t.Fatal("пустой token в ответе")
	}
	if ar.User.ID != testUserID {
		t.Fatalf("user.id = %d, хотим %d", ar.User.ID, testUserID)
	}
	if ar.ExpiresAt.IsZero() {
		t.Fatal("пустой expiresAt")
	}
}

// TestAuth_BadToken — неверный hash → 401.
func TestAuth_BadToken(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	// Подписываем другим токеном.
	pairs := validPairs()
	hash := signInitData("wrong:token", pairs)
	initData := buildInitData(pairs, hash)

	resp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, хотим 401", resp.StatusCode)
	}
}

// TestAuth_EmptyInitData — пустой initData → 400.
func TestAuth_EmptyInitData(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	resp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, хотим 400", resp.StatusCode)
	}
}

// ============================================================
// Bearer middleware tests
// ============================================================

// TestBearer_Missing — нет Authorization header → 401.
func TestBearer_Missing(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	resp := doRequest(t, srv, "GET", "/api/status", "", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, хотим 401", resp.StatusCode)
	}
}

// TestBearer_Invalid — некорректный токен → 401.
func TestBearer_Invalid(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	resp := doRequest(t, srv, "GET", "/api/status", "invalid-token", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, хотим 401", resp.StatusCode)
	}
}

// TestBearer_ValidAfterAuth — корректный токен после /api/auth → 200.
func TestBearer_ValidAfterAuth(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{
		Status: &StaticStatusProvider{DTO: StatusDTO{
			SystemState:     "READY",
			Exchanges:       []ExchangeStatusDTO{},
			ActiveIncidents: []string{},
		}},
	})
	defer srv.Close()

	// Аутентифицируемся.
	initData := buildValidInitData()
	authResp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("auth status = %d", authResp.StatusCode)
	}
	var ar AuthResponse
	decodeResp(t, authResp, &ar)

	// Запрашиваем /api/status с токеном.
	resp := doRequest(t, srv, "GET", "/api/status", ar.Token, "", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, хотим 200; body: %s", resp.StatusCode, body)
	}
}

// ============================================================
// Idempotency middleware tests
// ============================================================

// TestIdempotency_MissingKey — мутирующий endpoint без X-Idempotency-Key → 400.
func TestIdempotency_MissingKey(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	// Получаем токен.
	initData := buildValidInitData()
	authResp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	var ar AuthResponse
	decodeResp(t, authResp, &ar)

	// PUT /api/settings без idempotency key.
	resp := doRequest(t, srv, "PUT", "/api/settings", ar.Token, "", putSettingsRequest{
		Version:  1,
		Settings: json.RawMessage(`{}`),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, хотим 400 (нет idempotency key)", resp.StatusCode)
	}
}

// TestIdempotency_DuplicateKey — повторный запрос с тем же ключом → 409.
func TestIdempotency_DuplicateKey(t *testing.T) {
	settings := NewMemorySettingsProvider(json.RawMessage(`{}`))
	_, srv := newTestHandler(HandlerDeps{Settings: settings})
	defer srv.Close()

	// Получаем токен.
	initData := buildValidInitData()
	authResp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	var ar AuthResponse
	decodeResp(t, authResp, &ar)

	idemKey := "test-idem-key-001"

	// Первый запрос — должен пройти.
	resp1 := doRequest(t, srv, "PUT", "/api/settings", ar.Token, idemKey, putSettingsRequest{
		Version:  1,
		Settings: json.RawMessage(`{"leverage":3}`),
	})
	if resp1.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("первый PUT settings = %d; body: %s", resp1.StatusCode, body)
	}

	// Второй запрос с тем же ключом → 409.
	resp2 := doRequest(t, srv, "PUT", "/api/settings", ar.Token, idemKey, putSettingsRequest{
		Version:  2,
		Settings: json.RawMessage(`{"leverage":5}`),
	})
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("второй PUT settings = %d, хотим 409 (duplicate idem key)", resp2.StatusCode)
	}
}

// ============================================================
// Settings version conflict test
// ============================================================

// TestSettings_VersionConflict — устаревшая версия → 409.
func TestSettings_VersionConflict(t *testing.T) {
	settings := NewMemorySettingsProvider(json.RawMessage(`{}`))
	_, srv := newTestHandler(HandlerDeps{Settings: settings})
	defer srv.Close()

	// Получаем токен.
	initData := buildValidInitData()
	authResp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	var ar AuthResponse
	decodeResp(t, authResp, &ar)

	// Первый save — успешный.
	resp1 := doRequest(t, srv, "PUT", "/api/settings", ar.Token, "idem-v1", putSettingsRequest{
		Version:  1,
		Settings: json.RawMessage(`{"leverage":3}`),
	})
	if resp1.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp1.Body)
		t.Fatalf("первый save = %d; body: %s", resp1.StatusCode, body)
	}

	// Второй save с устаревшей версией (1 вместо 2) → 409.
	resp2 := doRequest(t, srv, "PUT", "/api/settings", ar.Token, "idem-v1-b", putSettingsRequest{
		Version:  1, // устаревшая версия
		Settings: json.RawMessage(`{"leverage":5}`),
	})
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("save со старой версией = %d, хотим 409", resp2.StatusCode)
	}
}

// ============================================================
// Candidates test
// ============================================================

// TestCandidates_Endpoint — GET /api/candidates возвращает список кандидатов.
func TestCandidates_Endpoint(t *testing.T) {
	cands := []scanner.Candidate{
		{
			Asset:         "BTC",
			LongExchange:  "binance",
			ShortExchange: "bybit",
			Eligible:      true,
			EvaluatedAt:   time.Now(),
		},
	}
	_, srv := newTestHandler(HandlerDeps{
		Candidates: &StaticCandidatesProvider{Items: cands},
	})
	defer srv.Close()

	// Получаем токен.
	initData := buildValidInitData()
	authResp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	var ar AuthResponse
	decodeResp(t, authResp, &ar)

	resp := doRequest(t, srv, "GET", "/api/candidates", ar.Token, "", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("candidates status = %d; body: %s", resp.StatusCode, body)
	}

	var dtos []CandidateDTO
	decodeResp(t, resp, &dtos)
	if len(dtos) != 1 {
		t.Fatalf("len(candidates) = %d, хотим 1", len(dtos))
	}
	if dtos[0].Asset != "BTC" {
		t.Fatalf("asset = %q, хотим BTC", dtos[0].Asset)
	}
}

// ============================================================
// PositionClose test
// ============================================================

// TestPositionClose_Accepted — POST /api/positions/{id}/close → 202.
func TestPositionClose_Accepted(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{
		Closer: &StaticCloseRequester{},
	})
	defer srv.Close()

	// Получаем токен.
	initData := buildValidInitData()
	authResp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	var ar AuthResponse
	decodeResp(t, authResp, &ar)

	resp := doRequest(t, srv, "POST", "/api/positions/pos-001/close", ar.Token, "idem-close-001", nil)
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("close status = %d, хотим 202; body: %s", resp.StatusCode, body)
	}
}

// ============================================================
// IdemStore test
// ============================================================

// TestMemoryIdemStore_SeenBehavior — первый вызов → false, второй → true.
func TestMemoryIdemStore_SeenBehavior(t *testing.T) {
	store := NewMemoryIdemStore()
	ctx := context.Background()

	seen1, err := store.Seen(ctx, "key1", "scope")
	if err != nil {
		t.Fatal(err)
	}
	if seen1 {
		t.Fatal("первый Seen должен возвращать false")
	}

	seen2, err := store.Seen(ctx, "key1", "scope")
	if err != nil {
		t.Fatal(err)
	}
	if !seen2 {
		t.Fatal("второй Seen должен возвращать true")
	}
}

// TestMemoryIdemStore_ScopeSeparation — одинаковый key в разных scope независимы.
func TestMemoryIdemStore_ScopeSeparation(t *testing.T) {
	store := NewMemoryIdemStore()
	ctx := context.Background()

	store.Seen(ctx, "key1", "scope-a")
	// Тот же ключ, но другой scope — не должен быть seen.
	seen, _ := store.Seen(ctx, "key1", "scope-b")
	if seen {
		t.Fatal("разные scope должны быть независимыми")
	}
}

// ============================================================
// Notifier rate-limit test
// ============================================================

// mockBot — fake Telegram сервер для тестов Notifier.
func newMockBotAndServer(t *testing.T) (*Bot, *httptest.Server, *int) {
	t.Helper()
	count := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			*count++
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":     true,
			"result": map[string]interface{}{"message_id": *count},
		})
	}))
	bot := NewBot(Config{
		Token:      "TEST_TOKEN",
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	return bot, srv, count
}

// TestNotifier_RateLimitsDuplicates — повторная отправка с тем же dedup key в пределах интервала игнорируется.
func TestNotifier_RateLimitsDuplicates(t *testing.T) {
	bot, srv, count := newMockBotAndServer(t)
	defer srv.Close()

	notifier := NewNotifier(bot, NotifierConfig{
		ChatID:      123456,
		MinInterval: time.Minute, // 1 минута — повторные отправки в тесте будут заблокированы
	}, nil)

	ctx := context.Background()
	key := "test-safe-halt"

	// Первый вызов должен отправить.
	if err := notifier.Notify(ctx, key, KindSafeHalt, SeverityCritical, "system halted"); err != nil {
		t.Fatalf("первый Notify: %v", err)
	}
	if *count != 1 {
		t.Fatalf("после первого вызова count = %d, хотим 1", *count)
	}

	// Второй вызов с тем же ключом в пределах MinInterval — должен быть заблокирован.
	if err := notifier.Notify(ctx, key, KindSafeHalt, SeverityCritical, "system halted again"); err != nil {
		t.Fatalf("второй Notify (rate-limited): %v", err)
	}
	if *count != 1 {
		t.Fatalf("после второго вызова count = %d, хотим 1 (второй должен быть заблокирован)", *count)
	}

	// Другой ключ — должен отправить.
	if err := notifier.Notify(ctx, "other-key", KindBotStart, SeverityInfo, "bot started"); err != nil {
		t.Fatalf("Notify с другим ключом: %v", err)
	}
	if *count != 2 {
		t.Fatalf("после третьего вызова (другой ключ) count = %d, хотим 2", *count)
	}
}

// TestNotifier_AllowsAfterInterval — после истечения интервала отправка разрешена.
func TestNotifier_AllowsAfterInterval(t *testing.T) {
	bot, srv, count := newMockBotAndServer(t)
	defer srv.Close()

	notifier := NewNotifier(bot, NotifierConfig{
		ChatID:      123456,
		MinInterval: 50 * time.Millisecond,
	}, nil)

	ctx := context.Background()
	key := "rate-test"

	// Первый вызов.
	notifier.Notify(ctx, key, KindBotStart, SeverityInfo, "first")
	if *count != 1 {
		t.Fatalf("count = %d, хотим 1", *count)
	}

	// Ждём пока интервал истечёт.
	time.Sleep(60 * time.Millisecond)

	// Второй вызов после интервала — должен пройти.
	notifier.Notify(ctx, key, KindBotStart, SeverityInfo, "second")
	if *count != 2 {
		t.Fatalf("после паузы count = %d, хотим 2", *count)
	}
}

// ============================================================
// Stub CredentialsProvider для тестов
// ============================================================

// fakeCredentialsProvider — тестовый stub CredentialsProvider.
type fakeCredentialsProvider struct {
	items               []CredentialDTO
	savedExchange       string
	savedKind           string
	savedAPIKey         string
	savedAPISecret      string
	savedPassphrase     string
	fingerprintToReturn string
	revokedExchange     string
	revokedKind         string
	saveErr             error
	listErr             error
	revokeErr           error
}

func (f *fakeCredentialsProvider) List(_ context.Context) ([]CredentialDTO, error) {
	return f.items, f.listErr
}

func (f *fakeCredentialsProvider) Save(_ context.Context, exchange, kind, apiKey, apiSecret, passphrase string) (string, error) {
	f.savedExchange = exchange
	f.savedKind = kind
	f.savedAPIKey = apiKey
	f.savedAPISecret = apiSecret
	f.savedPassphrase = passphrase
	return f.fingerprintToReturn, f.saveErr
}

func (f *fakeCredentialsProvider) Revoke(_ context.Context, exchange, kind string) error {
	f.revokedExchange = exchange
	f.revokedKind = kind
	return f.revokeErr
}

// fakeOwnerClaimer — тестовый stub OwnerClaimer.
type fakeOwnerClaimer struct {
	claimedID int64
	result    bool
	err       error
}

func (f *fakeOwnerClaimer) ClaimOwner(_ context.Context, telegramID int64) (bool, error) {
	f.claimedID = telegramID
	return f.result, f.err
}

// getAuthToken выполняет /api/auth и возвращает Bearer token.
func getAuthToken(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	initData := buildValidInitData()
	authResp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	if authResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(authResp.Body)
		t.Fatalf("auth failed %d: %s", authResp.StatusCode, body)
	}
	var ar AuthResponse
	decodeResp(t, authResp, &ar)
	return ar.Token
}

// ============================================================
// Credentials — GET /api/credentials
// ============================================================

// TestCredentials_List — GET /api/credentials возвращает список.
func TestCredentials_List(t *testing.T) {
	fp := &fakeCredentialsProvider{
		items: []CredentialDTO{
			{Exchange: "binance", Kind: "trade", Fingerprint: "ABCD1234...", CreatedAt: time.Now(), Revoked: false},
		},
	}
	_, srv := newTestHandler(HandlerDeps{Credentials: fp})
	defer srv.Close()

	token := getAuthToken(t, srv)
	resp := doRequest(t, srv, "GET", "/api/credentials", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/credentials = %d; body: %s", resp.StatusCode, body)
	}

	var items []CredentialDTO
	decodeResp(t, resp, &items)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, хотим 1", len(items))
	}
	if items[0].Exchange != "binance" {
		t.Errorf("exchange = %q, хотим binance", items[0].Exchange)
	}
}

// TestCredentials_List_NilProvider — 501 когда credentials не подключён.
func TestCredentials_List_NilProvider(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	token := getAuthToken(t, srv)
	resp := doRequest(t, srv, "GET", "/api/credentials", token, "", nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("GET /api/credentials без провайдера = %d, хотим 501", resp.StatusCode)
	}
}

// ============================================================
// Credentials — POST /api/credentials
// ============================================================

// TestCredentials_Save_201 — POST /api/credentials → 201 с fingerprint; passphrase доходит до провайдера.
func TestCredentials_Save_201(t *testing.T) {
	fp := &fakeCredentialsProvider{fingerprintToReturn: "MYFP..."}
	_, srv := newTestHandler(HandlerDeps{Credentials: fp})
	defer srv.Close()

	token := getAuthToken(t, srv)

	body := saveCredentialRequest{
		Exchange:   "okx",
		Kind:       "trade",
		APIKey:     "my-api-key",
		APISecret:  "my-api-secret",
		Passphrase: "my-passphrase",
	}
	resp := doRequest(t, srv, "POST", "/api/credentials", token, "idem-save-001", body)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/credentials = %d, хотим 201; body: %s", resp.StatusCode, b)
	}

	var result saveCredentialResponse
	decodeResp(t, resp, &result)
	if result.Fingerprint != "MYFP..." {
		t.Errorf("fingerprint = %q, хотим MYFP...", result.Fingerprint)
	}

	// Проверяем что passphrase дошла до провайдера.
	if fp.savedPassphrase != "my-passphrase" {
		t.Errorf("passphrase = %q, хотим my-passphrase", fp.savedPassphrase)
	}
	if fp.savedExchange != "okx" {
		t.Errorf("exchange = %q, хотим okx", fp.savedExchange)
	}
	if fp.savedKind != "trade" {
		t.Errorf("kind = %q, хотим trade", fp.savedKind)
	}
	if fp.savedAPIKey != "my-api-key" {
		t.Errorf("apiKey = %q, хотим my-api-key", fp.savedAPIKey)
	}
}

// TestCredentials_Save_EmptyAPIKey — пустой apiKey → 400.
func TestCredentials_Save_EmptyAPIKey(t *testing.T) {
	fp := &fakeCredentialsProvider{fingerprintToReturn: "MYFP..."}
	_, srv := newTestHandler(HandlerDeps{Credentials: fp})
	defer srv.Close()

	token := getAuthToken(t, srv)
	body := saveCredentialRequest{Exchange: "binance", Kind: "trade", APIKey: "", APISecret: "secret"}
	resp := doRequest(t, srv, "POST", "/api/credentials", token, "idem-bad-key", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("пустой apiKey: статус = %d, хотим 400", resp.StatusCode)
	}
}

// TestCredentials_Save_UnknownExchange — неизвестная биржа → 400.
func TestCredentials_Save_UnknownExchange(t *testing.T) {
	fp := &fakeCredentialsProvider{fingerprintToReturn: "MYFP..."}
	_, srv := newTestHandler(HandlerDeps{Credentials: fp})
	defer srv.Close()

	token := getAuthToken(t, srv)
	body := saveCredentialRequest{Exchange: "unknown_exchange", Kind: "trade", APIKey: "k", APISecret: "s"}
	resp := doRequest(t, srv, "POST", "/api/credentials", token, "idem-bad-exch", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("неизвестная биржа: статус = %d, хотим 400", resp.StatusCode)
	}
}

// TestCredentials_Save_NilProvider — 501 когда credentials не подключён.
func TestCredentials_Save_NilProvider(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	token := getAuthToken(t, srv)
	body := saveCredentialRequest{Exchange: "binance", Kind: "trade", APIKey: "k", APISecret: "s"}
	resp := doRequest(t, srv, "POST", "/api/credentials", token, "idem-nil", body)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("nil provider: статус = %d, хотим 501", resp.StatusCode)
	}
}

// TestCredentials_Save_InvalidKind — неверный kind → 400.
func TestCredentials_Save_InvalidKind(t *testing.T) {
	fp := &fakeCredentialsProvider{}
	_, srv := newTestHandler(HandlerDeps{Credentials: fp})
	defer srv.Close()

	token := getAuthToken(t, srv)
	body := saveCredentialRequest{Exchange: "binance", Kind: "invalid", APIKey: "k", APISecret: "s"}
	resp := doRequest(t, srv, "POST", "/api/credentials", token, "idem-bad-kind", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("неверный kind: статус = %d, хотим 400", resp.StatusCode)
	}
}

// ============================================================
// Credentials — DELETE /api/credentials/{exchange}/{kind}
// ============================================================

// TestCredentials_Revoke_204 — DELETE /api/credentials/binance/trade → 204.
func TestCredentials_Revoke_204(t *testing.T) {
	fp := &fakeCredentialsProvider{}
	_, srv := newTestHandler(HandlerDeps{Credentials: fp})
	defer srv.Close()

	token := getAuthToken(t, srv)
	resp := doRequest(t, srv, "DELETE", "/api/credentials/binance/trade", token, "", nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE /api/credentials/binance/trade = %d, хотим 204; body: %s", resp.StatusCode, b)
	}

	if fp.revokedExchange != "binance" {
		t.Errorf("revokedExchange = %q, хотим binance", fp.revokedExchange)
	}
	if fp.revokedKind != "trade" {
		t.Errorf("revokedKind = %q, хотим trade", fp.revokedKind)
	}
}

// TestCredentials_Revoke_NilProvider — 501 когда credentials не подключён.
func TestCredentials_Revoke_NilProvider(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	token := getAuthToken(t, srv)
	resp := doRequest(t, srv, "DELETE", "/api/credentials/binance/trade", token, "", nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("nil provider revoke: статус = %d, хотим 501", resp.StatusCode)
	}
}

// ============================================================
// ClaimOwner — вызывается при auth
// ============================================================

// TestClaimOwner_CalledOnAuth — ClaimOwner вызывается при auth; claimed=true → нет ошибки.
func TestClaimOwner_CalledOnAuth(t *testing.T) {
	owner := &fakeOwnerClaimer{result: true}
	_, srv := newTestHandler(HandlerDeps{Owner: owner})
	defer srv.Close()

	initData := buildValidInitData()
	resp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("auth = %d; body: %s", resp.StatusCode, body)
	}

	// Проверяем что ClaimOwner вызван с корректным telegram_id.
	if owner.claimedID != testUserID {
		t.Errorf("ClaimOwner вызван с telegramID=%d, хотим %d", owner.claimedID, testUserID)
	}
}

// TestClaimOwner_NilOwner — auth работает без Owner (nil-safe).
func TestClaimOwner_NilOwner(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	initData := buildValidInitData()
	resp := doRequest(t, srv, "POST", "/api/auth", "", "", AuthRequest{InitData: initData})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("auth без Owner = %d; body: %s", resp.StatusCode, body)
	}
}

// ============================================================
// Kill-switch tests
// ============================================================

// fakeKillSwitchProvider — stub KillSwitchProvider.
type fakeKillSwitchProvider struct {
	engageCalled bool
	engageReason string
	engageErr    error
	halted       bool
	haltedReason string
	statusErr    error
}

func (f *fakeKillSwitchProvider) Engage(_ context.Context, reason string) error {
	f.engageCalled = true
	f.engageReason = reason
	if f.engageErr == nil {
		f.halted = true
	}
	return f.engageErr
}

func (f *fakeKillSwitchProvider) Status(_ context.Context) (bool, string, error) {
	return f.halted, f.haltedReason, f.statusErr
}

// TestKillSwitch_Engage_200 — POST /api/killswitch → engage called, 200.
func TestKillSwitch_Engage_200(t *testing.T) {
	ks := &fakeKillSwitchProvider{}
	_, srv := newTestHandler(HandlerDeps{KillSwitch: ks})
	defer srv.Close()

	token := getAuthToken(t, srv)

	req := struct {
		Reason string `json:"reason"`
	}{Reason: "test emergency"}

	resp := doRequestWithHeaders(t, srv, "POST", "/api/killswitch", token, "idem-ks-001", nil, req)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/killswitch = %d, want 200; body: %s", resp.StatusCode, b)
	}

	if !ks.engageCalled {
		t.Error("Engage was not called")
	}

	var result map[string]string
	decodeResp(t, resp, &result)
	if result["status"] != "halted" {
		t.Errorf("status = %q, want halted", result["status"])
	}
}

// TestKillSwitch_Status_Get — GET /api/killswitch returns current state.
func TestKillSwitch_Status_Get(t *testing.T) {
	ks := &fakeKillSwitchProvider{halted: true, haltedReason: "test"}
	_, srv := newTestHandler(HandlerDeps{KillSwitch: ks})
	defer srv.Close()

	token := getAuthToken(t, srv)
	resp := doRequest(t, srv, "GET", "/api/killswitch", token, "", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/killswitch = %d; body: %s", resp.StatusCode, b)
	}

	var result struct {
		Halted bool   `json:"halted"`
		Reason string `json:"reason"`
	}
	decodeResp(t, resp, &result)
	if !result.Halted {
		t.Error("halted should be true")
	}
}

// TestKillSwitch_NilProvider_501 — 501 when KillSwitch not wired.
func TestKillSwitch_NilProvider_501(t *testing.T) {
	_, srv := newTestHandler(HandlerDeps{})
	defer srv.Close()

	token := getAuthToken(t, srv)

	// POST
	req := struct {
		Reason string `json:"reason"`
	}{Reason: "x"}
	resp := doRequestWithHeaders(t, srv, "POST", "/api/killswitch", token, "idem-nil-ks", nil, req)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("POST /api/killswitch nil provider = %d, want 501", resp.StatusCode)
	}
	resp.Body.Close()

	// GET
	resp2 := doRequest(t, srv, "GET", "/api/killswitch", token, "", nil)
	if resp2.StatusCode != http.StatusNotImplemented {
		t.Fatalf("GET /api/killswitch nil provider = %d, want 501", resp2.StatusCode)
	}
	resp2.Body.Close()
}

// ============================================================
// 2FA middleware tests
// ============================================================

// TestTwoFactor_MissingCode_401 — verifier set, no X-2FA-Code header → 401.
func TestTwoFactor_MissingCode_401(t *testing.T) {
	ks := &fakeKillSwitchProvider{}
	verifier := &MemorySharedSecretVerifier{Secret: "test-secret"}
	_, srv := newTestHandler(HandlerDeps{KillSwitch: ks, TwoFactor: verifier})
	defer srv.Close()

	token := getAuthToken(t, srv)

	req := struct {
		Reason string `json:"reason"`
	}{Reason: "x"}
	// No X-2FA-Code header — should be 401.
	resp := doRequestWithHeaders(t, srv, "POST", "/api/killswitch", token, "idem-2fa-miss", nil, req)
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("missing 2FA code = %d, want 401; body: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

// TestTwoFactor_WrongCode_403 — verifier set, wrong code → 403.
func TestTwoFactor_WrongCode_403(t *testing.T) {
	ks := &fakeKillSwitchProvider{}
	verifier := &MemorySharedSecretVerifier{Secret: "correct-secret"}
	_, srv := newTestHandler(HandlerDeps{KillSwitch: ks, TwoFactor: verifier})
	defer srv.Close()

	token := getAuthToken(t, srv)

	req := struct {
		Reason string `json:"reason"`
	}{Reason: "x"}
	resp := doRequestWithHeaders(t, srv, "POST", "/api/killswitch", token, "idem-2fa-bad", map[string]string{"X-2FA-Code": "wrong"}, req)
	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("wrong 2FA code = %d, want 403; body: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

// TestTwoFactor_NilVerifier_Allowed — nil verifier → 2FA disabled, request allowed.
func TestTwoFactor_NilVerifier_Allowed(t *testing.T) {
	ks := &fakeKillSwitchProvider{}
	_, srv := newTestHandler(HandlerDeps{KillSwitch: ks}) // TwoFactor: nil
	defer srv.Close()

	token := getAuthToken(t, srv)
	req := struct {
		Reason string `json:"reason"`
	}{Reason: "x"}
	// No X-2FA-Code, nil verifier → should be allowed.
	resp := doRequestWithHeaders(t, srv, "POST", "/api/killswitch", token, "idem-2fa-nil", nil, req)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("nil verifier: %d, want 200; body: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

// TestTwoFactor_CorrectCode_Allowed — correct code → request passes through.
func TestTwoFactor_CorrectCode_Allowed(t *testing.T) {
	ks := &fakeKillSwitchProvider{}
	verifier := &MemorySharedSecretVerifier{Secret: "my-2fa-code"}
	_, srv := newTestHandler(HandlerDeps{KillSwitch: ks, TwoFactor: verifier})
	defer srv.Close()

	token := getAuthToken(t, srv)
	req := struct {
		Reason string `json:"reason"`
	}{Reason: "x"}
	resp := doRequestWithHeaders(t, srv, "POST", "/api/killswitch", token, "idem-2fa-ok", map[string]string{"X-2FA-Code": "my-2fa-code"}, req)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("correct 2FA code: %d, want 200; body: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

// TestTwoFactor_CredentialsSave_Gated — POST /api/credentials also requires 2FA when set.
func TestTwoFactor_CredentialsSave_Gated(t *testing.T) {
	fp := &fakeCredentialsProvider{fingerprintToReturn: "fp-2fa"}
	verifier := &MemorySharedSecretVerifier{Secret: "cred-secret"}
	_, srv := newTestHandler(HandlerDeps{Credentials: fp, TwoFactor: verifier})
	defer srv.Close()

	token := getAuthToken(t, srv)
	body := saveCredentialRequest{
		Exchange:  "binance",
		Kind:      "trade",
		APIKey:    "key123",
		APISecret: "sec456",
	}
	// Without 2FA code → 401.
	resp := doRequestWithHeaders(t, srv, "POST", "/api/credentials", token, "idem-cred-2fa", nil, body)
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("credentials without 2FA: %d, want 401; body: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
}

// doRequestWithHeaders is like doRequest but accepts extra headers and optional body.
func doRequestWithHeaders(t *testing.T, srv *httptest.Server, method, path, token, idemKey string, extraHeaders map[string]string, body interface{}) *http.Response {
	t.Helper()
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, srv.URL+path, reqBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("X-Idempotency-Key", idemKey)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}
