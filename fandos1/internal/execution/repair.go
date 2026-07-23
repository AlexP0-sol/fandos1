// repair.go — устранение дельта-дисбаланса после partial fill / one-leg fill
// (раздел 10.3 промпта v2: сценарий 60 токенов против 50).
//
// Алгоритм:
//  1. Немедленно остановить следующие slices.
//  2. Сверить фактические позиции обеих ног.
//  3. ОДНОКРАТНО попытаться добрать недостающий объём на меньшей ноге.
//  4. Если получилось 60/60 — продолжить план.
//  5. Если не получилось — закрыть reduce-only лишнюю экспозицию на большей ноге,
//     привести ноги к минимальному общему фактически доступному qty,
//     зафиксировать DEGRADED.
//
// НИКОГДА не отправлять бесконечные попытки компенсирующего ордера (раздел 10.3).
package execution

import (
	"context"
	"errors"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// RepairDecision — результат анализа дельта-дисбаланса после slice.
type RepairDecision struct {
	Action RepairAction
	// ShortfallQty — недостающий объём на меньшей ноге (для добора).
	ShortfallQty decimal.Decimal
	// ExcessQty — лишний объём на большей ноге (для reduce-only закрытия).
	ExcessQty decimal.Decimal
	// CommonQty — минимальный общий фактически исполненный объём (после приведения).
	CommonQty decimal.Decimal
	// Reason — объяснение для audit/UI.
	Reason string
}

// RepairAction — действие ремонта.
type RepairAction string

const (
	// RepairNone — дельта в tolerance, ничего делать не нужно.
	RepairNone RepairAction = "none"
	// RepairTopUpShortLeg — добрать недостающий объём на меньшей ноге (однократная попытка).
	RepairTopUpShortLeg RepairAction = "topup_short_leg"
	// RepairReduceExcess — закрыть reduce-only лишний объём большей ноги (после неудачного добора).
	RepairReduceExcess RepairAction = "reduce_excess"
	// RepairHaltAndDegraded — дельта недопустима, продолжать нельзя → DEGRADED.
	RepairHaltAndDegraded RepairAction = "halt_degraded"
)

// AnalyzeMismatch сравнивает фактические filledQty двух ног после slice и возвращает решение.
// longFilled, shortFilled — абсолютные значения (short — abs).
// toleranceBase — допустимая разница (раздел 3.5 DeltaToleranceBase).
func AnalyzeMismatch(longFilled, shortFilled, toleranceBase decimal.Decimal) RepairDecision {
	// Дельта в absolute единицах.
	diff := longFilled.Sub(shortFilled).Abs()

	// В tolerance — ничего не делаем.
	if diff.LessThanOrEqual(toleranceBase) {
		return RepairDecision{
			Action:    RepairNone,
			CommonQty: decimal.Min(longFilled, shortFilled),
			Reason:    "delta within tolerance",
		}
	}

	// Определяем, какая нога меньше.
	if longFilled.GreaterThan(shortFilled) {
		// Long больше → short недостаёт.
		shortfall := longFilled.Sub(shortFilled)
		return RepairDecision{
			Action:       RepairTopUpShortLeg,
			ShortfallQty: shortfall,
			CommonQty:    shortFilled,
			Reason:       "short leg underfilled, attempt top-up",
		}
	}
	// Short больше → long недостаёт.
	shortfall := shortFilled.Sub(longFilled)
	return RepairDecision{
		Action:       RepairTopUpShortLeg,
		ShortfallQty: shortfall,
		CommonQty:    longFilled,
		Reason:       "long leg underfilled, attempt top-up",
	}
}

// TopUpResult — исход однократной попытки добора.
//
// Контракт: NewLongQty и NewShortQty ОБЯЗАНЫ быть заполнены вызывающим
// с актуальными фактическими fill по каждой ноге — даже при Success=false.
// ApplyTopUp использует эти значения для вычисления excess и CommonQty.
type TopUpResult struct {
	Success     bool
	FilledQty   decimal.Decimal // фактически исполнено при доборе
	NewLongQty  decimal.Decimal // обновлённое значение long (заполняется вызывающим всегда)
	NewShortQty decimal.Decimal // обновлённое short (заполняется вызывающим всегда)
}

// ApplyTopUp применяет результат добора к ногам и возвращает итоговый decision.
// Если добор не удался → RepairReduceExcess (закрыть лишнее на большей ноге).
// Если удался и дельта теперь в tolerance → RepairNone.
// Если частично удался, но дельта всё ещё велика → RepairReduceExcess.
//
// Особый случай: Success=true && FilledQty==0 — противоречивый вход (биржа
// подтвердила успех, но ничего не исполнила). Обрабатывается как failure (защитная логика).
func ApplyTopUp(decision RepairDecision, topUp TopUpResult, toleranceBase decimal.Decimal) RepairDecision {
	// Защитная логика: Success=true, но FilledQty==0 — обрабатываем как неудачу.
	if !topUp.Success || topUp.FilledQty.IsZero() {
		// Добор не сработал → закрываем избыток.
		excess := decision.ShortfallQty.Sub(topUp.FilledQty)
		if excess.IsNegative() {
			excess = decimal.Zero
		}
		return RepairDecision{
			Action:    RepairReduceExcess,
			ExcessQty: excess,
			CommonQty: decimal.Min(topUp.NewLongQty, topUp.NewShortQty),
			Reason:    "top-up failed, reduce excess on heavier leg",
		}
	}
	// Добор сработал — пересчитываем дельту.
	diff := topUp.NewLongQty.Sub(topUp.NewShortQty).Abs()
	if diff.LessThanOrEqual(toleranceBase) {
		return RepairDecision{
			Action:    RepairNone,
			CommonQty: decimal.Min(topUp.NewLongQty, topUp.NewShortQty),
			Reason:    "top-up succeeded, delta within tolerance",
		}
	}
	// Добор частичный → закрываем остаток избытка.
	excess := diff
	return RepairDecision{
		Action:    RepairReduceExcess,
		ExcessQty: excess,
		CommonQty: decimal.Min(topUp.NewLongQty, topUp.NewShortQty),
		Reason:    "top-up partial, reduce remaining excess",
	}
}

// ReduceExcessRequest — параметры reduce-only ордера для устранения избытка.
//
// Side — это СТОРОНА ОРДЕРА (направление), а не сторона ноги с избытком.
// Вычисляется через ReduceExcessAction(legSide):
//   - избыток на long-ноге → ордер SideShort (продаём);
//   - избыток на short-ноге → ордер SideLong (выкупаем).
type ReduceExcessRequest struct {
	Symbol    domain.ExchangeSymbol
	ExcessQty decimal.Decimal
	Side      domain.Side // СТОРОНА ОРДЕРА (не ноги): производная ReduceExcessAction(legSide)
}

// ReduceExcessAction — переводит Side ноги с избытком в Side reduce-only ордера.
// Long-excess: у нас лишний long → продаём (SideShort reduce-only).
// Short-excess: у нас лишний short → выкупаем (SideLong reduce-only).
func ReduceExcessAction(legSide domain.Side) domain.Side {
	if legSide == domain.SideLong {
		return domain.SideShort // закрыть часть long
	}
	return domain.SideLong // закрыть часть short
}

// PlaceReduceOrder размещает reduce-only ордер для устранения избытка.
// Должен использоваться ТОЛЬКО после неудачного top-up (раздел 10.3).
// Никогда не вызывать этот метод в цикле — один вызов на одну попытку repair.
//
// Используем OrderMarket (а не OrderMarketableLimitIOC) с Price=Zero, так как
// однократный ремонтный ордер требует гарантии исполнения; объём ограничен ExcessQty
// с флагом reduce-only, поэтому дополнительной рыночной защиты не нужно.
func PlaceReduceOrder(ctx context.Context, exec *OrderExecutor, req ReduceExcessRequest,
	clientID domain.ClientOrderID) error {
	if !req.ExcessQty.IsPositive() {
		return nil
	}
	_, err := exec.Place(ctx, domain.PlaceOrderRequest{
		ClientOrderID: clientID,
		Symbol:        req.Symbol,
		Side:          req.Side,
		OrderMode:     domain.OrderMarket, // рыночный — нет смысла в лимите для repair
		BaseQty:       req.ExcessQty,
		Price:         decimal.Zero, // market: цену задаёт биржа
		ReduceOnly:    true,
		TimeInForce:   domain.TIFIOC,
	})
	if err != nil {
		return err
	}
	return nil
}

// ErrRepairExhausted — repair-цикл исчерпал попытки, нужна деградация.
var ErrRepairExhausted = errors.New("execution: repair attempts exhausted, position degraded")
