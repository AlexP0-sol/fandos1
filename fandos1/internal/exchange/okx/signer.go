// Package okx реализует адаптер биржи OKX V5 для linear USDT-margined perpetual (SWAP).
//
// IMPORTANT: перед production-интеграцией сверить все endpoint-ы, поля запросов/ответов
// и правила подписи с АКТУАЛЬНОЙ официальной документацией OKX V5:
// https://www.okx.com/docs-v5/en/
//
// Реализация подписи (VERIFIED по официальной документации OKX V5):
//   - pre-hash = timestamp + METHOD + requestPath + body
//   - timestamp — ISO 8601 UTC с миллисекундами, например "2020-12-08T09:08:57.715Z"
//   - METHOD — uppercase: "GET" / "POST"
//   - requestPath — путь + query строка для GET, только путь для POST (тело в body)
//   - body — JSON-строка для POST, "" для GET
//   - sign = base64(HMAC-SHA256(secret, pre-hash))
//   - Заголовки: OK-ACCESS-KEY, OK-ACCESS-SIGN, OK-ACCESS-TIMESTAMP, OK-ACCESS-PASSPHRASE
package okx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"sort"
	"strings"
	"time"
)

// Signer подписывает запросы для OKX V5.
//
// Формат подписи (VERIFIED по официальной документации OKX V5):
//
//	pre-hash = timestamp + METHOD + requestPath + body
//	sign = base64(HMAC-SHA256(secretKey, pre-hash))
//
// timestamp — ISO 8601 UTC с миллисекундной точностью: "2020-12-08T09:08:57.715Z"
type Signer struct {
	apiKey     string
	secret     []byte
	passphrase string
}

// NewSigner создаёт signer. secret копируется во внутренний буфер.
func NewSigner(apiKey string, secret []byte, passphrase string) *Signer {
	s := &Signer{
		apiKey:     apiKey,
		secret:     make([]byte, len(secret)),
		passphrase: passphrase,
	}
	copy(s.secret, secret)
	return s
}

// APIKey возвращает api_key.
func (s *Signer) APIKey() string { return s.apiKey }

// Passphrase возвращает passphrase.
func (s *Signer) Passphrase() string { return s.passphrase }

// FormatTimestamp форматирует время в ISO8601 UTC с миллисекундной точностью.
// Пример: "2020-12-08T09:08:57.715Z"
// VERIFIED по официальной документации OKX V5.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// signRaw — HMAC-SHA256 от payload, base64-encoded.
func (s *Signer) signRaw(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Sign генерирует подпись OKX V5.
// timestamp — результат FormatTimestamp.
// method — "GET" или "POST" (uppercase).
// requestPath — путь, включая query-строку для GET (например "/api/v5/public/time" или
// "/api/v5/market/ticker?instId=BTC-USDT-SWAP").
// body — JSON-тело для POST, "" для GET.
// VERIFIED по официальной документации OKX V5.
func (s *Signer) Sign(timestamp, method, requestPath, body string) string {
	payload := timestamp + method + requestPath + body
	return s.signRaw(payload)
}

// AuthHeaders возвращает обязательные заголовки аутентификации OKX V5.
// VERIFIED: OK-ACCESS-KEY, OK-ACCESS-SIGN, OK-ACCESS-TIMESTAMP, OK-ACCESS-PASSPHRASE.
func (s *Signer) AuthHeaders(timestamp, signature string) map[string]string {
	return map[string]string{
		"OK-ACCESS-KEY":        s.apiKey,
		"OK-ACCESS-SIGN":       signature,
		"OK-ACCESS-TIMESTAMP":  timestamp,
		"OK-ACCESS-PASSPHRASE": s.passphrase,
	}
}

// Zero затирает secret (best effort).
func (s *Signer) Zero() {
	for i := range s.secret {
		s.secret[i] = 0
	}
}

// BuildSortedQuery — строит query-строку из map[string]string, сортируя по ключу.
// Используется для детерминированности тестов.
func BuildSortedQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}
