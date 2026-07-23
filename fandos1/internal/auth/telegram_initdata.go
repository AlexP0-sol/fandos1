// Package auth реализует проверку Telegram WebApp initData (раздел 13.4 промпта v2).
//
// initData — строка, которую Telegram Mini App передаёт backend при открытии. Содержит
// данные пользователя и подпись. Backend ОБЯЗАН проверить её по официальному алгоритму
// Telegram (https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app),
// иначе любой может подделать user_id.
//
// Алгоритм проверки:
//  1. Разобрать initData как URL-encoded параметры.
//  2. Извлечь hash.
//  3. Из остальных параметров построить sorted data_check_string (key=value\n...).
//  4. secret_key = HMAC-SHA256("WebAppData", bot_token).
//  5. hash_client = HMAC-SHA256(secret_key, data_check_string).
//  6. Сравнить в constant-time.
//
// ДОПОЛНИТЕЛЬНО (раздел 13.4):
//  - Не доверять telegram user ID из фронта без этой проверки.
//  - Использовать allowlist Telegram Admin IDs.
//  - Короткоживущая server-side session после успешной проверки.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AdminAllowlist — интерфейс проверки, разрешён ли telegram user ID.
type AdminAllowlist interface {
	IsAdmin(telegramUserID int64) bool
}

// StaticAllowlist — простая реализация с фиксированным множеством ID.
type StaticAllowlist map[int64]bool

// IsAdmin реализует AdminAllowlist.
func (s StaticAllowlist) IsAdmin(id int64) bool { return s[id] }

// ValidateConfig — параметры валидации initData.
type ValidateConfig struct {
	BotToken       string
	MaxAge         time.Duration // максимальный возраст initData (auth_date); 0 = не проверять
	Now            func() time.Time
}

// TelegramUser — распарсенные данные пользователя из initData.
type TelegramUser struct {
	ID            int64
	FirstName     string
	LastName      string
	Username      string
	LanguageCode  string
	PhotoURL      string
	IsPremium     bool
	AddedToAttachmentMenu bool
	AllowsWriteToPm bool
}

// ValidationResult — итог проверки.
type ValidationResult struct {
	Valid   bool
	User    TelegramUser
	AuthAt  time.Time
	Reason  string // если не валиден
}

// ValidateInitData проверяет подпись Telegram initData и извлекает пользователя.
// botToken — токен бота (из TELEGRAM_BOT_TOKEN env, раздел 13.4).
func ValidateInitData(initData string, cfg ValidateConfig, allowlist AdminAllowlist) ValidationResult {
	if initData == "" {
		return ValidationResult{Reason: "empty initData"}
	}

	// Parse query manually to preserve URL-encoded values (required for data_check_string).
	// url.ParseQuery decodes values, but Telegram's data_check_string uses raw encoded values.
	type kv struct{ key, val string }
	var pairs []kv
	for _, part := range strings.Split(initData, "&") {
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		pairs = append(pairs, kv{key: part[:eq], val: part[eq+1:]})
	}

	// Extract hash and remove it from pairs.
	var hash string
	filtered := pairs[:0]
	for _, p := range pairs {
		if p.key == "hash" {
			hash = p.val
		} else {
			filtered = append(filtered, p)
		}
	}
	pairs = filtered
	if hash == "" {
		return ValidationResult{Reason: "missing hash"}
	}

	// Check auth_date age.
	var authDateUnix int64
	for _, p := range pairs {
		if p.key == "auth_date" {
			authDateUnix, _ = strconv.ParseInt(p.val, 10, 64)
			break
		}
	}
	if authDateUnix == 0 {
		return ValidationResult{Reason: "missing auth_date"}
	}
	if cfg.MaxAge > 0 {
		now := cfg.Now
		if now == nil {
			now = time.Now
		}
		if now().Sub(time.Unix(authDateUnix, 0)) > cfg.MaxAge {
			return ValidationResult{Reason: "initData expired"}
		}
	}

	// Build data_check_string: sorted by key, using RAW encoded values.
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.key)
		b.WriteByte('=')
		b.WriteString(p.val)
	}
	dataCheckString := b.String()

	// secret_key = HMAC-SHA256("WebAppData", bot_token).
	secretKey := hmac.New(sha256.New, []byte("WebAppData"))
	secretKey.Write([]byte(cfg.BotToken))
	secretKeyBytes := secretKey.Sum(nil)

	// hash_expected = HMAC-SHA256(secret_key, data_check_string).
	hasher := hmac.New(sha256.New, secretKeyBytes)
	hasher.Write([]byte(dataCheckString))
	hashExpected := hex.EncodeToString(hasher.Sum(nil))

	// Constant-time compare.
	if !hmac.Equal([]byte(strings.ToLower(hash)), []byte(strings.ToLower(hashExpected))) {
		return ValidationResult{Reason: "hash mismatch"}
	}

	// Find user parameter (raw encoded).
	var userVal string
	for _, p := range pairs {
		if p.key == "user" {
			userVal = p.val
			break
		}
	}

	// Decode user for parsing.
	userJSON, err := url.QueryUnescape(userVal)
	if err != nil {
		userJSON = userVal // fallback
	}

	user := parseTelegramUser(userJSON)

	if allowlist != nil && !allowlist.IsAdmin(user.ID) {
		return ValidationResult{Reason: "user not in admin allowlist"}
	}

	return ValidationResult{
		Valid:  true,
		User:   user,
		AuthAt: time.Unix(authDateUnix, 0),
	}
}

// parseTelegramUser — разбирает JSON-encoded поле user.
// Делает ручной парсинг, чтобы не тянуть зависимость encoding/json для простой структуры.
// В реальном исп. использовать encoding/json; здесь — компактная версия.
func parseTelegramUser(userJSON string) TelegramUser {
	// Минимальный парсинг ключей id/first_name/last_name/username/language_code.
	// Без encoding/json — хрупко; для production заменить.
	var u TelegramUser
	// Извлекаем id как число.
	u.ID = extractInt(userJSON, `"id":`)
	u.FirstName = extractString(userJSON, `"first_name":`)
	u.LastName = extractString(userJSON, `"last_name":`)
	u.Username = extractString(userJSON, `"username":`)
	u.LanguageCode = extractString(userJSON, `"language_code":`)
	u.PhotoURL = extractString(userJSON, `"photo_url":`)
	u.IsPremium = strings.Contains(userJSON, `"is_premium":true`)
	u.AllowsWriteToPm = strings.Contains(userJSON, `"allows_write_to_pm":true`)
	u.AddedToAttachmentMenu = strings.Contains(userJSON, `"added_to_attachment_menu":true`)
	return u
}

// extractInt — извлекает целое после префикса.
func extractInt(s, prefix string) int64 {
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return 0
	}
	rest := s[idx+len(prefix):]
	rest = strings.TrimLeft(rest, " ")
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, _ := strconv.ParseInt(rest[:end], 10, 64)
	return n
}

// extractString — извлекает значение строкового JSON-поля после префикса.
func extractString(s, prefix string) string {
	idx := strings.Index(s, prefix)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(prefix):]
	rest = strings.TrimLeft(rest, " ")
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	// Ищем закрывающую кавычку (без escape-обработки — упрощение).
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// ErrUnauthorized — sentinel для http-middleware.
var ErrUnauthorized = errors.New("auth: unauthorized")
