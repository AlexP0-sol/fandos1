package config

import (
	"os"
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

// ── Новые тесты для всех добавленных проверок ──────────────────────────────

// TestHotValidateLeverageNotPositive — Leverage <= 0 отклоняется жёстко.
func TestHotValidateLeverageNotPositive(t *testing.T) {
	d := Defaults()
	d.Leverage = dec.Zero
	if _, err := d.Validate(); err == nil {
		t.Error("expected error for zero Leverage")
	}
	d.Leverage = dec.MustFromString("-1")
	if _, err := d.Validate(); err == nil {
		t.Error("expected error for negative Leverage")
	}
}

// TestHotValidateNegativeLosses — отрицательные лимиты убытков отклоняются.
func TestHotValidateNegativeLosses(t *testing.T) {
	d := Defaults()
	d.MaxDailyLossUSDT = dec.MustFromString("-1")
	if _, err := d.Validate(); err == nil {
		t.Error("expected error for negative MaxDailyLossUSDT")
	}

	d2 := Defaults()
	d2.MaxPositionLossUSDT = dec.MustFromString("-0.01")
	if _, err := d2.Validate(); err == nil {
		t.Error("expected error for negative MaxPositionLossUSDT")
	}
}

// TestHotValidateUnknownMarginMode — неизвестный MarginMode отклоняется.
func TestHotValidateUnknownMarginMode(t *testing.T) {
	d := Defaults()
	d.MarginMode = domain.MarginMode("portfolio")
	if _, err := d.Validate(); err == nil {
		t.Error("expected error for unknown MarginMode")
	}
}

// TestHotValidateUnknownPositionMode — неизвестный PositionMode отклоняется.
func TestHotValidateUnknownPositionMode(t *testing.T) {
	d := Defaults()
	d.PositionMode = domain.PositionMode("dual")
	if _, err := d.Validate(); err == nil {
		t.Error("expected error for unknown PositionMode")
	}
}

// TestHotValidateHighLeverageWarning — Leverage > 20 даёт предупреждение HIGH_LEVERAGE.
func TestHotValidateHighLeverageWarning(t *testing.T) {
	d := Defaults()
	d.MaxDailyLossUSDT = dec.MustFromString("100") // убираем NO_DAILY_STOP
	d.Leverage = dec.MustFromString("25")
	w, err := d.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasWarn(w, "HIGH_LEVERAGE") {
		t.Error("expected HIGH_LEVERAGE warning for Leverage=25")
	}
}

// TestLoadColdEnvIntParseError — DB_MAX_OPEN_CONNS задан, но не является числом.
func TestLoadColdEnvIntParseError(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("DB_MAX_OPEN_CONNS", "notanumber")
	if _, err := LoadCold(); err == nil {
		t.Fatal("expected error for unparseable DB_MAX_OPEN_CONNS")
	}
	os.Unsetenv("DB_MAX_OPEN_CONNS")
}

// TestLoadColdEnvDurParseError — SHUTDOWN_TIMEOUT задан, но не является duration.
func TestLoadColdEnvDurParseError(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("SHUTDOWN_TIMEOUT", "badvalue")
	if _, err := LoadCold(); err == nil {
		t.Fatal("expected error for unparseable SHUTDOWN_TIMEOUT")
	}
	os.Unsetenv("SHUTDOWN_TIMEOUT")
}

// TestLoadColdIdleConnsExceedOpen — DBMaxIdleConns > DBMaxOpenConns отклоняется.
func TestLoadColdIdleConnsExceedOpen(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("DB_MAX_OPEN_CONNS", "5")
	t.Setenv("DB_MAX_IDLE_CONNS", "10")
	defer os.Unsetenv("DB_MAX_OPEN_CONNS")
	defer os.Unsetenv("DB_MAX_IDLE_CONNS")
	if _, err := LoadCold(); err == nil {
		t.Fatal("expected error for idle > open conns")
	}
}

// TestLoadColdNonPositiveShutdownTimeout — SHUTDOWN_TIMEOUT=0 отклоняется.
func TestLoadColdNonPositiveShutdownTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("SHUTDOWN_TIMEOUT", "0s")
	defer os.Unsetenv("SHUTDOWN_TIMEOUT")
	if _, err := LoadCold(); err == nil {
		t.Fatal("expected error for SHUTDOWN_TIMEOUT=0")
	}
}

// TestLoadColdNonPositiveClockSyncInterval — CLOCK_SYNC_INTERVAL=0 отклоняется.
func TestLoadColdNonPositiveClockSyncInterval(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("CLOCK_SYNC_INTERVAL", "0s")
	defer os.Unsetenv("CLOCK_SYNC_INTERVAL")
	if _, err := LoadCold(); err == nil {
		t.Fatal("expected error for CLOCK_SYNC_INTERVAL=0")
	}
}
