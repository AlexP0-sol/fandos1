package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSettingsNotFound — строка singleton в strategy_settings ещё не создана.
// Вызывающий должен применить дефолтные настройки и создать запись через SaveHot.
var ErrSettingsNotFound = errors.New("repository: strategy_settings не инициализированы")

// ErrVersionConflict — попытка сохранить настройки с устаревшей версией.
// Означает, что кто-то другой уже сохранил более новую версию.
var ErrVersionConflict = errors.New("repository: конфликт версии strategy_settings")

// SettingsRepo — репозиторий горячих настроек стратегии.
type SettingsRepo struct {
	pool *pgxpool.Pool
}

// NewSettingsRepo создаёт SettingsRepo.
func NewSettingsRepo(pool *pgxpool.Pool) *SettingsRepo {
	return &SettingsRepo{pool: pool}
}

// LoadHot читает текущий payload и версию из singleton-строки strategy_settings.
// Если строки нет — возвращает ErrSettingsNotFound.
func (r *SettingsRepo) LoadHot(ctx context.Context) (payload []byte, version int64, err error) {
	var p []byte
	var v int64

	row := r.pool.QueryRow(ctx, `
		SELECT payload, version FROM strategy_settings WHERE singleton = TRUE
	`)
	if scanErr := row.Scan(&p, &v); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil, 0, ErrSettingsNotFound
		}
		return nil, 0, fmt.Errorf("repository: чтение strategy_settings: %w", scanErr)
	}
	return p, v, nil
}

// SaveHot сохраняет новый payload с версией expectedVersion+1.
// Использует оптимистичный CAS: UPDATE ... WHERE version = expectedVersion.
// Если ни одна строка не обновлена — возвращает ErrVersionConflict.
// При первоначальном создании (expectedVersion=0) выполняет INSERT OR UPDATE.
func (r *SettingsRepo) SaveHot(ctx context.Context, payloadJSON []byte, expectedVersion int64) error {
	if expectedVersion == 0 {
		// Первое сохранение: INSERT с обработкой конфликта по singleton.
		tag, err := r.pool.Exec(ctx, `
			INSERT INTO strategy_settings (singleton, version, payload, updated_at)
			VALUES (TRUE, 1, $1, now())
			ON CONFLICT (singleton) DO UPDATE
				SET payload    = EXCLUDED.payload,
				    version    = strategy_settings.version + 1,
				    updated_at = now()
				WHERE strategy_settings.version = 0
		`, payloadJSON)
		if err != nil {
			return fmt.Errorf("repository: первое сохранение strategy_settings: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrVersionConflict
		}
		return nil
	}

	// Обновление с CAS: обновляем только если текущая version == expectedVersion.
	tag, err := r.pool.Exec(ctx, `
		UPDATE strategy_settings
		SET payload    = $1,
		    version    = $2,
		    updated_at = now()
		WHERE singleton = TRUE
		  AND version   = $3
	`, payloadJSON, expectedVersion+1, expectedVersion)
	if err != nil {
		return fmt.Errorf("repository: обновление strategy_settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionConflict
	}
	return nil
}
