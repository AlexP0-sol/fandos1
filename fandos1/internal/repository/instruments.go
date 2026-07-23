package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// InstrumentRepo — репозиторий биржевых инструментов.
type InstrumentRepo struct {
	pool *pgxpool.Pool
}

// NewInstrumentRepo создаёт InstrumentRepo.
func NewInstrumentRepo(pool *pgxpool.Pool) *InstrumentRepo {
	return &InstrumentRepo{pool: pool}
}

// Replace выполняет массовый upsert инструментов в одной транзакции.
// ON CONFLICT (exchange, exchange_symbol) DO UPDATE — обновляет все изменяемые поля.
// Инструменты, отсутствующие в переданном слайсе, НЕ удаляются.
func (r *InstrumentRepo) Replace(ctx context.Context, instruments []domain.CanonicalInstrument) error {
	if len(instruments) == 0 {
		return nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("repository: начало транзакции Replace instruments: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	now := time.Now().UTC()

	// Используем pgx.CopyFrom для bulk-вставки через COPY protocol (эффективно).
	// Однако ON CONFLICT не поддерживается COPY — применяем batch INSERT ... ON CONFLICT.

	batch := &pgx.Batch{}
	for _, inst := range instruments {
		// Статус из domain.InstrumentStatus в строку для БД.
		status := mapInstrumentStatus(inst.Status)

		batch.Queue(`
			INSERT INTO exchange_instruments (
				exchange, exchange_symbol, canonical_asset,
				quote_asset, instrument_type, status,
				contract_multiplier, qty_step, min_qty, tick_size,
				max_leverage, funding_interval_sec, refreshed_at
			) VALUES (
				$1, $2, $3,
				$4, $5, $6,
				$7, $8, $9, $10,
				$11, $12, $13
			)
			ON CONFLICT (exchange, exchange_symbol) DO UPDATE SET
				canonical_asset      = EXCLUDED.canonical_asset,
				status               = EXCLUDED.status,
				contract_multiplier  = EXCLUDED.contract_multiplier,
				qty_step             = EXCLUDED.qty_step,
				min_qty              = EXCLUDED.min_qty,
				tick_size            = EXCLUDED.tick_size,
				max_leverage         = EXCLUDED.max_leverage,
				funding_interval_sec = EXCLUDED.funding_interval_sec,
				refreshed_at         = EXCLUDED.refreshed_at
		`,
			string(inst.Exchange),
			string(inst.ExchangeSymbol),
			string(inst.CanonicalBaseAsset),
			"USDT",
			string(inst.InstrumentType),
			status,
			decimalToNumeric(inst.ContractMultiplier),
			decimalToNumeric(inst.QtyStep),
			decimalToNumeric(inst.MinQty),
			decimalToNumeric(inst.TickSize),
			decimalToNumeric(inst.MaxLeverage),
			inst.FundingIntervalSec,
			now,
		)
	}

	br := tx.SendBatch(ctx, batch)
	for i := range instruments {
		if _, execErr := br.Exec(); execErr != nil {
			_ = br.Close()
			err = fmt.Errorf("repository: upsert инструмента [%d]: %w", i, execErr)
			return err
		}
	}
	if closeErr := br.Close(); closeErr != nil {
		err = fmt.Errorf("repository: закрытие batch инструментов: %w", closeErr)
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("repository: коммит Replace instruments: %w", err)
	}
	return nil
}

// mapInstrumentStatus приводит domain.InstrumentStatus к строке для БД.
// БД хранит: active / reduce_only / delisted / suspended.
func mapInstrumentStatus(s domain.InstrumentStatus) string {
	switch s {
	case domain.InstrumentStatusActive:
		return "active"
	case domain.InstrumentStatusReduceOnly:
		return "reduce_only"
	case domain.InstrumentStatusDelisted:
		return "delisted"
	case domain.InstrumentStatusHalted:
		return "suspended"
	default:
		return "suspended"
	}
}
