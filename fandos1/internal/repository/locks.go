package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LocksRepo — репозиторий системных блокировок (system_locks) и проверки готовности владельца.
type LocksRepo struct {
	pool *pgxpool.Pool
}

// NewLocksRepo создаёт LocksRepo.
func NewLocksRepo(pool *pgxpool.Pool) *LocksRepo {
	return &LocksRepo{pool: pool}
}

// Engage включает блокировку с указанным именем и причиной.
// Если запись с lock_name уже существует — обновляет её.
// Если запись не существует — создаёт новую.
func (r *LocksRepo) Engage(ctx context.Context, name, reason string) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO system_locks (lock_name, engaged, reason, engaged_at, released_at)
		VALUES ($1, TRUE, $2, $3, NULL)
		ON CONFLICT (lock_name) DO UPDATE SET
			engaged     = TRUE,
			reason      = EXCLUDED.reason,
			engaged_at  = EXCLUDED.engaged_at,
			released_at = NULL
	`, name, reason, now)
	if err != nil {
		return fmt.Errorf("repository: engage блокировки %q: %w", name, err)
	}
	return nil
}

// Release снимает блокировку с указанным именем.
// Если запись не существует — ничего не делает (идемпотентность).
func (r *LocksRepo) Release(ctx context.Context, name string) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		UPDATE system_locks
		SET engaged      = FALSE,
		    reason       = NULL,
		    released_at  = $1
		WHERE lock_name  = $2
	`, now, name)
	if err != nil {
		return fmt.Errorf("repository: release блокировки %q: %w", name, err)
	}
	return nil
}

// IsEngaged возвращает true, если блокировка с указанным именем активна.
// Если запись не существует — возвращает false (незарегистрированная блокировка = не активна).
func (r *LocksRepo) IsEngaged(ctx context.Context, name string) (bool, error) {
	var engaged bool
	err := r.pool.QueryRow(ctx, `
		SELECT engaged FROM system_locks WHERE lock_name = $1
	`, name).Scan(&engaged)
	if err != nil {
		// pgx.ErrNoRows — блокировка не зарегистрирована, считаем inactive.
		return false, nil
	}
	return engaged, nil
}

// OwnerReady возвращает true, если telegram_id владельца >= 1 (startup precondition).
// Seed-пользователь имеет telegram_id = -1, что означает «владелец не настроен».
// Система ОБЯЗАНА отказаться торговать, пока этот метод возвращает false.
func (r *LocksRepo) OwnerReady(ctx context.Context) (bool, error) {
	var ready bool
	err := r.pool.QueryRow(ctx, `
		SELECT telegram_id >= 1 FROM users LIMIT 1
	`).Scan(&ready)
	if err != nil {
		return false, fmt.Errorf("repository: проверка готовности владельца: %w", err)
	}
	return ready, nil
}
