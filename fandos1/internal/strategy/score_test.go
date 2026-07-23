package strategy

import (
	"testing"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// TestConfidenceToScore — маппинг ConfidenceLevel.
func TestConfidenceToScore(t *testing.T) {
	tests := []struct {
		c    domain.ConfidenceLevel
		want string
	}{
		{domain.ConfidenceHigh, "1"},
		{domain.ConfidenceMedium, "0.66"},
		{domain.ConfidenceLow, "0.33"},
		{domain.ConfidenceNone, "0"},
	}
	for _, tt := range tests {
		s := ConfidenceToScore(tt.c)
		if s.Value.String() != tt.want {
			t.Errorf("level %v → %s, want %s", tt.c, s.Value.String(), tt.want)
		}
	}
}

// TestTierToCounterpartyScore — Tier A безопаснее C.
func TestTierToCounterpartyScore(t *testing.T) {
	a := TierToCounterpartyScore(domain.CounterpartyTierA)
	b := TierToCounterpartyScore(domain.CounterpartyTierB)
	c := TierToCounterpartyScore(domain.CounterpartyTierC)
	if !a.Value.GreaterThan(b.Value) {
		t.Error("tier A should score higher than B")
	}
	if !b.Value.GreaterThan(c.Value) {
		t.Error("tier B should score higher than C")
	}
}

// TestADLQueueToScore — нулевая очередь = безопасно, полная = опасно.
func TestADLQueueToScore(t *testing.T) {
	zero := ADLQueueToScore(decimal.Zero)
	if !zero.Value.Equal(decimal.One) {
		t.Errorf("queue=0 score=%s, want 1", zero.Value.String())
	}
	full := ADLQueueToScore(decimal.One)
	if !full.Value.IsZero() {
		t.Errorf("queue=1 score=%s, want 0", full.Value.String())
	}
	half := ADLQueueToScore(decimal.MustFromString("0.3"))
	want := decimal.MustFromString("0.7")
	if !half.Value.Equal(want) {
		t.Errorf("queue=0.3 score=%s, want %s", half.Value.String(), want.String())
	}
	// >1 клипится в 0.
	over := ADLQueueToScore(decimal.MustFromString("1.5"))
	if !over.Value.IsZero() {
		t.Error("queue>1 should clip to 0")
	}
}

// TestDepthRatioToLiquidityScore — отношение depth/qty.
func TestDepthRatioToLiquidityScore(t *testing.T) {
	// ratio < 1 → 0 (недостаточно глубины).
	if s := DepthRatioToLiquidityScore(decimal.MustFromString("0.5")); !s.Value.IsZero() {
		t.Error("ratio 0.5 should give 0")
	}
	// ratio >= 5 → 1.
	if s := DepthRatioToLiquidityScore(decimal.MustFromString("5")); !s.Value.Equal(decimal.One) {
		t.Errorf("ratio 5 → %s, want 1", s.Value.String())
	}
	// ratio = 3 → (3-1)/4 = 0.5.
	s := DepthRatioToLiquidityScore(decimal.MustFromString("3"))
	want := decimal.MustFromString("0.5")
	if !s.Value.Equal(want) {
		t.Errorf("ratio 3 → %s, want %s", s.Value.String(), want.String())
	}
}

// TestDataQualityFromFlags
func TestDataQualityFromFlags(t *testing.T) {
	if s := DataQualityFromFlags(true, true); !s.Value.Equal(decimal.One) {
		t.Error("fresh+valid → 1")
	}
	if s := DataQualityFromFlags(false, true); !s.Value.IsZero() {
		t.Error("stale → 0")
	}
	if s := DataQualityFromFlags(true, false); !s.Value.IsZero() {
		t.Error("invalid sequence → 0")
	}
}

// TestCompositeWeightsSum — DefaultWeights суммируются в 1.
func TestCompositeWeightsSum(t *testing.T) {
	w := DefaultWeights
	sum := w.Liquidity.Add(w.FundingConfidence).Add(w.BasisStability).
		Add(w.ExecutionRisk).Add(w.Counterparty).Add(w.DataQuality).Add(w.ADLRisk)
	want := decimal.One
	if !sum.Equal(want) {
		t.Errorf("weights sum = %s, want 1", sum.String())
	}
}

// TestCompositeAllMax — все scores = 1 → composite = 1.
func TestCompositeAllMax(t *testing.T) {
	max := Score{decimal.One}
	cs := CandidateScores{
		LiquidityScore: max, FundingConfidenceScore: max, BasisStabilityScore: max,
		ExecutionRiskScore: max, CounterpartyRiskScore: max, DataQualityScore: max, ADLRiskScore: max,
	}
	got := cs.Composite(DefaultWeights)
	if !got.Equal(decimal.One) {
		t.Errorf("all-max composite = %s, want 1", got.String())
	}
}

// TestCompositeAllZero — все scores = 0 → composite = 0.
func TestCompositeAllZero(t *testing.T) {
	zero := Score{decimal.Zero}
	cs := CandidateScores{
		LiquidityScore: zero, FundingConfidenceScore: zero, BasisStabilityScore: zero,
		ExecutionRiskScore: zero, CounterpartyRiskScore: zero, DataQualityScore: zero, ADLRiskScore: zero,
	}
	got := cs.Composite(DefaultWeights)
	if !got.IsZero() {
		t.Errorf("all-zero composite = %s, want 0", got.String())
	}
}

// TestCompositeWeighted — проверка взвешенной комбинации.
// Только Counterparty = 1 (вес 0.20), остальные 0 → composite = 0.20.
func TestCompositeWeighted(t *testing.T) {
	zero := Score{decimal.Zero}
	cs := CandidateScores{
		CounterpartyRiskScore: Score{decimal.One},
		LiquidityScore:        zero, FundingConfidenceScore: zero, BasisStabilityScore: zero,
		ExecutionRiskScore: zero, DataQualityScore: zero, ADLRiskScore: zero,
	}
	got := cs.Composite(DefaultWeights)
	want := DefaultWeights.Counterparty
	if !got.Equal(want) {
		t.Errorf("composite = %s, want %s (counterparty weight only)", got.String(), want.String())
	}
}
