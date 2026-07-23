package repository_test

// Integration tests for RotationLogRepo against a live PostgreSQL instance.
// Run: go test ./internal/repository/ -count=1

import (
	"context"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/keyrotation"
	"github.com/thecd/fundarbitrage/internal/repository"
)

// testCredentialID obtains the credential_id of a known test credential,
// inserting a throwaway one if none exists.  Returns the id and a cleanup func.
func testCredentialID(t *testing.T) (int64, func()) {
	t.Helper()
	if testPool == nil {
		t.Skip("pool not initialised")
	}
	ctx := context.Background()

	// Use a deterministic test exchange/kind combo.
	exchange := "binance"
	kind := "trade"

	// Ensure a credential row exists (UPSERT so existing data is preserved).
	_, err := testPool.Exec(ctx, `
		INSERT INTO exchange_credentials
			(user_id, exchange, kind, key_fingerprint, blob_version, enc_dek, ciphertext)
		VALUES (1, $1, $2, 'test-kr-fp', 1, '\x00', '\x00')
		ON CONFLICT (user_id, exchange, kind) DO NOTHING
	`, exchange, kind)
	if err != nil {
		t.Fatalf("ensure credential row: %v", err)
	}

	var credID int64
	if err := testPool.QueryRow(ctx, `
		SELECT credential_id FROM exchange_credentials
		WHERE user_id=1 AND exchange=$1 AND kind=$2
	`, exchange, kind).Scan(&credID); err != nil {
		t.Fatalf("fetch credential_id: %v", err)
	}

	cleanup := func() {
		// Only clean up key_rotation_log rows we inserted (leave credential intact).
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM key_rotation_log WHERE initiator='test-integration'`)
	}
	return credID, cleanup
}

// ============================================================
// RotationLogRepo — integration tests
// ============================================================

// TestRotationLog_PlannedRotation inserts a PLANNED_ROTATION row and asserts it.
func TestRotationLog_PlannedRotation(t *testing.T) {
	if testPool == nil {
		t.Skip("pool not initialised")
	}

	credID, cleanup := testCredentialID(t)
	defer cleanup()

	repo := repository.NewRotationLogRepo(testPool)
	ctx := context.Background()

	details := map[string]any{"exchange": "binance", "kind": "trade"}
	if err := repo.Record(ctx, keyrotation.PlannedRotation, &credID, "test-integration", details); err != nil {
		t.Fatalf("Record PLANNED_ROTATION: %v", err)
	}

	var action string
	var gotCredID *int64
	var initiator string
	err := testPool.QueryRow(ctx, `
		SELECT action, credential_id, initiator
		FROM key_rotation_log
		WHERE initiator='test-integration' AND action='PLANNED_ROTATION'
		ORDER BY occurred_at DESC LIMIT 1
	`).Scan(&action, &gotCredID, &initiator)
	if err != nil {
		t.Fatalf("query inserted row: %v", err)
	}
	if action != "PLANNED_ROTATION" {
		t.Errorf("action = %q, want PLANNED_ROTATION", action)
	}
	if gotCredID == nil || *gotCredID != credID {
		t.Errorf("credential_id = %v, want %d", gotCredID, credID)
	}
	if initiator != "test-integration" {
		t.Errorf("initiator = %q, want test-integration", initiator)
	}
}

// TestRotationLog_EmergencyRotation inserts an EMERGENCY_ROTATION row and asserts it.
func TestRotationLog_EmergencyRotation(t *testing.T) {
	if testPool == nil {
		t.Skip("pool not initialised")
	}

	credID, cleanup := testCredentialID(t)
	defer cleanup()

	repo := repository.NewRotationLogRepo(testPool)
	ctx := context.Background()

	if err := repo.Record(ctx, keyrotation.EmergencyRotation, &credID, "test-integration", nil); err != nil {
		t.Fatalf("Record EMERGENCY_ROTATION: %v", err)
	}

	var action string
	err := testPool.QueryRow(ctx, `
		SELECT action FROM key_rotation_log
		WHERE initiator='test-integration' AND action='EMERGENCY_ROTATION'
		ORDER BY occurred_at DESC LIMIT 1
	`).Scan(&action)
	if err != nil {
		t.Fatalf("query EMERGENCY_ROTATION row: %v", err)
	}
	if action != "EMERGENCY_ROTATION" {
		t.Errorf("action = %q, want EMERGENCY_ROTATION", action)
	}
}

// TestRotationLog_KillSwitch inserts a KILL_SWITCH row with nil credential_id.
func TestRotationLog_KillSwitch(t *testing.T) {
	if testPool == nil {
		t.Skip("pool not initialised")
	}

	_, cleanup := testCredentialID(t) // ensure pool is live
	defer cleanup()

	repo := repository.NewRotationLogRepo(testPool)
	ctx := context.Background()

	// KILL_SWITCH must accept nil credential_id.
	if err := repo.Record(ctx, keyrotation.KillSwitch, nil, "test-integration", map[string]any{"revoked": 2}); err != nil {
		t.Fatalf("Record KILL_SWITCH with nil credentialID: %v", err)
	}

	var action string
	var gotCredID *int64
	err := testPool.QueryRow(ctx, `
		SELECT action, credential_id FROM key_rotation_log
		WHERE initiator='test-integration' AND action='KILL_SWITCH'
		ORDER BY occurred_at DESC LIMIT 1
	`).Scan(&action, &gotCredID)
	if err != nil {
		t.Fatalf("query KILL_SWITCH row: %v", err)
	}
	if action != "KILL_SWITCH" {
		t.Errorf("action = %q, want KILL_SWITCH", action)
	}
	if gotCredID != nil {
		t.Errorf("credential_id = %v, want nil for KILL_SWITCH", *gotCredID)
	}
}

// TestRotationLog_NilCredentialIDRejectedForNonKillSwitch verifies the constraint
// enforcement in the repo layer (before hitting the DB CHECK).
func TestRotationLog_NilCredentialIDRejectedForNonKillSwitch(t *testing.T) {
	if testPool == nil {
		t.Skip("pool not initialised")
	}

	repo := repository.NewRotationLogRepo(testPool)
	ctx := context.Background()

	for _, action := range []keyrotation.Action{keyrotation.PlannedRotation, keyrotation.EmergencyRotation} {
		err := repo.Record(ctx, action, nil, "test-integration", nil)
		if err == nil {
			t.Errorf("action %s with nil credential_id: expected error, got nil", action)
		}
	}
}

// TestRotationLog_OccurredAtIsSet checks that occurred_at is populated.
func TestRotationLog_OccurredAtIsSet(t *testing.T) {
	if testPool == nil {
		t.Skip("pool not initialised")
	}

	credID, cleanup := testCredentialID(t)
	defer cleanup()

	repo := repository.NewRotationLogRepo(testPool)
	ctx := context.Background()

	before := time.Now().UTC().Add(-time.Second)
	if err := repo.Record(ctx, keyrotation.PlannedRotation, &credID, "test-integration", nil); err != nil {
		t.Fatalf("Record: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	var occurredAt time.Time
	if err := testPool.QueryRow(ctx, `
		SELECT occurred_at FROM key_rotation_log
		WHERE initiator='test-integration'
		ORDER BY occurred_at DESC LIMIT 1
	`).Scan(&occurredAt); err != nil {
		t.Fatalf("query occurred_at: %v", err)
	}
	if occurredAt.Before(before) || occurredAt.After(after) {
		t.Errorf("occurred_at %v outside [%v, %v]", occurredAt, before, after)
	}
}
