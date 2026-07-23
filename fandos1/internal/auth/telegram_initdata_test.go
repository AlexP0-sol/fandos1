package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testBotToken — синтаксически валидный токен для тестов (не реальный).
const testBotToken = "5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"

// signInitData — эталонная реализация канонического алгоритма Telegram:
// data_check_string из ДЕКОДИРОВАННЫХ значений, отсортированных по ключу.
// Используется тестами для генерации валидных initData.
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

// buildInitData — собирает URL-encoded initData из декодированных пар + hash.
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

// userJSON — типичное значение поля user.
const userJSON = `{"id":279058397,"first_name":"dev","last_name":"","username":"devuser","language_code":"en"}`

func validPairs() map[string]string {
	return map[string]string{
		"query_id":  "AAHdF6IQAAAAAN0XohDhrOrc",
		"user":      userJSON,
		"auth_date": "1696588919",
	}
}

// TestKnownAnswerVector — контрольный вектор, вычисленный НЕЗАВИСИМОЙ реализацией
// (Python hmac/hashlib по официальному алгоритму Telegram). Защищает от случая,
// когда реализация и тестовый helper дрейфуют синхронно.
//
// data_check_string:
//
//	auth_date=1696588919
//	query_id=AAHdF6IQAAAAAN0XohDhrOrc
//	user={"id":279058397,"first_name":"dev","last_name":"","username":"devuser","language_code":"en"}
func TestKnownAnswerVector(t *testing.T) {
	const wantHash = "d4e1fc2fe2a0f5c4deae20579013af4eabec6190e2893c2b118f06dd1c67ebe7"

	// Helper обязан выдать тот же hash, что и независимая реализация.
	if got := signInitData(testBotToken, validPairs()); got != wantHash {
		t.Fatalf("test helper drifted from canonical algorithm:\n got %s\nwant %s", got, wantHash)
	}

	initData := buildInitData(validPairs(), wantHash)
	res := ValidateInitData(initData, ValidateConfig{BotToken: testBotToken}, StaticAllowlist{279058397: true})
	if !res.Valid {
		t.Fatalf("known-answer vector rejected: %s", res.Reason)
	}
	if res.User.ID != 279058397 {
		t.Errorf("user ID = %d, want 279058397", res.User.ID)
	}
	if res.User.Username != "devuser" {
		t.Errorf("username = %q, want devuser", res.User.Username)
	}
	if res.User.FirstName != "dev" {
		t.Errorf("first_name = %q, want dev", res.User.FirstName)
	}
	if res.User.LanguageCode != "en" {
		t.Errorf("language_code = %q, want en", res.User.LanguageCode)
	}
	if got := res.AuthAt.Unix(); got != 1696588919 {
		t.Errorf("AuthAt = %d, want 1696588919", got)
	}
}

// TestHashMismatch — подделанный hash отклоняется.
func TestHashMismatch(t *testing.T) {
	initData := buildInitData(validPairs(), strings.Repeat("0", 64))
	res := ValidateInitData(initData, ValidateConfig{BotToken: testBotToken}, StaticAllowlist{279058397: true})
	if res.Valid {
		t.Error("fake hash should be rejected")
	}
	if !strings.Contains(res.Reason, "hash") {
		t.Errorf("reason = %q, want hash-related", res.Reason)
	}
}

// TestTamperedField — валидная подпись + изменённое поле отклоняется.
func TestTamperedField(t *testing.T) {
	pairs := validPairs()
	hash := signInitData(testBotToken, pairs)
	// Подменяем user после подписания.
	pairs["user"] = `{"id":1,"first_name":"evil"}`
	res := ValidateInitData(buildInitData(pairs, hash), ValidateConfig{BotToken: testBotToken}, nil)
	if res.Valid {
		t.Error("tampered user field must be rejected")
	}
}

// TestWrongBotToken — подпись другим токеном отклоняется.
func TestWrongBotToken(t *testing.T) {
	hash := signInitData("1:other-token", validPairs())
	res := ValidateInitData(buildInitData(validPairs(), hash), ValidateConfig{BotToken: testBotToken}, nil)
	if res.Valid {
		t.Error("initData signed with another bot token must be rejected")
	}
}

// TestExpired — устаревший auth_date отклоняется при MaxAge > 0.
func TestExpired(t *testing.T) {
	authDate := int64(1000000000) // 2001 год.
	pairs := validPairs()
	pairs["auth_date"] = strconv.FormatInt(authDate, 10)
	hash := signInitData(testBotToken, pairs)

	fixedNow := time.Unix(authDate+1_000_000, 0)
	cfg := ValidateConfig{
		BotToken: testBotToken,
		MaxAge:   time.Minute,
		Now:      func() time.Time { return fixedNow },
	}
	res := ValidateInitData(buildInitData(pairs, hash), cfg, nil)
	if res.Valid {
		t.Error("expired initData should be rejected")
	}
	if !strings.Contains(res.Reason, "expired") {
		t.Errorf("reason = %q, want 'expired'", res.Reason)
	}
}

// TestFutureAuthDate — auth_date из будущего отклоняется при MaxAge > 0.
func TestFutureAuthDate(t *testing.T) {
	authDate := int64(2_000_000_000)
	pairs := validPairs()
	pairs["auth_date"] = strconv.FormatInt(authDate, 10)
	hash := signInitData(testBotToken, pairs)

	fixedNow := time.Unix(authDate-3600, 0) // «сейчас» на час раньше auth_date
	cfg := ValidateConfig{
		BotToken: testBotToken,
		MaxAge:   24 * time.Hour,
		Now:      func() time.Time { return fixedNow },
	}
	res := ValidateInitData(buildInitData(pairs, hash), cfg, nil)
	if res.Valid {
		t.Error("future auth_date should be rejected")
	}
}

// TestAllowlistBlocksNonAdmin — валидная подпись, но пользователь не в allowlist.
func TestAllowlistBlocksNonAdmin(t *testing.T) {
	hash := signInitData(testBotToken, validPairs())
	res := ValidateInitData(buildInitData(validPairs(), hash),
		ValidateConfig{BotToken: testBotToken}, StaticAllowlist{})
	if res.Valid {
		t.Error("non-admin should be rejected by allowlist")
	}
	if !strings.Contains(res.Reason, "allowlist") {
		t.Errorf("reason = %q, want allowlist-related", res.Reason)
	}
}

// TestNilAllowlistBypasses — nil allowlist = проверка отключена (только dev/tests).
func TestNilAllowlistBypasses(t *testing.T) {
	hash := signInitData(testBotToken, validPairs())
	res := ValidateInitData(buildInitData(validPairs(), hash),
		ValidateConfig{BotToken: testBotToken}, nil)
	if !res.Valid {
		t.Errorf("nil allowlist should bypass admin check, got reason %q", res.Reason)
	}
}

// TestUserJSONParsing — полный набор полей WebAppUser через encoding/json,
// включая экранированные кавычки, которые ломали старый ручной парсер.
func TestUserJSONParsing(t *testing.T) {
	pairs := validPairs()
	pairs["user"] = `{"id":12345,"first_name":"Alice \"Al\"","last_name":"Smith","username":"alice",` +
		`"language_code":"ru","is_premium":true,"allows_write_to_pm":true,"photo_url":"https://t.me/i/userpic/x.jpg"}`
	hash := signInitData(testBotToken, pairs)
	res := ValidateInitData(buildInitData(pairs, hash), ValidateConfig{BotToken: testBotToken}, nil)
	if !res.Valid {
		t.Fatalf("valid initData rejected: %s", res.Reason)
	}
	u := res.User
	if u.ID != 12345 || u.FirstName != `Alice "Al"` || u.LastName != "Smith" ||
		u.Username != "alice" || u.LanguageCode != "ru" || !u.IsPremium || !u.AllowsWriteToPM ||
		u.PhotoURL != "https://t.me/i/userpic/x.jpg" {
		t.Errorf("user fields parsed incorrectly: %+v", u)
	}
}

// TestMalformedInputs — граничные случаи не должны паниковать и должны отклоняться.
func TestMalformedInputs(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"no hash":         "auth_date=1&user=%7B%22id%22%3A1%7D",
		"no auth_date":    "user=%7B%22id%22%3A1%7D&hash=" + strings.Repeat("a", 64),
		"bad percent enc": "user=%ZZ&auth_date=1&hash=" + strings.Repeat("a", 64),
		"garbage":         "&&&===&&&",
		"hash only":       "hash=" + strings.Repeat("a", 64),
	}
	for name, initData := range cases {
		if res := ValidateInitData(initData, ValidateConfig{BotToken: testBotToken}, nil); res.Valid {
			t.Errorf("%s: malformed initData accepted", name)
		}
	}
	// Пустой токен бота.
	if res := ValidateInitData("auth_date=1&hash=aa", ValidateConfig{}, nil); res.Valid {
		t.Error("empty bot token accepted")
	}
}

// TestMissingUser — initData без user отклоняется (user ID нужен для allowlist).
func TestMissingUser(t *testing.T) {
	pairs := map[string]string{"auth_date": "1696588919", "query_id": "AAA"}
	hash := signInitData(testBotToken, pairs)
	res := ValidateInitData(buildInitData(pairs, hash), ValidateConfig{BotToken: testBotToken}, nil)
	if res.Valid {
		t.Error("initData without user must be rejected")
	}
}

// TestUppercaseHash — hash в верхнем регистре принимается (нормализация регистра).
func TestUppercaseHash(t *testing.T) {
	hash := strings.ToUpper(signInitData(testBotToken, validPairs()))
	res := ValidateInitData(buildInitData(validPairs(), hash), ValidateConfig{BotToken: testBotToken}, nil)
	if !res.Valid {
		t.Errorf("uppercase hash rejected: %s", res.Reason)
	}
}
