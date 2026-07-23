// credentials.go — серверные провайдеры для управления API-ключами бирж
// (раздел 13): envelope-шифрование master key-ом, fingerprint для UI,
// claim владельца при первом входе allowlisted-пользователя.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/thecd/fundarbitrage/internal/credentials"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/repository"
	"github.com/thecd/fundarbitrage/internal/telegram"
)

// CredentialPlaintext — формат расшифрованного секрета в blob-е:
// JSON {key, secret, passphrase} — passphrase нужна OKX/Bitget/KuCoin.
type CredentialPlaintext struct {
	Key        string `json:"key"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase,omitempty"`
}

// DBCredentialsProvider — telegram.CredentialsProvider поверх repository +
// envelope encryption. Owner v1 = user_id 1 (single-tenant, ADR-0001).
type DBCredentialsProvider struct {
	Boot   *Bootstrap
	UserID int64
}

// aad — привязка blob-а к контексту (защита от подмены между строками).
func credentialAAD(exchange, kind string) []byte {
	return []byte("tenant:default|exchange:" + exchange + "|kind:" + kind)
}

// Fingerprint — маскированное представление ключа для UI (никогда не секрет).
func Fingerprint(apiKey string) string {
	if len(apiKey) < 8 {
		return "****"
	}
	return apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
}

// Save шифрует и сохраняет ключи; plaintext живёт в памяти только на время вызова.
func (p *DBCredentialsProvider) Save(ctx context.Context, exchange, kind, apiKey, apiSecret, passphrase string) (string, error) {
	masterKey, err := credentials.LoadMasterKey(p.Boot.Cold.MasterKeyEnv)
	if err != nil {
		return "", fmt.Errorf("app: master key: %w", err)
	}
	defer credentials.Zero(masterKey)

	plain, err := json.Marshal(CredentialPlaintext{Key: apiKey, Secret: apiSecret, Passphrase: passphrase})
	if err != nil {
		return "", fmt.Errorf("app: marshal credential: %w", err)
	}
	defer credentials.Zero(plain)

	blob, err := credentials.Encrypt(masterKey, plain, credentialAAD(exchange, kind))
	if err != nil {
		return "", fmt.Errorf("app: encrypt credential: %w", err)
	}

	fp := Fingerprint(apiKey)
	repo := repository.NewCredentialsRepo(p.Boot.Pool)
	if err := repo.Save(ctx, p.UserID, exchange, kind, blob, fp); err != nil {
		return "", err
	}

	// Audit (без секретов!).
	if tx, terr := p.Boot.Pool.Begin(ctx); terr == nil {
		_ = p.Boot.Audit.WriteTx(ctx, tx, "user:miniapp", "CREDENTIAL_SAVED", exchange,
			map[string]string{"exchange": exchange, "kind": kind, "fingerprint": fp}, "ok")
		_ = tx.Commit(ctx)
	}
	return fp, nil
}

// List — метаданные ключей (fingerprint, статус) без шифроблобов.
func (p *DBCredentialsProvider) List(ctx context.Context) ([]telegram.CredentialDTO, error) {
	repo := repository.NewCredentialsRepo(p.Boot.Pool)
	infos, err := repo.List(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]telegram.CredentialDTO, 0, len(infos))
	for _, in := range infos {
		out = append(out, telegram.CredentialDTO{
			Exchange:    in.Exchange,
			Kind:        in.Kind,
			Fingerprint: in.Fingerprint,
			CreatedAt:   in.CreatedAt,
			Revoked:     in.Revoked,
		})
	}
	return out, nil
}

// Revoke — отзыв ключа (blob остаётся, Load его больше не отдаёт).
func (p *DBCredentialsProvider) Revoke(ctx context.Context, exchange, kind string) error {
	repo := repository.NewCredentialsRepo(p.Boot.Pool)
	if err := repo.Revoke(ctx, p.UserID, exchange, kind); err != nil {
		return err
	}
	if tx, terr := p.Boot.Pool.Begin(ctx); terr == nil {
		_ = p.Boot.Audit.WriteTx(ctx, tx, "user:miniapp", "CREDENTIAL_REVOKED", exchange,
			map[string]string{"exchange": exchange, "kind": kind}, "ok")
		_ = tx.Commit(ctx)
	}
	return nil
}

// LoadDecrypted — расшифровка ключей для live-wiring (worker). Вызывающий
// ОБЯЗАН стереть значения после использования (credentials.Zero на байтах
// не работает для string — храните минимально и не логируйте).
func LoadDecrypted(ctx context.Context, boot *Bootstrap, userID int64, exchange domain.ExchangeID, kind string) (CredentialPlaintext, error) {
	masterKey, err := credentials.LoadMasterKey(boot.Cold.MasterKeyEnv)
	if err != nil {
		return CredentialPlaintext{}, fmt.Errorf("app: master key: %w", err)
	}
	defer credentials.Zero(masterKey)

	repo := repository.NewCredentialsRepo(boot.Pool)
	blob, err := repo.Load(ctx, userID, string(exchange), kind)
	if err != nil {
		return CredentialPlaintext{}, err
	}
	plain, err := credentials.Decrypt(masterKey, blob, credentialAAD(string(exchange), kind))
	if err != nil {
		return CredentialPlaintext{}, fmt.Errorf("app: decrypt %s/%s: %w", exchange, kind, err)
	}
	defer credentials.Zero(plain)

	var out CredentialPlaintext
	if err := json.Unmarshal(plain, &out); err != nil {
		return CredentialPlaintext{}, fmt.Errorf("app: credential format: %w", err)
	}
	if strings.TrimSpace(out.Key) == "" || strings.TrimSpace(out.Secret) == "" {
		return CredentialPlaintext{}, fmt.Errorf("app: credential %s/%s is empty", exchange, kind)
	}
	return out, nil
}

// DBOwnerClaimer — telegram.OwnerClaimer поверх UsersRepo:
// первый allowlisted-пользователь становится владельцем (seed -1 → его telegram_id).
type DBOwnerClaimer struct {
	Boot *Bootstrap
}

// ClaimOwner выполняет атомарный клейм и пишет audit при успехе.
func (c *DBOwnerClaimer) ClaimOwner(ctx context.Context, telegramID int64) (bool, error) {
	repo := repository.NewUsersRepo(c.Boot.Pool)
	claimed, err := repo.ClaimOwner(ctx, telegramID)
	if err != nil {
		return false, err
	}
	if claimed {
		if tx, terr := c.Boot.Pool.Begin(ctx); terr == nil {
			_ = c.Boot.Audit.WriteTx(ctx, tx, "system:auth", "OWNER_CLAIMED", "",
				map[string]int64{"telegram_id": telegramID}, "ok")
			_ = tx.Commit(ctx)
		}
		c.Boot.Log.Info("owner claimed", "telegram_id", telegramID, "at", time.Now().UTC())
	}
	return claimed, nil
}
