// ws_manager.go — управление жизненным циклом WebSocket-соединений биржи (раздел 7.4, 6.3 промпта v2).
//
// Каждый адаптер биржи должен иметь независимый:
//   - rate limiter
//   - request queue
//   - circuit breaker
//   - reconnect backoff с jitter
//
// Этот файл реализует reconnect-strategy: exponential backoff с jitter + circuit breaker.
// Конкретная subscribe/unsubscribe-логика остаётся в адаптере; manager вызывает
// callback Connect при каждом (ре)коннекте и следит за обрывами.
package marketdata

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thecd/fundarbitrage/internal/domain"
)

// ErrAlreadyRunning — ошибка при повторном вызове Run().
var ErrAlreadyRunning = errors.New("marketdata: ConnectionManager уже запущен")

// BackoffConfig — параметры exponential backoff (раздел 7.4).
type BackoffConfig struct {
	Initial    time.Duration // первая задержка после обрыва (напр. 1s)
	Max        time.Duration // потолок задержки (напр. 60s)
	Multiplier float64       // множитель роста (напр. 2.0)
	Jitter     float64       // доля случайного разброса [0,1] (напр. 0.2 → ±20%)
}

// DefaultBackoff — рекомендованные параметры (раздел 7.4: backoff с jitter).
var DefaultBackoff = BackoffConfig{
	Initial:    1 * time.Second,
	Max:        60 * time.Second,
	Multiplier: 2.0,
	Jitter:     0.2,
}

// NextDelay считает задержку для данной попытки (attempt = 0 для первой).
// Без jitter формула: min(Initial × Multiplier^attempt, Max).
// С jitter: умножаем на (1 ± Jitter).
// Потолок применяется внутри цикла умножения — float64 не достигает +Inf.
func (b BackoffConfig) NextDelay(attempt int, r *rand.Rand) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	maxF := float64(b.Max)
	// Initial × Multiplier^attempt, с защитой от переполнения float:
	// прекращаем умножение, как только достигли потолка.
	d := float64(b.Initial)
	for i := 0; i < attempt; i++ {
		d *= b.Multiplier
		if d >= maxF {
			d = maxF
			break
		}
	}
	if d > maxF {
		d = maxF
	}
	// Jitter: равномерный множитель в [1-J, 1+J].
	if b.Jitter > 0 && r != nil {
		j := 1 + (r.Float64()*2-1)*b.Jitter
		d *= j
	}
	if d < 0 {
		return 0
	}
	return time.Duration(d)
}

// CircuitBreakerConfig — параметры circuit breaker (раздел 7.4).
type CircuitBreakerConfig struct {
	// FailureThreshold: при достижении этого числа последовательных неудачных (ре)коннектов
	// breaker «размыкается» — все новые попытки блокируются до ResetTimeout.
	FailureThreshold int
	// ResetTimeout: после размыкания — пауза перед одной «probe»-попыткой.
	ResetTimeout time.Duration
}

// DefaultCircuitBreaker — консервативные параметры.
var DefaultCircuitBreaker = CircuitBreakerConfig{
	FailureThreshold: 5,
	ResetTimeout:     5 * time.Minute,
}

// BreakerState — состояние circuit breaker.
type BreakerState int32

const (
	BreakerClosed BreakerState = iota // нормально, попытки разрешены
	BreakerOpen                       // разомкнут: попытки блокируются до reset
)

// ReconnectCallback — функция фактического (ре)коннекта, вызываемая manager-ом.
// Возвращает nil, если соединение установлено успешно; ошибку — если не удалось
// (manager повторит по backoff). ctx отменяется при shutdown.
type ReconnectCallback func(ctx context.Context) error

// ConnectionManager управляет жизненным циклом одного WS-соединения биржи.
// Запускает reconnect-loop в goroutine; наблюдает обрывы через NotifyDisconnect.
type ConnectionManager struct {
	exchange domain.ExchangeID

	backoff BackoffConfig
	breaker CircuitBreakerConfig

	// mu защищает все изменяемые поля ниже, включая state.
	mu        sync.Mutex
	state     int32     // BreakerState, защищён m.mu
	attempts  int       // последовательные неудачи
	openedAt  time.Time // когда разомкнут breaker (для ResetTimeout)
	connectCB ReconnectCallback

	// Метрики (раздел 17.1): атомарны, не нуждаются в mu.
	reconnectCount atomic.Int64
	circuitTrips   atomic.Int64

	// started — защита от двойного вызова Run.
	started atomic.Bool

	// rng для jitter. Инициализируется в Run; защищён локальной goroutine.
	rng *rand.Rand

	cancel context.CancelFunc
}

// NewConnectionManager создаёт manager для биржи.
func NewConnectionManager(ex domain.ExchangeID, cb ReconnectCallback) *ConnectionManager {
	return &ConnectionManager{
		exchange:  ex,
		backoff:   DefaultBackoff,
		breaker:   DefaultCircuitBreaker,
		connectCB: cb,
	}
}

// WithBackoff переопределяет параметры backoff.
func (m *ConnectionManager) WithBackoff(b BackoffConfig) *ConnectionManager {
	m.backoff = b
	return m
}

// WithBreaker переопределяет circuit breaker.
func (m *ConnectionManager) WithBreaker(b CircuitBreakerConfig) *ConnectionManager {
	m.breaker = b
	return m
}

// State возвращает текущее состояние breaker. Кратко захватывает mu для чтения.
func (m *ConnectionManager) State() BreakerState {
	m.mu.Lock()
	s := BreakerState(m.state)
	m.mu.Unlock()
	return s
}

// ReconnectCount — сколько всего reconnect-попыток сделано.
func (m *ConnectionManager) ReconnectCount() int64 {
	return m.reconnectCount.Load()
}

// CircuitTrips — сколько раз breaker размыкался.
func (m *ConnectionManager) CircuitTrips() int64 {
	return m.circuitTrips.Load()
}

// Run запускает reconnect-loop. Блокирует до отмены ctx.
// При первом вызове сразу делает connect; при обрывах — повторяет по backoff.
// Второй вызов Run возвращает ErrAlreadyRunning.
func (m *ConnectionManager) Run(ctx context.Context) error {
	if !m.started.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}

	// Свой rng на loop — jitter детерминирован в рамках одной goroutine.
	m.rng = rand.New(rand.NewSource(time.Now().UnixNano()))

	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	defer cancel()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Circuit breaker check.
		if !m.allowAttempt(ctx) {
			// breaker открыт: ждём ResetTimeout или отмены ctx.
			if err := sleepCtx(ctx, m.breaker.ResetTimeout); err != nil {
				return err
			}
			// probe-попытка: переходим в half-open (closed) и пробуем.
			// Также сбрасываем счётчик попыток, чтобы probe не вызвала
			// немедленное повторное размыкание (Open→Closed требует чистого старта).
			m.mu.Lock()
			if BreakerState(m.state) == BreakerOpen {
				m.state = int32(BreakerClosed)
				m.attempts = 0 // сброс при переходе Open→Closed
			}
			m.mu.Unlock()
		}

		m.reconnectCount.Add(1)
		err := m.connectCB(ctx)
		if err == nil {
			// Успех: сбрасываем счётчик неудач.
			m.mu.Lock()
			m.attempts = 0
			m.mu.Unlock()
			// Если обрыв произойдёт позже, callback должен снова вернуть ошибку
			// (для WS — это блокирующий вызов до обрыва). Ждём следующего reconnect.
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Неудача: инкремент счётчика, возможное размыкание breaker.
		m.mu.Lock()
		m.attempts++
		tripped := false
		if m.attempts >= m.breaker.FailureThreshold && BreakerState(m.state) != BreakerOpen {
			m.state = int32(BreakerOpen)
			m.openedAt = time.Now()
			tripped = true
		}
		attempts := m.attempts
		m.mu.Unlock()

		if tripped {
			m.circuitTrips.Add(1)
		}

		// Спим по backoff перед следующей попыткой.
		delay := m.backoff.NextDelay(attempts-1, m.rng)
		if err := sleepCtx(ctx, delay); err != nil {
			return err
		}
	}
}

// allowAttempt проверяет, можно ли делать попытку (breaker не открыт,
// или ResetTimeout уже прошёл).
func (m *ConnectionManager) allowAttempt(ctx context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if BreakerState(m.state) != BreakerOpen {
		return true
	}
	// Открыт. Проверяем ResetTimeout.
	if time.Since(m.openedAt) >= m.breaker.ResetTimeout {
		// Half-open → разрешаем одну probe.
		m.state = int32(BreakerClosed)
		m.attempts = 0 // сброс при переходе Open→Closed
		return true
	}
	return false
}

// Stop инициирует остановку reconnect-loop (аналог graceful shutdown, раздел 28).
func (m *ConnectionManager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// sleepCtx — sleep с отменой по ctx.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
