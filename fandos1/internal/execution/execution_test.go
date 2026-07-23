package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
	mockexchange "github.com/thecd/fundarbitrage/internal/exchange/mock"
)

const testSym = domain.ExchangeSymbol("BTCUSDT")

// ============================================================
// client_order_id tests
// ============================================================

func TestClientOrderIDFormatParse(t *testing.T) {
	parts := ClientOrderIDParts{
		PositionID: "pos-abc",
		LegSide:    domain.SideLong,
		SliceIndex: 3,
		Nonce:      1,
		Purpose:    "ENTRY",
	}
	id := Format(parts)
	want := "ENTRY:POS-ABC:LONG:3:1"
	if string(id) != want {
		t.Errorf("Format = %q, want %q", id, want)
	}
	parsed, ok := Parse(id)
	if !ok {
		t.Fatal("Parse failed")
	}
	// Parse возвращает uppercased positionID (POS-ABC), сравниваем с sanitized.
	if string(parsed.PositionID) != "POS-ABC" {
		t.Errorf("position id = %q, want POS-ABC", parsed.PositionID)
	}
	if parsed.LegSide != parts.LegSide ||
		parsed.SliceIndex != parts.SliceIndex || parsed.Nonce != parts.Nonce ||
		parsed.Purpose != parts.Purpose {
		t.Errorf("Parse mismatch: %+v vs %+v", parsed, parts)
	}
}

func TestClientOrderIDValidateLength(t *testing.T) {
	// Слишком длинный → невалиден.
	long := ClientOrderIDParts{
		PositionID: domain.PositionID("very-long-position-id-that-exceeds-thirty-two-chars-xxx"),
	}
	id := Format(long)
	if ValidateForExchange(id) {
		t.Error("expected invalid for long id")
	}
	// Короткий → валиден.
	short := Format(ClientOrderIDParts{PositionID: "p", LegSide: domain.SideLong, SliceIndex: 0, Nonce: 0, Purpose: "E"})
	if !ValidateForExchange(short) {
		t.Errorf("short id %q should be valid", short)
	}
}

func TestClientOrderIDRejectBadChars(t *testing.T) {
	// Прямой lowercase (минуя Format, который uppercas-ит) → должен быть отклонён.
	if ValidateForExchange("entry:pos:long:0:0") {
		t.Error("lowercase should be rejected by validator")
	}
	if ValidateForExchange("ENTRY POS LONG 0 0") {
		t.Error("spaces should be rejected")
	}
}

func TestParseRejectsForeignFormat(t *testing.T) {
	// Не наш формат — нет двоеточий.
	_, ok := Parse("manual-order-123")
	if ok {
		t.Error("foreign format should not parse")
	}
}

// ============================================================
// order_executor tests (QUERY_THEN_DECIDE)
// ============================================================

func setupMock(t *testing.T, fillFraction string) (*OrderExecutor, *mockexchange.Mock) {
	t.Helper()
	m := mockexchange.New(domain.ExchangeBinance)
	m.SetOrderBook(testSym,
		[]domain.PriceLevel{{Price: decimal.MustFromString("100"), Qty: decimal.MustFromString("10")}},
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.5"), Qty: decimal.MustFromString("10")}},
	)
	m.SetFillRule(testSym, mockexchange.FillRule{FillFraction: decimal.MustFromString(fillFraction)})
	exec := NewOrderExecutor(m, 1*time.Second)
	return exec, m
}

func TestPlaceSuccessViaAck(t *testing.T) {
	exec, _ := setupMock(t, "1")
	res, err := exec.Place(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "ENTRY-p1-LONG-0-0",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("5"),
		Price:         decimal.MustFromString("100.5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceAck {
		t.Errorf("source = %s, want ack", res.Source)
	}
	if res.Order.Status != domain.OrderStatusFilled {
		t.Errorf("status = %s, want filled", res.Order.Status)
	}
	if res.Order.AckState != domain.AckStateAcked {
		t.Errorf("ack state = %s, want acked", res.Order.AckState)
	}
}

func TestPlaceAckTimeoutRecoveredViaQuery(t *testing.T) {
	exec, m := setupMock(t, "1")
	m.AckTimeoutFor(testSym) // PlaceOrder вернёт ErrTimeout, ордер создан.

	res, err := exec.Place(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "ENTRY-p1-LONG-0-0",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("5"),
		Price:         decimal.MustFromString("100.5"),
	})
	if err != nil {
		t.Fatalf("expected recovery via query, got error: %v", err)
	}
	// Состояние восстановлено через query.
	if res.Source != SourceQuery {
		t.Errorf("source = %s, want query (recovery)", res.Source)
	}
	if res.Order.AckState != domain.AckStateQueried {
		t.Errorf("ack state = %s, want queried", res.Order.AckState)
	}
	if res.Order.Status != domain.OrderStatusFilled {
		t.Errorf("status = %s, want filled (order exists despite timeout)", res.Order.Status)
	}
}

func TestPlaceRateLimitedNoRecovery(t *testing.T) {
	m := mockexchange.New(domain.ExchangeBinance)
	m.SetRateLimited(true)
	exec := NewOrderExecutor(m, 1*time.Second)
	_, err := exec.Place(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "ENTRY-p1-LONG-0-0",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("5"),
	})
	if !errors.Is(err, exchange.ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
}

func TestIsSafeRetry(t *testing.T) {
	if !IsSafeRetry(exchange.ErrOrderNotFound) {
		t.Error("ErrOrderNotFound should be safe retry")
	}
	if IsSafeRetry(exchange.ErrTimeout) {
		t.Error("ErrTimeout should NOT be safe retry (ambiguous)")
	}
	if IsSafeRetry(exchange.ErrRateLimited) {
		t.Error("ErrRateLimited should not be safe retry")
	}
}

func TestIsAmbiguousTimeout(t *testing.T) {
	if !IsAmbiguousTimeout(exchange.ErrTimeout) {
		t.Error("ErrTimeout is ambiguous")
	}
	if !IsAmbiguousTimeout(exchange.ErrNetwork) {
		t.Error("ErrNetwork is ambiguous")
	}
	if IsAmbiguousTimeout(exchange.ErrRateLimited) {
		t.Error("ErrRateLimited is NOT ambiguous")
	}
}

// ============================================================
// repair tests (сценарий 60/50, раздел 10.3)
// ============================================================

func TestAnalyzeMismatchHedged(t *testing.T) {
	dec := AnalyzeMismatch(decimal.MustFromString("50"), decimal.MustFromString("50"), decimal.MustFromString("1"))
	if dec.Action != RepairNone {
		t.Errorf("action = %s, want none", dec.Action)
	}
}

func TestAnalyzeMismatchShortUnderfilled(t *testing.T) {
	// Long=60, Short=50 → short недостаёт 10.
	dec := AnalyzeMismatch(decimal.MustFromString("60"), decimal.MustFromString("50"), decimal.MustFromString("1"))
	if dec.Action != RepairTopUpShortLeg {
		t.Errorf("action = %s, want topup", dec.Action)
	}
	if !dec.ShortfallQty.Equal(decimal.MustFromString("10")) {
		t.Errorf("shortfall = %s, want 10", dec.ShortfallQty.String())
	}
	if !dec.CommonQty.Equal(decimal.MustFromString("50")) {
		t.Errorf("common = %s, want 50", dec.CommonQty.String())
	}
}

func TestAnalyzeMismatchLongUnderfilled(t *testing.T) {
	// Long=50, Short=60 → long недостаёт 10.
	dec := AnalyzeMismatch(decimal.MustFromString("50"), decimal.MustFromString("60"), decimal.MustFromString("1"))
	if dec.Action != RepairTopUpShortLeg {
		t.Errorf("action = %s, want topup", dec.Action)
	}
	if !dec.ShortfallQty.Equal(decimal.MustFromString("10")) {
		t.Errorf("shortfall = %s, want 10", dec.ShortfallQty.String())
	}
}

func TestApplyTopUpSuccess(t *testing.T) {
	dec := AnalyzeMismatch(decimal.MustFromString("60"), decimal.MustFromString("50"), decimal.MustFromString("1"))
	// Top-up на 10 короткой → 60/60.
	res := ApplyTopUp(dec, TopUpResult{
		Success: true, FilledQty: decimal.MustFromString("10"),
		NewLongQty: decimal.MustFromString("60"), NewShortQty: decimal.MustFromString("60"),
	}, decimal.MustFromString("1"))
	if res.Action != RepairNone {
		t.Errorf("action = %s, want none (balanced)", res.Action)
	}
}

func TestApplyTopUpFailedReduceExcess(t *testing.T) {
	dec := AnalyzeMismatch(decimal.MustFromString("60"), decimal.MustFromString("50"), decimal.MustFromString("1"))
	// Top-up не сработал → закрыть избыток 10.
	res := ApplyTopUp(dec, TopUpResult{
		Success: false, FilledQty: decimal.Zero,
		NewLongQty: decimal.MustFromString("60"), NewShortQty: decimal.MustFromString("50"),
	}, decimal.MustFromString("1"))
	if res.Action != RepairReduceExcess {
		t.Errorf("action = %s, want reduce_excess", res.Action)
	}
	if !res.ExcessQty.Equal(decimal.MustFromString("10")) {
		t.Errorf("excess = %s, want 10", res.ExcessQty.String())
	}
}

func TestApplyTopUpPartial(t *testing.T) {
	dec := AnalyzeMismatch(decimal.MustFromString("60"), decimal.MustFromString("50"), decimal.MustFromString("1"))
	// Top-up только 4 из 10 → дельта 60/54 = 6, избыток 6.
	res := ApplyTopUp(dec, TopUpResult{
		Success: true, FilledQty: decimal.MustFromString("4"),
		NewLongQty: decimal.MustFromString("60"), NewShortQty: decimal.MustFromString("54"),
	}, decimal.MustFromString("1"))
	if res.Action != RepairReduceExcess {
		t.Errorf("action = %s, want reduce_excess", res.Action)
	}
	if !res.ExcessQty.Equal(decimal.MustFromString("6")) {
		t.Errorf("excess = %s, want 6", res.ExcessQty.String())
	}
}

func TestReduceExcessActionLongExcess(t *testing.T) {
	// Long-excess → закрыть часть long = продать = SideShort.
	if got := ReduceExcessAction(domain.SideLong); got != domain.SideShort {
		t.Errorf("long excess → %s, want SHORT", got)
	}
}

func TestReduceExcessActionShortExcess(t *testing.T) {
	// Short-excess → закрыть часть short = выкупить = SideLong.
	if got := ReduceExcessAction(domain.SideShort); got != domain.SideLong {
		t.Errorf("short excess → %s, want LONG", got)
	}
}

// TestFull60To50Scenario — end-to-end: 60 long / 50 short → top-up 10 → balanced.
func TestFull60To50Scenario(t *testing.T) {
	exec, m := setupMock(t, "1")
	m.SetFillRule(testSym, mockexchange.FillRule{FillFraction: decimal.One})

	// Анализируем mismatch.
	dec := AnalyzeMismatch(decimal.MustFromString("60"), decimal.MustFromString("50"), decimal.MustFromString("1"))
	if dec.Action != RepairTopUpShortLeg {
		t.Fatal("expected top-up decision")
	}
	// Выполняем top-up через executor.
	res, err := exec.Place(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "REPAIR-p1-SHORT-0-1",
		Symbol:        testSym,
		Side:          domain.SideShort,
		BaseQty:       dec.ShortfallQty, // 10
		Price:         decimal.MustFromString("100"),
	})
	if err != nil {
		t.Fatal(err)
	}
	topUp := TopUpResult{
		Success: res.Order.Status == domain.OrderStatusFilled,
		FilledQty: res.Order.FilledQty,
		NewLongQty: decimal.MustFromString("60"),
		NewShortQty: decimal.MustFromString("50").Add(res.Order.FilledQty),
	}
	final := ApplyTopUp(dec, topUp, decimal.MustFromString("1"))
	if final.Action != RepairNone {
		t.Errorf("expected balanced after top-up, action=%s", final.Action)
	}
}
