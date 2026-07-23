// Package app собирает процесс из компонентов (DI-обвязка, раздел 15).
// Общий bootstrap для cmd/server и cmd/worker: конфиг → логгер → метрики →
// health → БД → репозитории → SAFE_HALT. Никакой бизнес-логики — только сборка.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thecd/fundarbitrage/internal/config"
	"github.com/thecd/fundarbitrage/internal/credentials"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/lifecycle"
	"github.com/thecd/fundarbitrage/internal/observability"
	"github.com/thecd/fundarbitrage/internal/repository"
)

// Bootstrap — собранное ядро процесса.
type Bootstrap struct {
	Cold    config.ColdConfig
	Log     *slog.Logger
	Metrics *observability.Registry
	App     *observability.AppMetrics
	Health  *observability.HealthChecker

	Pool      *pgxpool.Pool
	Positions *repository.PositionRepo
	Orders    *repository.OrderRepo
	Instrs    *repository.InstrumentRepo
	Settings  *repository.SettingsRepo
	Locks     *repository.LocksRepo
	Audit     *repository.AuditRepo

	Halter  *lifecycle.Halter
	SideLog *lifecycle.SideChannelLog
}

// New выполняет полный bootstrap. При недоступной БД возвращает ошибку —
// PostgreSQL является единственным источником истины (раздел 1.2.7),
// процесс без него не стартует.
func New(ctx context.Context) (*Bootstrap, error) {
	cold, err := config.LoadCold()
	if err != nil {
		return nil, fmt.Errorf("app: load cold config: %w", err)
	}

	level, lerr := observability.LogLevelFromString(cold.LogLevel)
	if lerr != nil {
		return nil, fmt.Errorf("app: log level: %w", lerr)
	}
	log := observability.NewLogger(os.Stdout, level, true)
	slog.SetDefault(log)

	reg := observability.NewRegistry()
	appMetrics := observability.NewAppMetrics(reg)
	health := observability.NewHealthChecker()

	pool, err := repository.NewPool(ctx, cold.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("app: connect postgres: %w", err)
	}

	sideLog := lifecycle.NewSideChannelLog("fandos-sidechannel.log")
	locks := repository.NewLocksRepo(pool)
	halter := lifecycle.NewHalter(locks, nil, sideLog)
	if err := halter.RestoreFromDB(ctx); err != nil {
		return nil, fmt.Errorf("app: restore SAFE_HALT: %w", err)
	}

	b := &Bootstrap{
		Cold:      *cold,
		Log:       log,
		Metrics:   reg,
		App:       appMetrics,
		Health:    health,
		Pool:      pool,
		Positions: repository.NewPositionRepo(pool),
		Orders:    repository.NewOrderRepo(pool),
		Instrs:    repository.NewInstrumentRepo(pool),
		Settings:  repository.NewSettingsRepo(pool),
		Locks:     locks,
		Audit:     repository.NewAuditRepo(),
		Halter:    halter,
		SideLog:   sideLog,
	}
	b.registerCoreHealthChecks()
	return b, nil
}

// registerCoreHealthChecks — базовые проверки /readyz (раздел 17.3).
func (b *Bootstrap) registerCoreHealthChecks() {
	b.Health.RegisterCheck("db", true, func(ctx context.Context) error {
		return b.Pool.Ping(ctx)
	})
	b.Health.RegisterCheck("master_key", b.Cold.RunMode == domain.RunModeLive, func(ctx context.Context) error {
		key, err := credentials.LoadMasterKey(b.Cold.MasterKeyEnv)
		if err != nil {
			return err
		}
		credentials.Zero(key)
		return nil
	})
	b.Health.RegisterCheck("safe_halt", false, func(ctx context.Context) error {
		if halted, reason := b.Halter.IsHalted(); halted {
			return fmt.Errorf("SAFE_HALT engaged: %s", reason)
		}
		return nil
	})
}

// EnsureSettingsSeeded гарантирует наличие singleton-строки strategy_settings:
// при первом старте пишутся значения по умолчанию (config.Defaults, version 1).
func (b *Bootstrap) EnsureSettingsSeeded(ctx context.Context) error {
	_, _, err := b.Settings.LoadHot(ctx)
	if err == nil {
		return nil
	}
	if err != repository.ErrSettingsNotFound {
		return fmt.Errorf("app: load settings: %w", err)
	}
	payload, merr := json.Marshal(settingsDTOFromHot(*config.Defaults()))
	if merr != nil {
		return fmt.Errorf("app: marshal default settings: %w", merr)
	}
	_, ierr := b.Pool.Exec(ctx,
		`INSERT INTO strategy_settings (singleton, version, payload) VALUES (TRUE, 1, $1)
		 ON CONFLICT (singleton) DO NOTHING`, payload)
	if ierr != nil {
		return fmt.Errorf("app: seed settings: %w", ierr)
	}
	b.Log.Info("strategy_settings seeded with defaults", "version", 1)
	return nil
}

// Preconditions — стартовые предусловия торговли (раздел 21, seed users).
// Возвращает список проверок; политику применения решает вызывающий:
// worker в dry_run логирует и продолжает наблюдение, в live — отказывается стартовать.
func (b *Bootstrap) Preconditions() []lifecycle.Precondition {
	return []lifecycle.Precondition{
		{Name: "owner_configured", Fn: func(ctx context.Context) error {
			ok, err := b.Locks.OwnerReady(ctx)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("owner telegram_id не настроен (seed -1): задайте владельца перед торговлей")
			}
			return nil
		}},
		{Name: "master_key", Fn: func(ctx context.Context) error {
			key, err := credentials.LoadMasterKey(b.Cold.MasterKeyEnv)
			if err != nil {
				return err
			}
			credentials.Zero(key)
			return nil
		}},
	}
}

// Close освобождает ресурсы.
func (b *Bootstrap) Close() {
	if b.Pool != nil {
		b.Pool.Close()
	}
}

// ShutdownTimeout — таймаут graceful shutdown из cold-конфига.
func (b *Bootstrap) ShutdownTimeout() time.Duration { return b.Cold.ShutdownTimeout }
