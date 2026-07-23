package execution

import (
	"context"
	"errors"
	"fmt"
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
		Purpose:    PurposeEntry,
	}
	id := Format(parts)
	// Ожидаемый формат: ENTRY_POS-ABC_LONG_3_1 (разделитель '_', компоненты [A-Z0-9-])
	want := "ENTRY_POS-ABC_LONG_3_1"
	if string(id) != want {
		t.Errorf("Format = %q, want %q", id, want)
	}

	parsed, ok := Parse(id)
	if !ok {
		t.Fatal("Parse failed")
	}
	// Parse возвращает sanitized positionID (POS-ABC), сравниваем с sanitized.
	if string(parsed.PositionID) != "POS-ABC" {
		t.Errorf("position id = %q, want POS-ABC", parsed.PositionID)
	}
	if parsed.LegSide != parts.LegSide ||
		parsed.SliceIndex != parts.SliceIndex || parsed.Nonce != parts.Nonce ||
		parsed.Purpose != parts.Purpose {
		t.Errorf("Parse mismatch: %+v vs %+v", parsed, parts)
	}
}

// TestClientOrderIDRoundTrip — Parse(Format(x)) возвращает санированные значения.
func TestClientOrderIDRoundTrip(t *testing.T) {
	parts := ClientOrderIDParts{
		PositionID: "pos_abc:raw!123", // содержит символы вне [A-Z0-9-]
		LegSide:    domain.SideShort,
		SliceIndex: 0,
		Nonce:      0,
		Purpose:    PurposeRepair,
	}
	id := Format(parts)
	parsed, ok := Parse(id)
	if !ok {
		t.Fatalf("Parse(%q) failed", id)
	}
	// Санированные значения после round-trip.
	// positionID "pos_abc:raw!123" → [A-Z0-9-] → "POSABC123"
	wantPos := sanitizeComponent(string(parts.PositionID))
	if string(parsed.PositionID) != wantPos {
		t.Errorf("round-trip positionID = %q, want sanitized %q", parsed.PositionID, wantPos)
	}
	if parsed.Purpose != parts.Purpose {
		t.Errorf("round-trip purpose = %q, want %q", parsed.Purpose, parts.Purpose)
	}
}

func TestClientOrderIDValidateLength(t *testing.T) {
	// Слишком длинный → невалиден. Сначала убеждаемся, что id действительно длинный.
	long := ClientOrderIDParts{
		PositionID: domain.PositionID("very-long-position-id-that-exceeds-thirty-two-chars-xxx"),
		Purpose:    PurposeEntry,
	}
	id := Format(long)
	// Гарантируем, что наш Format обрезает и результат укладывается в MaxLength.
	// (Если это изменится — тест сигнализирует.)
	if len(string(id)) > MaxLength {
		t.Errorf("Format должен обрезать до MaxLength=%d, но длина %d: %q", MaxLength, len(string(id)), id)
	}

	// Проверяем: вручную созданный слишком длинный id должен быть отклонён.
	tooLong := domain.ClientOrderID("ENTRY_VERY-LONG-POSITION-ID-THAT-EXCEEDS-32-CHARS_LONG_0_0")
	if len(string(tooLong)) <= MaxLength {
		t.Skipf("тестовый id не превышает MaxLength — нужно скорректировать строку")
	}
	t.Logf("tooLong id (len=%d): %q", len(string(tooLong)), tooLong)
	if Validate(tooLong) {
		t.Error("ожидался invalid для слишком длинного id")
	}

	// Короткий → валиден.
	short := Format(ClientOrderIDParts{
		PositionID: "p",
		LegSide:    domain.SideLong,
		SliceIndex: 0,
		Nonce:      0,
		Purpose:    PurposeEntry,
	})
	t.Logf("short id: %q", short)
	if !Validate(short) {
		t.Errorf("short id %q should be valid", short)
	}
}

func TestClientOrderIDRejectBadChars(t *testing.T) {
	// Прямые строки с недопустимыми символами — не наш формат; ни Validate, ни Bybit не пропустят.
	if Validate(domain.ClientOrderID("entry:pos:long:0:0")) {
		t.Error("colon should be rejected by Validate")
	}
	if Validate(domain.ClientOrderID("ENTRY POS LONG 0 0")) {
		t.Error("spaces should be rejected")
	}
	if ValidateForExchange(domain.ExchangeBybit, "entry:pos:long:0:0") {
		t.Error("bybit should reject colon")
	}
}

func TestParseRejectsForeignFormat(t *testing.T) {
	// Не наш формат — нет '_' с нужной структурой.
	_, ok := Parse("manual-order-123")
	if ok {
		t.Error("foreign format should not parse")
	}
	// Слишком мало полей
	_, ok = Parse("ENTRY_POS")
	if ok {
		t.Error("too few fields should not parse")
	}
}

// TestClientOrderIDValidateForExchangeBothPass — наши сгенерированные id
// проходят валидацию для Binance И Bybit.
func TestClientOrderIDValidateForExchangeBothPass(t *testing.T) {
	id := Format(ClientOrderIDParts{
		PositionID: "pos-1",
		LegSide:    domain.SideLong,
		SliceIndex: 2,
		Nonce:      0,
		Purpose:    PurposeEntry,
	})
	t.Logf("generated id: %q (len=%d)", id, len(string(id)))

	if !ValidateForExchange(domain.ExchangeBinance, id) {
		t.Errorf("generated id %q should pass Binance validator", id)
	}
	if !ValidateForExchange(domain.ExchangeBybit, id) {
		t.Errorf("generated id %q should pass Bybit validator", id)
	}
}

// TestValidateForExchangeBybitRejectsColon — Bybit не принимает двоеточие.
func TestValidateForExchangeBybitRejectsColon(t *testing.T) {
	withColon := domain.ClientOrderID("ENTRY:POS:LONG:0:0")
	if ValidateForExchange(domain.ExchangeBybit, withColon) {
		t.Error("bybit validator should reject colon separator")
	}
}

// TestValidateForExchangeBinanceAcceptsColon — Binance принимает двоеточие (в charset [A-Za-z0-9._:/-]).
func TestValidateForExchangeBinanceAcceptsColon(t *testing.T) {
	withColon := domain.ClientOrderID("ENTRY:POS:LONG:0:0")
	if !ValidateForExchange(domain.ExchangeBinance, withColon) {
		t.Error("binance validator should accept colon")
	}
}

// TestEnsureValidPanicsOnInvalidID — EnsureValid должен паниковать на невалидном id.
func TestEnsureValidPanicsOnInvalidID(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("EnsureValid должен паниковать при невалидном id, но не запаниковал")
		}
	}()
	// Содержит пробел — не пройдёт внутренний Validate.
	EnsureValid(domain.ClientOrderID("ENTRY POS LONG 0 0"))
}

// TestEnsureValidNopanicOnValidID — EnsureValid не должен паниковать на валидном id.
func TestEnsureValidNopanicOnValidID(t *testing.T) {
	id := Format(ClientOrderIDParts{
		PositionID: "p1",
		LegSide:    domain.SideLong,
		SliceIndex: 0,
		Nonce:      0,
		Purpose:    PurposeEntry,
	})
	// Не должен паниковать.
	EnsureValid(id)
}

// TestPurposeConstants — EXIT и EMERGENCY константы существуют с ожидаемыми значениями
// (параллельная перепись close.go использует именно эти строки).
func TestPurposeConstants(t *testing.T) {
	if string(PurposeExit) != "EXIT" {
		t.Errorf("PurposeExit = %q, want EXIT", PurposeExit)
	}
	if string(PurposeEmergency) != "EMERGENCY" {
		t.Errorf("PurposeEmergency = %q, want EMERGENCY", PurposeEmergency)
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
		ClientOrderID: "ENTRY-P1-LONG-0-0",
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
		ClientOrderID: "ENTRY-P1-LONG-0-0",
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
		ClientOrderID: "ENTRY-P1-LONG-0-0",
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

// TestPlaceAckOkQueryRejected — ack успешен, но GetOrder возвращает REJECTED.
// Source должен быть SourceQuery, статус — Rejected.
func TestPlaceAckOkQueryRejected(t *testing.T) {
	m := mockexchange.New(domain.ExchangeBinance)
	m.SetOrderBook(testSym,
		[]domain.PriceLevel{{Price: decimal.MustFromString("100"), Qty: decimal.MustFromString("10")}},
		[]domain.PriceLevel{{Price: decimal.MustFromString("100.5"), Qty: decimal.MustFromString("10")}},
	)
	// Настраиваем reject-сценарий: ордер будет принят (ack ok), но статус — Rejected.
	m.SetFillRule(testSym, mockexchange.FillRule{Reject: true})
	exec := NewOrderExecutor(m, 1*time.Second)

	res, err := exec.Place(context.Background(), domain.PlaceOrderRequest{
		ClientOrderID: "ENTRY-P1-LONG-0-0",
		Symbol:        testSym,
		Side:          domain.SideLong,
		BaseQty:       decimal.MustFromString("5"),
		Price:         decimal.MustFromString("100.5"),
	})
	// Ошибки нет — ack был успешным, состояние получено из query.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Source != SourceQuery {
		t.Errorf("source = %s, want query (ack ok but query shows rejected)", res.Source)
	}
	if res.Order.Status != domain.OrderStatusRejected {
		t.Errorf("status = %s, want rejected", res.Order.Status)
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

// TestApplyTopUpSuccessTrueButFilledZero — противоречивый вход: Success=true, FilledQty=0.
// Должен обрабатываться как failure (защитная логика).
func TestApplyTopUpSuccessTrueButFilledZero(t *testing.T) {
	dec := AnalyzeMismatch(decimal.MustFromString("60"), decimal.MustFromString("50"), decimal.MustFromString("1"))
	// Биржа подтвердила успех, но ничего не исполнила — противоречие.
	res := ApplyTopUp(dec, TopUpResult{
		Success:     true,
		FilledQty:   decimal.Zero, // противоречит Success=true
		NewLongQty:  decimal.MustFromString("60"),
		NewShortQty: decimal.MustFromString("50"),
	}, decimal.MustFromString("1"))
	// Должен трактоваться как неудача → reduce excess.
	if res.Action != RepairReduceExcess {
		t.Errorf("Success=true && FilledQty=0 должен давать RepairReduceExcess, got %s", res.Action)
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

// TestReduceExcessActionSymmetry — ReduceExcessAction инвертирует сторону.
func TestReduceExcessActionSymmetry(t *testing.T) {
	sides := []domain.Side{domain.SideLong, domain.SideShort}
	for _, s := range sides {
		out := ReduceExcessAction(s)
		if out == s {
			t.Errorf("ReduceExcessAction(%s) должен вернуть противоположную сторону, got %s", s, out)
		}
		// Двойная инверсия == исходная.
		if ReduceExcessAction(out) != s {
			t.Errorf("двойная инверсия %s → %s → %s, ожидается %s", s, out, ReduceExcessAction(out), s)
		}
	}
}

// TestPlaceReduceOrderMarketMode — PlaceReduceOrder использует OrderMarket, reduce-only, IOC,
// сторону из req.Side.
func TestPlaceReduceOrderMarketMode(t *testing.T) {
	exec, m := setupMock(t, "1")

	req := ReduceExcessRequest{
		Symbol:    testSym,
		ExcessQty: decimal.MustFromString("10"),
		Side:      domain.SideShort, // long-excess → продаём
	}
	clientID := domain.ClientOrderID("REPAIR_P1_SHORT_0_1")

	err := PlaceReduceOrder(context.Background(), exec, req, clientID)
	if err != nil {
		t.Fatalf("PlaceReduceOrder error: %v", err)
	}

	// Проверяем, что ордер создан с корректными параметрами.
	o, ok := m.OrderByClient(clientID)
	if !ok {
		t.Fatal("ордер не найден в mock после PlaceReduceOrder")
	}
	if o.OrderMode != domain.OrderMarket {
		t.Errorf("OrderMode = %s, want market", o.OrderMode)
	}
	if !o.ReduceOnly {
		t.Error("ReduceOnly должен быть true")
	}
	if o.Side != domain.SideShort {
		t.Errorf("Side = %s, want short", o.Side)
	}
	if !o.RequestedQty.Equal(req.ExcessQty) {
		t.Errorf("RequestedQty = %s, want %s", o.RequestedQty.String(), req.ExcessQty.String())
	}
}

// TestPlaceReduceOrderZeroExcessIsNoop — PlaceReduceOrder с ExcessQty=0 ничего не отправляет.
func TestPlaceReduceOrderZeroExcessIsNoop(t *testing.T) {
	exec, m := setupMock(t, "1")
	req := ReduceExcessRequest{
		Symbol:    testSym,
		ExcessQty: decimal.Zero,
		Side:      domain.SideShort,
	}
	clientID := domain.ClientOrderID("REPAIR_P1_SHORT_0_2")
	err := PlaceReduceOrder(context.Background(), exec, req, clientID)
	if err != nil {
		t.Fatalf("unexpected error for zero qty: %v", err)
	}
	_, ok := m.OrderByClient(clientID)
	if ok {
		t.Error("ордер не должен был быть создан при ExcessQty=0")
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
		ClientOrderID: "REPAIR-P1-SHORT-0-1",
		Symbol:        testSym,
		Side:          domain.SideShort,
		BaseQty:       dec.ShortfallQty, // 10
		Price:         decimal.MustFromString("100"),
	})
	if err != nil {
		t.Fatal(err)
	}
	topUp := TopUpResult{
		Success:     res.Order.Status == domain.OrderStatusFilled,
		FilledQty:   res.Order.FilledQty,
		NewLongQty:  decimal.MustFromString("60"),
		NewShortQty: decimal.MustFromString("50").Add(res.Order.FilledQty),
	}
	final := ApplyTopUp(dec, topUp, decimal.MustFromString("1"))
	if final.Action != RepairNone {
		t.Errorf("expected balanced after top-up, action=%s", final.Action)
	}
}

// TestClientOrderIDNoColonInFormat — сгенерированные id не содержат двоеточий
// (Bybit отклоняет ':').
func TestClientOrderIDNoColonInFormat(t *testing.T) {
	purposes := []Purpose{PurposeEntry, PurposeExit, PurposeRepair, PurposeEmergency}
	sides := []domain.LegSide{domain.SideLong, domain.SideShort}
	for _, p := range purposes {
		for _, s := range sides {
			id := Format(ClientOrderIDParts{
				PositionID: "pos-123",
				LegSide:    s,
				SliceIndex: 1,
				Nonce:      2,
				Purpose:    p,
			})
			idStr := string(id)
			for i, c := range idStr {
				if c == ':' {
					t.Errorf("id содержит ':' на позиции %d: %q (purpose=%s side=%s)", i, idStr, p, s)
				}
			}
			// Дополнительно: проверяем через Bybit validator.
			if !ValidateForExchange(domain.ExchangeBybit, id) {
				t.Errorf("id %q не прошёл Bybit validator (purpose=%s side=%s)", idStr, p, s)
			}
		}
	}
}

// Verify fmt package is used (for t.Logf / t.Errorf formatting already used above).
var _ = fmt.Sprintf
