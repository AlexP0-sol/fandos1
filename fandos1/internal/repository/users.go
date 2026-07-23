package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// UsersRepo — репозиторий таблицы users.
type UsersRepo struct {
	pool *pgxpool.Pool
}

// NewUsersRepo создаёт UsersRepo.
func NewUsersRepo(pool *pgxpool.Pool) *UsersRepo {
	return &UsersRepo{pool: pool}
}

// ClaimOwner атомарно присваивает telegram_id владельцу, если он ещё не задан
// (seed-значение telegram_id < 1).
//
//   - claimed=true, nil  — успешный клейм (строка обновлена).
//   - claimed=false, nil — либо повторный вход того же владельца (telegram_id уже = $1),
//     либо запись уже занята другим telegram_id; доступ решает allowlist.
func (r *UsersRepo) ClaimOwner(ctx context.Context, telegramID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE users SET telegram_id = $1 WHERE telegram_id < 1
	`, telegramID)
	if err != nil {
		return false, fmt.Errorf("repository: ClaimOwner: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// OwnerTelegramID возвращает telegram_id единственного владельца (seed tenant_id='default').
func (r *UsersRepo) OwnerTelegramID(ctx context.Context) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		SELECT telegram_id FROM users WHERE tenant_id = 'default' LIMIT 1
	`).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("repository: OwnerTelegramID: %w", err)
	}
	return id, nil
}
