// expected_pnl.go — расчёт ExpectedNetPnL (раздел 3.4 промпта v2).
//
// Это критерий eligibility кандидата: позиция открывается ТОЛЬКО если все обязательные фильтры
// пройдены и ExpectedNetPnL >= MinExpectedNetPnLUSDT.
//
// Формула:
//
//	ExpectedNetPnL =
//	    ExpectedFundingPnL
//	  + ExpectedBasisPnL
//	  - EstimatedEntryFees
//	  - EstimatedExitFees
//	  - EstimatedEntrySlippage
//	  - EstimatedExitSlippage
//	  - FundingUncertaintyReserve
//	  - BasisDivergenceReserve
//	  - CounterpartyRiskReserve
//	  - RebalanceCostReserve (если применимо)
//	  - SafetyReserve
//
// Не открывать позицию на основе одного funding rate.
package strategy

import (
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// PnLInput — все компоненты формулы ExpectedNetPnL.
// Заполняется scanner-ом из market data и настроек пользователя.
type PnLInput struct {
	// Положительные компоненты (что приносит доход).
	ExpectedFundingPnL   decimal.Decimal // из SumExpectedFundingCashFlow
	ExpectedBasisPnL     decimal.Decimal // из VWAP basis (entry + exit)

	// Отрицательные компоненты (что отнимает).
	EstimatedEntryFees   decimal.Decimal
	EstimatedExitFees    decimal.Decimal
	EstimatedEntrySlippage decimal.Decimal
	EstimatedExitSlippage  decimal.Decimal

	// Резервы (подушки безопасности, раздел 3.4).
	FundingUncertaintyReserve decimal.Decimal
	BasisDivergenceReserve    decimal.Decimal
	CounterpartyRiskReserve   decimal.Decimal
	RebalanceCostReserve      decimal.Decimal // 0 если ребаланс не нужен
	SafetyReserve             decimal.Decimal
}

// PnLBreakdown — детализированный расчёт для UI (раздел 14.2 «все резервы»).
type PnLBreakdown struct {
	Net         decimal.Decimal
	Components  []PnLComponent // упорядоченный список для отображения
}

// PnLComponent — одна строка детализации.
type PnLComponent struct {
	Name  string
	Value decimal.Decimal // со знаком: + доход, − расход/резерв
}

// ExpectedNetPnL считает полный net с детализацией.
// Все резервы — положительные числа, которые ВЫЧИТАЮТСЯ из дохода.
func ExpectedNetPnL(in PnLInput) PnLBreakdown {
	// Доходы.
	gross := in.ExpectedFundingPnL.Add(in.ExpectedBasisPnL)

	// Резервы и расходы суммарно вычитаются.
	deductions := decimal.Sum(
		in.EstimatedEntryFees,
		in.EstimatedExitFees,
		in.EstimatedEntrySlippage,
		in.EstimatedExitSlippage,
		in.FundingUncertaintyReserve,
		in.BasisDivergenceReserve,
		in.CounterpartyRiskReserve,
		in.RebalanceCostReserve,
		in.SafetyReserve,
	)

	net := gross.Sub(deductions)

	// Компоненты в порядке формулы (для UI).
	comps := []PnLComponent{
		{"ExpectedFundingPnL", in.ExpectedFundingPnL},
		{"ExpectedBasisPnL", in.ExpectedBasisPnL},
		{"EntryFees", in.EstimatedEntryFees.Neg()},
		{"ExitFees", in.EstimatedExitFees.Neg()},
		{"EntrySlippage", in.EstimatedEntrySlippage.Neg()},
		{"ExitSlippage", in.EstimatedExitSlippage.Neg()},
		{"FundingUncertaintyReserve", in.FundingUncertaintyReserve.Neg()},
		{"BasisDivergenceReserve", in.BasisDivergenceReserve.Neg()},
		{"CounterpartyRiskReserve", in.CounterpartyRiskReserve.Neg()},
		{"RebalanceCostReserve", in.RebalanceCostReserve.Neg()},
		{"SafetyReserve", in.SafetyReserve.Neg()},
	}
	// Уберём нулевые резервы из отображения для краткости UI.
	filtered := comps[:0]
	for _, c := range comps {
		if !c.Value.IsZero() || c.Name == "ExpectedFundingPnL" || c.Name == "ExpectedBasisPnL" {
			filtered = append(filtered, c)
		}
	}

	return PnLBreakdown{Net: net, Components: filtered}
}

// EligibilityCheck — результат проверки eligibility (раздел 3.4, 8.1).
type EligibilityCheck struct {
	Eligible bool
	Reason   string // причина отклонения, если !Eligible
}

// CheckEligibility — все обязательные фильтры + порог ExpectedNetPnL.
// Возвращает (eligible, reason). Вызывающий (scanner) использует это для ранжирования.
func CheckEligibility(netPnL decimal.Decimal, minNetPnLUSDT decimal.Decimal,
	confidence domain.ConfidenceLevel, minConfidence domain.ConfidenceLevel,
	secondsBeforeFunding int64, minSecondsBeforeFunding int64) EligibilityCheck {
	if !confidence.AtLeast(minConfidence) {
		return EligibilityCheck{false, "confidence below MinConfidenceLevel"}
	}
	if secondsBeforeFunding < minSecondsBeforeFunding {
		return EligibilityCheck{false, "less than MinSecondsBeforeFundingToEnter"}
	}
	if netPnL.LessThan(minNetPnLUSDT) {
		return EligibilityCheck{false, "ExpectedNetPnL below MinExpectedNetPnLUSDT"}
	}
	return EligibilityCheck{Eligible: true}
}
