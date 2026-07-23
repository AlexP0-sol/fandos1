package app

// keyrotation.go — wires a keyrotation.Manager from Bootstrap and exposes a
// KillSwitchProvider that satisfies the telegram.KillSwitchProvider interface.

import (
	"context"
	"fmt"

	"github.com/thecd/fundarbitrage/internal/keyrotation"
	"github.com/thecd/fundarbitrage/internal/repository"
)

// ============================================================
// credentialRevokerAdapter adapts repository.CredentialsRepo to
// keyrotation.CredentialRevoker for the owner (user_id=1).
// ============================================================

// ownerCredentialRevoker implements keyrotation.CredentialRevoker for user_id=1.
type ownerCredentialRevoker struct {
	repo   *repository.CredentialsRepo
	userID int64
}

// RevokeAll revokes every active credential for the owner.
// Best-effort: errors are accumulated but we continue for remaining entries.
func (r *ownerCredentialRevoker) RevokeAll(ctx context.Context) (int, error) {
	items, err := r.repo.List(ctx, r.userID)
	if err != nil {
		return 0, fmt.Errorf("app: revokeAll list credentials: %w", err)
	}

	revoked := 0
	var errs []error
	for _, item := range items {
		if item.Revoked {
			continue // already revoked
		}
		if e := r.repo.Revoke(ctx, r.userID, item.Exchange, item.Kind); e != nil {
			errs = append(errs, fmt.Errorf("app: revoke %s/%s: %w", item.Exchange, item.Kind, e))
			continue
		}
		revoked++
	}
	if len(errs) > 0 {
		combined := errs[0]
		for _, e := range errs[1:] {
			combined = fmt.Errorf("%w; %v", combined, e)
		}
		return revoked, combined
	}
	return revoked, nil
}

// Revoke revokes a single credential by (exchange, kind).
func (r *ownerCredentialRevoker) Revoke(ctx context.Context, exchange, kind string) error {
	return r.repo.Revoke(ctx, r.userID, exchange, kind)
}

// ============================================================
// NewKeyRotationManager builds a keyrotation.Manager from Bootstrap.
// ============================================================

// NewKeyRotationManager wires a Manager using:
//   - ownerCredentialRevoker (iterates credentials.List for user_id=1, revokes each)
//   - boot.Halter  (satisfied by *lifecycle.Halter)
//   - RotationLogRepo
func NewKeyRotationManager(boot *Bootstrap) *keyrotation.Manager {
	revoker := &ownerCredentialRevoker{
		repo:   repository.NewCredentialsRepo(boot.Pool),
		userID: 1, // single-tenant owner ADR-0001
	}
	logRepo := repository.NewRotationLogRepo(boot.Pool)
	return keyrotation.NewManager(revoker, boot.Halter, logRepo, nil)
}

// ============================================================
// DBKillSwitchProvider implements telegram.KillSwitchProvider.
// ============================================================

// DBKillSwitchProvider bridges keyrotation.Manager and the telegram HTTP layer.
type DBKillSwitchProvider struct {
	mgr  *keyrotation.Manager
	boot *Bootstrap
}

// NewDBKillSwitchProvider creates a DBKillSwitchProvider.
func NewDBKillSwitchProvider(boot *Bootstrap) *DBKillSwitchProvider {
	return &DBKillSwitchProvider{
		mgr:  NewKeyRotationManager(boot),
		boot: boot,
	}
}

// Engage triggers the kill switch: halts trading and revokes all credentials.
// initiator is set to "user:miniapp".
func (p *DBKillSwitchProvider) Engage(ctx context.Context, reason string) error {
	return p.mgr.KillSwitch(ctx, "user:miniapp", reason)
}

// Status returns the current SAFE_HALT state from the in-memory Halter flag.
func (p *DBKillSwitchProvider) Status(ctx context.Context) (halted bool, reason string, err error) {
	h, r := p.boot.Halter.IsHalted()
	return h, r, nil
}
