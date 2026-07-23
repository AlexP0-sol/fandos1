// Package marketdata реализует Level 2 кэш рыночных снимков с coalescing (раздел 7.3 промпта v2).
//
// Кэш хранит только ПОСЛЕДНЕЕ состояние каждого символа, а не каждое WS-сообщение.
// Это реализация coalescing-стратегии: public data можно «схлопывать», сохраняя последнее
// состояние и считая метрику replaced (перезаписей) для observability (раздел 17.1).
//
// Каждый символ хранится под atomic.Pointer → конкурентное чтение без блокировки в hot path.
// Update полностью заменяет snapshot (immutable per-update), что соответствует принципу
// «не мутировать живой объект».
package marketdata

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/thecd/fundarbitrage/internal/domain"
)

// cacheKey — составной ключ для snapshot.
type cacheKey struct {
	exchange domain.ExchangeID
	asset    domain.AssetSymbol
}

// snapshotEntry — атомарно-обновляемый слот одного символа.
type snapshotEntry struct {
	p        atomic.Pointer[domain.MarketSnapshot]
	replaced atomic.Int64 // число перезаписей (observability): каждый Swap с непустым old
}

// Cache — thread-safe coalescing-кэш рыночных снимков.
type Cache struct {
	mu      sync.RWMutex
	entries map[cacheKey]*snapshotEntry

	// Глобальные счётчики для метрик (раздел 17.1).
	totalUpdates  atomic.Int64
	totalReplaced atomic.Int64 // суммарно перезаписей (overwrites, counts replaced snapshots)
}

// New создаёт пустой кэш.
func New() *Cache {
	return &Cache{entries: make(map[cacheKey]*snapshotEntry)}
}

// getOrCreateEntry возвращает (или создаёт) entry для ключа под write-lock только при создании.
func (c *Cache) getOrCreateEntry(k cacheKey) *snapshotEntry {
	// Быстрый путь: читающая блокировка.
	c.mu.RLock()
	e, ok := c.entries[k]
	c.mu.RUnlock()
	if ok {
		return e
	}
	// Медленный путь: создаём под write-lock.
	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check под lock (гонка с другой goroutine).
	if e, ok := c.entries[k]; ok {
		return e
	}
	e = &snapshotEntry{}
	c.entries[k] = e
	return e
}

// Update заменяет снимок символа. Coalescing: если между вызовами Update никто не успел
// прочитать, промежуточные значения перезаписываются — это сознательно для public data (раздел 7.3).
// Каждая перезапись инкрементирует replaced-счётчик, позволяя оценить «плотность» обновлений.
func (c *Cache) Update(snap *domain.MarketSnapshot) {
	if snap == nil {
		return
	}
	k := cacheKey{exchange: snap.Exchange, asset: snap.CanonicalBaseAsset}
	e := c.getOrCreateEntry(k)

	// Если уже было значение — это перезапись (overwrite/replace).
	if old := e.p.Swap(snap); old != nil {
		e.replaced.Add(1)
		c.totalReplaced.Add(1)
	}
	c.totalUpdates.Add(1)
}

// Get возвращает текущий снимок символа или nil, если его нет.
// Возвращается указатель на immutable snapshot; мутировать нельзя.
func (c *Cache) Get(exchange domain.ExchangeID, asset domain.AssetSymbol) (*domain.MarketSnapshot, bool) {
	c.mu.RLock()
	e, ok := c.entries[cacheKey{exchange: exchange, asset: asset}]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	snap := e.p.Load()
	if snap == nil {
		return nil, false
	}
	return snap, true
}

// IsFresh — true, если снимок существует и его возраст ≤ maxAge (раздел 6.3, 7.1).
// now передаётся явно для тестируемости.
func (c *Cache) IsFresh(exchange domain.ExchangeID, asset domain.AssetSymbol, now time.Time, maxAge time.Duration) bool {
	snap, ok := c.Get(exchange, asset)
	if !ok {
		return false
	}
	if !snap.IsFresh {
		return false
	}
	return now.Sub(snap.LocalReceiveTime) <= maxAge
}

// SnapshotStats — per-symbol статистика для observability.
type SnapshotStats struct {
	// CoalescedUpdates — число перезаписей (overwrite): каждая замена непустого снимка.
	CoalescedUpdates int64
}

// StatsOf возвращает per-symbol статистику (для метрик).
func (c *Cache) StatsOf(exchange domain.ExchangeID, asset domain.AssetSymbol) (SnapshotStats, bool) {
	c.mu.RLock()
	e, ok := c.entries[cacheKey{exchange: exchange, asset: asset}]
	c.mu.RUnlock()
	if !ok {
		return SnapshotStats{}, false
	}
	return SnapshotStats{CoalescedUpdates: e.replaced.Load()}, true
}

// GlobalStats — глобальные счётчики кэша.
type GlobalStats struct {
	TotalUpdates int64
	// TotalReplaced — суммарное число перезаписей (overwrites):
	// сколько раз Update заменил уже существующий непрочитанный снимок.
	// Не потери данных, а измеримая интенсивность coalescing.
	TotalReplaced int64
	// TotalDrops — устаревший псевдоним TotalReplaced; оставлен для обратной совместимости.
	TotalDrops     int64
	TrackedSymbols int
}

// Global возвращает агрегированную статистику.
func (c *Cache) Global() GlobalStats {
	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	replaced := c.totalReplaced.Load()
	return GlobalStats{
		TotalUpdates:   c.totalUpdates.Load(),
		TotalReplaced:  replaced,
		TotalDrops:     replaced, // устаревший псевдоним
		TrackedSymbols: n,
	}
}
