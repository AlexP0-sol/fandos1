// Package repository реализует слой персистентности на PostgreSQL (pgx/v5).
// Каждый репозиторий принимает *pgxpool.Pool; пул создаётся один раз в NewPool.
package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thecd/fundarbitrage/internal/decimal"
)

// NewPool создаёт пул соединений с разумными дефолтами и проверяет Ping.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("repository: разбор DSN: %w", err)
	}

	// Разумные дефолты для production-пула.
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("repository: создание пула: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("repository: ping БД: %w", err)
	}

	return pool, nil
}

// decimalToNumeric конвертирует decimal.Decimal в строку для PostgreSQL NUMERIC.
// Никогда не использует float64 — только строковое представление.
func decimalToNumeric(d decimal.Decimal) string {
	return d.String()
}

// numericToDecimal сканирует строку PostgreSQL NUMERIC в decimal.Decimal.
// pgx/v5 возвращает NUMERIC как pgtype.Numeric; мы читаем через TextScanner.
func numericToDecimal(s string) (decimal.Decimal, error) {
	d, err := decimal.FromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("repository: разбор NUMERIC %q: %w", s, err)
	}
	return d, nil
}
