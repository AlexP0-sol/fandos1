package keyrotation

import (
	"context"
	"errors"
	"testing"
)

// ============================================================
// Fakes
// ============================================================

// fakeCall records a single operation call in order.
type fakeCall struct {
	op      string // "halt", "revokeAll", "revoke", "record"
	arg     string // human-readable argument summary
	action  Action
	credID  *int64
	haltErr bool
}

// fakeHalter records Halt calls.
type fakeHalter struct {
	calls []fakeCall
	err   error // optional error to return
}

func (f *fakeHalter) Halt(_ context.Context, reason string) error {
	f.calls = append(f.calls, fakeCall{op: "halt", arg: reason})
	return f.err
}

// fakeRevoker records RevokeAll/Revoke calls.
type fakeRevoker struct {
	calls      []fakeCall
	revokeAllN int   // number of credentials to report revoked
	revokeAllE error // error from RevokeAll
	revokeE    error // error from Revoke
}

func (f *fakeRevoker) RevokeAll(_ context.Context) (int, error) {
	f.calls = append(f.calls, fakeCall{op: "revokeAll"})
	return f.revokeAllN, f.revokeAllE
}

func (f *fakeRevoker) Revoke(_ context.Context, exchange, kind string) error {
	f.calls = append(f.calls, fakeCall{op: "revoke", arg: exchange + "/" + kind})
	return f.revokeE
}

// fakeLog records Record calls.
type fakeLog struct {
	calls []fakeCall
	err   error
}

func (f *fakeLog) Record(_ context.Context, action Action, credID *int64, initiator string, _ map[string]any) error {
	f.calls = append(f.calls, fakeCall{op: "record", arg: initiator, action: action, credID: credID})
	return f.err
}

// orderCollector collects all operation names across halter, revoker, and log
// to assert ordering.
type orderCollector struct {
	halter  *fakeHalter
	revoker *fakeRevoker
	log     *fakeLog
	order   []string // appended by each fake as it is called
}

// orderFakeHalter — appends to shared order slice.
type orderFakeHalter struct {
	collector *orderCollector
	err       error
}

func (f *orderFakeHalter) Halt(_ context.Context, _ string) error {
	f.collector.order = append(f.collector.order, "halt")
	return f.err
}

type orderFakeRevoker struct {
	collector *orderCollector
	err       error
}

func (f *orderFakeRevoker) RevokeAll(_ context.Context) (int, error) {
	f.collector.order = append(f.collector.order, "revokeAll")
	return 2, f.err
}

func (f *orderFakeRevoker) Revoke(_ context.Context, _, _ string) error {
	f.collector.order = append(f.collector.order, "revoke")
	return f.err
}

type orderFakeLog struct {
	collector *orderCollector
}

func (f *orderFakeLog) Record(_ context.Context, _ Action, _ *int64, _ string, _ map[string]any) error {
	f.collector.order = append(f.collector.order, "record")
	return nil
}

// ============================================================
// KillSwitch tests
// ============================================================

// TestKillSwitch_OrderHaltFirst ensures SAFE_HALT is the first operation.
func TestKillSwitch_OrderHaltFirst(t *testing.T) {
	col := &orderCollector{}
	halter := &orderFakeHalter{collector: col}
	revoker := &orderFakeRevoker{collector: col}
	log := &orderFakeLog{collector: col}

	mgr := NewManager(revoker, halter, log, nil)
	if err := mgr.KillSwitch(context.Background(), "operator", "test reason"); err != nil {
		t.Fatalf("KillSwitch unexpected error: %v", err)
	}

	if len(col.order) < 3 {
		t.Fatalf("expected at least 3 operations, got %d: %v", len(col.order), col.order)
	}
	if col.order[0] != "halt" {
		t.Errorf("first operation = %q, want halt", col.order[0])
	}
	if col.order[1] != "revokeAll" {
		t.Errorf("second operation = %q, want revokeAll", col.order[1])
	}
	if col.order[2] != "record" {
		t.Errorf("third operation = %q, want record", col.order[2])
	}
}

// TestKillSwitch_RecordsKillSwitchAction ensures KILL_SWITCH action is logged.
func TestKillSwitch_RecordsKillSwitchAction(t *testing.T) {
	halter := &fakeHalter{}
	revoker := &fakeRevoker{revokeAllN: 3}
	log := &fakeLog{}

	mgr := NewManager(revoker, halter, log, nil)
	if err := mgr.KillSwitch(context.Background(), "admin", "emergency"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(log.calls) != 1 {
		t.Fatalf("log.Record called %d times, want 1", len(log.calls))
	}
	c := log.calls[0]
	if c.action != KillSwitch {
		t.Errorf("logged action = %q, want KILL_SWITCH", c.action)
	}
	if c.credID != nil {
		t.Errorf("logged credentialID = %v, want nil for KILL_SWITCH", *c.credID)
	}
	if c.arg != "admin" {
		t.Errorf("logged initiator = %q, want admin", c.arg)
	}
}

// TestKillSwitch_HaltFailureStillAttemptsRevoke verifies best-effort behaviour:
// when Halt returns an error, RevokeAll is still called.
func TestKillSwitch_HaltFailureStillAttemptsRevoke(t *testing.T) {
	col := &orderCollector{}
	halterErr := errors.New("db unavailable")
	halter := &orderFakeHalter{collector: col, err: halterErr}
	revoker := &orderFakeRevoker{collector: col}
	log := &orderFakeLog{collector: col}

	mgr := NewManager(revoker, halter, log, nil)
	err := mgr.KillSwitch(context.Background(), "operator", "reason")
	if err == nil {
		t.Fatal("expected combined error when Halt fails, got nil")
	}
	if !errors.Is(err, halterErr) {
		t.Errorf("error does not wrap halt error: %v", err)
	}

	// RevokeAll must still have been called.
	foundRevoke := false
	for _, op := range col.order {
		if op == "revokeAll" {
			foundRevoke = true
			break
		}
	}
	if !foundRevoke {
		t.Errorf("revokeAll was not called after halt failure; order: %v", col.order)
	}
}

// TestKillSwitch_NilCredentialID verifies nil credential_id passed to Record.
func TestKillSwitch_NilCredentialID(t *testing.T) {
	halter := &fakeHalter{}
	revoker := &fakeRevoker{revokeAllN: 0}
	log := &fakeLog{}

	mgr := NewManager(revoker, halter, log, nil)
	_ = mgr.KillSwitch(context.Background(), "op", "test")

	if len(log.calls) == 0 {
		t.Fatal("log.Record not called")
	}
	if log.calls[0].credID != nil {
		t.Errorf("credentialID = %v, want nil", log.calls[0].credID)
	}
}

// ============================================================
// RotateKey tests
// ============================================================

// TestRotateKey_RevokesSpecificCredential checks exactly one revoke is called.
func TestRotateKey_RevokesSpecificCredential(t *testing.T) {
	halter := &fakeHalter{}
	revoker := &fakeRevoker{}
	log := &fakeLog{}

	mgr := NewManager(revoker, halter, log, nil)
	cid := int64(42)
	if err := mgr.RotateKey(context.Background(), "admin", "binance", "trade", PlannedRotation, cid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(revoker.calls) != 1 {
		t.Fatalf("revoker.Revoke called %d times, want 1", len(revoker.calls))
	}
	if revoker.calls[0].arg != "binance/trade" {
		t.Errorf("revoked %q, want binance/trade", revoker.calls[0].arg)
	}

	// Halt must NOT have been called.
	if len(halter.calls) != 0 {
		t.Errorf("halter.Halt called unexpectedly %d times", len(halter.calls))
	}
}

// TestRotateKey_LogsWithCredentialID ensures non-nil credentialID is passed.
func TestRotateKey_LogsWithCredentialID(t *testing.T) {
	halter := &fakeHalter{}
	revoker := &fakeRevoker{}
	log := &fakeLog{}

	mgr := NewManager(revoker, halter, log, nil)
	cid := int64(99)
	if err := mgr.RotateKey(context.Background(), "admin", "okx", "withdrawal", EmergencyRotation, cid); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(log.calls) != 1 {
		t.Fatalf("log.Record called %d times, want 1", len(log.calls))
	}
	c := log.calls[0]
	if c.action != EmergencyRotation {
		t.Errorf("action = %q, want EMERGENCY_ROTATION", c.action)
	}
	if c.credID == nil {
		t.Fatal("credentialID is nil, want non-nil")
	}
	if *c.credID != cid {
		t.Errorf("credentialID = %d, want %d", *c.credID, cid)
	}
}

// TestRotateKey_RejectsKillSwitchAction ensures KillSwitch action is rejected.
func TestRotateKey_RejectsKillSwitchAction(t *testing.T) {
	mgr := NewManager(&fakeRevoker{}, &fakeHalter{}, &fakeLog{}, nil)
	err := mgr.RotateKey(context.Background(), "op", "binance", "trade", KillSwitch, 1)
	if err == nil {
		t.Fatal("expected error for KillSwitch action in RotateKey, got nil")
	}
}

// TestRotateKey_RevokeError propagates revoke error.
func TestRotateKey_RevokeError(t *testing.T) {
	revokeErr := errors.New("db down")
	revoker := &fakeRevoker{revokeE: revokeErr}
	mgr := NewManager(revoker, &fakeHalter{}, &fakeLog{}, nil)
	err := mgr.RotateKey(context.Background(), "op", "binance", "trade", PlannedRotation, 1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, revokeErr) {
		t.Errorf("error does not wrap revokeErr: %v", err)
	}
}
