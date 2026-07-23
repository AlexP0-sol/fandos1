// score.go — candidate scoring (раздел 8.4 промпта v2).
//
// Помимо ExpectedNetPnL, сканер рассчитывает набор risk/quality scores:
//
//	LiquidityScore.
//	FundingConfidenceScore.
//	BasisStabilityScore.
//	ExecutionRiskScore.
//	CounterpartyRiskScore (обязательно).
//	DataQualityScore.
//	ADLRiskScore.
//
// Эти scores влияют на ранжирование кандидатов и на общее решение о входе
// (кандидат с высоким net PnL, но низким ADLRiskScore может быть отклонён).
package strategy

import (
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// Score — нормализованный score в [0, 1]. Выше = лучше (кроме risk scores, где выше = безопаснее).
type Score struct {
	Value decimal.Decimal // [0, 1]
}

// CandidateScores — набор всех scores кандидата.
type CandidateScores struct {
	LiquidityScore         Score
	FundingConfidenceScore Score
	BasisStabilityScore    Score
	ExecutionRiskScore     Score // выше = безопаснее (меньше риск исполнения)
	CounterpartyRiskScore  Score // выше = безопаснее контрагент
	DataQualityScore       Score
	ADLRiskScore           Score // выше = безопаснее (меньше ADL-риск)
}

// CompositeScore — взвешенная сумма всех scores (раздел 8.4).
// Используется для ранжирования кандидатов. Веса настраиваемые.
type CompositeWeights struct {
	Liquidity         decimal.Decimal
	FundingConfidence decimal.Decimal
	BasisStability    decimal.Decimal
	ExecutionRisk     decimal.Decimal
	Counterparty      decimal.Decimal
	DataQuality       decimal.Decimal
	ADLRisk           decimal.Decimal
}

// DefaultWeights — консервативные веса (сумма = 1.0).
// Риск-scores перевешивают: безопасный контрагент и низкий ADL важнее чистой ликвидности.
var DefaultWeights = CompositeWeights{
	Liquidity:         decimal.MustFromString("0.10"),
	FundingConfidence: decimal.MustFromString("0.15"),
	BasisStability:    decimal.MustFromString("0.10"),
	ExecutionRisk:     decimal.MustFromString("0.15"),
	Counterparty:      decimal.MustFromString("0.20"),
	DataQuality:       decimal.MustFromString("0.15"),
	ADLRisk:           decimal.MustFromString("0.15"),
}

// Composite — взвешенная сумма scores ∈ [0, 1].
func (s CandidateScores) Composite(w CompositeWeights) decimal.Decimal {
	sum := decimal.Zero
	sum = sum.Add(s.LiquidityScore.Value.Mul(w.Liquidity))
	sum = sum.Add(s.FundingConfidenceScore.Value.Mul(w.FundingConfidence))
	sum = sum.Add(s.BasisStabilityScore.Value.Mul(w.BasisStability))
	sum = sum.Add(s.ExecutionRiskScore.Value.Mul(w.ExecutionRisk))
	sum = sum.Add(s.CounterpartyRiskScore.Value.Mul(w.Counterparty))
	sum = sum.Add(s.DataQualityScore.Value.Mul(w.DataQuality))
	sum = sum.Add(s.ADLRiskScore.Value.Mul(w.ADLRisk))
	return sum
}

// ============================================================
// Helpers для построения scores из рыночных данных
// ============================================================

// ConfidenceToScore — маппинг ConfidenceLevel в [0, 1].
func ConfidenceToScore(c domain.ConfidenceLevel) Score {
	switch c {
	case domain.ConfidenceHigh:
		return Score{decimal.MustFromString("1.0")}
	case domain.ConfidenceMedium:
		return Score{decimal.MustFromString("0.66")}
	case domain.ConfidenceLow:
		return Score{decimal.MustFromString("0.33")}
	default:
		return Score{decimal.Zero}
	}
}

// TierToCounterpartyScore — маппинг CounterpartyRiskTier в [0, 1].
// Tier A → безопаснее (высокий score), C → опаснее (низкий score).
func TierToCounterpartyScore(t domain.CounterpartyRiskTier) Score {
	switch t {
	case domain.CounterpartyTierA:
		return Score{decimal.MustFromString("1.0")}
	case domain.CounterpartyTierB:
		return Score{decimal.MustFromString("0.6")}
	case domain.CounterpartyTierC:
		return Score{decimal.MustFromString("0.3")}
	default:
		return Score{decimal.Zero}
	}
}

// ADLQueueToScore — маппинг позиции в ADL-очереди [0, 1] в safety score.
// queue=0 → безопасно (score=1), queue=1 → max risk (score=0).
// queue — максимум из long/short очереди ноги.
func ADLQueueToScore(queue decimal.Decimal) Score {
	if queue.IsZero() {
		return Score{decimal.One}
	}
	if queue.GreaterThanOrEqual(decimal.One) {
		return Score{decimal.Zero}
	}
	// 1 - queue
	return Score{decimal.One.Sub(queue)}
}

// DepthRatioToLiquidityScore — маппинг отношения (доступная глубина / требуемый объём) в [0, 1].
// ratio >= 5 → 1.0 (отличная ликвидность); ratio < 1 → 0 (недостаточно).
func DepthRatioToLiquidityScore(ratio decimal.Decimal) Score {
	if ratio.LessThan(decimal.One) {
		return Score{decimal.Zero}
	}
	if ratio.GreaterThanOrEqual(decimal.MustFromString("5")) {
		return Score{decimal.One}
	}
	// Линейная интерполяция [1, 5] → [0, 1]: (ratio - 1) / 4.
	return Score{ratio.Sub(decimal.One).Div(decimal.MustFromString("4"))}
}

// DataQualityFromFlags — score на основе флагов свежести/sequence.
func DataQualityFromFlags(isFresh, sequenceValid bool) Score {
	if !isFresh || !sequenceValid {
		return Score{decimal.Zero}
	}
	return Score{decimal.One}
}
