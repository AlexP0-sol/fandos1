// Package binance реализует адаптер для Binance USDT-M Futures.
//
// ВАЖНО (раздел 2 промпта v2): перед production-интеграцией сверить все endpoint,
// названия полей и правила подписи с АКТУАЛЬНОЙ официальной документацией Binance
// (https://developers.binance.com/docs/derivatives/usds-margined-futures).
// Не использовать этот код как единственный источник — он основан на структуре API,
// известной на момент написания, и требует верификации.
//
// Реализовано в этом файле: signer (HMAC-SHA256) — стабилен во всех версиях API.
// Остальные части адаптера (REST-клиент, парсеры, WS) — добавляются после сверки
// с актуальной документацией и contract-тестов на fixtures.
package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// Signer подписывает query strings для Binance USDT-M Futures.
// Алгоритм: HMAC-SHA256 с API secret как ключ, hex-кодированный результат.
type Signer struct {
	secret []byte
}

// NewSigner создаёт signer. secret копируется во внутренний буфер.
// Zero() вызывается извне при очистке.
func NewSigner(secret []byte) *Signer {
	s := &Signer{secret: make([]byte, len(secret))}
	copy(s.secret, secret)
	return s
}

// Sign возвращает hex-encoded HMAC-SHA256 от payload.
func (s *Signer) Sign(payload string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignQuery добавляет timestamp и recvWindow, считает signature и возвращает полную
// query string для приватного запроса.
//
// Пример: SignQuery("symbol=BTCUSDT&quantity=1", 1650000000000, 5000)
// → "symbol=BTCUSDT&quantity=1&timestamp=1650000000000&recvWindow=5000&signature=..."
func (s *Signer) SignQuery(query string, timestampMs int64, recvWindowMs int64) string {
	full := query
	if full != "" {
		full += "&"
	}
	full += "timestamp=" + strconv.FormatInt(timestampMs, 10)
	full += "&recvWindow=" + strconv.FormatInt(recvWindowMs, 10)
	sig := s.Sign(full)
	return full + "&signature=" + sig
}

// Zero затирает secret. Best effort (см. ограничения Go GC, THREAT_MODEL).
func (s *Signer) Zero() {
	for i := range s.secret {
		s.secret[i] = 0
	}
}

// VerifyConstantTime — проверка подписи в constant-time (защита от timing-атак).
// Используется только в тестах/отладке; для production-запросов биржа сама проверяет.
func VerifyConstantTime(secret []byte, payload, wantHex string) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	got := mac.Sum(nil)
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// Now — обёртка над time.Now для тестируемости (раздел 24 — clock sync).
var Now = func() time.Time { return time.Now() }

// NowMs — миллисекунды UTC для Binance timestamp.
func NowMs() int64 { return Now().UnixMilli() }

// ErrInvalidTimestamp — таймстамп вне recvWindow (раздел 24 — clock skew).
var ErrInvalidTimestamp = errors.New("binance: timestamp outside recvWindow")
