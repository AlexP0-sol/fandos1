package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AuditRepo — запись в append-only таблицу audit_log.
// UPDATE/DELETE на уровне прав роли запрещены — этот код только INSERT.
type AuditRepo struct {
	pool interface {
		// пул не используется в WriteTx — вставка всегда в переданной транзакции
	}
}

// NewAuditRepo создаёт AuditRepo.
func NewAuditRepo() *AuditRepo {
	return &AuditRepo{}
}

// WriteTx вставляет запись аудита в переданную транзакцию tx.
// params маршалируется в JSONB; вызывающий ОБЯЗАН предварительно редактировать
// секреты (ключи API, пароли и т.п.) до передачи в params.
// correlationID — может быть пустой строкой (сохраняется как NULL).
func (r *AuditRepo) WriteTx(
	ctx context.Context,
	tx pgx.Tx,
	actor, action, correlationID string,
	params any,
	result string,
) error {
	// Маршалируем params в JSON; nil → SQL NULL.
	var paramsJSON []byte
	if params != nil {
		var err error
		paramsJSON, err = json.Marshal(params)
		if err != nil {
			return fmt.Errorf("repository: маршалинг audit params: %w", err)
		}
	}

	// Пустой correlationID сохраняем как NULL.
	var corrID *string
	if correlationID != "" {
		corrID = &correlationID
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO audit_log (actor, action, correlation_id, params, result)
		VALUES ($1, $2, $3, $4, $5)
	`, actor, action, corrID, paramsJSON, result)
	if err != nil {
		return fmt.Errorf("repository: вставка audit_log: %w", err)
	}
	return nil
}
