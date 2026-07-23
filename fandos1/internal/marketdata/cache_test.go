package marketdata

import (
	"sync"
	"testing"
	"time"

	"github.com/thecd/fundarbitrage/internal/domain"
)

func mkSnap(ex domain.ExchangeID, asset string, ts time.Time) *domain.MarketSnapshot {
	return &domain.MarketSnapshot{
		Exchange:           ex,
		CanonicalBaseAsset: domain.AssetSymbol(asset),
		LocalReceiveTime:   ts,
		IsFresh:            true,
	}
}

// TestUpdateGet — базовый round-trip.
func TestUpdateGet(t *testing.T) {
	c := New()
	now := time.Now()
	c.Update(mkSnap(domain.ExchangeBinance, "BTC", now))

	snap, ok := c.Get(domain.ExchangeBinance, "BTC")
	if !ok {
		t.Fatal("expected BTC snapshot")
	}
	if snap.CanonicalBaseAsset != "BTC" {
		t.Errorf("asset=%s, want BTC", snap.CanonicalBaseAsset)
	}
}

// TestGetMissing — запрос отсутствующего символа.
func TestGetMissing(t *testing.T) {
	c := New()
	if _, ok := c.Get(domain.ExchangeBinance, "BTC"); ok {
		t.Error("expected miss for empty cache")
	}
}

// TestUpdateNil — nil-снимок игнорируется.
func TestUpdateNil(t *testing.T) {
	c := New()
	c.Update(nil)
	if _, ok := c.Get(domain.ExchangeBinance, "BTC"); ok {
		t.Error("nil update should not create entry")
	}
}

// TestCoalescing — множественные Updates схлопывают промежуточные значения.
func TestCoalescing(t *testing.T) {
	c := New()
	now := time.Now()

	// Первое обновление.
	c.Update(mkSnap(domain.ExchangeBinance, "BTC", now))
	// 5 «промежуточных» — они coalesce.
	for i := 0; i < 5; i++ {
		c.Update(mkSnap(domain.ExchangeBinance, "BTC", now.Add(time.Duration(i)*time.Second)))
	}
	// 6-е должно остаться видимым (последнее).
	final := mkSnap(domain.ExchangeBinance, "BTC", now.Add(99*time.Second))
	c.Update(final)

	got, ok := c.Get(domain.ExchangeBinance, "BTC")
	if !ok {
		t.Fatal("expected snapshot")
	}
	if !got.LocalReceiveTime.Equal(final.LocalReceiveTime) {
		t.Errorf("kept wrong snapshot: ts=%v, want %v", got.LocalReceiveTime, final.LocalReceiveTime)
	}

	stats, _ := c.StatsOf(domain.ExchangeBinance, "BTC")
	if stats.CoalescedUpdates != 6 {
		t.Errorf("coalesced=%d, want 6", stats.CoalescedUpdates)
	}
}

// TestIsFresh — свежесть по возрасту.
func TestIsFresh(t *testing.T) {
	c := New()
	now := time.Now()
	c.Update(mkSnap(domain.ExchangeBinance, "BTC", now.Add(-10*time.Second)))

	// maxAge=5s, snapshot 10s старый → не свежий.
	if c.IsFresh(domain.ExchangeBinance, "BTC", now, 5*time.Second) {
		t.Error("10s-old snapshot should be stale for 5s maxAge")
	}
	// maxAge=15s → свежий.
	if !c.IsFresh(domain.ExchangeBinance, "BTC", now, 15*time.Second) {
		t.Error("10s-old snapshot should be fresh for 15s maxAge")
	}
}

// TestIsFreshMissingSymbol — отсутствующего символа нет в кэше.
func TestIsFreshMissingSymbol(t *testing.T) {
	c := New()
	if c.IsFresh(domain.ExchangeBinance, "BTC", time.Now(), 60*time.Second) {
		t.Error("missing symbol should not be fresh")
	}
}

// TestIsFreshFlagFalse — снимок с IsFresh=false не свежий независимо от возраста.
func TestIsFreshFlagFalse(t *testing.T) {
	c := New()
	now := time.Now()
	s := mkSnap(domain.ExchangeBinance, "BTC", now)
	s.IsFresh = false
	c.Update(s)
	if c.IsFresh(domain.ExchangeBinance, "BTC", now, 60*time.Second) {
		t.Error("IsFresh=false flag should make snapshot stale")
	}
}

// TestGlobalStats — глобальные счётчики.
func TestGlobalStats(t *testing.T) {
	c := New()
	now := time.Now()
	// 3 разных символа, по 2 обновления каждый.
	for _, asset := range []string{"BTC", "ETH", "SOL"} {
		c.Update(mkSnap(domain.ExchangeBinance, asset, now))
		c.Update(mkSnap(domain.ExchangeBinance, asset, now))
	}
	g := c.Global()
	if g.TotalUpdates != 6 {
		t.Errorf("updates=%d, want 6", g.TotalUpdates)
	}
	// TotalReplaced — новое имя (перезаписи).
	if g.TotalReplaced != 3 {
		t.Errorf("replaced=%d, want 3 (по одному на каждый 2-й update)", g.TotalReplaced)
	}
	// TotalDrops — устаревший псевдоним, равен TotalReplaced.
	if g.TotalDrops != g.TotalReplaced {
		t.Errorf("TotalDrops(%d) != TotalReplaced(%d): псевдоним сломан", g.TotalDrops, g.TotalReplaced)
	}
	if g.TrackedSymbols != 3 {
		t.Errorf("tracked=%d, want 3", g.TrackedSymbols)
	}
}

// TestConcurrentUpdates — гонка обновлений не должна паниковать или приводить к потере.
// (полная race-проверка требует CGO; здесь проверяем корректность без -race.)
func TestConcurrentUpdates(t *testing.T) {
	c := New()
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Update(mkSnap(domain.ExchangeBinance, "BTC", now))
			}
		}(i)
	}
	// Параллельные читатели.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = c.Get(domain.ExchangeBinance, "BTC")
			}
		}()
	}
	wg.Wait()
	// В конце BTC точно существует.
	if _, ok := c.Get(domain.ExchangeBinance, "BTC"); !ok {
		t.Error("BTC should exist after concurrent updates")
	}
}

// TestMultipleExchanges — изоляция по биржам.
func TestMultipleExchanges(t *testing.T) {
	c := New()
	now := time.Now()
	c.Update(mkSnap(domain.ExchangeBinance, "BTC", now))
	c.Update(mkSnap(domain.ExchangeBybit, "BTC", now))

	if _, ok := c.Get(domain.ExchangeBinance, "BTC"); !ok {
		t.Error("Binance BTC missing")
	}
	if _, ok := c.Get(domain.ExchangeBybit, "BTC"); !ok {
		t.Error("Bybit BTC missing")
	}
	if _, ok := c.Get(domain.ExchangeOKX, "BTC"); ok {
		t.Error("OKX BTC should not exist")
	}
}
