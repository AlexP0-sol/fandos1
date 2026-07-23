package orderbook

import (
	"testing"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

func lvl(price, qty string) domain.PriceLevel {
	return domain.PriceLevel{
		Price: decimal.MustFromString(price),
		Qty:   decimal.MustFromString(qty),
	}
}

// TestVWAPSingleLevel — весь объём на одном уровне → VWAP = цене уровня.
func TestVWAPSingleLevel(t *testing.T) {
	asks := []domain.PriceLevel{lvl("100.00", "10")}
	r := VWAPBuy(asks, decimal.MustFromString("5"))
	if !r.FullyFilled {
		t.Error("expected full fill")
	}
	if !r.VWAP.Equal(decimal.MustFromString("100.00")) {
		t.Errorf("VWAP=%s, want 100.00", r.VWAP.String())
	}
	if !r.FilledQty.Equal(decimal.MustFromString("5")) {
		t.Errorf("filled=%s, want 5", r.FilledQty.String())
	}
	if !r.FilledValue.Equal(decimal.MustFromString("500.00")) {
		t.Errorf("value=%s, want 500.00", r.FilledValue.String())
	}
}

// TestVWAPMultiLevel — проход через два уровня.
// asks: 100.00×3, 101.00×10. Buy 5: 3@100 + 2@101 = 300+202 = 502 / 5 = 100.4
func TestVWAPMultiLevel(t *testing.T) {
	asks := []domain.PriceLevel{
		lvl("100.00", "3"),
		lvl("101.00", "10"),
	}
	r := VWAPBuy(asks, decimal.MustFromString("5"))
	if !r.FullyFilled {
		t.Error("expected full fill")
	}
	wantVWAP := decimal.MustFromString("100.4")
	if !r.VWAP.Equal(wantVWAP) {
		t.Errorf("VWAP=%s, want %s", r.VWAP.String(), wantVWAP.String())
	}
	wantValue := decimal.MustFromString("502.00")
	if !r.FilledValue.Equal(wantValue) {
		t.Errorf("value=%s, want %s", r.FilledValue.String(), wantValue.String())
	}
}

// TestVWAPInsufficientDepth — объём больше глубины стакана.
// asks суммарно 4, запрашиваем 5 → partial fill.
func TestVWAPInsufficientDepth(t *testing.T) {
	asks := []domain.PriceLevel{
		lvl("100.00", "3"),
		lvl("101.00", "1"),
	}
	r := VWAPBuy(asks, decimal.MustFromString("5"))
	if r.FullyFilled {
		t.Error("should NOT be fully filled (thin book)")
	}
	if !r.FilledQty.Equal(decimal.MustFromString("4")) {
		t.Errorf("filled=%s, want 4", r.FilledQty.String())
	}
	// 3×100 + 1×101 = 401 / 4 = 100.25
	wantVWAP := decimal.MustFromString("100.25")
	if !r.VWAP.Equal(wantVWAP) {
		t.Errorf("VWAP=%s, want %s", r.VWAP.String(), wantVWAP.String())
	}
}

// TestVWAPSell — продажа через bids.
// bids: 100.00×5. Sell 3 → 3×100 = 300/3 = 100.
func TestVWAPSell(t *testing.T) {
	bids := []domain.PriceLevel{lvl("100.00", "5")}
	r := VWAPSell(bids, decimal.MustFromString("3"))
	if !r.FullyFilled {
		t.Error("expected full fill")
	}
	if !r.VWAP.Equal(decimal.MustFromString("100.00")) {
		t.Errorf("VWAP=%s, want 100", r.VWAP.String())
	}
}

// TestVWAPEmptyBook — пустой стакан.
func TestVWAPEmptyBook(t *testing.T) {
	r := VWAPBuy(nil, decimal.MustFromString("5"))
	if r.FullyFilled || !r.VWAP.IsZero() {
		t.Error("empty book should give zero VWAP, not filled")
	}
}

// TestVWAPZeroQty — нулевой запрос.
func TestVWAPZeroQty(t *testing.T) {
	asks := []domain.PriceLevel{lvl("100.00", "10")}
	r := VWAPBuy(asks, decimal.Zero)
	if r.FullyFilled || !r.VWAP.IsZero() {
		t.Error("zero qty should give zero result")
	}
}

// TestVWAPNegativeQty — отрицательный запрос отклоняется.
func TestVWAPNegativeQty(t *testing.T) {
	r := VWAPBuy([]domain.PriceLevel{lvl("100.00", "10")}, decimal.MustFromString("-5"))
	if r.FullyFilled || !r.VWAP.IsZero() {
		t.Error("negative qty should give zero result")
	}
}

// TestSlippageBps — отклонение VWAP от best в bps.
func TestSlippageBps(t *testing.T) {
	asks := []domain.PriceLevel{
		lvl("100.00", "1"),
		lvl("101.00", "9"), // 9 уровней по 101
	}
	r := VWAPBuy(asks, decimal.MustFromString("10"))
	// VWAP = (1×100 + 9×101)/10 = (100+909)/10 = 100.9
	// best = 100.00, |100.9 - 100|/100 × 10000 = 90 bps
	if !r.VWAP.Equal(decimal.MustFromString("100.9")) {
		t.Fatalf("VWAP=%s, want 100.9", r.VWAP.String())
	}
	slippage := r.SlippageBps(decimal.MustFromString("100.00"))
	want := decimal.MustFromString("90")
	if !slippage.Equal(want) {
		t.Errorf("slippage=%s bps, want %s", slippage.String(), want.String())
	}
}

// TestEntryBasis — спред входа short bid / long ask.
// short bids лучше (дороже) → basis положительный (выгодный edge).
// long asks: 100×10. short bids: 101×10. qty=5.
// LongVWAP=100, ShortVWAP=101. Basis = 101/100 - 1 = 0.01 (1%)
func TestEntryBasis(t *testing.T) {
	longAsks := []domain.PriceLevel{lvl("100.00", "10")}
	shortBids := []domain.PriceLevel{lvl("101.00", "10")}
	basis, longVWAP, shortVWAP, ok := EntryBasis(longAsks, shortBids, decimal.MustFromString("5"))
	if !ok {
		t.Fatal("expected ok")
	}
	if !longVWAP.Equal(decimal.MustFromString("100")) {
		t.Errorf("longVWAP=%s, want 100", longVWAP.String())
	}
	if !shortVWAP.Equal(decimal.MustFromString("101")) {
		t.Errorf("shortVWAP=%s, want 101", shortVWAP.String())
	}
	wantBasis := decimal.MustFromString("0.01")
	if !basis.Equal(wantBasis) {
		t.Errorf("basis=%s, want %s", basis.String(), wantBasis.String())
	}
}

// TestEntryBasisAdverse — short дешевле long → basis отрицательный.
// long asks: 101×10. short bids: 100×10. Basis = 100/101 - 1 ≈ -0.0099
func TestEntryBasisAdverse(t *testing.T) {
	longAsks := []domain.PriceLevel{lvl("101.00", "10")}
	shortBids := []domain.PriceLevel{lvl("100.00", "10")}
	basis, _, _, ok := EntryBasis(longAsks, shortBids, decimal.MustFromString("5"))
	if !ok {
		t.Fatal("expected ok")
	}
	if !basis.IsNegative() {
		t.Errorf("basis=%s, want negative", basis.String())
	}
}

// TestEntryBasisThinBook — один из стаканов тонкий → ok=false.
func TestEntryBasisThinBook(t *testing.T) {
	longAsks := []domain.PriceLevel{lvl("100.00", "1")} // мало
	shortBids := []domain.PriceLevel{lvl("101.00", "10")}
	_, _, _, ok := EntryBasis(longAsks, shortBids, decimal.MustFromString("5"))
	if ok {
		t.Error("expected not ok when long side thin")
	}
}

// TestExitBasis — спред выхода: продаём long (bids), выкупаем short (asks).
func TestExitBasis(t *testing.T) {
	longBids := []domain.PriceLevel{lvl("105.00", "10")}
	shortAsks := []domain.PriceLevel{lvl("100.00", "10")}
	basis, longExit, shortExit, ok := ExitBasis(longBids, shortAsks, decimal.MustFromString("5"))
	if !ok {
		t.Fatal("expected ok")
	}
	if !longExit.Equal(decimal.MustFromString("105")) {
		t.Errorf("longExit=%s, want 105", longExit.String())
	}
	if !shortExit.Equal(decimal.MustFromString("100")) {
		t.Errorf("shortExit=%s, want 100", shortExit.String())
	}
	// 105/100 - 1 = 0.05 (5%)
	want := decimal.MustFromString("0.05")
	if !basis.Equal(want) {
		t.Errorf("exit basis=%s, want %s", basis.String(), want.String())
	}
}

// TestCheckDepthSufficient — быстрая проверка без VWAP.
func TestCheckDepthSufficient(t *testing.T) {
	levels := []domain.PriceLevel{
		lvl("100", "3"),
		lvl("101", "2"),
	}
	if !CheckDepthSufficient(levels, decimal.MustFromString("5")) {
		t.Error("depth 5 should be sufficient")
	}
	if CheckDepthSufficient(levels, decimal.MustFromString("6")) {
		t.Error("depth 6 should NOT be sufficient")
	}
	if !CheckDepthSufficient(levels, decimal.MustFromString("1")) {
		t.Error("depth 1 should be sufficient")
	}
}
