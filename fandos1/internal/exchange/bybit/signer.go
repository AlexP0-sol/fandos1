// Package bybit реализует адаптер для Bybit V5 Linear USDT Perpetual.
//
// ВАЖНО (раздел 2 промпта v2): перед production-интеграцией сверить все endpoint,
// названия полей и правила подписи с АКТУАЛЬНОЙ официальной документацией Bybit V5
// (https://bybit-exchange.github.io/docs/v5/intro).
//
// Реализовано в этом файле: signer (HMAC-SHA256, Bybit V5 payload-формат).
package bybit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signer подписывает запросы для Bybit V5.
//
// V5 payload (для GET): timestamp + api_key + recv_window + query_string
// V5 payload (для POST): timestamp + api_key + recv_window + body_json
// signature = HMAC-SHA256(secret, payload), hex-encoded.
type Signer struct {
	apiKey string
	secret []byte
}

// NewSigner создаёт signer. secret копируется во внутренний буфер.
func NewSigner(apiKey string, secret []byte) *Signer {
	s := &Signer{apiKey: apiKey, secret: make([]byte, len(secret))}
	copy(s.secret, secret)
	return s
}

// APIKey возвращает api_key (для использования в query/header).
func (s *Signer) APIKey() string { return s.apiKey }

// SignRaw — HMAC-SHA256 от payload, hex-encoded.
func (s *Signer) SignRaw(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignGet — подпись для GET-запроса.
// query — параметры в виде "k1=v1&k2=v2" (без api_key/timestamp/recv_window/sign).
// Возвращает payload, который использовался для подписи, и итоговую signature.
func (s *Signer) SignGet(timestampMs int64, recvWindowMs int64, query string) (payload, signature string) {
	payload = strconv.FormatInt(timestampMs, 10) + s.apiKey + strconv.FormatInt(recvWindowMs, 10) + query
	sig := s.SignRaw(payload)
	return payload, sig
}

// SignPost — подпись для POST-запроса.
// bodyJSON — JSON-тело запроса.
func (s *Signer) SignPost(timestampMs int64, recvWindowMs int64, bodyJSON string) (payload, signature string) {
	payload = strconv.FormatInt(timestampMs, 10) + s.apiKey + strconv.FormatInt(recvWindowMs, 10) + bodyJSON
	sig := s.SignRaw(payload)
	return payload, sig
}

// Zero затирает secret (best effort).
func (s *Signer) Zero() {
	for i := range s.secret {
		s.secret[i] = 0
	}
}

// AuthHeaders возвращает обязательные заголовки аутентификации Bybit V5.
// В V5 аутентификация идёт ТОЛЬКО через заголовки (не через query-параметры,
// как в устаревших v2/v3 API): X-BAPI-API-KEY, X-BAPI-TIMESTAMP,
// X-BAPI-RECV-WINDOW, X-BAPI-SIGN. signature — результат SignGet/SignPost.
func (s *Signer) AuthHeaders(timestampMs int64, recvWindowMs int64, signature string) map[string]string {
	return map[string]string{
		"X-BAPI-API-KEY":     s.apiKey,
		"X-BAPI-TIMESTAMP":   strconv.FormatInt(timestampMs, 10),
		"X-BAPI-RECV-WINDOW": strconv.FormatInt(recvWindowMs, 10),
		"X-BAPI-SIGN":        signature,
	}
}

// BuildSortedQuery — Bybit V5 ожидает параметры в query в ЛЮБОМ порядке,
// но для детерминированности тестов мы сортируем по ключу.
// Вход: map[string]string; выход: "k1=v1&k2=v2&..." (отсортировано).
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

// Now — обёртка для тестируемости.
var Now = func() time.Time { return time.Now() }

// NowMs — миллисекунды UTC.
func NowMs() int64 { return Now().UnixMilli() }

// ErrInvalidTimestamp — timestamp вне recv_window (раздел 24 — clock skew).
var ErrInvalidTimestamp = errors.New("bybit: timestamp outside recv_window")
