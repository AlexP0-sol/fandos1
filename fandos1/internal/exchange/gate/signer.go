// Package gate реализует адаптер биржи Gate.io V4 для USDT-perpetual фьючерсов.
//
// signer.go — HMAC-SHA512 подпись запросов Gate.io APIv4.
//
// Алгоритм подписи Gate.io V4 (VERIFIED — официальная документация gateio.ws):
//
//	signature_string = Method + "\n" + URL_Path + "\n" + QueryString + "\n" + HexEncode(SHA512(Body)) + "\n" + Timestamp
//	SIGN = HexEncode(HMAC-SHA512(secret, signature_string))
//
// Заголовки запроса: KEY (API-ключ), Timestamp (Unix секунды строкой), SIGN (signature).
// Passphrase НЕ используется (в отличие от OKX/Bitget/KuCoin).
package gate

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"sort"
	"strings"
)

// Signer хранит API-ключ и секрет Gate.io и генерирует подпись V4.
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

// APIKey возвращает api_key.
func (s *Signer) APIKey() string { return s.apiKey }

// hashBody вычисляет HexEncode(SHA512(body)). При пустом body — хэш пустой строки.
func hashBody(body string) string {
	h := sha512.New()
	h.Write([]byte(body))
	return hex.EncodeToString(h.Sum(nil))
}

// Sign возвращает подпись Gate.io V4.
//
// Параметры:
//   - method: HTTP-метод в UPPERCASE ("GET", "POST", "DELETE")
//   - urlPath: путь запроса без хоста и query, например "/api/v4/futures/usdt/orders"
//   - queryString: строка query без URL-encode в том же порядке что и в URL;
//     пустая строка ("") если параметров нет
//   - body: тело запроса строкой; пустая строка ("") для GET/DELETE без тела
//   - timestampSec: Unix timestamp в секундах (целое, как строка в заголовке)
func (s *Signer) Sign(method, urlPath, queryString, body string, timestampSec int64) string {
	tsStr := int64ToStr(timestampSec)
	bodyHash := hashBody(body)

	sigString := method + "\n" +
		urlPath + "\n" +
		queryString + "\n" +
		bodyHash + "\n" +
		tsStr

	mac := hmac.New(sha512.New, s.secret)
	mac.Write([]byte(sigString))
	return hex.EncodeToString(mac.Sum(nil))
}

// AuthHeaders возвращает обязательные заголовки аутентификации Gate.io V4.
// KEY, Timestamp (unix seconds), SIGN.
func (s *Signer) AuthHeaders(timestampSec int64, sign string) map[string]string {
	return map[string]string{
		"KEY":       s.apiKey,
		"Timestamp": int64ToStr(timestampSec),
		"SIGN":      sign,
	}
}

// Zero затирает секрет (best effort).
func (s *Signer) Zero() {
	for i := range s.secret {
		s.secret[i] = 0
	}
}

// BuildSortedQuery строит детерминированную query-строку из map.
// Сортирует параметры по ключу для воспроизводимости тестов.
// ВАЖНО: Gate.io требует query_string в том же порядке, что и в URL;
// в адаптере мы используем сортировку для детерминированности (порядок значения
// для подписи только при точном повторении).
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

// int64ToStr конвертирует int64 в строку без зависимостей strconv для экспорта.
func int64ToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
