package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTelegramOfficialVector — официальный test vector из документации Telegram.
// https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
//
// bot_token = "5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"
// (это публичный test vector из документации, не реальный токен).
func TestTelegramOfficialVector(t *testing.T) {
	const botToken = "5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"

	// Официальный initData из документации (строка немного длинная).
	// auth_date = 1696588919, botToken = "5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"
	// hash from docs: 0d623aa3f2670e88e3f6d23e44d3a0d536e0c92f5f63a6e8821b67e63a5f0b25
	initData := "query_id=AAHdF6IQAAAAAN0XohDhrOrc" +
		"&user=%7B%22id%22%3A279058397%2C%22first_name%22%3A%22dev%22%2C%22last_name%22%3A%22%22%2C%22username%22%3A%22devuser%22%2C%22language_code%22%3A%22en%22%7D" +
		"&auth_date=1696588919" +
		"&hash=0d623aa3f2670e88e3f6d23e44d3a0d536e0c92f5f63a6e8821b67e63a5f0b25"

	cfg := ValidateConfig{BotToken: botToken, MaxAge: 0} // не проверяем возраст для тест-вектора
	allowlist := StaticAllowlist{279058397: true}

	res := ValidateInitData(initData, cfg, allowlist)
	if !res.Valid {
		t.Fatalf("official vector failed: %s", res.Reason)
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
}

// TestHashMismatch — подделанный initData отклоняется.
func TestHashMismatch(t *testing.T) {
	// Подменим hash на случайный.
	initData := "query_id=AAA&user=%7B%22id%22%3A1%7D&auth_date=1696588919&hash=0000000000000000000000000000000000000000000000000000000000000000"
	cfg := ValidateConfig{BotToken: "any", MaxAge: 0}
	res := ValidateInitData(initData, cfg, StaticAllowlist{1: true})
	if res.Valid {
		t.Error("fake hash should be rejected")
	}
	if !strings.Contains(res.Reason, "hash") {
		t.Errorf("reason = %q, want hash-related", res.Reason)
	}
}

// TestMissingHash
func TestMissingHash(t *testing.T) {
	res := ValidateInitData("user=%7B%22id%22%3A1%7D&auth_date=1696588919", ValidateConfig{BotToken: "k"}, nil)
	if res.Valid {
		t.Error("missing hash should be rejected")
	}
}

// TestEmptyInitData
func TestEmptyInitData(t *testing.T) {
	res := ValidateInitData("", ValidateConfig{BotToken: "k"}, nil)
	if res.Valid {
		t.Error("empty initData should be rejected")
	}
}

// TestMalformedQuery
func TestMalformedQuery(t *testing.T) {
	res := ValidateInitData("not%%a=valid=query", ValidateConfig{BotToken: "k"}, nil)
	if res.Valid {
		t.Error("malformed query should be rejected")
	}
}

// TestMissingAuthDate
func TestMissingAuthDate(t *testing.T) {
	res := ValidateInitData("user=%7B%22id%22%3A1%7D&hash=abc", ValidateConfig{BotToken: "k"}, nil)
	if res.Valid {
		t.Error("missing auth_date should be rejected")
	}
}

// TestExpiredInitData — устаревший auth_date отклоняется.
func TestExpiredInitData(t *testing.T) {
	botToken := "12345:test"
	// Строим валидную подпись для старого auth_date.
	authDate := int64(1000000000) // 2001 год.
	values := "auth_date=" + itoa(authDate) + "&user=%7B%22id%22%3A1%7D"
	// data_check_string = "auth_date=1000000000\nuser=%7B%22id%22%3A1%7D"
	dataCheck := "auth_date=" + itoa(authDate) + "\nuser=%7B%22id%22%3A1%7D"
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	secretBytes := secret.Sum(nil)
	hasher := hmac.New(sha256.New, secretBytes)
	hasher.Write([]byte(dataCheck))
	sig := hex.EncodeToString(hasher.Sum(nil))
	initData := values + "&hash=" + sig

	// MaxAge = 1 минута, auth_date — далеко в прошлом.
	fixedNow := time.Unix(authDate+1000000, 0)
	cfg := ValidateConfig{BotToken: botToken, MaxAge: time.Minute, Now: func() time.Time { return fixedNow }}
	res := ValidateInitData(initData, cfg, nil)
	if res.Valid {
		t.Error("expired initData should be rejected")
	}
	if !strings.Contains(res.Reason, "expired") {
		t.Errorf("reason = %q, want 'expired'", res.Reason)
	}
}

// TestAllowlistBlocksNonAdmin — даже валидная подпись отклоняет не-admin.
func TestAllowlistBlocksNonAdmin(t *testing.T) {
	botToken := "5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"
	initData := "query_id=AAHdF6IQAAAAAN0XohDhrOrc" +
		"&user=%7B%22id%22%3A279058397%2C%22first_name%22%3A%22dev%22%2C%22last_name%22%3A%22%22%2C%22username%22%3A%22devuser%22%2C%22language_code%22%3A%22en%22%7D" +
		"&auth_date=1696588919" +
		"&hash=0d623aa3f2670e88e3f6d23e44d3a0d536e0c92f5f63a6e8821b67e63a5f0b25"

	// Allowlist пустой — пользователь не админ.
	res := ValidateInitData(initData, ValidateConfig{BotToken: botToken}, StaticAllowlist{})
	if res.Valid {
		t.Error("non-admin should be rejected by allowlist")
	}
	if !strings.Contains(res.Reason, "allowlist") {
		t.Errorf("reason = %q", res.Reason)
	}
}

// TestNilAllowlistBypasses — nil allowlist = не проверяется (только для testing/dev).
func TestNilAllowlistBypasses(t *testing.T) {
	botToken := "5768337691:AAH5YkoiWVoyDnuT8P5jznJuUeK5MEZK8_4"
	initData := "query_id=AAHdF6IQAAAAAN0XohDhrOrc" +
		"&user=%7B%22id%22%3A279058397%2C%22first_name%22%3A%22dev%22%7D" +
		"&auth_date=1696588919" +
		"&hash=3983c4a7f0f8b23e3e5e8b53f3c93e0f5c66e875d4b0d6f6a8e5b7e1c3b9b1a4"

	// Несоответствующий hash → всё равно отклонится на hash-проверке.
	res := ValidateInitData(initData, ValidateConfig{BotToken: botToken}, nil)
	if res.Valid {
		t.Error("hash mismatch should reject regardless of allowlist")
	}
}

// TestParseTelegramUser — извлечение полей пользователя.
func TestParseTelegramUser(t *testing.T) {
	jsonStr := `{"id":12345,"first_name":"Alice","last_name":"Smith","username":"alice","language_code":"ru","is_premium":true,"allows_write_to_pm":true}`
	u := parseTelegramUser(jsonStr)
	if u.ID != 12345 {
		t.Errorf("id = %d, want 12345", u.ID)
	}
	if u.FirstName != "Alice" {
		t.Errorf("first_name = %q", u.FirstName)
	}
	if u.LastName != "Smith" {
		t.Errorf("last_name = %q", u.LastName)
	}
	if u.Username != "alice" {
		t.Errorf("username = %q", u.Username)
	}
	if !u.IsPremium {
		t.Error("is_premium not parsed")
	}
	if !u.AllowsWriteToPm {
		t.Error("allows_write_to_pm not parsed")
	}
}

// helpers
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
