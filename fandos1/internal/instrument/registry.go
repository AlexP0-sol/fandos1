// Package instrument реализует канонический реестр инструментов (раздел 6.1, 7.1 промпта v2).
//
// Реестр хранит нормализованные CanonicalInstrument по (ExchangeID, CanonicalBaseAsset).
// Это Level 1 архитектуры сканера: обновляется редко (30–60 минут), не на каждый тик.
//
// ВАЖНО (раздел 6.1): символы нельзя строить конкатенацией asset+USDT. Форматы различаются
// между биржами (BTCUSDT, BTC_USDT, BTC-USDT-SWAP, XBTUSDTM и т.д.), поэтому реестр хранит
// явное mapping CanonicalBaseAsset → ExchangeSymbol per-биржа.
package instrument

import (
	"fmt"
	"sync"
	"time"

	"github.com/thecd/fundarbitrage/internal/domain"
)

// Registry — thread-safe in-memory реестр инструментов.
// Источник данных — exchange.GetInstruments(); персистентность в БД (таблица exchange_instruments,
// миграция 0002 — заготовка). В памяти хранится актуальный снимок для быстрого lookup в hot path.
type Registry struct {
	mu sync.RWMutex

	// byKey — первичный lookup по (exchange, canonical asset).
	byKey map[registryKey]domain.CanonicalInstrument

	// byExchangeSymbol — обратный lookup: биржевый символ → инструмент (для входящих WS-событий).
	byExchangeSymbol map[symbolKey]domain.CanonicalInstrument

	// byExchange — все инструменты одной биржи (для итерации сканером).
	byExchange map[domain.ExchangeID][]domain.CanonicalInstrument

	// byAsset — все инструменты одного канонического актива по всем биржам
	// (быстрый match long/short кандидатов одной пары на разных биржах).
	byAsset map[domain.AssetSymbol][]domain.CanonicalInstrument

	// canonicalAssets — упорядоченное множество канонических активов с хотя бы одним инструментом.
	canonicalAssets []domain.AssetSymbol

	// lastRefresh — время последнего успешного обновления (для health/UI).
	lastRefresh time.Time
}

type registryKey struct {
	exchange domain.ExchangeID
	asset    domain.AssetSymbol
}

type symbolKey struct {
	exchange domain.ExchangeID
	symbol   domain.ExchangeSymbol
}

// New создаёт пустой реестр.
func New() *Registry {
	return &Registry{
		byKey:            make(map[registryKey]domain.CanonicalInstrument),
		byExchangeSymbol: make(map[symbolKey]domain.CanonicalInstrument),
		byExchange:       make(map[domain.ExchangeID][]domain.CanonicalInstrument),
		byAsset:          make(map[domain.AssetSymbol][]domain.CanonicalInstrument),
	}
}

// Replace атомарно заменяет всё содержимое реестра новым снимком инструментов.
// Вызывается Level 1-задачей (каждые 30–60 мин) после получения GetInstruments от биржи.
// Все in-flight читатели продолжат видеть консистентный снимок (RWMutex).
func (r *Registry) Replace(all []domain.CanonicalInstrument) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Очищаем старые maps.
	r.byKey = make(map[registryKey]domain.CanonicalInstrument, len(all))
	r.byExchangeSymbol = make(map[symbolKey]domain.CanonicalInstrument, len(all))
	r.byExchange = make(map[domain.ExchangeID][]domain.CanonicalInstrument)
	r.byAsset = make(map[domain.AssetSymbol][]domain.CanonicalInstrument)

	assetSet := make(map[domain.AssetSymbol]struct{})

	for _, ins := range all {
		// Пропускаем инструменты не нашего типа (раздел 1.1: только LINEAR_USDT_PERPETUAL).
		if ins.InstrumentType != domain.InstrumentLinearUSDTPerpetual {
			continue
		}
		// Пропускаем инструменты с невалидным exchange id.
		if !ins.Exchange.IsValid() {
			continue
		}

		key := registryKey{exchange: ins.Exchange, asset: ins.CanonicalBaseAsset}
		// Если уже есть запись с тем же ключом — это конфликт нормализации; последняя выигрывает,
		// но это сигнал о баге в адаптере. В production здесь должен быть metric/log; в v1 — silent override.
		r.byKey[key] = ins
		r.byExchangeSymbol[symbolKey{exchange: ins.Exchange, symbol: ins.ExchangeSymbol}] = ins
		r.byExchange[ins.Exchange] = append(r.byExchange[ins.Exchange], ins)
		r.byAsset[ins.CanonicalBaseAsset] = append(r.byAsset[ins.CanonicalBaseAsset], ins)
		assetSet[ins.CanonicalBaseAsset] = struct{}{}
	}

	// Упорядоченный список канонических активов.
	r.canonicalAssets = r.canonicalAssets[:0]
	for a := range assetSet {
		r.canonicalAssets = append(r.canonicalAssets, a)
	}

	r.lastRefresh = time.Now()
}

// Get возвращает инструмент по (exchange, canonical asset).
// ok=false, если инструмент не зарегистрирован.
func (r *Registry) Get(exchange domain.ExchangeID, asset domain.AssetSymbol) (domain.CanonicalInstrument, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ins, ok := r.byKey[registryKey{exchange: exchange, asset: asset}]
	return ins, ok
}

// LookupSymbol возвращает канонический актив по биржевому символу (обратный lookup).
// Используется при разборе входящих WS-сообщений, где есть только символ биржи.
func (r *Registry) LookupSymbol(exchange domain.ExchangeID, symbol domain.ExchangeSymbol) (domain.CanonicalInstrument, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ins, ok := r.byExchangeSymbol[symbolKey{exchange: exchange, symbol: symbol}]
	return ins, ok
}

// SymbolFor возвращает биржевой символ для canonical asset.
// ok=false если инструмент не зарегистрирован.
// Это замена конкатенации asset+USDT (раздел 6.1): всегда через реестр.
func (r *Registry) SymbolFor(exchange domain.ExchangeID, asset domain.AssetSymbol) (domain.ExchangeSymbol, bool) {
	ins, ok := r.Get(exchange, asset)
	if !ok {
		return "", false
	}
	return ins.ExchangeSymbol, true
}

// AssetsByExchange возвращает инструменты одной биржи (для итерации Level 2-сканером).
// Возвращает копию слайса, чтобы вызывающий не мутировал внутреннее состояние.
func (r *Registry) AssetsByExchange(exchange domain.ExchangeID) []domain.CanonicalInstrument {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.byExchange[exchange]
	out := make([]domain.CanonicalInstrument, len(src))
	copy(out, src)
	return out
}

// InstrumentsForAsset возвращает все инструменты одного canonical asset по всем биржам.
// Используется scanner-ом для построения пар (long на бирже A, short на бирже B) — раздел 8.
func (r *Registry) InstrumentsForAsset(asset domain.AssetSymbol) []domain.CanonicalInstrument {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.byAsset[asset]
	out := make([]domain.CanonicalInstrument, len(src))
	copy(out, src)
	return out
}

// AllCanonicalAssets возвращает упорядоченный список канонических активов.
func (r *Registry) AllCanonicalAssets() []domain.AssetSymbol {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.AssetSymbol, len(r.canonicalAssets))
	copy(out, r.canonicalAssets)
	return out
}

// LastRefresh — время последнего успешного Replace (для health/UI).
func (r *Registry) LastRefresh() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastRefresh
}

// Stats — сводная статистика для observability (раздел 17).
type Stats struct {
	TotalInstruments int
	ByExchange       map[domain.ExchangeID]int
	ByAsset          map[domain.AssetSymbol]int
	LastRefresh      time.Time
}

// Stats возвращает сводку по реестру.
func (r *Registry) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := Stats{
		TotalInstruments: len(r.byKey),
		ByExchange:       make(map[domain.ExchangeID]int, len(r.byExchange)),
		ByAsset:          make(map[domain.AssetSymbol]int, len(r.byAsset)),
		LastRefresh:      r.lastRefresh,
	}
	for ex, list := range r.byExchange {
		s.ByExchange[ex] = len(list)
	}
	for a, list := range r.byAsset {
		s.ByAsset[a] = len(list)
	}
	return s
}

// String — краткое описание для логов.
func (s Stats) String() string {
	return fmt.Sprintf("registry{total=%d, exchanges=%d, assets=%d, refreshed=%s}",
		s.TotalInstruments, len(s.ByExchange), len(s.ByAsset), s.LastRefresh.Format(time.RFC3339))
}
