// Package lifecycle управляет жизненным циклом процесса (разделы 15.3, 17.4, 28):
//   - стартовые предусловия (owner настроен, master key читается, часы в лимите);
//   - SAFE_HALT: глобальная остановка торговли с персистентным замком;
//   - Supervisor: запуск компонентов, каскадная остановка, graceful shutdown;
//   - DB-watchdog: при недоступности БД → SAFE_HALT + durable side-channel лог (28.2).
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ============================================================
// Стартовые предусловия
// ============================================================

// Precondition — одна проверка перед стартом торговли.
type Precondition struct {
	Name string
	Fn   func(ctx context.Context) error
}

// CheckPreconditions выполняет все проверки; возвращает СПИСОК всех ошибок
// (не первую): оператор должен видеть полную картину.
func CheckPreconditions(ctx context.Context, checks []Precondition) []error {
	var errs []error
	for _, c := range checks {
		if err := c.Fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("precondition %s: %w", c.Name, err))
		}
	}
	return errs
}

// ============================================================
// SAFE_HALT
// ============================================================

// LockControl — персистентный замок (system_locks в БД; реализует repository).
type LockControl interface {
	Engage(ctx context.Context, name, reason string) error
	Release(ctx context.Context, name string) error
	IsEngaged(ctx context.Context, name string) (bool, error)
}

// Notifier — уведомление оператора (Telegram; nil-safe).
type Notifier interface {
	Notify(ctx context.Context, severity, text string)
}

// SafeHaltName — имя замка в system_locks (сеется миграцией 0001).
const SafeHaltName = "SAFE_HALT"

// Halter — управление SAFE_HALT. Держит и in-memory флаг (мгновенная проверка
// в горячих путях), и персистентный замок (истина, переживающая рестарт).
type Halter struct {
	locks    LockControl
	notifier Notifier
	sideLog  *SideChannelLog

	mu     sync.RWMutex
	halted bool
	reason string
}

// NewHalter создаёт Halter. notifier и sideLog могут быть nil.
func NewHalter(locks LockControl, notifier Notifier, sideLog *SideChannelLog) *Halter {
	return &Halter{locks: locks, notifier: notifier, sideLog: sideLog}
}

// RestoreFromDB читает состояние замка при старте (истина — в БД).
func (h *Halter) RestoreFromDB(ctx context.Context) error {
	engaged, err := h.locks.IsEngaged(ctx, SafeHaltName)
	if err != nil {
		return fmt.Errorf("lifecycle: restore SAFE_HALT: %w", err)
	}
	h.mu.Lock()
	h.halted = engaged
	h.mu.Unlock()
	return nil
}

// Halt включает SAFE_HALT. In-memory флаг ставится ДАЖЕ если БД недоступна
// (торговлю надо остановить немедленно); ошибка персиста возвращается и уходит
// в durable side-channel (раздел 28.2).
func (h *Halter) Halt(ctx context.Context, reason string) error {
	h.mu.Lock()
	h.halted = true
	h.reason = reason
	h.mu.Unlock()

	if h.sideLog != nil {
		h.sideLog.Write("SAFE_HALT engaged: " + reason)
	}
	if h.notifier != nil {
		h.notifier.Notify(ctx, "CRITICAL", "SAFE_HALT: "+reason)
	}
	if err := h.locks.Engage(ctx, SafeHaltName, reason); err != nil {
		if h.sideLog != nil {
			h.sideLog.Write("SAFE_HALT persist FAILED: " + err.Error())
		}
		return fmt.Errorf("lifecycle: persist SAFE_HALT: %w", err)
	}
	return nil
}

// Resume снимает SAFE_HALT (только по явному действию оператора, раздел 4.3).
// Порядок обратный: сначала БД, потом память — если персист снятия не удался,
// система остаётся остановленной.
func (h *Halter) Resume(ctx context.Context) error {
	if err := h.locks.Release(ctx, SafeHaltName); err != nil {
		return fmt.Errorf("lifecycle: release SAFE_HALT: %w", err)
	}
	h.mu.Lock()
	h.halted = false
	h.reason = ""
	h.mu.Unlock()
	if h.sideLog != nil {
		h.sideLog.Write("SAFE_HALT released")
	}
	return nil
}

// IsHalted — быстрая in-memory проверка для горячих путей (scanner/execution).
func (h *Halter) IsHalted() (bool, string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.halted, h.reason
}

// ============================================================
// Durable side-channel (раздел 28.2)
// ============================================================

// SideChannelLog — append-only файл для критических событий при недоступной БД.
// Каждая запись — отдельная строка с UTC-таймстампом; O_SYNC для долговечности.
type SideChannelLog struct {
	mu   sync.Mutex
	path string
}

// NewSideChannelLog создаёт лог по указанному пути (каталог должен существовать).
func NewSideChannelLog(path string) *SideChannelLog {
	return &SideChannelLog{path: path}
}

// Write добавляет запись. Ошибки записи глотаются осознанно: side-channel —
// последний рубеж, падать из-за него нельзя; печатаем в stderr как fallback.
func (s *SideChannelLog) Write(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := time.Now().UTC().Format(time.RFC3339Nano) + " " + msg + "\n"
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|os.O_SYNC, 0o600)
	if err != nil {
		fmt.Fprint(os.Stderr, "side-channel open failed: "+err.Error()+"; msg: "+line)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		fmt.Fprint(os.Stderr, "side-channel write failed: "+err.Error()+"; msg: "+line)
	}
}

// ============================================================
// DB watchdog (раздел 17.4, 28)
// ============================================================

// DBWatchdog следит за доступностью БД; после N подряд неудачных ping —
// SAFE_HALT (торговать без источника истины нельзя).
type DBWatchdog struct {
	Ping      func(ctx context.Context) error
	Halter    *Halter
	Threshold int           // подряд неудач до SAFE_HALT
	Interval  time.Duration // период проверки
}

// Run — цикл до отмены ctx.
func (w *DBWatchdog) Run(ctx context.Context) {
	if w.Threshold <= 0 {
		w.Threshold = 3
	}
	if w.Interval <= 0 {
		w.Interval = 5 * time.Second
	}
	failures := 0
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, w.Interval)
			err := w.Ping(pctx)
			cancel()
			if err == nil {
				failures = 0
				continue
			}
			failures++
			if failures >= w.Threshold {
				if halted, _ := w.Halter.IsHalted(); !halted {
					// Персист замка скорее всего тоже упадёт (БД лежит) —
					// это ожидаемо: in-memory флаг остановит торговлю,
					// side-channel зафиксирует событие.
					_ = w.Halter.Halt(ctx, fmt.Sprintf("database unavailable (%d consecutive ping failures): %v", failures, err))
				}
			}
		}
	}
}

// ============================================================
// Supervisor — запуск и каскадная остановка компонентов
// ============================================================

// Component — долгоживущий компонент процесса (WS-менеджер, scanner loop, HTTP...).
// Run обязан уважать ctx и возвращаться после отмены.
type Component struct {
	Name string
	Run  func(ctx context.Context) error
}

// ErrComponentFailed — компонент завершился с ошибкой (весь процесс останавливается).
var ErrComponentFailed = errors.New("lifecycle: component failed")

// Supervisor запускает компоненты и останавливает ВСЕ при падении любого.
type Supervisor struct {
	ShutdownTimeout time.Duration
}

// Run блокируется до: (а) отмены parent ctx — штатная остановка; (б) падения
// любого компонента — каскадная остановка с ошибкой. Возвращает первую ошибку.
// Компоненты, не завершившиеся за ShutdownTimeout после отмены, бросаются
// (возвращается ошибка таймаута) — процесс не должен зависать навсегда.
func (s *Supervisor) Run(parent context.Context, components []Component) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	type exit struct {
		name string
		err  error
	}
	exits := make(chan exit, len(components))
	for _, c := range components {
		c := c
		go func() {
			err := c.Run(ctx)
			exits <- exit{name: c.Name, err: err}
		}()
	}

	remaining := len(components)
	var firstErr error

	// Ждём первый исход: падение компонента или отмена parent.
	select {
	case <-parent.Done():
		// Штатная остановка.
	case e := <-exits:
		remaining--
		if e.err != nil && !errors.Is(e.err, context.Canceled) {
			firstErr = fmt.Errorf("%w: %s: %v", ErrComponentFailed, e.name, e.err)
		}
	}
	cancel() // каскадная остановка всех остальных

	timeout := s.ShutdownTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for remaining > 0 {
		select {
		case e := <-exits:
			remaining--
			if firstErr == nil && e.err != nil && !errors.Is(e.err, context.Canceled) {
				firstErr = fmt.Errorf("%w: %s: %v", ErrComponentFailed, e.name, e.err)
			}
		case <-deadline.C:
			return fmt.Errorf("lifecycle: %d component(s) did not stop within %s (first error: %v)", remaining, timeout, firstErr)
		}
	}
	return firstErr
}
