// Package allocation реализует распределение капитала между кандидатами (раздел 25 промпта v2).
//
// Когда несколько eligible кандидатов одновременно требуют капитал, нельзя открывать каждый
// на максимальный объём — это превысит лимиты биржи и портфеля. Allocation решает портфельную
// задачу: какие позиции и какого размера открыть в пределах общего risk-бюджета, лимитов
// per-exchange и корреляционных ограничений.
//
// Алгоритм (минимальный, раздел 25.3):
//  1. Собрать eligible кандидатов с их score.
//  2. Отфильтровать по hard limits (per-exchange exposure, correlation).
//  3. Жадно выбрать набор в пределах risk-бюджета, приоритет — по composite score.
//  4. Вернуть allocation-план (какие пары, какого размера).
package allocation

import (
	"sort"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// CandidateRequest — запрос кандидата на капитал (вход аллокатора).
type CandidateRequest struct {
	ID            string // уникальный id (для трассировки)
	Asset         domain.AssetSymbol
	LongExchange  domain.ExchangeID
	ShortExchange domain.ExchangeID
	CompositeScore decimal.Decimal // из strategy.CandidateScores.Composite
	// DesiredQty — объём, который кандидат хотел бы (max из risk engine: liquidity/margin/limits).
	DesiredQty    decimal.Decimal
	// NotionalPerUnit — оценка стоимости единицы (mark price), для учёта exposure в USDT.
	NotionalPerUnit decimal.Decimal
}

// Limits — портфельные ограничения (раздел 25.2, 5.2).
type Limits struct {
	// MaxExposurePerExchangeUSDT — максимальный notional на одну биржу (counterparty концентрация).
	MaxExposurePerExchangeUSDT map[domain.ExchangeID]decimal.Decimal
	// CurrentExposureUSDT — уже занятый notional на бирже (открытые позиции).
	CurrentExposureUSDT map[domain.ExchangeID]decimal.Decimal
	// MaxCorrelatedNotionalUSDT — суммарный лимит по скоррелированным парам (один актив).
	MaxCorrelatedNotionalUSDT map[domain.AssetSymbol]decimal.Decimal
	// CurrentCorrelatedUSDT — уже занятый notional по активу.
	CurrentCorrelatedUSDT map[domain.AssetSymbol]decimal.Decimal
	// TotalBudgetUSDT — общий риск-бюджет портфеля.
	TotalBudgetUSDT decimal.Decimal
	// CurrentTotalUSDT — уже занятый суммарный notional портфеля.
	CurrentTotalUSDT decimal.Decimal
}

// Allocation — результат для одного кандидата.
type Allocation struct {
	Request   CandidateRequest
	AllocatedQty decimal.Decimal
	AllocatedNotional decimal.Decimal
	Rejected  bool
	Reason    string // если Rejected
}

// Plan — итоговый план аллокации.
type Plan struct {
	Allocations []Allocation
	// TotalAllocatedUSDT — суммарный выделенный notional.
	TotalAllocatedUSDT decimal.Decimal
	// RemainingBudgetUSDT — остаток риск-бюджета.
	RemainingBudgetUSDT decimal.Decimal
}

// Allocate решает портфельную задачу жадно (раздел 25.3).
// Кандидаты сортируются по CompositeScore (выше = лучше), затем каждому выделяется
// максимально возможный объём в пределах всех лимитов. Если лимиты не дают открыть
// ничего — кандидат rejected с причиной.
//
// Не открывает кандидатов, у которых DesiredQty ≤ 0 или CompositeScore ≤ 0.
func Allocate(requests []CandidateRequest, limits Limits) Plan {
	// 1. Сортировка по CompositeScore убыванию.
	sorted := make([]CandidateRequest, len(requests))
	copy(sorted, requests)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CompositeScore.GreaterThan(sorted[j].CompositeScore)
	})

	// 2. Mutable копии лимитов (будем «тратить» budget по мере аллокации).
	exchAvail := copyMap(limits.MaxExposurePerExchangeUSDT)
	exchUsed := copyMap(limits.CurrentExposureUSDT)
	assetAvail := copyMap(limits.MaxCorrelatedNotionalUSDT)
	assetUsed := copyMap(limits.CurrentCorrelatedUSDT)
	totalAvail := limits.TotalBudgetUSDT
	totalUsed := limits.CurrentTotalUSDT

	plan := Plan{}

	for _, req := range sorted {
		allocation := Allocation{Request: req}

		// Базовые sanity-checks.
		if !req.DesiredQty.IsPositive() {
			allocation.Rejected = true
			allocation.Reason = "non-positive desired qty"
			plan.Allocations = append(plan.Allocations, allocation)
			continue
		}
		if !req.CompositeScore.IsPositive() {
			allocation.Rejected = true
			allocation.Reason = "non-positive composite score"
			plan.Allocations = append(plan.Allocations, allocation)
			continue
		}
		if req.NotionalPerUnit.IsZero() {
			allocation.Rejected = true
			allocation.Reason = "zero notional per unit"
			plan.Allocations = append(plan.Allocations, allocation)
			continue
		}

		// Desired notional кандидата.
		desiredNotional := req.DesiredQty.Mul(req.NotionalPerUnit)

		// 3. Применяем лимиты — берём min по всем ограничениям.
		allowedNotional := desiredNotional

		// Per-exchange exposure (long leg).
		if cap, ok := exchAvail[req.LongExchange]; ok {
			avail := cap.Sub(exchUsed[req.LongExchange])
			if avail.LessThan(allowedNotional) {
				allowedNotional = avail
			}
		}
		// Per-exchange exposure (short leg).
		if cap, ok := exchAvail[req.ShortExchange]; ok {
			avail := cap.Sub(exchUsed[req.ShortExchange])
			if avail.LessThan(allowedNotional) {
				allowedNotional = avail
			}
		}
		// Per-asset correlated notional.
		if cap, ok := assetAvail[req.Asset]; ok {
			avail := cap.Sub(assetUsed[req.Asset])
			if avail.LessThan(allowedNotional) {
				allowedNotional = avail
			}
		}
		// Total budget.
		totalRemain := totalAvail.Sub(totalUsed)
		if totalRemain.LessThan(allowedNotional) {
			allowedNotional = totalRemain
		}

		// 4. Если nothing allowed — reject.
		if !allowedNotional.IsPositive() {
			allocation.Rejected = true
			allocation.Reason = "no budget under limits"
			plan.Allocations = append(plan.Allocations, allocation)
			continue
		}

		// 5. Конвертируем allowed notional в qty (round down — не превышать).
		allocatedQty := allowedNotional.Div(req.NotionalPerUnit)
		// На всякий случай — не больше DesiredQty.
		if allocatedQty.GreaterThan(req.DesiredQty) {
			allocatedQty = req.DesiredQty
		}
		allocatedNotional := allocatedQty.Mul(req.NotionalPerUnit)

		allocation.AllocatedQty = allocatedQty
		allocation.AllocatedNotional = allocatedNotional

		// 6. Списываем с budget.
		exchUsed[req.LongExchange] = exchUsed[req.LongExchange].Add(allocatedNotional)
		exchUsed[req.ShortExchange] = exchUsed[req.ShortExchange].Add(allocatedNotional)
		assetUsed[req.Asset] = assetUsed[req.Asset].Add(allocatedNotional)
		totalUsed = totalUsed.Add(allocatedNotional)

		plan.Allocations = append(plan.Allocations, allocation)
		plan.TotalAllocatedUSDT = plan.TotalAllocatedUSDT.Add(allocatedNotional)
	}
	plan.RemainingBudgetUSDT = totalAvail.Sub(totalUsed)
	return plan
}

// copyMap — shallow copy decimal-map.
func copyMap[K comparable](src map[K]decimal.Decimal) map[K]decimal.Decimal {
	out := make(map[K]decimal.Decimal, len(src))
	for k, v := range src {
		out[k] = v
	}
	// Defaults для ключей без значения обрабатываются вызывающим через Sub(Zero).
	return out
}

// EligibleAllocations — helper: возвращает только непринятые (не rejected).
func EligibleAllocations(p Plan) []Allocation {
	var out []Allocation
	for _, a := range p.Allocations {
		if !a.Rejected {
			out = append(out, a)
		}
	}
	return out
}

// FirstRejectReason — helper для отладки/UI.
func FirstRejectReason(p Plan) (string, bool) {
	for _, a := range p.Allocations {
		if a.Rejected {
			return a.Reason, true
		}
	}
	return "", false
}
