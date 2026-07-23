package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// memLocks — in-memory реализация LockControl для тестов.
type memLocks struct {
	mu      sync.Mutex
	engaged map[string]string
	failOps bool // имитация недоступной БД
}

func newMemLocks() *memLocks { return &memLocks{engaged: map[string]string{}} }

func (m *memLocks) Engage(_ context.Context, name, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOps {
		return errors.New("db down")
	}
	m.engaged[name] = reason
	return nil
}
func (m *memLocks) Release(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOps {
		return errors.New("db down")
	}
	delete(m.engaged, name)
	return nil
}
func (m *memLocks) IsEngaged(_ context.Context, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOps {
		return false, errors.New("db down")
	}
	_, ok := m.engaged[name]
	return ok, nil
}

type memNotifier struct {
	mu   sync.Mutex
	msgs []string
}

func (n *memNotifier) Notify(_ context.Context, severity, text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.msgs = append(n.msgs, severity+": "+text)
}

func TestPreconditionsCollectAllErrors(t *testing.T) {
	checks := []Precondition{
		{Name: "ok", Fn: func(context.Context) error { return nil }},
		{Name: "owner", Fn: func(context.Context) error { return errors.New("telegram_id not set") }},
		{Name: "clock", Fn: func(context.Context) error { return errors.New("offset too big") }},
	}
	errs := CheckPreconditions(context.Background(), checks)
	if len(errs) != 2 {
		t.Fatalf("errors = %d, want 2 (все, а не первая)", len(errs))
	}
}

func TestHaltEngagesAndNotifies(t *testing.T) {
	locks := newMemLocks()
	notif := &memNotifier{}
	h := NewHalter(locks, notif, nil)

	if err := h.Halt(context.Background(), "delta breach"); err != nil {
		t.Fatal(err)
	}
	if halted, reason := h.IsHalted(); !halted || reason != "delta breach" {
		t.Error("in-memory halt state wrong")
	}
	if ok, _ := locks.IsEngaged(context.Background(), SafeHaltName); !ok {
		t.Error("persistent lock not engaged")
	}
	if len(notif.msgs) != 1 || !strings.Contains(notif.msgs[0], "CRITICAL") {
		t.Errorf("notification missing: %v", notif.msgs)
	}
}

func TestHaltWhenDBDownStillHaltsInMemory(t *testing.T) {
	locks := newMemLocks()
	locks.failOps = true
	dir := t.TempDir()
	side := NewSideChannelLog(filepath.Join(dir, "side.log"))
	h := NewHalter(locks, nil, side)

	err := h.Halt(context.Background(), "db gone")
	if err == nil {
		t.Fatal("persist error expected")
	}
	// Ключевое: торговля остановлена НЕСМОТРЯ на недоступную БД.
	if halted, _ := h.IsHalted(); !halted {
		t.Error("must be halted in memory even when DB persist fails")
	}
	// Side-channel зафиксировал и сам halt, и ошибку персиста.
	data, _ := os.ReadFile(filepath.Join(dir, "side.log"))
	if !strings.Contains(string(data), "SAFE_HALT engaged") ||
		!strings.Contains(string(data), "persist FAILED") {
		t.Errorf("side-channel incomplete:\n%s", data)
	}
}

func TestResumeReleasesLock(t *testing.T) {
	locks := newMemLocks()
	h := NewHalter(locks, nil, nil)
	_ = h.Halt(context.Background(), "x")
	if err := h.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if halted, _ := h.IsHalted(); halted {
		t.Error("must not be halted after resume")
	}
	if ok, _ := locks.IsEngaged(context.Background(), SafeHaltName); ok {
		t.Error("lock must be released")
	}
}

func TestResumeFailsClosedWhenDBDown(t *testing.T) {
	locks := newMemLocks()
	h := NewHalter(locks, nil, nil)
	_ = h.Halt(context.Background(), "x")
	locks.failOps = true
	if err := h.Resume(context.Background()); err == nil {
		t.Fatal("resume must fail when DB down")
	}
	// Fail-closed: остаёмся остановленными.
	if halted, _ := h.IsHalted(); !halted {
		t.Error("must remain halted when release persist fails")
	}
}

func TestRestoreFromDB(t *testing.T) {
	locks := newMemLocks()
	_ = locks.Engage(context.Background(), SafeHaltName, "was halted before restart")
	h := NewHalter(locks, nil, nil)
	if err := h.RestoreFromDB(context.Background()); err != nil {
		t.Fatal(err)
	}
	if halted, _ := h.IsHalted(); !halted {
		t.Error("halt state must survive restart via DB")
	}
}

func TestDBWatchdogHaltsAfterThreshold(t *testing.T) {
	locks := newMemLocks()
	h := NewHalter(locks, nil, nil)
	var mu sync.Mutex
	failing := true
	w := &DBWatchdog{
		Ping: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if failing {
				return errors.New("no db")
			}
			return nil
		},
		Halter:    h,
		Threshold: 3,
		Interval:  5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		if halted, _ := h.IsHalted(); halted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("watchdog did not halt in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestDBWatchdogResetsOnSuccess(t *testing.T) {
	locks := newMemLocks()
	h := NewHalter(locks, nil, nil)
	calls := 0
	var mu sync.Mutex
	w := &DBWatchdog{
		Ping: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls%2 == 0 {
				return errors.New("flaky")
			} // чередуем: не набирается 3 подряд
			return nil
		},
		Halter:    h,
		Threshold: 3,
		Interval:  3 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	w.Run(ctx)
	if halted, _ := h.IsHalted(); halted {
		t.Error("alternating failures must not accumulate to threshold")
	}
}

func TestSupervisorGracefulStop(t *testing.T) {
	stopped := make(chan string, 2)
	comp := func(name string) Component {
		return Component{Name: name, Run: func(ctx context.Context) error {
			<-ctx.Done()
			stopped <- name
			return ctx.Err()
		}}
	}
	s := &Supervisor{ShutdownTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, []Component{comp("a"), comp("b")}) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("graceful stop must return nil, got %v", err)
	}
	if len(stopped) != 2 {
		t.Errorf("stopped = %d, want 2", len(stopped))
	}
}

func TestSupervisorCascadeOnFailure(t *testing.T) {
	otherStopped := make(chan struct{})
	comps := []Component{
		{Name: "crasher", Run: func(ctx context.Context) error {
			return errors.New("boom")
		}},
		{Name: "steady", Run: func(ctx context.Context) error {
			<-ctx.Done()
			close(otherStopped)
			return ctx.Err()
		}},
	}
	s := &Supervisor{ShutdownTimeout: time.Second}
	err := s.Run(context.Background(), comps)
	if !errors.Is(err, ErrComponentFailed) {
		t.Fatalf("want ErrComponentFailed, got %v", err)
	}
	select {
	case <-otherStopped:
	case <-time.After(time.Second):
		t.Error("steady component was not cascaded down")
	}
}

func TestSupervisorShutdownTimeout(t *testing.T) {
	comps := []Component{
		{Name: "hang", Run: func(ctx context.Context) error {
			select {} // игнорирует ctx — злостный компонент
		}},
	}
	s := &Supervisor{ShutdownTimeout: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, comps) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not stop") {
			t.Fatalf("want shutdown-timeout error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor hung")
	}
}

func TestSideChannelLogAppends(t *testing.T) {
	p := filepath.Join(t.TempDir(), "side.log")
	l := NewSideChannelLog(p)
	l.Write("first")
	l.Write("second")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "first") || !strings.Contains(lines[1], "second") {
		t.Errorf("unexpected content:\n%s", data)
	}
}
