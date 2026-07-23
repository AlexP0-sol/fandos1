// Package bitget реализует адаптер биржи Bitget V2 для USDT-M Perpetual Futures.
//
// Подпись REST (VERIFIED по официальной документации Bitget API classic/quickStart/intro):
//
//	preHash = timestamp + METHOD + requestPath + ("?" + queryString)? + body
//	signature = base64(HMAC-SHA256(secretKey, preHash))
//
// Обязательные заголовки: ACCESS-KEY, ACCESS-SIGN, ACCESS-TIMESTAMP,
// ACCESS-PASSPHRASE, Content-Type: application/json, locale: en-US.
package bitget

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signer подписывает запросы для Bitget V2.
//
// VERIFIED: Bitget V2 (и classic V2) использует:
//
//	preHash = timestamp + METHOD.toUpperCase() + requestPath + "?" + queryString + body
//	signature = base64(HMAC-SHA256(secretKey, preHash))
//
// Если queryString пустой, "?" опускается.
// Если body пустой (GET), body опускается.
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

// APIKey возвращает API-ключ.
func (s *Signer) APIKey() string { return s.apiKey }

// Passphrase возвращает passphrase.
func (s *Signer) Passphrase() string { return s.passphrase }

// SignRaw — HMAC-SHA256 от preHash, base64-encoded.
// VERIFIED: итоговая подпись — base64 (не hex, как у Bybit).
func (s *Signer) SignRaw(preHash string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(preHash))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// buildPreHash строит строку для подписи.
//
// VERIFIED (Bitget classic quickStart/intro):
//
//	preHash = timestamp + METHOD + requestPath [+ "?" + queryString] [+ body]
func buildPreHash(timestampMs int64, method, requestPath, queryString, body string) string {
	var b strings.Builder
	b.WriteString(strconv.FormatInt(timestampMs, 10))
	b.WriteString(strings.ToUpper(method))
	b.WriteString(requestPath)
	if queryString != "" {
		b.WriteByte('?')
		b.WriteString(queryString)
	}
	b.WriteString(body)
	return b.String()
}

// SignGET строит и подписывает GET preHash. Возвращает preHash и signature.
func (s *Signer) SignGET(timestampMs int64, requestPath, queryString string) (preHash, signature string) {
	preHash = buildPreHash(timestampMs, "GET", requestPath, queryString, "")
	signature = s.SignRaw(preHash)
	return preHash, signature
}

// SignPOST строит и подписывает POST preHash. Возвращает preHash и signature.
func (s *Signer) SignPOST(timestampMs int64, requestPath, body string) (preHash, signature string) {
	preHash = buildPreHash(timestampMs, "POST", requestPath, "", body)
	signature = s.SignRaw(preHash)
	return preHash, signature
}

// AuthHeaders возвращает все обязательные заголовки аутентификации Bitget V2.
//
// VERIFIED (Bitget API docs):
//   - ACCESS-KEY — API key
//   - ACCESS-SIGN — base64(HMAC-SHA256(secret, preHash))
//   - ACCESS-TIMESTAMP — unix ms
//   - ACCESS-PASSPHRASE — passphrase установленный при создании API key
//   - Content-Type — application/json
//   - locale — en-US
func (s *Signer) AuthHeaders(timestampMs int64, signature string) map[string]string {
	return map[string]string{
		"ACCESS-KEY":        s.apiKey,
		"ACCESS-SIGN":       signature,
		"ACCESS-TIMESTAMP":  strconv.FormatInt(timestampMs, 10),
		"ACCESS-PASSPHRASE": s.passphrase,
		"Content-Type":      "application/json",
		"locale":            "en-US",
	}
}

// Zero затирает secret (best effort).
func (s *Signer) Zero() {
	for i := range s.secret {
		s.secret[i] = 0
	}
}

// BuildSortedQuery строит query-string из map, сортируя ключи для детерминированности.
func BuildSortedQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
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

// Now — обёртка для тестируемости.
var Now = func() time.Time { return time.Now() }

// NowMs — миллисекунды UTC.
func NowMs() int64 { return Now().UnixMilli() }
