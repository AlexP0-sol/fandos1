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
