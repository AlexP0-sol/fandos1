// Package rebalance реализует двигатель ребалансировки капитала (раздел 12 промпта v2).
// Этап 10: plan-only + test-transfer барьер.
//
// Пакет содержит:
//   - Planner   — расчёт цели 50/50 и генерация PlanProposal (раздел 12.4).
//   - StateMachine — машина состояний плана ребалансировки (раздел 12.5).
//   - Executor  — двухфазный движок TEST/MAIN (раздел 12.6).
//   - CircuitBreaker — защитник вывода (раздел 26.4).
//   - address   — helpers для governance адресов (раздел 26.1).
package rebalance

import (
	"fmt"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// ============================================================
// Config — конфигурация планировщика ребалансировки.
// ============================================================

// Config — параметры планировщика 50/50 (раздел 12.4).
// Все суммы в USDT, точность decimal.
type Config struct {
	// TolerancePct — допустимое отклонение от цели 50/50 в процентах (например, 5 = ±5%).
	// Если перекос не превышает TolerancePct — план не нужен.
	TolerancePct decimal.Decimal

	// MinTransferUSDT — минимальная сумма перевода, ниже которой план не создаётся.
	MinTransferUSDT decimal.Decimal

	// ReserveUSDT — оперативный резерв на бирже-источнике, который нельзя переводить.
	// Каждая биржа имеет свой резерв; если не задан — используется это значение.
	ReserveUSDT decimal.Decimal
}

// ============================================================
// PlanProposal — предложение плана ребалансировки.
// ============================================================

// PlanProposal — результат работы Planner: предложение перевода средств.
// nil означает, что система в пределах допуска или сумма ниже минимума.
type PlanProposal struct {
	// FromExchange — биржа-источник (избыток капитала).
	FromExchange domain.ExchangeID
	// ToExchange — биржа-получатель (дефицит капитала).
	ToExchange domain.ExchangeID
	// Asset — актив для перевода; в v1 всегда "USDT".
	Asset string
	// GrossAmount — валовая сумма к переводу (до вычета комиссии), округлена вниз до центов.
	GrossAmount decimal.Decimal
	// Reason — человекочитаемое обоснование (для UI и аудита).
	Reason string
}

// ============================================================
// Planner — вычисление предложения ребалансировки.
// ============================================================

// Planner вычисляет PlanProposal по текущим балансам (раздел 12.4).
// Не имеет состояния; можно вызывать параллельно.
type Planner struct {
	cfg Config
}

// NewPlanner создаёт Planner с заданной конфигурацией.
// Возвращает ошибку, если конфигурация некорректна.
func NewPlanner(cfg Config) (*Planner, error) {
	if cfg.TolerancePct.IsNegative() {
		return nil, fmt.Errorf("rebalance: TolerancePct не может быть отрицательным")
	}
	if cfg.MinTransferUSDT.IsNegative() {
		return nil, fmt.Errorf("rebalance: MinTransferUSDT не может быть отрицательным")
	}
	if cfg.ReserveUSDT.IsNegative() {
		return nil, fmt.Errorf("rebalance: ReserveUSDT не может быть отрицательным")
	}
	return &Planner{cfg: cfg}, nil
}

// Propose вычисляет предложение ребалансировки для пары бирж.
//
// equity — эффективный капитал (NetTransferableEquity) по каждой бирже в USDT.
// reserves — переопределение резерва по конкретной бирже; если не задан — берётся cfg.ReserveUSDT.
//
// Алгоритм (раздел 12.4):
//  1. total = сумма по всем биржам.
//  2. target = total / 2.
//  3. Определить биржу с избытком (excess) и биржу с дефицитом (deficit).
//  4. amount = min(excess - target, target - deficit) — не превышать ни одну сторону.
//  5. Вычесть резерв биржи-источника.
//  6. Если отклонение укладывается в TolerancePct от target — вернуть nil.
//  7. Если amount ниже MinTransferUSDT — вернуть nil.
//  8. Округлить вниз до центов (2 знака).
//
// Функция поддерживает ровно две биржи; для большего числа расширить в будущих этапах.
func (p *Planner) Propose(
	equity map[domain.ExchangeID]decimal.Decimal,
	reserves map[domain.ExchangeID]decimal.Decimal,
) (*PlanProposal, error) {
	if len(equity) != 2 {
		return nil, fmt.Errorf("rebalance: Planner поддерживает ровно 2 биржи, получено %d", len(equity))
	}

	// Собираем пары бирж и считаем total.
	exchanges := make([]domain.ExchangeID, 0, 2)
	total := decimal.Zero
	for ex, eq := range equity {
		if eq.IsNegative() {
			return nil, fmt.Errorf("rebalance: equity биржи %q отрицательна: %s", ex, eq.String())
		}
		exchanges = append(exchanges, ex)
		total = total.Add(eq)
	}

	// Пустой суммарный баланс — ничего делать не нужно.
	if total.IsZero() {
		return nil, nil
	}

	// target = total / 2.
	two := decimal.MustNew(2, 0)
	target := total.Div(two)

	// Определяем источник (избыток) и получателя (дефицит).
	exA, exB := exchanges[0], exchanges[1]
	eqA, eqB := equity[exA], equity[exB]

	var fromEx, toEx domain.ExchangeID
	var fromEq, toEq decimal.Decimal

	// fromEx — тот, у кого больше target.
	if eqA.GreaterThan(eqB) {
		fromEx, fromEq = exA, eqA
		toEx, toEq = exB, eqB
	} else {
		fromEx, fromEq = exB, eqB
		toEx, toEq = exA, eqA
	}

	// Проверка допуска: если перекос < TolerancePct от target — не нужно.
	// imbalancePct = (fromEq - target) / target * 100
	excess := fromEq.Sub(target)
	hundred := decimal.MustNew(100, 0)

	if !target.IsZero() {
		imbalancePct := excess.Div(target).Mul(hundred)
		if imbalancePct.LessThan(p.cfg.TolerancePct) {
			// Укладывается в допуск.
			return nil, nil
		}
	}

	// amount = min(excess, target - toEq) — не перелить destination.
	deficiency := target.Sub(toEq)
	amount := decimal.Min(excess, deficiency)

	// Вычитаем резерв биржи-источника.
	reserve := p.cfg.ReserveUSDT
	if r, ok := reserves[fromEx]; ok {
		reserve = r
	}
	availableOnSource := fromEq.Sub(reserve)
	if availableOnSource.LessThanOrEqual(decimal.Zero) {
		// Источник полностью занят резервом.
		return nil, nil
	}
	if amount.GreaterThan(availableOnSource) {
		amount = availableOnSource
	}

	// Проверка минимума.
	if amount.LessThan(p.cfg.MinTransferUSDT) || amount.IsZero() {
		return nil, nil
	}

	// Округляем вниз до 2 знаков (центы).
	centStep := decimal.MustNew(1, -2) // 0.01
	quantized, _ := amount.Quantize(centStep)
	if quantized.IsZero() {
		return nil, nil
	}

	// Повторная проверка минимума после квантизации.
	if quantized.LessThan(p.cfg.MinTransferUSDT) {
		return nil, nil
	}

	reason := fmt.Sprintf(
		"rebalance_50_50: %s имеет %s USDT (target %s), дефицит %s USDT, перевести %s USDT",
		fromEx, fromEq.StringFixed(2), target.StringFixed(2), deficiency.StringFixed(2), quantized.StringFixed(2),
	)

	return &PlanProposal{
		FromExchange: fromEx,
		ToExchange:   toEx,
		Asset:        "USDT",
		GrossAmount:  quantized,
		Reason:       reason,
	}, nil
}
