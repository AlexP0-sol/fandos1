package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thecd/fundarbitrage/internal/credentials"
)

// ErrCredentialNotFound — учётные данные не найдены или отозваны.
var ErrCredentialNotFound = errors.New("repository: credential not found")

// CredentialInfo — метаданные ключа без шифроблобов (для отображения в UI).
type CredentialInfo struct {
	Exchange    string
	Kind        string
	Fingerprint string
	CreatedAt   time.Time
	RotatedAt   *time.Time
	Revoked     bool
}

// CredentialsRepo — репозиторий API-ключей бирж (exchange_credentials).
type CredentialsRepo struct {
	pool *pgxpool.Pool
}

// NewCredentialsRepo создаёт CredentialsRepo.
func NewCredentialsRepo(pool *pgxpool.Pool) *CredentialsRepo {
	return &CredentialsRepo{pool: pool}
}

// Save сохраняет зашифрованный блоб для пары (userID, exchange, kind).
// UPSERT: при замене существующей записи обновляет enc_dek/ciphertext/blob_version/key_fingerprint,
// устанавливает rotated_at=now() и снимает отзыв (revoked_at=NULL).
func (r *CredentialsRepo) Save(
	ctx context.Context,
	userID int64,
	exchange, kind string,
	blob *credentials.Blob,
	fingerprint string,
) error {
	now := time.Now().UTC()
	_, err := r.pool.Exec(ctx, `
		INSERT INTO exchange_credentials
			(user_id, exchange, kind, key_fingerprint, blob_version, enc_dek, ciphertext, rotated_at, revoked_at)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, NULL, NULL)
		ON CONFLICT (user_id, exchange, kind) DO UPDATE SET
			key_fingerprint = EXCLUDED.key_fingerprint,
			blob_version    = EXCLUDED.blob_version,
			enc_dek         = EXCLUDED.enc_dek,
			ciphertext      = EXCLUDED.ciphertext,
			rotated_at      = $8,
			revoked_at      = NULL
	`,
		userID, exchange, kind,
		fingerprint, blob.Version, blob.EncDEK, blob.Ciphertext,
		now,
	)
	if err != nil {
		return fmt.Errorf("repository: сохранение credential (%s/%s): %w", exchange, kind, err)
	}
	return nil
}

// Load возвращает Blob для неотозванного credential.
// Возвращает ErrCredentialNotFound если запись отсутствует или отозвана.
func (r *CredentialsRepo) Load(
	ctx context.Context,
	userID int64,
	exchange, kind string,
) (*credentials.Blob, error) {
	var blobVersion uint8
	var encDEK, ciphertext []byte

	err := r.pool.QueryRow(ctx, `
		SELECT blob_version, enc_dek, ciphertext
		FROM exchange_credentials
		WHERE user_id  = $1
		  AND exchange = $2
		  AND kind     = $3
		  AND revoked_at IS NULL
	`, userID, exchange, kind).Scan(&blobVersion, &encDEK, &ciphertext)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCredentialNotFound
		}
		return nil, fmt.Errorf("repository: чтение credential (%s/%s): %w", exchange, kind, err)
	}

	return &credentials.Blob{
		Version:    blobVersion,
		EncDEK:     encDEK,
		Ciphertext: ciphertext,
	}, nil
}

// List возвращает метаданные всех ключей пользователя (без шифроблобов).
func (r *CredentialsRepo) List(ctx context.Context, userID int64) ([]CredentialInfo, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT exchange, kind, key_fingerprint, created_at, rotated_at,
		       (revoked_at IS NOT NULL) AS revoked
		FROM exchange_credentials
		WHERE user_id = $1
		ORDER BY exchange, kind
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("repository: список credentials: %w", err)
	}
	defer rows.Close()

	var result []CredentialInfo
	for rows.Next() {
		var info CredentialInfo
		if err := rows.Scan(
			&info.Exchange,
			&info.Kind,
			&info.Fingerprint,
			&info.CreatedAt,
			&info.RotatedAt,
			&info.Revoked,
		); err != nil {
			return nil, fmt.Errorf("repository: сканирование credential: %w", err)
		}
		result = append(result, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository: итерация credentials: %w", err)
	}
	return result, nil
}

// Revoke устанавливает revoked_at=now() для указанного credential.
func (r *CredentialsRepo) Revoke(ctx context.Context, userID int64, exchange, kind string) error {
	now := time.Now().UTC()
	tag, err := r.pool.Exec(ctx, `
		UPDATE exchange_credentials
		SET revoked_at = $1
		WHERE user_id  = $2
		  AND exchange = $3
		  AND kind     = $4
		  AND revoked_at IS NULL
	`, now, userID, exchange, kind)
	if err != nil {
		return fmt.Errorf("repository: отзыв credential (%s/%s): %w", exchange, kind, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

// HasActive возвращает true если есть неотозванный credential для (userID, exchange, kind).
func (r *CredentialsRepo) HasActive(ctx context.Context, userID int64, exchange, kind string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM exchange_credentials
			WHERE user_id  = $1
			  AND exchange = $2
			  AND kind     = $3
			  AND revoked_at IS NULL
		)
	`, userID, exchange, kind).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("repository: проверка активного credential (%s/%s): %w", exchange, kind, err)
	}
	return exists, nil
}
