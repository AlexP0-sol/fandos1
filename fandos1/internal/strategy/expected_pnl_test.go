package strategy

import (
	"testing"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// TestExpectedNetPnLPositive — все доходы больше расходов → положительный net.
func TestExpectedNetPnLPositive(t *testing.T) {
	in := PnLInput{
		ExpectedFundingPnL:         decimal.MustFromString("10"),
		ExpectedBasisPnL:           decimal.MustFromString("5"),
		EstimatedEntryFees:         decimal.MustFromString("1"),
		EstimatedExitFees:          decimal.MustFromString("1"),
		EstimatedEntrySlippage:     decimal.MustFromString("0.5"),
		EstimatedExitSlippage:      decimal.MustFromString("0.5"),
		FundingUncertaintyReserve:  decimal.MustFromString("2"),
		BasisDivergenceReserve:     decimal.MustFromString("1"),
		CounterpartyRiskReserve:    decimal.MustFromString("1.5"),
		SafetyReserve:              decimal.MustFromString("2"),
	}
	// gross = 15, deductions = 1+1+0.5+0.5+2+1+1.5+2 = 9.5, net = 5.5
	bd := ExpectedNetPnL(in)
	want := decimal.MustFromString("5.5")
	if !bd.Net.Equal(want) {
		t.Errorf("net = %s, want %s", bd.Net.String(), want.String())
	}
}

// TestExpectedNetPnLNegative — расходы превышают доходы → отрицательный net.
func TestExpectedNetPnLNegative(t *testing.T) {
	in := PnLInput{
		ExpectedFundingPnL:      decimal.MustFromString("1"),
		EstimatedEntryFees:       decimal.MustFromString("3"),
		EstimatedExitFees:        decimal.MustFromString("3"),
	}
	bd := ExpectedNetPnL(in)
	if !bd.Net.IsNegative() {
		t.Errorf("net = %s, want negative", bd.Net.String())
	}
}

// TestExpectedNetPnLZeroReserves — нулевые резервы исключаются из детализации, но доходы остаются.
func TestExpectedNetPnLZeroReserves(t *testing.T) {
	in := PnLInput{
		ExpectedFundingPnL: decimal.MustFromString("10"),
	}
	bd := ExpectedNetPnL(in)
	if !bd.Net.Equal(decimal.MustFromString("10")) {
		t.Errorf("net = %s, want 10", bd.Net.String())
	}
	// Должны остаться только компоненты с ненулевым значением + обязательные доходы.
	for _, c := range bd.Components {
		if c.Name != "ExpectedFundingPnL" && c.Name != "ExpectedBasisPnL" {
			t.Errorf("unexpected component %s when all reserves zero", c.Name)
		}
	}
}

// TestExpectedNetPnLRebalanceReserve — ребаланс-резерв включается когда задан.
func TestExpectedNetPnLRebalanceReserve(t *testing.T) {
	in := PnLInput{
		ExpectedFundingPnL:   decimal.MustFromString("10"),
		RebalanceCostReserve: decimal.MustFromString("2"),
	}
	bd := ExpectedNetPnL(in)
	if !bd.Net.Equal(decimal.MustFromString("8")) {
		t.Errorf("net = %s, want 8 (10 - 2 rebalance)", bd.Net.String())
	}
	found := false
	for _, c := range bd.Components {
		if c.Name == "RebalanceCostReserve" {
			found = true
		}
	}
	if !found {
		t.Error("RebalanceCostReserve missing from breakdown")
	}
}

// TestCheckEligibilityPass — все фильтры пройдены.
func TestCheckEligibilityPass(t *testing.T) {
	res := CheckEligibility(
		decimal.MustFromString("5"),  // net
		decimal.MustFromString("1"),  // min
		domain.ConfidenceHigh,        // confidence
		domain.ConfidenceMedium,      // minConfidence
		60,                           // secondsBeforeFunding
		30,                           // minSeconds
	)
	if !res.Eligible {
		t.Errorf("expected eligible, reason=%s", res.Reason)
	}
}

// TestCheckEligibilityConfidenceTooLow
func TestCheckEligibilityConfidenceTooLow(t *testing.T) {
	res := CheckEligibility(
		decimal.MustFromString("100"),
		decimal.MustFromString("1"),
		domain.ConfidenceLow,    // ниже требуемого
		domain.ConfidenceMedium,
		60, 30,
	)
	if res.Eligible {
		t.Error("low confidence should be rejected")
	}
}

// TestCheckEligibilityTooCloseToFunding — нарушение MinSecondsBeforeFundingToEnter.
func TestCheckEligibilityTooCloseToFunding(t *testing.T) {
	res := CheckEligibility(
		decimal.MustFromString("100"),
		decimal.MustFromString("1"),
		domain.ConfidenceHigh,
		domain.ConfidenceMedium,
		10,  // осталось 10 сек
		30,  // минимум 30
	)
	if res.Eligible {
		t.Error("too close to funding should be rejected")
	}
}

// TestCheckEligibilityPnLTooLow
func TestCheckEligibilityPnLTooLow(t *testing.T) {
	res := CheckEligibility(
		decimal.MustFromString("0.5"),
		decimal.MustFromString("1"), // больше
		domain.ConfidenceHigh,
		domain.ConfidenceMedium,
		60, 30,
	)
	if res.Eligible {
		t.Error("PnL below threshold should be rejected")
	}
}

// TestExpectedNetPnLExactBreakdownSum — сумма компонентов = net (consistency check).
func TestExpectedNetPnLExactBreakdownSum(t *testing.T) {
	in := PnLInput{
		ExpectedFundingPnL:      decimal.MustFromString("10"),
		ExpectedBasisPnL:        decimal.MustFromString("3"),
		EstimatedEntryFees:      decimal.MustFromString("1"),
		SafetyReserve:           decimal.MustFromString("2"),
	}
	bd := ExpectedNetPnL(in)
	var sum decimal.Decimal
	for _, c := range bd.Components {
		sum = sum.Add(c.Value)
	}
	if !sum.Equal(bd.Net) {
		t.Errorf("sum of components %s != net %s", sum.String(), bd.Net.String())
	}
}
