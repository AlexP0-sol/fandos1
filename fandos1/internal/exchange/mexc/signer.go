// Package mexc реализует адаптер биржи MEXC (Futures Contract API v1) для linear USDT perpetual.
//
// Подпись приватных запросов:
//
//	stringToSign = accessKey + requestTime + parameterString
//	signature    = hex(HMAC-SHA256(apiSecret, stringToSign))
//
// Для GET:  parameterString = отсортированный query string (key=value&..., без URL-кодирования)
// Для POST: parameterString = сырое JSON-тело запроса (без сортировки ключей)
//
// Заголовки: ApiKey, Request-Time (мс), Signature, Content-Type: application/json.
//
// VERIFIED: Официальная документация MEXC Contract API v1 + официальный Go SDK
// (github.com/mexcdevelop/mexc-api-demo/go/clients/futures/signer.go).
package mexc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// Signer подписывает запросы для MEXC Futures Contract API v1.
//
// VERIFIED: stringToSign = accessKey + requestTime + parameterString.
// GET:  parameterString = sorted key=value&... (no URL-encoding, & delimited).
// POST: parameterString = raw JSON body string.
type Signer struct {
	apiKey string
	secret []byte
}

// NewSigner создаёт Signer. secret копируется во внутренний буфер.
func NewSigner(apiKey string, secret []byte) *Signer {
	s := &Signer{apiKey: apiKey, secret: make([]byte, len(secret))}
	copy(s.secret, secret)
	return s
}

// APIKey возвращает api key.
func (s *Signer) APIKey() string { return s.apiKey }

// signRaw вычисляет hex(HMAC-SHA256(secret, payload)).
func (s *Signer) signRaw(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignGET подписывает GET-запрос.
// paramStr — отсортированный query string без auth-параметров (BuildSortedQuery).
// Возвращает timestampMs (строка), signature.
//
// VERIFIED: stringToSign = accessKey + requestTime + sortedQueryString.
func (s *Signer) SignGET(timestampMs int64, paramStr string) (tsStr, signature string) {
	ts := strconv.FormatInt(timestampMs, 10)
	payload := s.apiKey + ts + paramStr
	return ts, s.signRaw(payload)
}

// SignPOST подписывает POST-запрос.
// bodyJSON — сырая JSON-строка тела.
// Возвращает timestampMs (строка), signature.
//
// VERIFIED: stringToSign = accessKey + requestTime + bodyJSON.
func (s *Signer) SignPOST(timestampMs int64, bodyJSON string) (tsStr, signature string) {
	ts := strconv.FormatInt(timestampMs, 10)
	payload := s.apiKey + ts + bodyJSON
	return ts, s.signRaw(payload)
}

// AuthHeaders возвращает обязательные заголовки аутентификации MEXC Contract API.
//
// VERIFIED: ApiKey, Request-Time, Signature, Content-Type.
func (s *Signer) AuthHeaders(tsStr, signature string) map[string]string {
	return map[string]string{
		"ApiKey":       s.apiKey,
		"Request-Time": tsStr,
		"Signature":    signature,
		"Content-Type": "application/json",
	}
}

// BuildSortedQuery строит отсортированный query string из map[string]string.
// Сортировка по ключу — детерминированность тестов + требование MEXC для подписи GET.
//
// VERIFIED: официальный Java-пример в MEXC Contract API docs использует SortedMap.
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

// Zero затирает secret (best-effort).
func (s *Signer) Zero() {
	for i := range s.secret {
		s.secret[i] = 0
	}
}
