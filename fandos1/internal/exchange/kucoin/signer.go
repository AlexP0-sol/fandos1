// Package kucoin реализует адаптер биржи KuCoin Futures для linear USDT perpetual.
//
// Этот файл содержит signer для KC-API v2:
//
//	str = timestamp + METHOD + endpoint(+?query) + body
//	KC-API-SIGN      = base64(HMAC-SHA256(secret, str))
//	KC-API-PASSPHRASE = base64(HMAC-SHA256(secret, passphrase))
//
// VERIFIED (KuCoin docs 2026-07):
//   - KC-API-KEY-VERSION: 2
//   - KC-API-SIGN: base64-encoded HMAC-SHA256(secret, timestamp+METHOD+endpoint+body)
//   - KC-API-PASSPHRASE: base64-encoded HMAC-SHA256(secret, passphrase)  (v2 format)
//   - For GET/DELETE: endpoint includes query string, e.g. /api/v1/ticker?symbol=XBTUSDTM
//   - For POST: body is JSON string; endpoint without query.
package kucoin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"sort"
	"strings"
)

// Signer подписывает запросы для KuCoin Futures API v2.
//
// VERIFIED: KC-API-KEY-VERSION: 2 требует HMAC-подпись passphrase,
// а не plain passphrase (как в v1).
type Signer struct {
	apiKey           string
	secret           []byte
	signedPassphrase string // base64(HMAC-SHA256(secret, passphrase)) — вычисляется один раз
}

// NewSigner создаёт signer для KC-API v2. secret и passphrase копируются внутрь.
// Passphrase обязательна (строгое требование KuCoin v2 auth).
func NewSigner(apiKey string, secret []byte, passphrase string) *Signer {
	s := &Signer{
		apiKey: apiKey,
		secret: make([]byte, len(secret)),
	}
	copy(s.secret, secret)
	// Вычисляем подпись passphrase один раз при создании.
	s.signedPassphrase = signBase64(secret, passphrase)
	return s
}

// signBase64 — HMAC-SHA256(key, msg), base64-encoded.
func signBase64(key []byte, msg string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Sign вычисляет KC-API-SIGN для произвольного str_to_sign.
// str = timestamp + METHOD + endpoint(+?query) + body
func (s *Signer) Sign(strToSign string) string {
	return signBase64(s.secret, strToSign)
}

// AuthHeaders возвращает map HTTP-заголовков для KC-API v2 аутентификации.
//
// VERIFIED заголовки:
//   - KC-API-KEY
//   - KC-API-SIGN      = base64(HMAC-SHA256(secret, strToSign))
//   - KC-API-TIMESTAMP = ms
//   - KC-API-PASSPHRASE = base64(HMAC-SHA256(secret, passphrase))
//   - KC-API-KEY-VERSION = "2"
func (s *Signer) AuthHeaders(timestampMs int64, strToSign string) map[string]string {
	ts := int64ToString(timestampMs)
	return map[string]string{
		"KC-API-KEY":         s.apiKey,
		"KC-API-SIGN":        signBase64(s.secret, strToSign),
		"KC-API-TIMESTAMP":   ts,
		"KC-API-PASSPHRASE":  s.signedPassphrase,
		"KC-API-KEY-VERSION": "2",
	}
}

// StrToSignGET строит str_to_sign для GET/DELETE запроса.
// endpoint — путь с query string, например /api/v1/ticker?symbol=XBTUSDTM.
// VERIFIED: body пустой для GET.
func StrToSignGET(timestampMs int64, method, endpointWithQuery string) string {
	ts := int64ToString(timestampMs)
	return ts + method + endpointWithQuery
}

// StrToSignPOST строит str_to_sign для POST запроса.
// endpoint без query string; bodyJSON — полное JSON-тело.
// VERIFIED: POST body = JSON string.
func StrToSignPOST(timestampMs int64, method, endpoint, bodyJSON string) string {
	ts := int64ToString(timestampMs)
	return ts + method + endpoint + bodyJSON
}

// Zero затирает secret (best effort).
func (s *Signer) Zero() {
	for i := range s.secret {
		s.secret[i] = 0
	}
}

// BuildSortedQuery строит детерминированную query string из map параметров.
// Параметры сортируются по ключу.
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

// int64ToString — быстрое преобразование int64 в строку.
func int64ToString(n int64) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 20)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
