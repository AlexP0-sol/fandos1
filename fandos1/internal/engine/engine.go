// Package engine — оркестратор торгового цикла (разделы 9–11 промпта v2):
// eligible-кандидат → аллокация → persistent state machine → preflight →
// параллельный вход обеих ног → repair дельта-дисбаланса → HEDGED → MONITORING →
// выход (запрос оператора / смена знака funding) → coordinated close → CLOSED.
//
// Движок НЕ решает «что хорошо» (это scanner/strategy) и НЕ знает биржевых
// деталей (это adapters). Он владеет последовательностью и инвариантом
// дельта-нейтральности: одна нога никогда не остаётся без другой незамеченной.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/exchange"
	"github.com/thecd/fundarbitrage/internal/execution"
	"github.com/thecd/fundarbitrage/internal/instrument"
	"github.com/thecd/fundarbitrage/internal/marketdata"
	"github.com/thecd/fundarbitrage/internal/portfolio"
	"github.com/thecd/fundarbitrage/internal/repository"
	"github.com/thecd/fundarbitrage/internal/scanner"
)

// Halter — быстрая проверка SAFE_HALT (lifecycle.Halter).
type Halter interface {
	IsHalted() (bool, string)
}

// Notifier — уведомления оператора (nil-safe в движке).
type Notifier interface {
	Notify(ctx context.Context, severity, text string)
}

// Deps — зависимости движка.
type Deps struct {
	Adapters  map[domain.ExchangeID]exchange.ExchangeAdapter
	Executors map[domain.ExchangeID]*execution.OrderExecutor
	Registry  *instrument.Registry
	Cache     *marketdata.Cache
	Scanner   *scanner.Scanner
	Positions *repository.PositionRepo
	Persister portfolio.Persister
	Orders    *repository.OrderRepo
	Halter    Halter
	Notifier  Notifier // может быть nil
	Log       *slog.Logger
	Clock     func() time.Time
}

// Config — параметры движка (подмножество HotSettings, раздел 5).
type Config struct {
	Scan             scanner.Config
	MaxOpenPositions int
	// OrdersEnabled=false — наблюдательный режим: сканируем, но не открываем
	// (owner не настроен / предусловия не выполнены).
	OrdersEnabled            bool
	ProtectionTicks          int
	MaxRequotes              int
	DeltaToleranceBase       decimal.Decimal
	ExitIfFundingSignChanges bool
	Interval                 time.Duration
}

// openPosition — активная позиция с резолвленными символами ног.
type openPosition struct {
	pos      *portfolio.Position
	longSym  domain.ExchangeSymbol
	shortSym domain.ExchangeSymbol
	longEx   domain.ExchangeID
	shortEx  domain.ExchangeID
	asset    domain.AssetSymbol
	tickSize decimal.Decimal
}

// Engine — торговый оркестратор.
type Engine struct {
	deps Deps
	cfg  Config

	mu        sync.Mutex
	open      map[domain.PositionID]*openPosition
	closeReq  map[domain.PositionID]string // position → причина запрошенного закрытия
	seq       int64                        // монотонный суффикс для PositionID
	notifyMu  sync.Mutex
	lastNotif map[string]time.Time
}

// New создаёт движок. deps.Positions нужен только для Restore (может быть nil
// в unit-тестах); deps.Orders/Notifier — опциональны.
func New(deps Deps, cfg Config) (*Engine, error) {
	if deps.Scanner == nil || deps.Persister == nil ||
		deps.Registry == nil || deps.Cache == nil || deps.Halter == nil || deps.Log == nil {
		return nil, errors.New("engine: missing required dependency")
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if cfg.MaxOpenPositions <= 0 {
		cfg.MaxOpenPositions = 1
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.MaxRequotes <= 0 {
		cfg.MaxRequotes = 3
	}
	return &Engine{
		deps:      deps,
		cfg:       cfg,
		open:      make(map[domain.PositionID]*openPosition),
		closeReq:  make(map[domain.PositionID]string),
		lastNotif: make(map[string]time.Time),
	}, nil
}

// RequestClose помечает позицию к закрытию (вызывается outbox-обработчиком
// по запросу оператора из Mini App). Потокобезопасно.
func (e *Engine) RequestClose(positionID, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if reason == "" {
		reason = "operator request"
	}
	e.closeReq[domain.PositionID(positionID)] = reason
	e.deps.Log.Info("close requested", "position_id", positionID, "reason", reason)
}

// Restore загружает активные позиции из БД при старте (раздел 15.2: истина в БД).
// Символы ног резолвятся через registry; если реестр ещё пуст, резолв повторится
// в мониторинге на следующем тике.
func (e *Engine) Restore(ctx context.Context) error {
	if e.deps.Positions == nil {
		return errors.New("engine: Restore requires Positions repository")
	}
	active, err := e.deps.Positions.LoadByStates(ctx,
		"OPENING", "PARTIALLY_HEDGED", "HEDGED", "MONITORING",
		"EXIT_REQUESTED", "EXITING", "RECONCILING", "DEGRADED")
	if err != nil {
		return fmt.Errorf("engine: restore positions: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range active {
		snap := p.Snapshot()
		op := &openPosition{
			pos:    p,
			asset:  snap.Asset,
			longEx: snap.LongExchange, shortEx: snap.ShortExchange,
		}
		e.resolveSymbols(op)
		e.open[snap.ID] = op
		e.deps.Log.Info("position restored", "position_id", string(snap.ID), "state", string(snap.State))
	}
	return nil
}

// resolveSymbols дополняет openPosition символами/тиком из реестра (идемпотентно).
func (e *Engine) resolveSymbols(op *openPosition) bool {
	if op.longSym != "" && op.shortSym != "" && !op.tickSize.IsZero() {
		return true
	}
	li, lok := e.deps.Registry.Get(op.longEx, op.asset)
	si, sok := e.deps.Registry.Get(op.shortEx, op.asset)
	if !lok || !sok {
		return false
	}
	op.longSym, op.shortSym = li.ExchangeSymbol, si.ExchangeSymbol
	op.tickSize = decimal.Max(li.TickSize, si.TickSize)
	return true
}

// Run — основной цикл до отмены ctx.
func (e *Engine) Run(ctx context.Context) error {
	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			e.Tick(ctx)
		}
	}
}

// Tick — одна итерация: мониторинг открытых позиций + попытка открытия новой.
func (e *Engine) Tick(ctx context.Context) {
	if halted, reason := e.deps.Halter.IsHalted(); halted {
		e.notifyOnce(ctx, "engine_halted", "WARN", "engine paused: SAFE_HALT: "+reason)
		return
	}
	e.monitor(ctx)
	e.tryOpen(ctx)
}

// ============================================================
// Мониторинг и выход (раздел 11)
// ============================================================

func (e *Engine) monitor(ctx context.Context) {
	e.mu.Lock()
	ops := make([]*openPosition, 0, len(e.open))
	for _, op := range e.open {
		ops = append(ops, op)
	}
	e.mu.Unlock()

	for _, op := range ops {
		snap := op.pos.Snapshot()
		switch snap.State {
		case portfolio.StateMonitoring:
			// 1. Запрос оператора.
			e.mu.Lock()
			reason, requested := e.closeReq[snap.ID]
			e.mu.Unlock()
			if requested {
				e.closePosition(ctx, op, reason)
				continue
			}
			// 2. Смена знака funding-спреда (раздел 11.2).
			if e.cfg.ExitIfFundingSignChanges && e.fundingSpreadFlipped(op) {
				e.closePosition(ctx, op, "funding spread sign changed")
				continue
			}
		case portfolio.StateDegraded:
			e.notifyOnce(ctx, "degraded-"+string(snap.ID), "CRITICAL",
				fmt.Sprintf("position %s DEGRADED: требуется внимание оператора", snap.ID))
			// Запрос оператора на закрытие работает и из DEGRADED.
			e.mu.Lock()
			reason, requested := e.closeReq[snap.ID]
			e.mu.Unlock()
			if requested {
				e.closePosition(ctx, op, reason)
			}
		case portfolio.StateClosed, portfolio.StateFailed:
			e.forget(snap.ID)
		}
	}
}

// fundingSpreadFlipped — true, если ожидаемый funding-спред стал неположительным:
// позиция открывалась в расчёте на rate(shortEx) > rate(longEx).
func (e *Engine) fundingSpreadFlipped(op *openPosition) bool {
	longSnap, lok := e.deps.Cache.Get(op.longEx, op.asset)
	shortSnap, sok := e.deps.Cache.Get(op.shortEx, op.asset)
	if !lok || !sok {
		return false // нет данных — не дёргаемся (staleness ловит риск-контур)
	}
	spread := shortSnap.PredictedFundingRate.Sub(longSnap.PredictedFundingRate)
	return !spread.IsPositive()
}

// closePosition — координированное закрытие (раздел 11.3) с переходами state machine.
func (e *Engine) closePosition(ctx context.Context, op *openPosition, reason string) {
	if !e.resolveSymbols(op) {
		e.deps.Log.Warn("close deferred: symbols not resolved yet", "position_id", string(op.pos.Snapshot().ID))
		return
	}
	now := e.deps.Clock()
	snap := op.pos.Snapshot()
	log := e.deps.Log.With("position_id", string(snap.ID))

	if snap.State != portfolio.StateExitRequested {
		if err := op.pos.TransitionTo(portfolio.StateExitRequested, now, reason, "system:engine", e.deps.Persister); err != nil {
			log.Error("transition EXIT_REQUESTED failed", "err", err.Error())
			return
		}
	}
	if err := op.pos.TransitionTo(portfolio.StateExiting, now, reason, "system:engine", e.deps.Persister); err != nil {
		log.Error("transition EXITING failed", "err", err.Error())
		return
	}

	cur := op.pos.Snapshot()
	res, err := execution.CoordinatedClose(ctx, execution.CloseRequest{
		PositionID:        cur.ID,
		LongSymbol:        op.longSym,
		LongRemaining:     cur.LongBaseQty,
		LongExecutor:      e.deps.Executors[op.longEx],
		ShortSymbol:       op.shortSym,
		ShortRemaining:    cur.ShortBaseQty,
		ShortExecutor:     e.deps.Executors[op.shortEx],
		LongBookProvider:  cacheBook{cache: e.deps.Cache, exchange: op.longEx, asset: op.asset},
		ShortBookProvider: cacheBook{cache: e.deps.Cache, exchange: op.shortEx, asset: op.asset},
	}, execution.CloseConfig{
		CloseProtectionTicks: e.cfg.ProtectionTicks,
		MaxRequotes:          e.cfg.MaxRequotes,
		TickSize:             op.tickSize,
	})

	// Обновляем остатки по ФАКТИЧЕСКИ закрытому (честный per-leg учёт).
	newLong := cur.LongBaseQty.Sub(res.LongClosedQty)
	newShort := cur.ShortBaseQty.Sub(res.ShortClosedQty)
	if qerr := op.pos.SetQuantities(newLong, newShort, e.deps.Clock()); qerr != nil {
		log.Error("set quantities after close failed", "err", qerr.Error())
	}

	switch {
	case err == nil:
		// Полное закрытие: RECONCILING → CLOSED.
		_ = op.pos.TransitionTo(portfolio.StateReconciling, e.deps.Clock(), "close complete, verifying", "system:engine", e.deps.Persister)
		if terr := op.pos.TransitionTo(portfolio.StateClosed, e.deps.Clock(), reason, "system:engine", e.deps.Persister); terr != nil {
			log.Error("transition CLOSED failed", "err", terr.Error())
			return
		}
		log.Info("position closed",
			"long_closed", res.LongClosedQty.String(), "short_closed", res.ShortClosedQty.String(), "reason", reason)
		e.notify(ctx, "INFO", fmt.Sprintf("позиция %s закрыта (%s)", snap.ID, reason))
		e.forget(snap.ID)

	case errors.Is(err, execution.ErrCloseAmbiguous):
		// Состояние ноги неизвестно — reconciliation обязателен до любых ордеров.
		_ = op.pos.TransitionTo(portfolio.StateReconciling, e.deps.Clock(), res.Reason, "system:engine", e.deps.Persister)
		_ = op.pos.TransitionTo(portfolio.StateDegraded, e.deps.Clock(),
			"ambiguous close: manual reconciliation required", "system:engine", e.deps.Persister)
		e.notify(ctx, "CRITICAL", fmt.Sprintf("позиция %s: неопределённое состояние ноги при закрытии — нужна ручная сверка", snap.ID))

	default:
		// Частичное закрытие / нет ликвидности: DEGRADED, остатки зафиксированы.
		_ = op.pos.TransitionTo(portfolio.StateDegraded, e.deps.Clock(), res.Reason, "system:engine", e.deps.Persister)
		e.notify(ctx, "ERROR", fmt.Sprintf("позиция %s: закрытие не завершено (%s), остатки long=%s short=%s",
			snap.ID, res.Reason, res.ResidualLongQty.String(), res.ResidualShortQty.String()))
	}
	// Запрос закрытия обработан (успешно или деградацией) — снимаем флаг.
	e.mu.Lock()
	delete(e.closeReq, snap.ID)
	e.mu.Unlock()
}

// ============================================================
// Открытие (разделы 9–10)
// ============================================================

func (e *Engine) tryOpen(ctx context.Context) {
	e.mu.Lock()
	openCount := len(e.open)
	e.mu.Unlock()
	if openCount >= e.cfg.MaxOpenPositions {
		return
	}

	cands := e.deps.Scanner.Scan(e.cfg.Scan, e.deps.Clock())
	best := e.pickCandidate(cands)
	if best == nil {
		return
	}
	e.deps.Log.Info("eligible candidate",
		"asset", string(best.Asset),
		"long", string(best.LongExchange), "short", string(best.ShortExchange),
		"net_pnl_usdt", best.PnLBreakdown.Net.String(),
		"score", best.CompositeScore.StringFixed(3))

	if !e.cfg.OrdersEnabled {
		e.notifyOnce(ctx, "observe-only", "WARN",
			"наблюдательный режим: кандидаты есть, но открытие отключено (предусловия/owner)")
		return
	}
	e.openPosition(ctx, best)
}

// pickCandidate — лучший eligible, не дублирующий уже открытую пару актив+биржи.
func (e *Engine) pickCandidate(cands []scanner.Candidate) *scanner.Candidate {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range cands {
		c := &cands[i]
		if !c.Eligible {
			continue
		}
		dup := false
		for _, op := range e.open {
			if op.asset == c.Asset {
				dup = true
				break
			}
		}
		if !dup {
			return c
		}
	}
	return nil
}

// openPosition — полный цикл открытия одной позиции.
func (e *Engine) openPosition(ctx context.Context, cand *scanner.Candidate) {
	now := e.deps.Clock()
	e.mu.Lock()
	e.seq++
	id := domain.PositionID(fmt.Sprintf("P%d-%d-%s", now.UnixMilli(), e.seq,
		strings.ToUpper(string(cand.Asset))))
	e.mu.Unlock()

	log := e.deps.Log.With("position_id", string(id), "asset", string(cand.Asset))
	pos := portfolio.NewPosition(id, cand.Asset, cand.LongExchange, cand.ShortExchange, now)

	li, lok := e.deps.Registry.Get(cand.LongExchange, cand.Asset)
	si, sok := e.deps.Registry.Get(cand.ShortExchange, cand.Asset)
	if !lok || !sok {
		log.Warn("instruments missing in registry, skip")
		return
	}
	op := &openPosition{
		pos: pos, asset: cand.Asset,
		longEx: cand.LongExchange, shortEx: cand.ShortExchange,
		longSym: li.ExchangeSymbol, shortSym: si.ExchangeSymbol,
		tickSize: decimal.Max(li.TickSize, si.TickSize),
	}

	// Объём: целевой qty, квантованный по шагам ОБЕИХ бирж (раздел 9.2: вниз).
	step := decimal.Max(li.QtyStep, si.QtyStep)
	qty, _ := e.cfg.Scan.TargetQty.Quantize(step)
	if !qty.IsPositive() || qty.LessThan(decimal.Max(li.MinQty, si.MinQty)) {
		log.Warn("target qty below exchange minimums, skip",
			"qty", qty.String(), "min_long", li.MinQty.String(), "min_short", si.MinQty.String())
		return
	}
	pos.TargetBaseQty = qty

	transition := func(to portfolio.State, reason string) bool {
		if err := pos.TransitionTo(to, e.deps.Clock(), reason, "system:engine", e.deps.Persister); err != nil {
			log.Error("transition failed", "to", string(to), "err", err.Error())
			return false
		}
		return true
	}

	if !transition(portfolio.StateQualified, "passed filters+eligibility") {
		return
	}
	if !transition(portfolio.StatePreparing, "preflight") {
		return
	}

	// Preflight (раздел 10.1): плечо/режимы. Ошибки некритичных настроек — warn.
	for _, leg := range []struct {
		ex  domain.ExchangeID
		sym domain.ExchangeSymbol
	}{{op.longEx, op.longSym}, {op.shortEx, op.shortSym}} {
		if a, ok := e.deps.Adapters[leg.ex]; ok {
			if err := a.SetLeverage(ctx, domain.SetLeverageRequest{Symbol: leg.sym, Leverage: decimal.FromInt(2)}); err != nil {
				log.Warn("preflight SetLeverage", "exchange", string(leg.ex), "err", err.Error())
			}
		}
	}

	if !transition(portfolio.StateOpening, "placing entry orders") {
		return
	}

	// Вход: обе ноги параллельно (marketable-limit IOC с protection от BBO).
	longSnap, lok2 := e.deps.Cache.Get(op.longEx, op.asset)
	shortSnap, sok2 := e.deps.Cache.Get(op.shortEx, op.asset)
	if !lok2 || !sok2 {
		_ = transition(portfolio.StateFailed, "market data missing at entry")
		return
	}
	protection := op.tickSize.MulInt(int64(e.cfg.ProtectionTicks))
	longPx := longSnap.BestAsk.Add(protection)   // покупаем long чуть дороже ask
	shortPx := shortSnap.BestBid.Sub(protection) // продаём short чуть дешевле bid

	longRes, shortRes := e.placeEntryLegs(ctx, id, op, qty, longPx, shortPx)

	// Неопределённое состояние ЛЮБОЙ ноги при входе → DEGRADED + стоп (раздел 10.2).
	if longRes.ambiguous || shortRes.ambiguous {
		_ = transition(portfolio.StateDegraded, "entry leg state unknown (ack+query timeout)")
		e.registerOpen(id, op)
		e.notify(ctx, "CRITICAL", fmt.Sprintf("позиция %s: неопределённое состояние ноги при входе", id))
		return
	}

	longFilled, shortFilled := longRes.filled, shortRes.filled
	e.recordEntryOrders(ctx, id, op, longRes, shortRes)

	if longFilled.IsZero() && shortFilled.IsZero() {
		_ = transition(portfolio.StateFailed, "no fills on entry")
		return
	}
	if err := pos.SetQuantities(longFilled, shortFilled, e.deps.Clock()); err != nil {
		log.Error("set quantities failed", "err", err.Error())
	}

	// Дельта-дисбаланс → repair (раздел 10.3: одна попытка добора, затем reduce).
	decision := execution.AnalyzeMismatch(longFilled, shortFilled, e.cfg.DeltaToleranceBase)
	if decision.Action != execution.RepairNone {
		if !transition(portfolio.StatePartiallyHedged, decision.Reason) {
			return
		}
		longFilled, shortFilled = e.repairImbalance(ctx, id, op, decision, longFilled, shortFilled, longPx, shortPx)
		if err := pos.SetQuantities(longFilled, shortFilled, e.deps.Clock()); err != nil {
			log.Error("set quantities after repair failed", "err", err.Error())
		}
		diff := longFilled.Sub(shortFilled).Abs()
		if diff.GreaterThan(e.cfg.DeltaToleranceBase) || (longFilled.IsZero() && shortFilled.IsZero()) {
			_ = transition(portfolio.StateDegraded, "delta mismatch not repaired")
			e.registerOpen(id, op)
			e.notify(ctx, "CRITICAL", fmt.Sprintf("позиция %s: дельта не выровнена (long=%s short=%s)",
				id, longFilled.String(), shortFilled.String()))
			return
		}
	}

	if !transition(portfolio.StateHedged, fmt.Sprintf("hedged long=%s short=%s", longFilled.String(), shortFilled.String())) {
		return
	}
	if !transition(portfolio.StateMonitoring, "entering monitoring") {
		return
	}
	e.registerOpen(id, op)
	log.Info("position opened",
		"long", string(op.longEx), "short", string(op.shortEx),
		"qty_long", longFilled.String(), "qty_short", shortFilled.String())
	e.notify(ctx, "INFO", fmt.Sprintf("открыта позиция %s: %s long@%s / short@%s, qty %s",
		id, cand.Asset, op.longEx, op.shortEx, longFilled.String()))
}

// legResult — исход входа одной ноги.
type legResult struct {
	filled    decimal.Decimal
	order     domain.Order
	ambiguous bool
	err       error
}

// placeEntryLegs — параллельная отправка entry-ордеров обеих ног.
func (e *Engine) placeEntryLegs(ctx context.Context, id domain.PositionID, op *openPosition,
	qty, longPx, shortPx decimal.Decimal) (legResult, legResult) {

	place := func(ex domain.ExchangeID, sym domain.ExchangeSymbol, side domain.Side, px decimal.Decimal) legResult {
		exec, ok := e.deps.Executors[ex]
		if !ok {
			return legResult{err: fmt.Errorf("engine: no executor for %s", ex)}
		}
		res, err := exec.Place(ctx, domain.PlaceOrderRequest{
			ClientOrderID: execution.Format(execution.ClientOrderIDParts{
				PositionID: id, LegSide: side, SliceIndex: 0, Nonce: 0, Purpose: execution.PurposeEntry,
			}),
			Symbol:      sym,
			Side:        side,
			OrderMode:   domain.OrderMarketableLimitIOC,
			BaseQty:     qty,
			Price:       px,
			ReduceOnly:  false,
			TimeInForce: domain.TIFIOC,
		})
		if err != nil {
			if errors.Is(err, exchange.ErrOrderNotFound) {
				return legResult{filled: decimal.Zero, err: err} // ордер точно не создан
			}
			if execution.IsAmbiguousTimeout(err) {
				return legResult{ambiguous: true, err: err}
			}
			return legResult{filled: decimal.Zero, err: err}
		}
		filled := res.Order.FilledQty
		if filled.GreaterThan(qty) {
			filled = qty
		}
		return legResult{filled: filled, order: res.Order}
	}

	var longRes, shortRes legResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); longRes = place(op.longEx, op.longSym, domain.SideLong, longPx) }()
	go func() { defer wg.Done(); shortRes = place(op.shortEx, op.shortSym, domain.SideShort, shortPx) }()
	wg.Wait()
	return longRes, shortRes
}

// repairImbalance — раздел 10.3: ОДНА попытка добора отстающей ноги,
// затем reduce-only лишнего на большей. Возвращает финальные fill-объёмы ног.
func (e *Engine) repairImbalance(ctx context.Context, id domain.PositionID, op *openPosition,
	decision execution.RepairDecision, longFilled, shortFilled, longPx, shortPx decimal.Decimal) (decimal.Decimal, decimal.Decimal) {

	log := e.deps.Log.With("position_id", string(id))

	// Отстающая нога и её параметры.
	lagIsLong := longFilled.LessThan(shortFilled)
	lagEx, lagSym, lagSide, lagPx := op.shortEx, op.shortSym, domain.SideShort, shortPx
	if lagIsLong {
		lagEx, lagSym, lagSide, lagPx = op.longEx, op.longSym, domain.SideLong, longPx
	}

	// 1. Однократный добор.
	topFilled := decimal.Zero
	if exec, ok := e.deps.Executors[lagEx]; ok {
		res, err := exec.Place(ctx, domain.PlaceOrderRequest{
			ClientOrderID: execution.Format(execution.ClientOrderIDParts{
				PositionID: id, LegSide: lagSide, SliceIndex: 1, Nonce: 0, Purpose: execution.PurposeRepair,
			}),
			Symbol: lagSym, Side: lagSide,
			OrderMode: domain.OrderMarketableLimitIOC,
			BaseQty:   decision.ShortfallQty, Price: lagPx,
			TimeInForce: domain.TIFIOC,
		})
		if err == nil {
			topFilled = res.Order.FilledQty
			if topFilled.GreaterThan(decision.ShortfallQty) {
				topFilled = decision.ShortfallQty
			}
		} else if execution.IsAmbiguousTimeout(err) {
			log.Error("repair top-up ambiguous — treating as unfilled, delta stays", "err", err.Error())
		}
	}
	if lagIsLong {
		longFilled = longFilled.Add(topFilled)
	} else {
		shortFilled = shortFilled.Add(topFilled)
	}

	// 2. Пересчёт: если дельта всё ещё вне tolerance — reduce-only лишнего.
	after := execution.ApplyTopUp(decision, execution.TopUpResult{
		Success: topFilled.IsPositive(), FilledQty: topFilled,
		NewLongQty: longFilled, NewShortQty: shortFilled,
	}, e.cfg.DeltaToleranceBase)

	if after.Action == execution.RepairReduceExcess && after.ExcessQty.IsPositive() {
		heavyIsLong := longFilled.GreaterThan(shortFilled)
		heavyEx, heavySym, heavyLegSide := op.shortEx, op.shortSym, domain.SideShort
		if heavyIsLong {
			heavyEx, heavySym, heavyLegSide = op.longEx, op.longSym, domain.SideLong
		}
		if exec, ok := e.deps.Executors[heavyEx]; ok {
			err := execution.PlaceReduceOrder(ctx, exec, execution.ReduceExcessRequest{
				Symbol: heavySym, ExcessQty: after.ExcessQty,
				Side: execution.ReduceExcessAction(heavyLegSide),
			}, execution.Format(execution.ClientOrderIDParts{
				PositionID: id, LegSide: heavyLegSide, SliceIndex: 2, Nonce: 0, Purpose: execution.PurposeRepair,
			}))
			if err != nil {
				log.Error("reduce excess failed", "err", err.Error())
			} else {
				// Reduce исполнен: тяжёлая нога уменьшена до общего уровня.
				common := decimal.Min(longFilled, shortFilled)
				longFilled, shortFilled = common, common
			}
		}
	}
	return longFilled, shortFilled
}

// recordEntryOrders — персист ордеров входа (идемпотентно по client_order_id).
func (e *Engine) recordEntryOrders(ctx context.Context, id domain.PositionID, op *openPosition, longRes, shortRes legResult) {
	if e.deps.Orders == nil {
		return
	}
	for _, rec := range []struct {
		res legResult
		ex  domain.ExchangeID
	}{{longRes, op.longEx}, {shortRes, op.shortEx}} {
		if rec.res.order.ClientOrderID == "" {
			continue
		}
		if err := e.deps.Orders.UpsertOrder(ctx, rec.res.order, rec.ex, string(id), ""); err != nil {
			e.deps.Log.Warn("persist order failed", "err", err.Error())
		}
	}
}

// registerOpen — регистрация активной позиции.
func (e *Engine) registerOpen(id domain.PositionID, op *openPosition) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.open[id] = op
}

func (e *Engine) forget(id domain.PositionID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.open, id)
	delete(e.closeReq, id)
}

// OpenCount — число активных позиций (для метрик/статуса).
func (e *Engine) OpenCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.open)
}

// ============================================================
// Вспомогательное
// ============================================================

// cacheBook — BookProvider поверх marketdata.Cache (ключ — биржа+актив).
type cacheBook struct {
	cache    *marketdata.Cache
	exchange domain.ExchangeID
	asset    domain.AssetSymbol
}

func (b cacheBook) BestBid(domain.ExchangeSymbol) (decimal.Decimal, bool) {
	s, ok := b.cache.Get(b.exchange, b.asset)
	if !ok || s.BestBid.IsZero() {
		return decimal.Zero, false
	}
	return s.BestBid, true
}

func (b cacheBook) BestAsk(domain.ExchangeSymbol) (decimal.Decimal, bool) {
	s, ok := b.cache.Get(b.exchange, b.asset)
	if !ok || s.BestAsk.IsZero() {
		return decimal.Zero, false
	}
	return s.BestAsk, true
}

// notify — уведомление оператора (nil-safe).
func (e *Engine) notify(ctx context.Context, severity, text string) {
	if e.deps.Notifier != nil {
		e.deps.Notifier.Notify(ctx, severity, text)
	}
}

// notifyOnce — не чаще раза в 10 минут на ключ (защита от спама).
func (e *Engine) notifyOnce(ctx context.Context, key, severity, text string) {
	e.notifyMu.Lock()
	last, ok := e.lastNotif[key]
	now := e.deps.Clock()
	if ok && now.Sub(last) < 10*time.Minute {
		e.notifyMu.Unlock()
		return
	}
	e.lastNotif[key] = now
	e.notifyMu.Unlock()
	e.deps.Log.Warn(text)
	e.notify(ctx, severity, text)
}
