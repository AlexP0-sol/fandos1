// Package clocks реализует синхронизацию часов (раздел 24 промпта v2):
// SNTP-замер offset против пула NTP-серверов, сверка с серверным временем бирж,
// сигнал остановки торговли при превышении MaxClockOffsetMs.
//
// Точный локальный час критичен: подпись запросов Binance/Bybit включает timestamp
// и recvWindow — дрейф часов приводит к отказам подписи (раздел 24.2/24.3).
package clocks

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// ntpEpochOffset — секунды между эпохой NTP (1900) и Unix (1970).
const ntpEpochOffset = 2208988800

// ErrNoServers — не задан ни один NTP-сервер.
var ErrNoServers = errors.New("clocks: no NTP servers configured")

// Sample — один замер offset.
type Sample struct {
	Server     string
	OffsetMs   int64 // положительный: локальные часы отстают
	RTTMs      int64
	MeasuredAt time.Time
}

// QuerySNTP выполняет один SNTP-запрос (RFC 4330, mode 3 client).
// addr — "host:port" (стандартный порт 123). Возвращает offset локальных часов
// относительно сервера: offset = ((t1-t0)+(t2-t3))/2.
func QuerySNTP(ctx context.Context, addr string, clock func() time.Time) (Sample, error) {
	if clock == nil {
		clock = time.Now
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return Sample{}, fmt.Errorf("clocks: dial %s: %w", addr, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(clock().Add(3 * time.Second))
	}

	// 48-байтный пакет; LI=0, VN=4, Mode=3 (client) → 0x23.
	req := make([]byte, 48)
	req[0] = 0x23

	t0 := clock()
	// Transmit timestamp клиента (для честного RTT сервер его эхом вернёт в originate).
	putNTPTime(req[40:], t0)

	if _, err := conn.Write(req); err != nil {
		return Sample{}, fmt.Errorf("clocks: write %s: %w", addr, err)
	}
	resp := make([]byte, 48)
	if _, err := conn.Read(resp); err != nil {
		return Sample{}, fmt.Errorf("clocks: read %s: %w", addr, err)
	}
	t3 := clock()

	mode := resp[0] & 0x07
	if mode != 4 && mode != 5 { // server / broadcast
		return Sample{}, fmt.Errorf("clocks: unexpected NTP mode %d from %s", mode, addr)
	}
	stratum := resp[1]
	if stratum == 0 {
		return Sample{}, fmt.Errorf("clocks: kiss-of-death from %s", addr)
	}

	t1 := ntpTime(resp[32:]) // receive time сервера
	t2 := ntpTime(resp[40:]) // transmit time сервера

	offset := (t1.Sub(t0) + t2.Sub(t3)) / 2
	rtt := t3.Sub(t0) - t2.Sub(t1)

	return Sample{
		Server:     addr,
		OffsetMs:   offset.Milliseconds(),
		RTTMs:      rtt.Milliseconds(),
		MeasuredAt: t3,
	}, nil
}

// putNTPTime кодирует time.Time в 64-битный NTP-формат (секунды.дробь).
func putNTPTime(b []byte, t time.Time) {
	secs := uint64(t.Unix()) + ntpEpochOffset
	frac := uint64(t.Nanosecond()) * (1 << 32) / 1_000_000_000
	binary.BigEndian.PutUint32(b[0:4], uint32(secs))
	binary.BigEndian.PutUint32(b[4:8], uint32(frac))
}

// ntpTime декодирует 64-битный NTP-timestamp.
func ntpTime(b []byte) time.Time {
	secs := binary.BigEndian.Uint32(b[0:4])
	frac := binary.BigEndian.Uint32(b[4:8])
	ns := int64(frac) * 1_000_000_000 >> 32
	return time.Unix(int64(secs)-ntpEpochOffset, ns)
}

// ============================================================
// Service — периодический замер + статус
// ============================================================

// Status — итог последнего замера для clock_sync_state (раздел 24).
type Status struct {
	OffsetMs    int64
	WithinLimit bool
	MeasuredAt  time.Time
	Source      string
	Err         string // непустой при неудачном замере
}

// StatusSink — приёмник статуса (persist в clock_sync_state, метрики, SAFE_HALT).
type StatusSink func(Status)

// Config — параметры сервиса.
type Config struct {
	Servers          []string      // ["pool.ntp.org:123", ...]
	MaxClockOffsetMs int64         // лимит из ColdConfig (раздел 24.3)
	Interval         time.Duration // период замера
	Clock            func() time.Time
}

// Service — периодический clock-sync.
type Service struct {
	cfg  Config
	sink StatusSink
}

// NewService создаёт сервис. sink обязателен (иначе замеры некуда девать).
func NewService(cfg Config, sink StatusSink) (*Service, error) {
	if len(cfg.Servers) == 0 {
		return nil, ErrNoServers
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if sink == nil {
		return nil, errors.New("clocks: nil sink")
	}
	return &Service{cfg: cfg, sink: sink}, nil
}

// MeasureOnce опрашивает серверы по очереди до первого успеха.
func (s *Service) MeasureOnce(ctx context.Context) Status {
	var lastErr error
	for _, srv := range s.cfg.Servers {
		qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		sample, err := QuerySNTP(qctx, srv, s.cfg.Clock)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		abs := sample.OffsetMs
		if abs < 0 {
			abs = -abs
		}
		return Status{
			OffsetMs:    sample.OffsetMs,
			WithinLimit: abs <= s.cfg.MaxClockOffsetMs,
			MeasuredAt:  sample.MeasuredAt,
			Source:      sample.Server,
		}
	}
	return Status{
		WithinLimit: false, // нет замера = нельзя доверять часам (консервативно)
		MeasuredAt:  s.cfg.Clock(),
		Err:         fmt.Sprintf("all NTP servers failed: %v", lastErr),
	}
}

// Run — цикл замеров до отмены ctx. Каждый результат уходит в sink.
func (s *Service) Run(ctx context.Context) {
	// Первый замер сразу, не ждём тикер.
	s.sink(s.MeasureOnce(ctx))
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sink(s.MeasureOnce(ctx))
		}
	}
}

// ExchangeOffset — сверка с серверным временем биржи (раздел 24.1):
// возвращает offset локальных часов относительно биржи в мс.
// halfRTT — половина измеренного round-trip (компенсация сетевой задержки).
func ExchangeOffset(exchangeTime, localAtReceive time.Time, halfRTT time.Duration) int64 {
	return exchangeTime.Add(halfRTT).Sub(localAtReceive).Milliseconds()
}
