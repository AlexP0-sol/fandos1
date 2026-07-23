// Package keyrotation implements key lifecycle management (section 27).
// Pure logic with no direct DB access — all side-effects are injected via interfaces.
package keyrotation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ============================================================
// Action constants
// ============================================================

// Action represents a key-rotation log action type.
// Values MUST match the CHECK constraint in key_rotation_log.
type Action string

const (
	PlannedRotation   Action = "PLANNED_ROTATION"
	EmergencyRotation Action = "EMERGENCY_ROTATION"
	KillSwitch        Action = "KILL_SWITCH"
)

// ============================================================
// Interfaces
// ============================================================

// CredentialRevoker revokes exchange API credentials.
type CredentialRevoker interface {
	// RevokeAll revokes every active credential for the managed owner.
	// Returns the count of credentials revoked.  Errors are accumulated
	// but revocation continues for remaining credentials (best-effort).
	RevokeAll(ctx context.Context) (revoked int, err error)
	// Revoke revokes one specific credential by (exchange, kind).
	Revoke(ctx context.Context, exchange, kind string) error
}

// Halter stops all trading by engaging SAFE_HALT.
// Satisfied by *lifecycle.Halter.
type Halter interface {
	Halt(ctx context.Context, reason string) error
}

// RotationLog records key-rotation events.
type RotationLog interface {
	// Record writes one rotation event.
	// credentialID must be non-nil for PlannedRotation and EmergencyRotation;
	// it may be nil only for KillSwitch.  Implementations MUST enforce this
	// constraint and return a clear error when it is violated.
	Record(ctx context.Context, action Action, credentialID *int64, initiator string, details map[string]any) error
}

// ============================================================
// Manager
// ============================================================

// Manager orchestrates key-lifecycle operations.
type Manager struct {
	revoker CredentialRevoker
	halter  Halter
	log     RotationLog
	clock   func() time.Time
}

// NewManager creates a Manager.  clock may be nil (defaults to time.Now).
func NewManager(revoker CredentialRevoker, halter Halter, log RotationLog, clock func() time.Time) *Manager {
	if clock == nil {
		clock = time.Now
	}
	return &Manager{revoker: revoker, halter: halter, log: log, clock: clock}
}

// KillSwitch is the emergency brake (section 27.1):
//  1. Engage SAFE_HALT to stop all trading immediately.
//  2. Best-effort revoke ALL credentials.
//  3. Log the KILL_SWITCH event (credential_id = nil per schema).
//
// SAFE_HALT is always attempted first.  Even if it fails, revoke and log are
// still attempted.  Combined errors from all failing steps are returned so
// the caller sees the full picture, but the in-memory halt flag is set
// regardless (lifecycle.Halter guarantees this).
func (m *Manager) KillSwitch(ctx context.Context, initiator, reason string) error {
	var errs []error

	// Step 1 — SAFE_HALT MUST come first.
	if err := m.halter.Halt(ctx, reason); err != nil {
		errs = append(errs, fmt.Errorf("keyrotation: engage SAFE_HALT: %w", err))
	}

	// Step 2 — Best-effort revoke all credentials.
	revoked, revokeErr := m.revoker.RevokeAll(ctx)
	if revokeErr != nil {
		errs = append(errs, fmt.Errorf("keyrotation: revoke all credentials (revoked=%d): %w", revoked, revokeErr))
	}

	// Step 3 — Record the event; credential_id is nil for KILL_SWITCH.
	details := map[string]any{
		"revoked": revoked,
		// NOTE: reason is intentionally NOT included in details to avoid
		// storing potentially sensitive operator notes in the log JSON.
	}
	if logErr := m.log.Record(ctx, KillSwitch, nil, initiator, details); logErr != nil {
		errs = append(errs, fmt.Errorf("keyrotation: record KILL_SWITCH: %w", logErr))
	}

	return errors.Join(errs...)
}

// RotateKey revokes one credential identified by (exchange, kind) and records
// the rotation event.  credentialID must be non-nil because the log schema
// requires a non-null credential_id for PLANNED_ROTATION and EMERGENCY_ROTATION.
//
// The actual re-entry of new keys is a separate operator action via the
// credentials Save UI; this call only invalidates the old key.
func (m *Manager) RotateKey(ctx context.Context, initiator, exchange, kind string, action Action, credentialID int64) error {
	if action == KillSwitch {
		return fmt.Errorf("keyrotation: use KillSwitch() for KILL_SWITCH action, not RotateKey")
	}

	// Revoke the specific credential so it cannot be used until re-entered.
	if err := m.revoker.Revoke(ctx, exchange, kind); err != nil {
		return fmt.Errorf("keyrotation: revoke %s/%s: %w", exchange, kind, err)
	}

	// Record with a non-nil credential_id (required by CHECK constraint).
	cid := credentialID
	details := map[string]any{
		"exchange": exchange,
		"kind":     kind,
	}
	if err := m.log.Record(ctx, action, &cid, initiator, details); err != nil {
		return fmt.Errorf("keyrotation: record %s: %w", action, err)
	}

	return nil
}
