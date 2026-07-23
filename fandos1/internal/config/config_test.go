package config

import (
	"testing"

	dec "github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

func hasWarn(w []Warning, code string) bool {
	for _, w := range w {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestDefaultsAreSafe — defaults проходят валидацию без жёстких ошибок
// и не содержат опасных предупреждений (безопасные defaults — CONFIG_MODEL 1.4).
func TestDefaultsAreSafe(t *testing.T) {
	d := Defaults()
	w, err := d.Validate()
	if err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	// Допускается только NO_DAILY_STOP (default не задаёт MaxDailyLossUSDT).
	if len(w) != 1 || !hasWarn(w, "NO_DAILY_STOP") {
		t.Fatalf("defaults should have exactly NO_DAILY_STOP warning, got %v", w)
	}
}

// TestStoreSwapVersionGuard — устаревшая версия отклоняется (защита от stale reload).
func TestStoreSwapVersionGuard(t *testing.T) {
	d := Defaults()
	d.Version = 5
	s, _, err := NewStore(d)
	if err != nil {
		t.Fatal(err)
	}
	// Та же версия → stale, отклонить
	old := *d
	if _, err := s.Swap(&old); err == nil {
		t.Fatal("expected error for same version")
	}
	// Меньшая версия → отклонить
	older := *d
	older.Version = 3
	if _, err := s.Swap(&older); err == nil {
		t.Fatal("expected error for older version")
	}
	// Большая → принять
	newer := *d
	newer.Version = 6
	if _, err := s.Swap(&newer); err != nil {
		t.Fatalf("expected ok for newer version: %v", err)
	}
	if s.Current().Version != 6 {
		t.Errorf("current version = %d, want 6", s.Current().Version)
	}
}

// TestStoreSwapValidates — Swap отклоняет невалидные настройки.
func TestStoreSwapValidates(t *testing.T) {
	d := Defaults()
	d.Version = 1
	s, _, _ := NewStore(d)

	bad := *d
	bad.Version = 2
	bad.OrderAckTimeoutMs = 0 // невалидно
	if _, err := s.Swap(&bad); err == nil {
		t.Fatal("expected error for invalid settings")
	}
}

// TestAckTimeoutBehaviorStrict — только QUERY_THEN_DECIDE в v1 (раздел 5.3).
func TestAckTimeoutBehaviorStrict(t *testing.T) {
	d := Defaults()
	d.AckTimeoutBehavior = "BLIND_RETRY"
	if _, err := d.Validate(); err == nil {
		t.Fatal("expected error for non-QUERY_THEN_DECIDE behavior")
	}
}

// TestWarnings — опасные комбинации дают соответствующие предупреждения.
func TestWarnings(t *testing.T) {
	t.Run("MARKET_MODE", func(t *testing.T) {
		d := Defaults()
		d.OrderMode = domain.OrderMarket
		w, err := d.Validate()
		if err != nil {
			t.Fatal(err)
		}
		if !hasWarn(w, "MARKET_MODE") {
			t.Error("expected MARKET_MODE warning")
		}
	})
	t.Run("NO_RISK_SNAP", func(t *testing.T) {
		d := Defaults()
		d.RiskSnapAfterMaxDailyLoss = false
		w, _ := d.Validate()
		if !hasWarn(w, "NO_RISK_SNAP") {
			t.Error("expected NO_RISK_SNAP warning")
		}
	})
	t.Run("LATE_ENTRY", func(t *testing.T) {
		d := Defaults()
		d.MinSecondsBeforeFundingToEnter = 3
		w, _ := d.Validate()
		if !hasWarn(w, "LATE_ENTRY") {
			t.Error("expected LATE_ENTRY warning")
		}
	})
}

// TestLoadCold — загрузка COLD с defaults и валидацией.
func TestLoadCold(t *testing.T) {
	t.Run("missing_dsn", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "")
		if _, err := LoadCold(); err == nil {
			t.Fatal("expected error for missing DATABASE_URL")
		}
	})
	t.Run("ok", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://localhost/db")
		t.Setenv("RUN_MODE", "live")
		c, err := LoadCold()
		if err != nil {
			t.Fatal(err)
		}
		if c.RunMode != domain.RunModeLive {
			t.Errorf("RunMode = %s, want live", c.RunMode)
		}
	})
	t.Run("bad_runmode", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://localhost/db")
		t.Setenv("RUN_MODE", "production")
		if _, err := LoadCold(); err == nil {
			t.Fatal("expected error for unknown RUN_MODE")
		}
	})
}

// TestDefaultsConcurrency — Store безопасен для конкурентного чтения.
func TestDefaultsConcurrency(t *testing.T) {
	d := Defaults()
	d.MaxDailyLossUSDT = dec.MustFromString("100")
	d.Version = 1
	s, _, _ := NewStore(d)
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				_ = s.Current()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 50; i++ {
		<-done
	}
}
