package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/portfolio"
)

// PositionRepo — репозиторий позиций. Реализует чтение/запись positions в БД.
type PositionRepo struct {
	pool  *pgxpool.Pool
	audit *AuditRepo
}

// NewPositionRepo создаёт PositionRepo.
func NewPositionRepo(pool *pgxpool.Pool) *PositionRepo {
	return &PositionRepo{pool: pool, audit: NewAuditRepo()}
}

// Upsert сохраняет или обновляет позицию в таблице positions.
// При конфликте по position_id обновляет только изменяемые поля.
func (r *PositionRepo) Upsert(ctx context.Context, pos *portfolio.Position) error {
	snap := pos.Snapshot()

	var exitAt *time.Time
	if pos.ExitedAt != nil {
		exitAt = pos.ExitedAt
	}

	var exitReason *string
	if pos.ExitReason != "" {
		exitReason = &pos.ExitReason
	}

	var entryReason *string
	if pos.EntryReason != "" {
		entryReason = &pos.EntryReason
	}

	_, err := r.pool.Exec(ctx, `
		INSERT INTO positions (
			position_id, canonical_asset, state,
			long_exchange, short_exchange,
			target_qty, actual_delta,
			realised_pnl, funding_pnl, fees_paid,
			entry_reason, exit_reason, exit_at,
			created_at, updated_at
		) VALUES (
			$1, $2, $3,
			$4, $5,
			$6, $7,
			$8, $9, $10,
			$11, $12, $13,
			$14, $15
		)
		ON CONFLICT (position_id) DO UPDATE SET
			state        = EXCLUDED.state,
			actual_delta = EXCLUDED.actual_delta,
			realised_pnl = EXCLUDED.realised_pnl,
			funding_pnl  = EXCLUDED.funding_pnl,
			fees_paid    = EXCLUDED.fees_paid,
			exit_reason  = EXCLUDED.exit_reason,
			exit_at      = EXCLUDED.exit_at,
			updated_at   = EXCLUDED.updated_at
	`,
		string(snap.ID),
		string(snap.Asset),
		string(snap.State),
		string(snap.LongExchange),
		string(snap.ShortExchange),
		decimalToNumeric(snap.TargetBaseQty),
		decimalToNumeric(snap.DeltaBase),
		decimalToNumeric(snap.RealisedPnL),
		decimalToNumeric(snap.FundingPnL),
		decimalToNumeric(snap.FeesPaid),
		entryReason,
		exitReason,
		exitAt,
		snap.CreatedAt,
		snap.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("repository: upsert позиции %s: %w", snap.ID, err)
	}
	return nil
}

// positionRow — вспомогательная структура для сканирования строки positions.
type positionRow struct {
	positionID     string
	canonicalAsset string
	state          string
	longExchange   string
	shortExchange  string
	targetQty      string
	actualDelta    string
	realisedPnL    string
	fundingPnL     string
	feesPaid       string
	entryReason    *string
	exitReason     *string
	exitAt         *time.Time
	createdAt      time.Time
	updatedAt      time.Time
}

// Load загружает позицию по ID. Возвращает pgx.ErrNoRows (обёрнутый) если не найдена.
func (r *PositionRepo) Load(ctx context.Context, id domain.PositionID) (*portfolio.Position, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT
			position_id, canonical_asset, state,
			long_exchange, short_exchange,
			target_qty::text, actual_delta::text,
			realised_pnl::text, funding_pnl::text, fees_paid::text,
			entry_reason, exit_reason, exit_at,
			created_at, updated_at
		FROM positions
		WHERE position_id = $1
	`, string(id))

	var pr positionRow
	err := row.Scan(
		&pr.positionID, &pr.canonicalAsset, &pr.state,
		&pr.longExchange, &pr.shortExchange,
		&pr.targetQty, &pr.actualDelta,
		&pr.realisedPnL, &pr.fundingPnL, &pr.feesPaid,
		&pr.entryReason, &pr.exitReason, &pr.exitAt,
		&pr.createdAt, &pr.updatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("repository: позиция %s не найдена: %w", id, err)
		}
		return nil, fmt.Errorf("repository: чтение позиции %s: %w", id, err)
	}

	return restorePosition(pr)
}

// LoadByStates загружает все позиции в указанных состояниях (для восстановления при запуске).
func (r *PositionRepo) LoadByStates(ctx context.Context, states ...string) ([]*portfolio.Position, error) {
	if len(states) == 0 {
		return nil, nil
	}

	// Преобразуем []string в []any для pgx.
	args := make([]any, len(states))
	for i, s := range states {
		args[i] = s
	}

	// Строим плейсхолдеры $1, $2, ...
	placeholders := ""
	for i := range states {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += fmt.Sprintf("$%d", i+1)
	}

	rows, err := r.pool.Query(ctx, `
		SELECT
			position_id, canonical_asset, state,
			long_exchange, short_exchange,
			target_qty::text, actual_delta::text,
			realised_pnl::text, funding_pnl::text, fees_paid::text,
			entry_reason, exit_reason, exit_at,
			created_at, updated_at
		FROM positions
		WHERE state = ANY($1::text[])
		ORDER BY created_at
	`, states)
	if err != nil {
		return nil, fmt.Errorf("repository: запрос позиций по состояниям: %w", err)
	}
	defer rows.Close()

	var result []*portfolio.Position
	for rows.Next() {
		var pr positionRow
		err := rows.Scan(
			&pr.positionID, &pr.canonicalAsset, &pr.state,
			&pr.longExchange, &pr.shortExchange,
			&pr.targetQty, &pr.actualDelta,
			&pr.realisedPnL, &pr.fundingPnL, &pr.feesPaid,
			&pr.entryReason, &pr.exitReason, &pr.exitAt,
			&pr.createdAt, &pr.updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("repository: сканирование строки позиции: %w", err)
		}
		pos, err := restorePosition(pr)
		if err != nil {
			return nil, err
		}
		result = append(result, pos)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository: итерация позиций: %w", err)
	}
	return result, nil
}

// restorePosition восстанавливает объект Position из БД-строки.
// Использует portfolio.RestorePosition — конструктор восстановления из персистентного хранилища.
func restorePosition(pr positionRow) (*portfolio.Position, error) {
	targetQty, err := numericToDecimal(pr.targetQty)
	if err != nil {
		return nil, fmt.Errorf("repository: target_qty позиции %s: %w", pr.positionID, err)
	}
	actualDelta, err := numericToDecimal(pr.actualDelta)
	if err != nil {
		return nil, fmt.Errorf("repository: actual_delta позиции %s: %w", pr.positionID, err)
	}
	realisedPnL, err := numericToDecimal(pr.realisedPnL)
	if err != nil {
		return nil, fmt.Errorf("repository: realised_pnl позиции %s: %w", pr.positionID, err)
	}
	fundingPnL, err := numericToDecimal(pr.fundingPnL)
	if err != nil {
		return nil, fmt.Errorf("repository: funding_pnl позиции %s: %w", pr.positionID, err)
	}
	feesPaid, err := numericToDecimal(pr.feesPaid)
	if err != nil {
		return nil, fmt.Errorf("repository: fees_paid позиции %s: %w", pr.positionID, err)
	}

	var entryReason string
	if pr.entryReason != nil {
		entryReason = *pr.entryReason
	}
	var exitReason string
	if pr.exitReason != nil {
		exitReason = *pr.exitReason
	}

	return portfolio.RestorePosition(portfolio.RestoreArgs{
		ID:            domain.PositionID(pr.positionID),
		Asset:         domain.AssetSymbol(pr.canonicalAsset),
		LongExchange:  domain.ExchangeID(pr.longExchange),
		ShortExchange: domain.ExchangeID(pr.shortExchange),
		State:         portfolio.State(pr.state),
		TargetBaseQty: targetQty,
		ActualDelta:   actualDelta,
		RealisedPnL:   realisedPnL,
		FundingPnL:    fundingPnL,
		FeesPaid:      feesPaid,
		EntryReason:   entryReason,
		ExitReason:    exitReason,
		ExitedAt:      pr.exitAt,
		CreatedAt:     pr.createdAt,
		UpdatedAt:     pr.updatedAt,
	}), nil
}

// ============================================================
// Persister — реализация portfolio.Persister
// ============================================================

// Persister реализует portfolio.Persister: атомарно персистирует переход
// (positions upsert + position_transitions INSERT + audit_log INSERT) в одной транзакции.
type Persister struct {
	pool  *pgxpool.Pool
	posR  *PositionRepo
	audit *AuditRepo
}

// NewPersister создаёт Persister.
func NewPersister(pool *pgxpool.Pool) *Persister {
	return &Persister{
		pool:  pool,
		posR:  NewPositionRepo(pool),
		audit: NewAuditRepo(),
	}
}

// OnTransition атомарно сохраняет:
//  1. Обновлённую позицию (upsert).
//  2. Запись в position_transitions.
//  3. Запись в audit_log.
//
// Если транзакция завершается с ошибкой — все три INSERT откатываются.
func (p *Persister) OnTransition(pos *portfolio.Position, t portfolio.Transition) error {
	ctx := context.Background()

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("repository: начало транзакции OnTransition: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	// 1. Upsert позиции через транзакцию.
	if err = p.upsertTx(ctx, tx, pos); err != nil {
		return err
	}

	// 2. Вставка перехода.
	if err = p.insertTransitionTx(ctx, tx, pos, t); err != nil {
		return err
	}

	// 3. Аудит перехода.
	auditParams := map[string]string{
		"from":   string(t.From),
		"to":     string(t.To),
		"reason": t.Reason,
	}
	if err = p.audit.WriteTx(ctx, tx, t.Actor, "position.transition", string(pos.ID), auditParams, "ok"); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("repository: коммит OnTransition: %w", err)
	}
	return nil
}

// upsertTx выполняет upsert позиции внутри уже открытой транзакции.
func (p *Persister) upsertTx(ctx context.Context, tx pgx.Tx, pos *portfolio.Position) error {
	snap := pos.Snapshot()

	var exitAt *time.Time
	if pos.ExitedAt != nil {
		exitAt = pos.ExitedAt
	}
	var exitReason *string
	if pos.ExitReason != "" {
		exitReason = &pos.ExitReason
	}
	var entryReason *string
	if pos.EntryReason != "" {
		entryReason = &pos.EntryReason
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO positions (
			position_id, canonical_asset, state,
			long_exchange, short_exchange,
			target_qty, actual_delta,
			realised_pnl, funding_pnl, fees_paid,
			entry_reason, exit_reason, exit_at,
			created_at, updated_at
		) VALUES (
			$1, $2, $3,
			$4, $5,
			$6, $7,
			$8, $9, $10,
			$11, $12, $13,
			$14, $15
		)
		ON CONFLICT (position_id) DO UPDATE SET
			state        = EXCLUDED.state,
			actual_delta = EXCLUDED.actual_delta,
			realised_pnl = EXCLUDED.realised_pnl,
			funding_pnl  = EXCLUDED.funding_pnl,
			fees_paid    = EXCLUDED.fees_paid,
			exit_reason  = EXCLUDED.exit_reason,
			exit_at      = EXCLUDED.exit_at,
			updated_at   = EXCLUDED.updated_at
	`,
		string(snap.ID),
		string(snap.Asset),
		string(snap.State),
		string(snap.LongExchange),
		string(snap.ShortExchange),
		decimalToNumeric(snap.TargetBaseQty),
		decimalToNumeric(snap.DeltaBase),
		decimalToNumeric(snap.RealisedPnL),
		decimalToNumeric(snap.FundingPnL),
		decimalToNumeric(snap.FeesPaid),
		entryReason,
		exitReason,
		exitAt,
		snap.CreatedAt,
		snap.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("repository: upsert позиции в транзакции: %w", err)
	}
	return nil
}

// insertTransitionTx вставляет запись перехода в position_transitions.
func (p *Persister) insertTransitionTx(ctx context.Context, tx pgx.Tx, pos *portfolio.Position, t portfolio.Transition) error {
	var reason *string
	if t.Reason != "" {
		reason = &t.Reason
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO position_transitions (position_id, from_state, to_state, reason, occurred_at)
		VALUES ($1, $2, $3, $4, $5)
	`, string(pos.ID), string(t.From), string(t.To), reason, t.At)
	if err != nil {
		return fmt.Errorf("repository: вставка position_transitions: %w", err)
	}
	return nil
}
