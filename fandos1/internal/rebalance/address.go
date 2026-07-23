package rebalance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ============================================================
// Governance адресов (раздел 26.1).
// ============================================================

// ErrAddressRevoked — адрес был отозван и не может использоваться.
var ErrAddressRevoked = errors.New("rebalance: адрес маршрута отозван")

// ErrAddressNeverApproved — адрес ни разу не был одобрён (approved_at IS NULL).
var ErrAddressNeverApproved = errors.New("rebalance: адрес маршрута не был одобрён")

// ValidateRoute проверяет, что маршрут может быть использован для перевода (раздел 26.1).
//
// Правила:
//  1. approvedAt != nil: адрес должен быть явно одобрён.
//  2. Если у маршрута есть RevokedAt (представлен как nil ApprovedAt после отзыва)
//     — отклоняем. В текущей модели отозванный адрес передаётся с approvedAt=nil.
//
// addressApprovedAt — значение wallet_addresses.approved_at; nil = не одобрён.
//
// Функция НЕ проверяет формат адреса — это задача адаптера биржи.
func ValidateRoute(route Route, addressApprovedAt *time.Time) error {
	if addressApprovedAt == nil {
		return fmt.Errorf("%w: маршрут %d (%s → %s)",
			ErrAddressNeverApproved, route.RouteID, route.FromExchange, route.ToExchange)
	}
	// Дата одобрения есть → адрес прошёл проверку.
	return nil
}

// AddressFingerprint вычисляет fingerprint адреса для аудита (раздел 26.1, 26.5).
// Fingerprint = первые 12 символов hex(sha256(address)).
// Полный адрес НИКОГДА не попадает в логи — только fingerprint.
func AddressFingerprint(address string) string {
	sum := sha256.Sum256([]byte(address))
	full := hex.EncodeToString(sum[:])
	// Первые 12 hex-символов = 6 байт = достаточно для идентификации в аудите.
	if len(full) > 12 {
		return full[:12]
	}
	return full
}

// MemoFingerprint аналогично вычисляет fingerprint memo/tag.
// Если memo пустой — возвращает пустую строку.
func MemoFingerprint(memo string) string {
	if memo == "" {
		return ""
	}
	return AddressFingerprint(memo)
}
