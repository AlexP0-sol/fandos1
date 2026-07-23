// Package auth реализует проверку Telegram WebApp initData (раздел 13.4 промпта v2).
//
// initData — строка, которую Telegram Mini App передаёт backend при открытии. Содержит
// данные пользователя и подпись. Backend ОБЯЗАН проверить её по официальному алгоритму
// Telegram (https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app),
// иначе любой может подделать user_id.
//
// Алгоритм проверки:
//  1. Разобрать initData как application/x-www-form-urlencoded (значения URL-ДЕКОДИРУЮТСЯ —
//     это критично: data_check_string строится из декодированных значений).
//  2. Извлечь hash и исключить его из data_check_string.
//  3. Из остальных пар построить data_check_string: отсортированные по ключу строки
//     "key=<decoded value>", соединённые '\n'.
//  4. secret_key = HMAC-SHA256(key="WebAppData", message=bot_token).
//  5. hash_expected = HMAC-SHA256(key=secret_key, message=data_check_string), hex.
//  6. Сравнить с полученным hash в constant-time.
//
// ДОПОЛНИТЕЛЬНО (раздел 13.4):
//   - Не доверять telegram user ID из фронта без этой проверки.
//   - Использовать allowlist Telegram Admin IDs.
//   - Короткоживущая server-side session после успешной проверки.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	BotToken string
	MaxAge   time.Duration // максимальный возраст initData (auth_date); 0 = не проверять
	Now      func() time.Time
}

// TelegramUser — распарсенные данные пользователя из initData.
// Поля соответствуют объекту WebAppUser из документации Telegram.
type TelegramUser struct {
	ID                    int64  `json:"id"`
	FirstName             string `json:"first_name"`
	LastName              string `json:"last_name"`
	Username              string `json:"username"`
	LanguageCode          string `json:"language_code"`
	PhotoURL              string `json:"photo_url"`
	IsPremium             bool   `json:"is_premium"`
	AddedToAttachmentMenu bool   `json:"added_to_attachment_menu"`
	AllowsWriteToPM       bool   `json:"allows_write_to_pm"`
}

// ValidationResult — итог проверки.
type ValidationResult struct {
	Valid  bool
	User   TelegramUser
	AuthAt time.Time
	Reason string // если не валиден
}

// maxFutureSkew — допустимый сдвиг auth_date в будущее (расхождение часов с Telegram).
const maxFutureSkew = 5 * time.Minute

// ValidateInitData проверяет подпись Telegram initData и извлекает пользователя.
// botToken — токен бота (из TELEGRAM_BOT_TOKEN env, раздел 13.4).
func ValidateInitData(initData string, cfg ValidateConfig, allowlist AdminAllowlist) ValidationResult {
	if initData == "" {
		return ValidationResult{Reason: "empty initData"}
	}
	if cfg.BotToken == "" {
		return ValidationResult{Reason: "empty bot token"}
	}

	// 1. Разбор initData. Значения URL-декодируются — data_check_string Telegram
	// строится из ДЕКОДИРОВАННЫХ значений. url.ParseQuery не используется, потому что
	// он агрегирует повторяющиеся ключи и теряет порядок диагностики; декодируем сами.
	type kv struct{ key, val string }
	var pairs []kv
	var hash string
	for _, part := range strings.Split(initData, "&") {
		if part == "" {
			continue
		}
		rawKey, rawVal, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return ValidationResult{Reason: "malformed key encoding"}
		}
		val, err := url.QueryUnescape(rawVal)
		if err != nil {
			return ValidationResult{Reason: "malformed value encoding"}
		}
		if key == "hash" {
			hash = val
			continue
		}
		pairs = append(pairs, kv{key: key, val: val})
	}
	if hash == "" {
		return ValidationResult{Reason: "missing hash"}
	}

	// 2. auth_date: обязателен; проверяем возраст и разумность.
	var authDateUnix int64
	for _, p := range pairs {
		if p.key == "auth_date" {
			authDateUnix, _ = strconv.ParseInt(p.val, 10, 64)
			break
		}
	}
	if authDateUnix <= 0 {
		return ValidationResult{Reason: "missing auth_date"}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	authAt := time.Unix(authDateUnix, 0)
	if cfg.MaxAge > 0 {
		if now().Sub(authAt) > cfg.MaxAge {
			return ValidationResult{Reason: "initData expired"}
		}
		if authAt.Sub(now()) > maxFutureSkew {
			return ValidationResult{Reason: "auth_date in the future"}
		}
	}

	// 3. data_check_string: сортировка по ключу, декодированные значения.
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

	// 4. secret_key = HMAC-SHA256(key="WebAppData", message=bot_token).
	secretKey := hmac.New(sha256.New, []byte("WebAppData"))
	secretKey.Write([]byte(cfg.BotToken))
	secretKeyBytes := secretKey.Sum(nil)

	// 5. hash_expected = HMAC-SHA256(key=secret_key, message=data_check_string).
	hasher := hmac.New(sha256.New, secretKeyBytes)
	hasher.Write([]byte(dataCheckString))
	hashExpected := hex.EncodeToString(hasher.Sum(nil))

	// 6. Constant-time сравнение. hash нормализуем к нижнему регистру:
	// hex.EncodeToString всегда выдаёт нижний регистр.
	if !hmac.Equal([]byte(strings.ToLower(hash)), []byte(hashExpected)) {
		return ValidationResult{Reason: "hash mismatch"}
	}

	// 7. Пользователь: JSON-поле user (уже декодировано на шаге 1).
	var user TelegramUser
	for _, p := range pairs {
		if p.key == "user" {
			if err := json.Unmarshal([]byte(p.val), &user); err != nil {
				return ValidationResult{Reason: "malformed user JSON"}
			}
			break
		}
	}
	if user.ID == 0 {
		return ValidationResult{Reason: "missing user"}
	}

	// 8. Allowlist (раздел 13.4): nil = не проверять (только для тестов/dev).
	if allowlist != nil && !allowlist.IsAdmin(user.ID) {
		return ValidationResult{Reason: "user not in admin allowlist"}
	}

	return ValidationResult{
		Valid:  true,
		User:   user,
		AuthAt: authAt,
	}
}

// ErrUnauthorized — sentinel для http-middleware.
var ErrUnauthorized = errors.New("auth: unauthorized")
