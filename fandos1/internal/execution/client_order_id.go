// client_order_id.go — генерация детерминированных idempotency-идентификаторов ордеров
// (раздел 5.3 промпта v2: ClientOrderIdScheme).
//
// Каждая биржа имеет свои ограничения на clientOrderId (длина, алфавит, уникальность, срок
// хранения). Backend генерирует детерминированные id по схеме, включающей:
//   - position_id
//   - leg (long/short)
//   - slice index
//   - nonce (для retry/repair — различает повторные попытки)
//   - purpose (ENTRY/EXIT/REPAIR/EMERGENCY)
//
// Это позволяет при рестарте/таймауте однозначно сопоставить биржевой ордер с внутренним
// планом и избежать дублирования.
//
// Формат после Format: <PURPOSE>_<POSID>_<LEG>_<SLICE>_<NONCE>
//   - Разделитель '_' (подчёркивание) — удовлетворяет ограничениям Bybit ([A-Za-z0-9_-]).
//   - Все компоненты санируются в алфавит [A-Z0-9-] (заглавные, цифры, дефис).
//     '_' внутри компонентов невозможен (отфильтровывается при санировании) — поэтому
//     сплит по '_' однозначен и Parse→Format round-trip корректен для санированных входов.
//   - Все символы — ASCII-only; длина в байтах == длина в символах.
//   - Суммарная длина ≤ MaxLength (32). Если компонент POSID слишком длинный — обрезается
//     детерминированно: оставляется хвост после санирования (tail), чтобы сохранить
//     специфику последних символов (UUID-суффиксы).
//
// Свойство round-trip: Parse(Format(x)) возвращает санированные значения, а не исходные.
// Например, positionID "pos-abc" → санировано в "POS-ABC"; Parse вернёт "POS-ABC".
package execution

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/thecd/fundarbitrage/internal/domain"
)

// Purpose — тип цели ордера (typed enum). Гарантирует корректные константы на сайтах вызова.
type Purpose string

const (
	// PurposeEntry — вход в позицию.
	PurposeEntry Purpose = "ENTRY"
	// PurposeExit — плановый выход из позиции.
	PurposeExit Purpose = "EXIT"
	// PurposeRepair — ремонт дельта-дисбаланса (раздел 10.3).
	PurposeRepair Purpose = "REPAIR"
	// PurposeEmergency — экстренное закрытие (раздел 11.3, UltimateEmergencyClosePolicy).
	PurposeEmergency Purpose = "EMERGENCY"
)

// ClientOrderIDParts — структурированные компоненты clientOrderId.
type ClientOrderIDParts struct {
	PositionID domain.PositionID
	LegSide    domain.LegSide
	SliceIndex int
	Nonce      int     // 0 для первой попытки, >0 для repair/retry
	Purpose    Purpose // типизированный enum; sanitize всё равно применяется для защиты
}

// Format — форматирование clientOrderId по единой схеме.
// Формат: <PURPOSE>_<POSID>_<LEG>_<SLICE>_<NONCE>
// Разделитель '_'; компоненты санируются в [A-Z0-9-].
// Если суммарная длина > MaxLength — PositionID обрезается детерминированно (хвост).
//
// Гарантии:
//   - результат проходит ValidateForExchange для Bybit и Binance;
//   - Parse(Format(x)) возвращает санированные значения (не исходные сырые).
func Format(parts ClientOrderIDParts) domain.ClientOrderID {
	leg := "LONG"
	if parts.LegSide == domain.SideShort {
		leg = "SHORT"
	}
	// Санируем purpose в [A-Z0-9-]; пустой — ENTRY.
	purpose := sanitizeComponent(string(parts.Purpose))
	if purpose == "" {
		purpose = string(PurposeEntry)
	}
	// Санируем positionID в [A-Z0-9-], '_' будет отфильтрован.
	posID := sanitizeComponent(string(parts.PositionID))

	// Суффикс фиксированной структуры: _<leg>_<slice>_<nonce>.
	suffix := fmt.Sprintf("_%s_%d_%d", leg, parts.SliceIndex, parts.Nonce)

	// Максимальная длина prefix-части: MaxLength - len("_") - len(purpose) - len(suffix).
	// prefix = purpose + "_" + posID (обрезанный при необходимости).
	maxPosLen := MaxLength - len(purpose) - 1 - len(suffix)
	if maxPosLen < 0 {
		maxPosLen = 0
	}
	if len(posID) > maxPosLen {
		// Детерминированно оставляем хвост — UUID-суффиксы более специфичны.
		posID = posID[len(posID)-maxPosLen:]
	}

	s := purpose + "_" + posID + suffix
	return domain.ClientOrderID(s)
}

// sanitizeComponent — uppercase + фильтр алфавита [A-Z0-9-].
// '_' никогда не включается, чтобы разделитель '_' в Format оставался однозначным.
// Все символы — ASCII-only: длина в байтах == длина в символах.
func sanitizeComponent(s string) string {
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

// allowedComponentRE — проверяет, что компонент содержит только [A-Z0-9-].
var allowedComponentRE = regexp.MustCompile(`^[A-Z0-9-]+$`)

// Parse — обратная операция: разбирает clientOrderId обратно на parts.
// Использует сплит по '_' (компоненты не содержат '_' после санирования).
// Ожидает ровно 5 полей: purpose, positionID, leg, slice, nonce.
//
// Важно: Parse(Format(x)) возвращает санированные значения, а не исходные сырые входы.
// Например, positionID "pos-abc" → Format → "POS-ABC" → Parse → PositionID="POS-ABC".
//
// Возвращает ok=false, если формат не соответствует схеме (не наш ордер — например,
// ручной или из другой системы).
func Parse(s domain.ClientOrderID) (ClientOrderIDParts, bool) {
	str := string(s)
	fields := strings.Split(str, "_")
	if len(fields) != 5 {
		return ClientOrderIDParts{}, false
	}
	purpose, posID, leg, sliceStr, nonceStr := fields[0], fields[1], fields[2], fields[3], fields[4]

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
		Purpose:    Purpose(purpose),
	}, true
}

// MaxLength — максимальная длина clientOrderId (байты; ASCII-only, поэтому байты == символы).
// Реальные ограничения: Binance 36, Bybit 36, OKX 32, Bitget 32, KuCoin 32, Gate 32, MEXC 32.
// Берём минимум = 32 как безопасный потолок для всех поддерживаемых бирж.
const MaxLength = 32

// Validate — внутренняя валидация: проверяет длину и алфавит [A-Z0-9_-].
// Используется для internal-проверок (не привязана к конкретной бирже).
func Validate(id domain.ClientOrderID) bool {
	s := string(id)
	if len(s) == 0 || len(s) > MaxLength {
		return false
	}
	for _, c := range s {
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		isHyphen := c == '-'
		isUnderscore := c == '_'
		if !isUpper && !isDigit && !isHyphen && !isUnderscore {
			return false
		}
	}
	return true
}

// ValidateForExchange — true, если clientOrderId проходит ограничения конкретной биржи
// (длина и алфавит). Зависит от биржи:
//
//   - Binance: max 36, алфавит [A-Za-z0-9._:/-]
//   - Bybit:   max 36, алфавит [A-Za-z0-9_-]
//   - default: max 32, алфавит [A-Z0-9_-] (консервативный потолок)
func ValidateForExchange(exchange domain.ExchangeID, id domain.ClientOrderID) bool {
	s := string(id)
	if len(s) == 0 {
		return false
	}
	switch exchange {
	case domain.ExchangeBinance:
		// Binance: max 36, алфавит [A-Za-z0-9._:/-]
		if len(s) > 36 {
			return false
		}
		for _, c := range s {
			isAlpha := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
			isDigit := c >= '0' && c <= '9'
			isSpecial := c == '.' || c == '_' || c == ':' || c == '/' || c == '-'
			if !isAlpha && !isDigit && !isSpecial {
				return false
			}
		}
		return true
	case domain.ExchangeBybit:
		// Bybit: max 36, алфавит [A-Za-z0-9_-]
		if len(s) > 36 {
			return false
		}
		for _, c := range s {
			isAlpha := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
			isDigit := c >= '0' && c <= '9'
			isSpecial := c == '_' || c == '-'
			if !isAlpha && !isDigit && !isSpecial {
				return false
			}
		}
		return true
	default:
		// Консервативный потолок для остальных бирж: max 32, [A-Z0-9_-].
		if len(s) > MaxLength {
			return false
		}
		for _, c := range s {
			isUpper := c >= 'A' && c <= 'Z'
			isDigit := c >= '0' && c <= '9'
			isSpecial := c == '_' || c == '-'
			if !isUpper && !isDigit && !isSpecial {
				return false
			}
		}
		return true
	}
}

// EnsureValid — panic-гард для использования в production-пути.
// Проверяет через внутренний Validate (не привязан к конкретной бирже).
func EnsureValid(id domain.ClientOrderID) {
	if !Validate(id) {
		panic(fmt.Sprintf("execution: invalid client order id %q", id))
	}
}
