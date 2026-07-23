package decimal

import (
	"math"
	"testing"
)

// TestArithmetics проверяет базовые операции на точных значениях.
// Принцип 1.2.1: никаких float64-погрешностей.
func TestArithmetics(t *testing.T) {
	tests := []struct {
		name string
		op   func() Decimal
		want string
	}{
		{"0.1+0.2", func() Decimal { return MustFromString("0.1").Add(MustFromString("0.2")) }, "0.3"},
		{"0.3-0.1", func() Decimal { return MustFromString("0.3").Sub(MustFromString("0.1")) }, "0.2"},
		{"0.1*3", func() Decimal { return MustFromString("0.1").MulInt(3) }, "0.3"},
		{"1/3 round 4", func() Decimal { return FromInt(1).Div(MustFromString("3")).Round(4) }, "0.3333"},
		{"-abs", func() Decimal { return MustFromString("-5.5").Abs() }, "5.5"},
		{"neg", func() Decimal { return MustFromString("5.5").Neg() }, "-5.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.op()
			if got.String() != tt.want {
				t.Errorf("got %s, want %s", got.String(), tt.want)
			}
		})
	}
}

// TestQuantize проверяет приведение к шагу — критично для объёма (раздел 9.2).
// Никогда не превышать разрешённый объём → round down.
func TestQuantize(t *testing.T) {
	// qty=10.7, step=2 → 10, residue 0.7
	q, r := MustFromString("10.7").Quantize(MustFromString("2"))
	if q.String() != "10" {
		t.Errorf("quantized: got %s, want 10", q.String())
	}
	if r.String() != "0.7" {
		t.Errorf("residue: got %s, want 0.7", r.String())
	}
	// qty=10, step=3 → 9, residue 1
	q2, r2 := MustFromString("10").Quantize(MustFromString("3"))
	if q2.String() != "9" || r2.String() != "1" {
		t.Errorf("got q=%s r=%s, want q=9 r=1", q2.String(), r2.String())
	}
	// Полный стакан: точное кратное
	q3, r3 := MustFromString("8").Quantize(MustFromString("2"))
	if !q3.Equal(MustFromString("8")) || !r3.IsZero() {
		t.Errorf("exact multiple failed: q=%s r=%s", q3.String(), r3.String())
	}
}

// TestQuantizePanicOnZeroStep — деление на нулевой шаг должно паниковать.
func TestQuantizePanicOnZeroStep(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on zero step")
		}
	}()
	MustFromString("1").Quantize(Zero)
}

// TestComparisons — операции сравнения.
func TestComparisons(t *testing.T) {
	a := MustFromString("5")
	b := MustFromString("3")
	if !a.GreaterThan(b) {
		t.Error("5 > 3 failed")
	}
	if !b.LessThan(a) {
		t.Error("3 < 5 failed")
	}
	if !a.GreaterThanOrEqual(a) {
		t.Error("5 >= 5 failed")
	}
	if !b.LessThanOrEqual(b) {
		t.Error("3 <= 3 failed")
	}
	if !a.Equal(FromInt(5)) {
		t.Error("5 == 5 failed")
	}
}

// TestMinMax
func TestMinMax(t *testing.T) {
	a, b := MustFromString("5"), MustFromString("3")
	if !Min(a, b).Equal(b) {
		t.Error("Min failed")
	}
	if !Max(a, b).Equal(a) {
		t.Error("Max failed")
	}
}

// TestSum
func TestSum(t *testing.T) {
	s := Sum(MustFromString("1.1"), MustFromString("2.2"), MustFromString("3.3"))
	if s.String() != "6.6" {
		t.Errorf("got %s, want 6.6", s.String())
	}
	if !Sum().IsZero() {
		t.Error("empty sum should be zero")
	}
}

// TestRoundTripFixed64ToDecimal — ADR-0002 критерий: round-trip lossless.
func TestRoundTripFixed64ToDecimal(t *testing.T) {
	cases := []Fixed64{
		MustFixed(0, 8),
		MustFixed(1, 8),
		MustFixed(123456789, 8),  // 1.23456789
		MustFixed(-123456789, 8), // -1.23456789
		MustFixed(100000000, 8),  // 1.0
		MustFixed(1, 0),          // целое
		MustFixed(1234567890, 6), // 1234.56789
		MustFixed(99999999999, 5),
		MustFixed(math.MaxInt64, 8),
		MustFixed(math.MinInt64+1, 8),
	}
	for i, f := range cases {
		d := f.ToDecimal()
		back, err := FromDecimal(d, f.Scale())
		if err != nil {
			t.Errorf("case %d (%s → %s): round-trip error: %v", i, f.String(), d.String(), err)
			continue
		}
		if back != f {
			t.Errorf("case %d (%s): round-trip mismatch, got %s", i, f.String(), back.String())
		}
	}
}

// TestFromDecimalPrecision — precision loss детектируется.
// Truncate(places) в shopspring = обрезка до N decimal places.
// Значение "1.123456789" имеет 9 decimal places → при scale=8 это lossy.
func TestFromDecimalPrecision(t *testing.T) {
	// 9 decimal places при scale=8 → lossy
	_, err := FromDecimal(MustFromString("1.123456789"), 8)
	if err == nil {
		t.Error("expected lossy error for 9 decimal places at scale 8")
	}

	// 8 decimal places при scale=8 → ok
	f, err := FromDecimal(MustFromString("1.12345678"), 8)
	if err != nil {
		t.Fatalf("expected ok for 8 dp at scale 8, got %v", err)
	}
	if f.Unscaled() != 112345678 {
		t.Errorf("unscaled: got %d, want 112345678", f.Unscaled())
	}
	if f.String() != "1.12345678" {
		t.Errorf("string: got %s, want 1.12345678", f.String())
	}

	// Целое при scale=0
	f2, err := FromDecimal(FromInt(42), 0)
	if err != nil {
		t.Fatalf("integer conv: %v", err)
	}
	if f2.Unscaled() != 42 || f2.Scale() != 0 {
		t.Errorf("got unscaled=%d scale=%d, want 42/0", f2.Unscaled(), f2.Scale())
	}

	// 3 decimal places при scale=8 — lossy (лишние нули после trunc ок)
	f3, err := FromDecimal(MustFromString("1.001"), 8)
	if err != nil {
		t.Fatalf("expected ok for 3 dp at scale 8, got %v", err)
	}
	if f3.Unscaled() != 100100000 {
		t.Errorf("unscaled: got %d, want 100100000", f3.Unscaled())
	}
}

// TestFixed64Overflow — все варианты переполнения.
func TestFixed64Overflow(t *testing.T) {
	max := MustFixed(math.MaxInt64, 8)
	one := MustFixed(1, 8)
	_, err := max.AddWith(one)
	if err == nil {
		t.Error("expected overflow on MaxInt64+1")
	}
	min := MustFixed(math.MinInt64, 8)
	_, err = min.SubWith(one)
	if err == nil {
		t.Error("expected overflow on MinInt64-1")
	}
	big := MustFixed(math.MaxInt64/2+1, 8)
	_, err = big.MulInt(2)
	if err == nil {
		t.Error("expected overflow")
	}
}

// TestFixed64ScaleMismatch
func TestFixed64ScaleMismatch(t *testing.T) {
	a := MustFixed(10, 8)
	b := MustFixed(10, 6)
	if _, err := a.AddWith(b); err == nil {
		t.Error("expected scale mismatch error")
	}
}

// TestFixed64Arithmetic — нормальные операции.
func TestFixed64Arithmetic(t *testing.T) {
	a := MustFixed(100000000, 8) // 1.0
	b := MustFixed(50000000, 8)  // 0.5

	s, err := a.AddWith(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.Unscaled() != 150000000 {
		t.Errorf("add: got %d, want 150000000", s.Unscaled())
	}

	d, err := a.SubWith(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Unscaled() != 50000000 {
		t.Errorf("sub: got %d, want 50000000", d.Unscaled())
	}

	n, err := b.Neg()
	if err != nil {
		t.Fatal(err)
	}
	if n.Unscaled() != -50000000 {
		t.Errorf("neg: got %d, want -50000000", n.Unscaled())
	}

	m, err := a.MulInt(3)
	if err != nil {
		t.Fatal(err)
	}
	if m.Unscaled() != 300000000 {
		t.Errorf("mul: got %d, want 300000000", m.Unscaled())
	}
}

// TestFixed64NegEdge — MinInt64 нельзя инвертировать.
func TestFixed64NegEdge(t *testing.T) {
	min := MustFixed(math.MinInt64, 8)
	_, err := min.Neg()
	if err == nil {
		t.Error("expected overflow negating MinInt64")
	}
}

// TestNewFixedInvalidScale
func TestNewFixedInvalidScale(t *testing.T) {
	if _, err := NewFixed(1, -1); err == nil {
		t.Error("expected error for scale=-1")
	}
	if _, err := NewFixed(1, 19); err == nil {
		t.Error("expected error for scale=19")
	}
}

// TestFromStringErrors
func TestFromStringErrors(t *testing.T) {
	bad := []string{"", "abc", "1.2.3", "1,2"}
	for _, s := range bad {
		if _, err := FromString(s); err == nil {
			t.Errorf("expected error parsing %q", s)
		}
	}
}

// TestFromStringScientific
func TestFromStringScientific(t *testing.T) {
	d, err := FromString("1e2")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Equal(FromInt(100)) {
		t.Errorf("got %s, want 100", d.String())
	}
}

// TestFixed64String — форматирование.
func TestFixed64String(t *testing.T) {
	tests := []struct {
		f    Fixed64
		want string
	}{
		{MustFixed(0, 8), "0.00000000"},
		{MustFixed(1, 8), "0.00000001"},
		{MustFixed(123456789, 8), "1.23456789"},
		{MustFixed(-123456789, 8), "-1.23456789"},
		{MustFixed(100000000, 8), "1.00000000"},
		{MustFixed(42, 0), "42"},
		{MustFixed(100, 2), "1.00"},
	}
	for _, tt := range tests {
		got := tt.f.String()
		if got != tt.want {
			t.Errorf("Fixed64(%d,%d).String() = %q, want %q", tt.f.Unscaled(), tt.f.Scale(), got, tt.want)
		}
	}
}
