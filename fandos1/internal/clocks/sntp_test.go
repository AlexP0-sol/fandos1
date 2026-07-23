package clocks

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// fakeNTPServer поднимает локальный UDP-сервер, отвечающий как NTP-сервер
// с заданным сдвигом часов serverSkew относительно локальных.
func fakeNTPServer(t *testing.T, serverSkew time.Duration, mode byte, stratum byte) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })

	go func() {
		buf := make([]byte, 48)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 {
				continue
			}
			now := time.Now().Add(serverSkew)
			resp := make([]byte, 48)
			resp[0] = mode // LI/VN/Mode; для валидного ответа сервер = 0x24 (VN=4, mode=4)
			resp[1] = stratum
			// originate = transmit клиента (эхо).
			copy(resp[24:32], buf[40:48])
			// receive и transmit сервера — «сейчас» по часам сервера.
			putNTPTime(resp[32:], now)
			putNTPTime(resp[40:], now)
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestQuerySNTPZeroOffset(t *testing.T) {
	addr := fakeNTPServer(t, 0, 0x24, 2)
	s, err := QuerySNTP(context.Background(), addr, time.Now)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if s.OffsetMs < -50 || s.OffsetMs > 50 {
		t.Errorf("offset = %d ms, want ~0 (loopback)", s.OffsetMs)
	}
}

func TestQuerySNTPSkewedServer(t *testing.T) {
	// Сервер «спешит» на 2 секунды → положительный offset ~2000 мс.
	addr := fakeNTPServer(t, 2*time.Second, 0x24, 2)
	s, err := QuerySNTP(context.Background(), addr, time.Now)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if s.OffsetMs < 1900 || s.OffsetMs > 2100 {
		t.Errorf("offset = %d ms, want ~2000", s.OffsetMs)
	}
}

func TestQuerySNTPKissOfDeath(t *testing.T) {
	addr := fakeNTPServer(t, 0, 0x24, 0) // stratum 0 = KoD
	if _, err := QuerySNTP(context.Background(), addr, time.Now); err == nil {
		t.Error("kiss-of-death must be rejected")
	}
}

func TestQuerySNTPWrongMode(t *testing.T) {
	addr := fakeNTPServer(t, 0, 0x23, 2) // mode 3 (client) — не серверный ответ
	if _, err := QuerySNTP(context.Background(), addr, time.Now); err == nil {
		t.Error("non-server mode must be rejected")
	}
}

func TestServiceMeasureOnceWithinLimit(t *testing.T) {
	addr := fakeNTPServer(t, 0, 0x24, 2)
	var got Status
	svc, err := NewService(Config{
		Servers:          []string{addr},
		MaxClockOffsetMs: 500,
	}, func(st Status) { got = st })
	if err != nil {
		t.Fatal(err)
	}
	st := svc.MeasureOnce(context.Background())
	if !st.WithinLimit {
		t.Errorf("offset %d ms must be within 500ms limit", st.OffsetMs)
	}
	_ = got
}

func TestServiceMeasureOnceExceedsLimit(t *testing.T) {
	addr := fakeNTPServer(t, 3*time.Second, 0x24, 2)
	svc, err := NewService(Config{
		Servers:          []string{addr},
		MaxClockOffsetMs: 1000,
	}, func(Status) {})
	if err != nil {
		t.Fatal(err)
	}
	st := svc.MeasureOnce(context.Background())
	if st.WithinLimit {
		t.Errorf("offset %d ms must EXCEED 1000ms limit", st.OffsetMs)
	}
}

func TestServiceFallbackToSecondServer(t *testing.T) {
	dead := "127.0.0.1:1" // закрытый порт
	alive := fakeNTPServer(t, 0, 0x24, 2)
	svc, err := NewService(Config{
		Servers:          []string{dead, alive},
		MaxClockOffsetMs: 500,
	}, func(Status) {})
	if err != nil {
		t.Fatal(err)
	}
	st := svc.MeasureOnce(context.Background())
	if st.Err != "" {
		t.Fatalf("expected success via second server, got err %q", st.Err)
	}
	if st.Source != alive {
		t.Errorf("source = %s, want %s", st.Source, alive)
	}
}

func TestServiceAllServersDead(t *testing.T) {
	svc, err := NewService(Config{
		Servers:          []string{"127.0.0.1:1"},
		MaxClockOffsetMs: 500,
	}, func(Status) {})
	if err != nil {
		t.Fatal(err)
	}
	st := svc.MeasureOnce(context.Background())
	if st.Err == "" || st.WithinLimit {
		t.Error("all-dead must produce error status with WithinLimit=false (консервативно)")
	}
}

func TestNewServiceValidation(t *testing.T) {
	if _, err := NewService(Config{}, func(Status) {}); err == nil {
		t.Error("no servers must be rejected")
	}
	if _, err := NewService(Config{Servers: []string{"x:123"}}, nil); err == nil {
		t.Error("nil sink must be rejected")
	}
}

func TestNTPTimeRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Microsecond)
	b := make([]byte, 8)
	putNTPTime(b, now)
	back := ntpTime(b)
	if d := back.Sub(now); d < -time.Millisecond || d > time.Millisecond {
		t.Errorf("round-trip drift %v", d)
	}
	// Проверка порядка байт: первые 4 байта — секунды NTP.
	secs := binary.BigEndian.Uint32(b[:4])
	if int64(secs) != now.Unix()+ntpEpochOffset {
		t.Error("seconds field mismatch")
	}
}

func TestExchangeOffset(t *testing.T) {
	local := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	exch := local.Add(150 * time.Millisecond)
	// Биржа «спешит» на 150мс, RTT-компенсация 20мс → offset = 170мс.
	if got := ExchangeOffset(exch, local, 20*time.Millisecond); got != 170 {
		t.Errorf("exchange offset = %d, want 170", got)
	}
}
