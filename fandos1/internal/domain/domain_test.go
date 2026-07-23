package domain

import (
	"testing"
	"time"
)

// TestSideSign — Sign возвращает +1 для long, -1 для short.
func TestSideSign(t *testing.T) {
	if SideLong.Sign() != 1 {
		t.Errorf("SideLong.Sign() = %d, want 1", SideLong.Sign())
	}
	if SideShort.Sign() != -1 {
		t.Errorf("SideShort.Sign() = %d, want -1", SideShort.Sign())
	}
}

// TestSideSignPanic — Sign паникует при невалидном значении.
func TestSideSignPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for invalid Side, got none")
		}
	}()
	invalid := Side("unknown")
	_ = invalid.Sign() // должен паниковать
}

// TestConfidenceLevelAtLeast — AtLeast проверяет порог уверенности.
func TestConfidenceLevelAtLeast(t *testing.T) {
	tests := []struct {
		c    ConfidenceLevel
		min  ConfidenceLevel
		want bool
	}{
		{ConfidenceHigh, ConfidenceHigh, true},
		{ConfidenceHigh, ConfidenceMedium, true},
		{ConfidenceHigh, ConfidenceLow, true},
		{ConfidenceHigh, ConfidenceNone, true},
		{ConfidenceMedium, ConfidenceHigh, false},
		{ConfidenceLow, ConfidenceMedium, false},
		{ConfidenceNone, ConfidenceLow, false},
	}
	for _, tc := range tests {
		got := tc.c.AtLeast(tc.min)
		if got != tc.want {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", tc.c, tc.min, got, tc.want)
		}
	}
}

// TestIsStale — проверяет все ветки IsStale.
func TestIsStale(t *testing.T) {
	now := time.Now()
	maxAge := 5 * time.Second

	// Свежий снимок — не устаревший.
	fresh := &MarketSnapshot{
		IsFresh:          true,
		LocalReceiveTime: now.Add(-1 * time.Second),
	}
	if fresh.IsStale(now, maxAge) {
		t.Error("свежий снимок (1s старый, maxAge=5s) не должен быть устаревшим")
	}

	// Старый снимок (возраст > maxAge).
	stale := &MarketSnapshot{
		IsFresh:          true,
		LocalReceiveTime: now.Add(-10 * time.Second),
	}
	if !stale.IsStale(now, maxAge) {
		t.Error("снимок возрастом 10s при maxAge=5s должен быть устаревшим")
	}

	// IsFresh=false — всегда устаревший независимо от возраста.
	notFreshFlag := &MarketSnapshot{
		IsFresh:          false,
		LocalReceiveTime: now, // только что получен
	}
	if !notFreshFlag.IsStale(now, maxAge) {
		t.Error("снимок с IsFresh=false должен быть устаревшим всегда")
	}
}

// TestOrderStatusIsTerminal — IsTerminal для всех статусов.
func TestOrderStatusIsTerminal(t *testing.T) {
	terminal := []OrderStatus{
		OrderStatusFilled,
		OrderStatusCancelled,
		OrderStatusRejected,
		OrderStatusExpired,
	}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%s должен быть терминальным", s)
		}
	}

	nonTerminal := []OrderStatus{
		OrderStatusNew,
		OrderStatusAcknowledged,
		OrderStatusPartiallyFilled,
		OrderStatusNotFound,
	}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%s не должен быть терминальным", s)
		}
	}
}

// TestSupportedExchangesIsValidConsistency — SupportedExchanges и IsValid используют один источник истины.
func TestSupportedExchangesIsValidConsistency(t *testing.T) {
	all := SupportedExchanges()

	// Все из SupportedExchanges должны быть валидными.
	for _, ex := range all {
		if !ex.IsValid() {
			t.Errorf("биржа %q из SupportedExchanges не проходит IsValid()", ex)
		}
	}

	// Неизвестная биржа не должна проходить IsValid.
	unknown := ExchangeID("unknown_exchange")
	if unknown.IsValid() {
		t.Error("неизвестная биржа не должна быть валидной")
	}

	// SupportedExchanges возвращает копию (мутация не влияет на внутренний слайс).
	copy1 := SupportedExchanges()
	if len(copy1) == 0 {
		t.Fatal("SupportedExchanges вернул пустой слайс")
	}
	copy1[0] = ExchangeID("mutated")
	copy2 := SupportedExchanges()
	if copy2[0] == ExchangeID("mutated") {
		t.Error("мутация возвращённого слайса затронула внутренний список")
	}

	// Количество элементов в SupportedExchanges совпадает.
	if len(all) != len(supportedExchanges) {
		t.Errorf("длина SupportedExchanges()=%d не совпадает с внутренним слайсом %d",
			len(all), len(supportedExchanges))
	}
}
