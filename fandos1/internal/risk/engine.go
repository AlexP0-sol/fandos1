// Package risk реализует risk engine (раздел 5.2, 11.2, 23 промпта v2).
//
// Risk engine контролирует лимиты и инварианты:
//   - Дельта-нейтральность (раздел 3.5): abs(DeltaBase) <= tolerance
//   - Margin / liquidation distance (раздел 5.2)
//   - ADL exposure limit per-exchange (раздел 23.4)
//   - MaxDailyLoss + RiskSnapAfterMaxDailyLoss (раздел 5.2)
//   - CounterpartyRiskTier с haircut (раздел 5.2)
//
// Engine НЕ размещает ордера — он только оценивает и сигнализирует о нарушениях.
// Решения (close/halt) принимает execution/portfolio на основе CheckResult.
package risk

import (
	"fmt"
	"sort"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// Limits — risk-лимиты для проверки (подмножество HotSettings, раздел 5.2).
type Limits struct {
	DeltaToleranceBase                  decimal.Decimal
	DeltaToleranceUSD                   decimal.Decimal
	MaxDailyLossUSDT                    decimal.Decimal
	MaxPositionLossUSDT                 decimal.Decimal
	MinimumDistanceToLiquidationPercent decimal.Decimal // минимальная дистанция до ликвидации, %
	EmergencyMarginRatio                decimal.Decimal // при каком margin ratio — экстренное закрытие
	MaxExposurePerExchangeUSDT          map[domain.ExchangeID]decimal.Decimal
	ADLExposureLimitPercent             map[domain.ExchangeID]decimal.Decimal
	// CounterpartyHaircutFraction — доля notional в резерв по tier биржи (дробное значение, не %).
	CounterpartyHaircutFraction map[domain.CounterpartyRiskTier]decimal.Decimal
	RiskSnapAfterMaxDailyLoss   bool
}

// PositionInput — рыночные данные одной позиции для risk-check.
type PositionInput struct {
	Asset                      domain.AssetSymbol
	LongExchange               domain.ExchangeID
	ShortExchange              domain.ExchangeID
	LongBaseQty                decimal.Decimal
	ShortBaseQty               decimal.Decimal // abs
	MarkPrice                  decimal.Decimal // консервативная цена для DeltaUSD
	UnrealizedPnL              decimal.Decimal
	MarginRatioPerLeg          map[domain.ExchangeID]decimal.Decimal // 0..1, выше = ближе к ликвидации
	LiquidationDistancePercent map[domain.ExchangeID]decimal.Decimal
}

// Severity — критичность нарушения.
type Severity int

const (
	SeverityNone Severity = iota
	SeverityWarning
	SeverityCritical // требует немедленного действия
)

// Violation — одно нарушение лимита.
type Violation struct {
	Severity Severity
	Code     string
	Message  string
	// Exchange — биржа, связанная с нарушением. Нулевое значение означает «не привязано к бирже».
	Exchange domain.ExchangeID
}

// CheckResult — итог проверки одной позиции.
type CheckResult struct {
	Violations []Violation
	DeltaBase  decimal.Decimal
	DeltaUSD   decimal.Decimal
}

// HasCritical — true, если хотя бы одно нарушение критично (требует экстренного закрытия).
func (r CheckResult) HasCritical() bool {
	for _, v := range r.Violations {
		if v.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// HasWarning — true при наличии warning (не требует немедленного действия, но сигнал).
func (r CheckResult) HasWarning() bool {
	for _, v := range r.Violations {
		if v.Severity == SeverityWarning {
			return true
		}
	}
	return false
}

// CheckPosition — проверяет инварианты одной позиции (раздел 3.5, 5.2, 11.2).
// ОБЯЗАТЕЛЬНО вызывается execution-координатором ПЕРЕД любым PlaceOrder:
// нарушение дельта-нейтральности или приближение к ликвидации требует немедленного
// решения (закрыть/остановить) до размещения новых ордеров.
func CheckPosition(p PositionInput, limits Limits) CheckResult {
	res := CheckResult{}

	// 1. Дельта-нейтральность (раздел 3.5).
	res.DeltaBase = p.LongBaseQty.Sub(p.ShortBaseQty.Abs())
	res.DeltaUSD = res.DeltaBase.Mul(p.MarkPrice)

	if res.DeltaBase.Abs().GreaterThan(limits.DeltaToleranceBase) {
		res.Violations = append(res.Violations, Violation{
			Severity: SeverityCritical,
			Code:     "DELTA_BASE_BREACH",
			Message:  "DeltaBase превысила tolerance — направленная экспозиция",
		})
	}
	if res.DeltaUSD.Abs().GreaterThan(limits.DeltaToleranceUSD) {
		res.Violations = append(res.Violations, Violation{
			Severity: SeverityCritical,
			Code:     "DELTA_USD_BREACH",
			Message:  "DeltaUSD превысила tolerance",
		})
	}

	// 2. Margin ratio (раздел 5.2, 11.2 P0).
	// Итерируем по отсортированным ключам для детерминированного вывода.
	// Одно нарушение per breaching exchange (без break).
	{
		keys := sortedExchangeKeys(p.MarginRatioPerLeg)
		for _, ex := range keys {
			mr := p.MarginRatioPerLeg[ex]
			if mr.GreaterThanOrEqual(limits.EmergencyMarginRatio) {
				res.Violations = append(res.Violations, Violation{
					Severity: SeverityCritical,
					Code:     "EMERGENCY_MARGIN_RATIO",
					Message:  fmt.Sprintf("margin ratio на бирже %s достиг emergency уровня — риск ликвидации", ex),
					Exchange: ex,
				})
				// Не break: фиксируем нарушение по каждой бирже.
			}
		}
	}

	// 3. Liquidation distance.
	// Одно нарушение per breaching exchange (без break).
	{
		keys := sortedExchangeKeys(p.LiquidationDistancePercent)
		for _, ex := range keys {
			dist := p.LiquidationDistancePercent[ex]
			if dist.LessThan(limits.MinimumDistanceToLiquidationPercent) {
				res.Violations = append(res.Violations, Violation{
					Severity: SeverityCritical,
					Code:     "LIQUIDATION_TOO_CLOSE",
					Message:  fmt.Sprintf("дистанция до liquidation на бирже %s ниже минимума", ex),
					Exchange: ex,
				})
				// Не break: фиксируем нарушение по каждой бирже.
			}
		}
	}

	// 4. Max position loss.
	if limits.MaxPositionLossUSDT.IsPositive() && p.UnrealizedPnL.Abs().GreaterThan(limits.MaxPositionLossUSDT) {
		// Убыток только если PnL отрицательный.
		if p.UnrealizedPnL.IsNegative() {
			res.Violations = append(res.Violations, Violation{
				Severity: SeverityCritical,
				Code:     "MAX_POSITION_LOSS",
				Message:  "убыток позиции превысил MaxPositionLossUSDT",
			})
		}
	}

	return res
}

// sortedExchangeKeys возвращает ключи map в отсортированном порядке (детерминизм).
func sortedExchangeKeys(m map[domain.ExchangeID]decimal.Decimal) []domain.ExchangeID {
	keys := make([]domain.ExchangeID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return string(keys[i]) < string(keys[j])
	})
	return keys
}

// DailyLossStatus — состояние дневного стопа (раздел 5.2 RiskSnapAfterMaxDailyLoss).
type DailyLossStatus struct {
	Date             time.Time       // торговый день (UTC, midnight-aligned)
	RealisedLossUSDT decimal.Decimal // суммарный реализованный убыток за день (только negative PnL)
	Snapped          bool            // true, если сработал RiskSnapAfterMaxDailyLoss → SAFE_HALT
}

// CheckDailyLoss — проверка дневного лимита убытка.
// realizedPnLToday — суммарный реализованный PnL за день (может быть отрицательным).
// date — торговый день (используется для заполнения DailyLossStatus.Date).
func CheckDailyLoss(realizedPnLToday decimal.Decimal, limits Limits, date time.Time) (DailyLossStatus, []Violation) {
	var vs []Violation
	st := DailyLossStatus{
		Date:             date,
		RealisedLossUSDT: decimal.Zero,
	}

	// Убыток — только отрицательная часть.
	loss := decimal.Zero
	if realizedPnLToday.IsNegative() {
		loss = realizedPnLToday.Abs()
	}
	st.RealisedLossUSDT = loss

	if limits.MaxDailyLossUSDT.IsPositive() && loss.GreaterThan(limits.MaxDailyLossUSDT) {
		vs = append(vs, Violation{
			Severity: SeverityCritical,
			Code:     "MAX_DAILY_LOSS",
			Message:  "дневной убыток превысил MaxDailyLossUSDT",
		})
		if limits.RiskSnapAfterMaxDailyLoss {
			st.Snapped = true
		}
	}
	return st, vs
}

// CounterpartyReserve — haircut на контрагента для ExpectedNetPnL (раздел 3.4).
// Возвращает долю notional, которую нужно отложить в резерв по tier биржи.
func CounterpartyReserve(notional decimal.Decimal, tier domain.CounterpartyRiskTier,
	haircuts map[domain.CounterpartyRiskTier]decimal.Decimal) decimal.Decimal {
	if haircuts == nil {
		return decimal.Zero
	}
	h, ok := haircuts[tier]
	if !ok || !h.IsPositive() {
		return decimal.Zero
	}
	return notional.Mul(h)
}

// ADLExposureBreached — true, если ADL exposure на бирже выше лимита (раздел 23.4).
func ADLExposureBreached(currentExposure, limitUSDT decimal.Decimal) (bool, decimal.Decimal) {
	if !limitUSDT.IsPositive() {
		return false, decimal.Zero
	}
	if currentExposure.GreaterThan(limitUSDT) {
		return true, currentExposure.Sub(limitUSDT)
	}
	return false, decimal.Zero
}
