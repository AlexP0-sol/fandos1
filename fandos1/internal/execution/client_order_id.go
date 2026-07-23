// client_order_id.go — генерация детерминированных idempotency-идентификаторов ордеров
// (раздел 5.3 промпта v2: ClientOrderIdScheme).
//
// Каждая биржа имеет свои ограничения на clientOrderId (длина, алфавит, уникальность, срок
// хранения). Backend генерирует детерминированные id по схеме, включающей:
//   - position_id
//   - leg (long/short)
//   - slice index
//   - nonce (для retry/repair — различает повторные попытки)
//
// Это позволяет при рестарте/таймауте однозначно сопоставить биржевой ордер с внутренним
// планом и избежать дублирования.
package execution

import (
	"fmt"
	"strings"

	"github.com/thecd/fundarbitrage/internal/domain"
)

// ClientOrderIDParts — структурированные компоненты clientOrderId.
type ClientOrderIDParts struct {
	PositionID  domain.PositionID
	LegSide     domain.LegSide
	SliceIndex  int
	Nonce       int    // 0 для первой попытки, >0 для repair/retry
	Purpose     string // "ENTRY" / "EXIT" / "REPAIR"
}

// Format — форматирование clientOrderId по единой схеме.
// Формат: <purpose>:<positionID>:<leg>:<slice>:<nonce>
// Пример: ENTRY:posabc:LONG:0:0, REPAIR:posabc:SHORT:3:1
//
// Разделитель ':' выбран так, чтобы positionID мог содержать дефисы (ID часто генерируются
// как UUID-подобные строки). Двоеточие проходит валидацию не у всех бирж, поэтому
// ValidateForExchange проверяет И Format-результат, и входные parts — некорректные
// positionID нужно очищать заранее (sanitize).
//
// Алфавит: заглавные буквы, цифры, дефис (для positionID) — совместимо со всеми биржами
// (Binance/Bybit 36, OKX/Bitget/KuCoin/Gate/MEXC 32).
func Format(parts ClientOrderIDParts) domain.ClientOrderID {
	leg := "LONG"
	if parts.LegSide == domain.SideShort {
		leg = "SHORT"
	}
	purpose := parts.Purpose
	if purpose == "" {
		purpose = "ENTRY"
	}
	// Sanitize positionID: оставляем только [A-Z0-9-], uppercase.
	posID := sanitizePositionID(string(parts.PositionID))
	s := fmt.Sprintf("%s:%s:%s:%d:%d", purpose, posID, leg, parts.SliceIndex, parts.Nonce)
	return domain.ClientOrderID(s)
}

// sanitizePositionID — uppercase + фильтрация алфавита [A-Z0-9-].
func sanitizePositionID(s string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(s) {
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		isHyphen := c == '-'
		if isUpper || isDigit || isHyphen {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Parse — обратная операция: разбирает clientOrderId обратно на parts.
// Использует ручной сплит по ':' (позиция может содержать дефисы, но не ':').
// Ожидает ровно 5 полей: purpose, positionID, leg, slice, nonce.
// Возвращает ok=false, если формат не соответствует схеме (не наш ордер — например,
// ручной или из другой системы).
func Parse(s domain.ClientOrderID) (ClientOrderIDParts, bool) {
	str := string(s)
	parts := strings.Split(str, ":")
	if len(parts) != 5 {
		return ClientOrderIDParts{}, false
	}
	purpose, posID, leg, sliceStr, nonceStr := parts[0], parts[1], parts[2], parts[3], parts[4]

	var side domain.LegSide
	switch leg {
	case "LONG":
		side = domain.SideLong
	case "SHORT":
		side = domain.SideShort
	default:
		return ClientOrderIDParts{}, false
	}

	var slice, nonce int
	if _, err := fmt.Sscanf(sliceStr, "%d", &slice); err != nil {
		return ClientOrderIDParts{}, false
	}
	if _, err := fmt.Sscanf(nonceStr, "%d", &nonce); err != nil {
		return ClientOrderIDParts{}, false
	}
	return ClientOrderIDParts{
		PositionID: domain.PositionID(posID),
		LegSide:    side,
		SliceIndex: slice,
		Nonce:      nonce,
		Purpose:    purpose,
	}, true
}

// MaxLength — максимальная длина clientOrderId по всем поддерживаемым биржам.
// Реальные ограничения: Binance 36, Bybit 36, OKX 32, Bitget 32, KuCoin 32, Gate 32, MEXC 32.
// Берём минимум = 32 как безопасный потолок.
const MaxLength = 32

// ValidateForExchange — true, если clientOrderId проходит ограничения конкретной биржи
// (длина ≤ MaxLength, алфавит — заглавные буквы/цифры/дефис/двоеточие).
func ValidateForExchange(id domain.ClientOrderID) bool {
	s := string(id)
	if len(s) == 0 || len(s) > MaxLength {
		return false
	}
	for _, c := range s {
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		isHyphen := c == '-'
		isColon := c == ':'
		if !isUpper && !isDigit && !isHyphen && !isColon {
			return false
		}
	}
	return true
}

// EnsureValid — panic-гард для использования в production-пути.
func EnsureValid(id domain.ClientOrderID) {
	if !ValidateForExchange(id) {
		panic(fmt.Sprintf("execution: invalid client order id %q", id))
	}
}
