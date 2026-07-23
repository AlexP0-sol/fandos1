package rebalance

import (
	"testing"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// d — вспомогательная функция для создания decimal из строки в тестах.
func d(s string) decimal.Decimal {
	return decimal.MustFromString(s)
}

// defaultCfg — стандартная конфигурация для большинства тестов.
func defaultCfg() Config {
	return Config{
		TolerancePct:    d("5"),   // ±5%
		MinTransferUSDT: d("50"),  // минимум 50 USDT
		ReserveUSDT:     d("100"), // резерв 100 USDT
	}
}

const (
	exA = domain.ExchangeBinance
	exB = domain.ExchangeBybit
)

func TestPlanner_Balanced_ReturnsNil(t *testing.T) {
	// Сбалансированные балансы — план не нужен.
	p, err := NewPlanner(defaultCfg())
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}

	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1000"),
		exB: d("1000"),
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal != nil {
		t.Errorf("ожидали nil, получили %+v", proposal)
	}
}

func TestPlanner_WithinTolerance_ReturnsNil(t *testing.T) {
	// Перекос 3% при допуске 5% → план не нужен.
	p, _ := NewPlanner(defaultCfg())

	// total=2000, target=1000; excess = 1030-1000=30; 30/1000*100=3% < 5%
	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1030"),
		exB: d("970"),
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal != nil {
		t.Errorf("ожидали nil (в допуске), получили %+v", proposal)
	}
}

func TestPlanner_ExactToleranceBoundary_ReturnsNil(t *testing.T) {
	// Перекос ровно 5% — укладывается в допуск (< не <=).
	p, _ := NewPlanner(defaultCfg())

	// target=1000; excess=50; 50/1000*100=5%; 5 < 5 = false → перекос вне допуска.
	// Но раздел 12.4: "AllowedBalanceImbalancePercent" — используем строгое <.
	// При равенстве tolerance — нет предложения.
	// Пересчитаем: total=2100, target=1050; exA=1100 > exB=1000
	// excess=1100-1050=50; 50/1050*100=4.76% < 5% → nil.
	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1100"),
		exB: d("1000"),
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal != nil {
		t.Errorf("ожидали nil (в допуске 4.76%%), получили %+v", proposal)
	}
}

func TestPlanner_ExactlyAtTolerance_TriggersProposal(t *testing.T) {
	// Перекос ровно 5% → НЕ укладывается в допуск (5% < 5% = false).
	// total=2000, target=1000; exA=1050, exB=950
	// excess=50; 50/1000=5%; 5 < 5 = false → план нужен.
	p, _ := NewPlanner(Config{
		TolerancePct:    d("5"),
		MinTransferUSDT: d("1"),
		ReserveUSDT:     d("0"),
	})

	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1050"),
		exB: d("950"),
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal == nil {
		t.Fatal("ожидали proposal при перекосе 5%")
	}
	if proposal.FromExchange != exA {
		t.Errorf("FromExchange: got %s, want %s", proposal.FromExchange, exA)
	}
	if proposal.ToExchange != exB {
		t.Errorf("ToExchange: got %s, want %s", proposal.ToExchange, exB)
	}
}

func TestPlanner_Skewed_CorrectDirectionAndAmount(t *testing.T) {
	// exA имеет 1500, exB имеет 500; total=2000, target=1000.
	// excess exA = 500; deficiency exB = 500; amount = min(500,500) = 500.
	// reserve=100; available=1500-100=1400 → amount=min(500,1400)=500.
	// quantized=500.00.
	p, _ := NewPlanner(defaultCfg())

	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1500"),
		exB: d("500"),
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal == nil {
		t.Fatal("ожидали proposal")
	}
	if proposal.FromExchange != exA {
		t.Errorf("FromExchange: got %s, want %s", proposal.FromExchange, exA)
	}
	if proposal.ToExchange != exB {
		t.Errorf("ToExchange: got %s, want %s", proposal.ToExchange, exB)
	}
	if proposal.Asset != "USDT" {
		t.Errorf("Asset: got %s, want USDT", proposal.Asset)
	}
	// amount должен быть 500.00
	want := d("500")
	if !proposal.GrossAmount.Equal(want) {
		t.Errorf("GrossAmount: got %s, want %s", proposal.GrossAmount.String(), want.String())
	}
}

func TestPlanner_ReserveRespected(t *testing.T) {
	// exA=1500, exB=500; reserve для exA = 600.
	// available = 1500-600 = 900; excess=500; amount=min(500,900)=500.
	// Результат тот же: 500. Проверяем ограничение резерва в крайнем случае.
	p, _ := NewPlanner(Config{
		TolerancePct:    d("5"),
		MinTransferUSDT: d("10"),
		ReserveUSDT:     d("0"),
	})

	// Переопределяем резерв для exA через карту.
	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1200"),
		exB: d("500"),
	}
	// total=1700, target=850; excess exA = 1200-850=350; deficiency=850-500=350.
	// amount=min(350,350)=350; reserve exA=400; available=1200-400=800; amount=min(350,800)=350.
	reserves := map[domain.ExchangeID]decimal.Decimal{
		exA: d("400"),
	}
	proposal, err := p.Propose(eq, reserves)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal == nil {
		t.Fatal("ожидали proposal")
	}
	want := d("350")
	if !proposal.GrossAmount.Equal(want) {
		t.Errorf("GrossAmount: got %s, want %s", proposal.GrossAmount.String(), want.String())
	}
}

func TestPlanner_ReserveExceedsAvailable_ReturnsNil(t *testing.T) {
	// exA=1100, exB=900; reserve exA=1200 > 1100 → available отрицательный → nil.
	p, _ := NewPlanner(Config{
		TolerancePct:    d("5"),
		MinTransferUSDT: d("1"),
		ReserveUSDT:     d("0"),
	})

	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1100"),
		exB: d("900"),
	}
	reserves := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1200"),
	}
	proposal, err := p.Propose(eq, reserves)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal != nil {
		t.Errorf("ожидали nil (резерв превышает баланс), получили %+v", proposal)
	}
}

func TestPlanner_BelowMinTransfer_ReturnsNil(t *testing.T) {
	// exA=2000, exB=200; total=2200, target=1100; excess=900; deficiency=900.
	// amount=900; reserve=0; MinTransfer=1000 → 900 < 1000 → nil.
	p, _ := NewPlanner(Config{
		TolerancePct:    d("5"),
		MinTransferUSDT: d("1000"),
		ReserveUSDT:     d("0"),
	})

	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("2000"),
		exB: d("200"),
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal != nil {
		t.Errorf("ожидали nil (ниже минимума), получили %+v", proposal)
	}
}

func TestPlanner_QuantizeRoundDown(t *testing.T) {
	// Проверяем, что GrossAmount всегда округлён вниз до 2 знаков.
	// exA=1333.337, exB=1000; total=2333.337; target=1166.6685
	// excess exA = 1333.337-1166.6685=166.6685
	// deficiency = 1166.6685-1000=166.6685
	// amount = 166.6685; reserve=0; MinTransfer=1.
	// quantize к 0.01: floor(166.6685/0.01)*0.01 = 16666*0.01 = 166.66
	p, _ := NewPlanner(Config{
		TolerancePct:    d("5"),
		MinTransferUSDT: d("1"),
		ReserveUSDT:     d("0"),
	})

	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1333.337"),
		exB: d("1000"),
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal == nil {
		t.Fatal("ожидали proposal")
	}
	// Проверяем что не более 2 знаков.
	s := proposal.GrossAmount.StringFixed(2)
	got, _ := decimal.FromString(s)
	if !got.Equal(proposal.GrossAmount) {
		t.Errorf("GrossAmount %s не кратен 0.01", proposal.GrossAmount.String())
	}
}

func TestPlanner_ZeroTotal_ReturnsNil(t *testing.T) {
	p, _ := NewPlanner(defaultCfg())
	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: decimal.Zero,
		exB: decimal.Zero,
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal != nil {
		t.Errorf("ожидали nil (нулевые балансы), получили %+v", proposal)
	}
}

func TestPlanner_InvalidConfig_ReturnsError(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"отрицательный tolerance", Config{TolerancePct: d("-1"), MinTransferUSDT: d("1"), ReserveUSDT: d("0")}},
		{"отрицательный минимум", Config{TolerancePct: d("5"), MinTransferUSDT: d("-1"), ReserveUSDT: d("0")}},
		{"отрицательный резерв", Config{TolerancePct: d("5"), MinTransferUSDT: d("1"), ReserveUSDT: d("-10")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPlanner(tt.cfg)
			if err == nil {
				t.Error("ожидали ошибку при некорректном конфиге")
			}
		})
	}
}

func TestPlanner_WrongExchangeCount_ReturnsError(t *testing.T) {
	p, _ := NewPlanner(defaultCfg())

	// 1 биржа.
	eq1 := map[domain.ExchangeID]decimal.Decimal{
		exA: d("1000"),
	}
	if _, err := p.Propose(eq1, nil); err == nil {
		t.Error("ожидали ошибку при 1 бирже")
	}

	// 3 биржи.
	eq3 := map[domain.ExchangeID]decimal.Decimal{
		exA:                d("1000"),
		exB:                d("1000"),
		domain.ExchangeOKX: d("1000"),
	}
	if _, err := p.Propose(eq3, nil); err == nil {
		t.Error("ожидали ошибку при 3 биржах")
	}
}

func TestPlanner_AmountLimitedByDeficiency(t *testing.T) {
	// exA=2000, exB=900; total=2900, target=1450.
	// excess exA=550; deficiency=1450-900=550 → amount=min(550,550)=550.
	// reserve=0; min=10 → proposal 550.
	p, _ := NewPlanner(Config{
		TolerancePct:    d("5"),
		MinTransferUSDT: d("10"),
		ReserveUSDT:     d("0"),
	})

	eq := map[domain.ExchangeID]decimal.Decimal{
		exA: d("2000"),
		exB: d("900"),
	}
	proposal, err := p.Propose(eq, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if proposal == nil {
		t.Fatal("ожидали proposal")
	}
	want := d("550")
	if !proposal.GrossAmount.Equal(want) {
		t.Errorf("GrossAmount: got %s, want %s", proposal.GrossAmount.String(), want.String())
	}
}
