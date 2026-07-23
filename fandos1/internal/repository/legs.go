// legs.go — персист состояния ног позиции (position_legs, раздел 16.1).
//
// В таблице positions хранится только НЕТТО-дельта; точный объём каждой ноги
// (long/short) — в position_legs. Без этого при рестарте сплит ног теряется и
// coordinated close не знает, сколько закрывать на каждой бирже. Здесь — запись
// ног при открытии/repair и восстановление точных объёмов при старте.
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
)

// LegsRepo пишет и читает ноги позиции.
type LegsRepo struct {
	pool *pgxpool.Pool
}

// NewLegsRepo создаёт репозиторий ног.
func NewLegsRepo(pool *pgxpool.Pool) *LegsRepo {
	return &LegsRepo{pool: pool}
}

// LegState — состояние одной ноги для персиста.
type LegState struct {
	PositionID domain.PositionID
	Side       domain.Side // long | short
	Exchange   domain.ExchangeID
	Symbol     domain.ExchangeSymbol
	BaseQty    decimal.Decimal
	EntryVWAP  decimal.Decimal
	Status     string
}

// legID детерминированно строит leg_id из позиции и стороны (одна нога на сторону).
func legID(positionID domain.PositionID, side domain.Side) string {
	return string(positionID) + "-" + string(side)
}

// Upsert записывает/обновляет ногу (идемпотентно по (position_id, side)).
func (r *LegsRepo) Upsert(ctx context.Context, leg LegState) error {
	var entryVWAP *string
	if !leg.EntryVWAP.IsZero() {
		s := decimalToNumeric(leg.EntryVWAP)
		entryVWAP = &s
	}
	status := leg.Status
	if status == "" {
		status = "open"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO position_legs (
			leg_id, position_id, exchange, exchange_symbol, side,
			base_qty, entry_vwap, status, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now())
		ON CONFLICT (position_id, side) DO UPDATE SET
			exchange        = EXCLUDED.exchange,
			exchange_symbol = EXCLUDED.exchange_symbol,
			base_qty        = EXCLUDED.base_qty,
			entry_vwap      = COALESCE(EXCLUDED.entry_vwap, position_legs.entry_vwap),
			status          = EXCLUDED.status,
			updated_at      = now()`,
		legID(leg.PositionID, leg.Side), string(leg.PositionID), string(leg.Exchange),
		string(leg.Symbol), string(leg.Side),
		decimalToNumeric(leg.BaseQty), entryVWAP, status,
	)
	if err != nil {
		return fmt.Errorf("repository: upsert leg %s/%s: %w", leg.PositionID, leg.Side, err)
	}
	return nil
}

// LegQuantities — восстановленные объёмы ног позиции.
type LegQuantities struct {
	LongBaseQty  decimal.Decimal
	ShortBaseQty decimal.Decimal
	Found        bool // false, если ноги ещё не записаны
}

// Load читает объёмы обеих ног позиции (для точного восстановления при рестарте).
func (r *LegsRepo) Load(ctx context.Context, positionID domain.PositionID) (LegQuantities, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT side, base_qty FROM position_legs WHERE position_id = $1`, string(positionID))
	if err != nil {
		return LegQuantities{}, fmt.Errorf("repository: load legs %s: %w", positionID, err)
	}
	defer rows.Close()

	out := LegQuantities{LongBaseQty: decimal.Zero, ShortBaseQty: decimal.Zero}
	for rows.Next() {
		var side, qtyStr string
		if err := rows.Scan(&side, &qtyStr); err != nil {
			return LegQuantities{}, fmt.Errorf("repository: scan leg %s: %w", positionID, err)
		}
		qty, derr := numericToDecimal(qtyStr)
		if derr != nil {
			return LegQuantities{}, fmt.Errorf("repository: leg qty %s: %w", positionID, derr)
		}
		switch domain.Side(side) {
		case domain.SideLong:
			out.LongBaseQty = qty
			out.Found = true
		case domain.SideShort:
			out.ShortBaseQty = qty
			out.Found = true
		}
	}
	if err := rows.Err(); err != nil {
		return LegQuantities{}, fmt.Errorf("repository: iterate legs %s: %w", positionID, err)
	}
	return out, nil
}
