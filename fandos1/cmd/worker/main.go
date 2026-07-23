// cmd/worker — торговый движок (раздел 15): market data, scanner, risk loop,
// outbox dispatcher, clock sync, DB watchdog. Ордеры НЕ отправляются в dry_run.
//
// Режимы (раздел 19): dry_run (по умолчанию) — полный цикл оценки без ордеров;
// live — требует настроенного владельца, master key и учётных данных бирж.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thecd/fundarbitrage/internal/app"
	"github.com/thecd/fundarbitrage/internal/clocks"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	"github.com/thecd/fundarbitrage/internal/engine"
	"github.com/thecd/fundarbitrage/internal/exchange"
	mockexchange "github.com/thecd/fundarbitrage/internal/exchange/mock"
	"github.com/thecd/fundarbitrage/internal/execution"
	"github.com/thecd/fundarbitrage/internal/instrument"
	"github.com/thecd/fundarbitrage/internal/lifecycle"
	"github.com/thecd/fundarbitrage/internal/marketdata"
	"github.com/thecd/fundarbitrage/internal/outbox"
	"github.com/thecd/fundarbitrage/internal/repository"
	"github.com/thecd/fundarbitrage/internal/scanner"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker: fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	boot, err := app.New(ctx)
	if err != nil {
		return err
	}
	defer boot.Close()
	log := boot.Log.With("proc", "worker")

	if err := boot.EnsureSettingsSeeded(ctx); err != nil {
		return err
	}

	// Стартовые предусловия: в live — жёсткий отказ, в dry_run — наблюдение.
	preconditionsOK := true
	if errs := lifecycle.CheckPreconditions(ctx, boot.Preconditions()); len(errs) > 0 {
		preconditionsOK = false
		for _, e := range errs {
			log.Error("precondition failed", "err", e.Error())
		}
		if boot.Cold.RunMode == domain.RunModeLive {
			return fmt.Errorf("live mode blocked: %d precondition(s) failed", len(errs))
		}
		log.Warn("продолжаем в наблюдательном режиме (dry_run): торговля недоступна до устранения предусловий")
	}

	// Биржи: dry_run → mock-биржи ВСЕХ 7 площадок с демо-данными (полный контур
	// scanner→engine виден вживую); live → реальные адаптеры из учётных данных БД.
	var adapters map[domain.ExchangeID]exchange.ExchangeAdapter
	if boot.Cold.RunMode == domain.RunModeLive {
		adapters, err = app.BuildLiveAdapters(ctx, boot)
		if err != nil {
			return fmt.Errorf("live wiring: %w", err)
		}
		log.Info("live: адаптеры построены из учётных данных", "exchanges", len(adapters))
	} else {
		adapters = map[domain.ExchangeID]exchange.ExchangeAdapter{}
		for i, ex := range domain.SupportedExchanges() {
			adapters[ex] = seededMock(ex, i)
		}
		log.Info("dry_run: mock-биржи с демо-инструментами", "exchanges", len(adapters))
	}

	registry := instrument.New()
	cache := marketdata.New()

	// Движок исполнения: в наблюдательном режиме сканирует без ордеров.
	// FANDOS_DEMO_ORDERS=1 — включить ордера в dry_run на mock-биржах для демо
	// полного цикла (безопасно: реальные биржи в dry_run не строятся вовсе).
	ordersEnabled := preconditionsOK
	if boot.Cold.RunMode != domain.RunModeLive && os.Getenv("FANDOS_DEMO_ORDERS") == "1" {
		ordersEnabled = true
	}
	executors := map[domain.ExchangeID]*execution.OrderExecutor{}
	for ex, a := range adapters {
		executors[ex] = execution.NewOrderExecutor(a, 3*time.Second)
	}
	eng, err := engine.New(engine.Deps{
		Adapters:  adapters,
		Executors: executors,
		Registry:  registry,
		Cache:     cache,
		Scanner:   scanner.New(registry, cache),
		Positions: boot.Positions,
		Persister: repository.NewPersister(boot.Pool),
		Orders:    boot.Orders,
		Halter:    boot.Halter,
		Log:       log,
		Clock:     time.Now,
	}, engine.Config{
		Scan: scanner.Config{
			MinQuoteVolume24h:       decimal.FromInt(1000),
			MinOrderBookDepthUSDT:   decimal.FromInt(100),
			MaxDataAgeMs:            30_000,
			MinConfidenceLevel:      domain.ConfidenceLow,
			MinSecondsBeforeFunding: 60,
			MinExpectedNetPnL:       decimal.MustFromString("0.5"),
			TargetQty:               decimal.MustFromString("0.05"),
			FeeRateBps:              decimal.MustFromString("5"),
			Horizon:                 24 * time.Hour,
		},
		MaxOpenPositions:         1,
		OrdersEnabled:            ordersEnabled,
		ProtectionTicks:          2,
		MaxRequotes:              3,
		DeltaToleranceBase:       decimal.MustFromString("0.0005"),
		ExitIfFundingSignChanges: true,
		Interval:                 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	if err := eng.Restore(ctx); err != nil {
		return fmt.Errorf("engine restore: %w", err)
	}
	log.Info("engine ready", "orders_enabled", ordersEnabled, "restored_open", eng.OpenCount())

	components := []lifecycle.Component{
		{Name: "ops-http", Run: opsHTTP(boot)},
		{Name: "db-watchdog", Run: func(ctx context.Context) error {
			w := &lifecycle.DBWatchdog{Ping: boot.Pool.Ping, Halter: boot.Halter, Threshold: 3, Interval: 5 * time.Second}
			w.Run(ctx)
			return nil
		}},
		{Name: "outbox-dispatcher", Run: func(ctx context.Context) error {
			d := outbox.NewDispatcher(boot.Pool, 8, 2*time.Second)
			d.Run(ctx, func(ev outbox.Event) error {
				// Маршрутизация: запрос закрытия из Mini App → движок.
				if ev.Topic == "position" && ev.Kind == "close_request" {
					var p app.CloseRequestPayload
					if jerr := json.Unmarshal(ev.Payload, &p); jerr != nil {
						return fmt.Errorf("bad close_request payload: %w", jerr)
					}
					eng.RequestClose(p.PositionID, p.Reason)
					return nil
				}
				log.Info("outbox event", "topic", ev.Topic, "kind", ev.Kind)
				return nil
			}, 2*time.Second)
			return nil
		}},
		{Name: "market-refresh", Run: marketRefreshLoop(boot, adapters, registry, cache)},
		{Name: "engine", Run: eng.Run},
	}

	// Clock sync — только если заданы NTP-серверы (в контейнере UDP наружу закрыт).
	if len(boot.Cold.NTPServers) > 0 {
		servers := make([]string, 0, len(boot.Cold.NTPServers))
		for _, s := range boot.Cold.NTPServers {
			servers = append(servers, s+":123")
		}
		svc, cerr := clocks.NewService(clocks.Config{
			Servers:          servers,
			MaxClockOffsetMs: boot.Cold.MaxClockOffsetMs,
			Interval:         boot.Cold.ClockSyncInterval,
		}, func(st clocks.Status) {
			boot.App.ClockOffsetMs.Set(float64(st.OffsetMs))
			if !st.WithinLimit && boot.Cold.RunMode == domain.RunModeLive {
				_ = boot.Halter.Halt(ctx, fmt.Sprintf("clock offset %dms beyond limit", st.OffsetMs))
			}
		})
		if cerr != nil {
			return cerr
		}
		components = append(components, lifecycle.Component{Name: "clock-sync", Run: func(ctx context.Context) error {
			svc.Run(ctx)
			return nil
		}})
	} else {
		log.Warn("NTP-серверы не заданы — clock sync отключён (недопустимо для live)")
	}

	log.Info("worker started", "run_mode", string(boot.Cold.RunMode), "components", len(components))
	sup := &lifecycle.Supervisor{ShutdownTimeout: boot.ShutdownTimeout()}
	err = sup.Run(ctx, components)
	log.Info("worker stopped", "err", errStr(err))
	return err
}

// opsHTTP — /metrics, /healthz, /readyz на PrometheusAddr.
func opsHTTP(boot *app.Bootstrap) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		mux := http.NewServeMux()
		mux.Handle("/metrics", boot.Metrics.Handler())
		mux.Handle("/healthz", boot.Health.LivenessHandler())
		mux.Handle("/readyz", boot.Health.ReadinessHandler())
		srv := &http.Server{Addr: boot.Cold.PrometheusAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
}

// marketRefreshLoop — Level 1/2: instruments + funding/ticker → registry/cache.
// v1: REST-поллинг (для mock достаточен); WS-стримы подключаются per-биржа
// через marketdata.ConnectionManager при live-интеграции.
func marketRefreshLoop(boot *app.Bootstrap, adapters map[domain.ExchangeID]exchange.ExchangeAdapter,
	registry *instrument.Registry, cache *marketdata.Cache) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		refresh := func() {
			var all []domain.CanonicalInstrument
			for ex, a := range adapters {
				ins, err := a.GetInstruments(ctx)
				if err != nil {
					boot.Log.Warn("instruments fetch failed", "exchange", string(ex), "err", err.Error())
					continue
				}
				all = append(all, ins...)
				for _, in := range ins {
					fi, ferr := a.GetFunding(ctx, in.ExchangeSymbol)
					tk, terr := a.GetTicker(ctx, in.ExchangeSymbol)
					ob, oerr := a.GetOrderBookSnapshot(ctx, in.ExchangeSymbol, 1)
					if ferr != nil || terr != nil || oerr != nil {
						continue
					}
					snap := &domain.MarketSnapshot{
						Exchange:             ex,
						CanonicalBaseAsset:   in.CanonicalBaseAsset,
						ExchangeSymbol:       in.ExchangeSymbol,
						MarkPrice:            tk.MarkPrice,
						LastPrice:            tk.LastPrice,
						QuoteVolume24h:       tk.QuoteVolume24h,
						RealizedFundingRate:  fi.RealizedFundingRate,
						PredictedFundingRate: fi.PredictedFundingRate,
						FundingIntervalSec:   fi.FundingIntervalSec,
						NextFundingTime:      fi.NextFundingTime,
						FundingConfidence:    fi.Confidence,
						IsFresh:              true,
						SequenceValid:        true,
						LocalReceiveTime:     time.Now(),
					}
					if len(ob.Bids) > 0 {
						snap.BestBid = ob.Bids[0].Price
						snap.BidDepthForTargetQty = ob.Bids[0].Price.Mul(ob.Bids[0].Qty)
					}
					if len(ob.Asks) > 0 {
						snap.BestAsk = ob.Asks[0].Price
						snap.AskDepthForTargetQty = ob.Asks[0].Price.Mul(ob.Asks[0].Qty)
					}
					if snap.MarkPrice.IsZero() {
						snap.MarkPrice = snap.LastPrice
					}
					cache.Update(snap)
				}
			}
			if len(all) > 0 {
				registry.Replace(all)
				if err := boot.Instrs.Replace(ctx, all); err != nil {
					boot.Log.Warn("persist instruments failed", "err", err.Error())
				}
			}
		}
		refresh()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				refresh()
			}
		}
	}
}

// seededMock — mock-биржа с демо-инструментами, стаканами и funding-данными
// (только dry_run: полный контур scanner→engine виден без реальных бирж).
// idx задаёт лёгкий перекос цены/funding между площадками для ненулевого спреда.
func seededMock(ex domain.ExchangeID, idx int) *mockexchange.Mock {
	m := mockexchange.New(ex)
	mk := func(sym, asset string) domain.CanonicalInstrument {
		return domain.CanonicalInstrument{
			Exchange:           ex,
			ExchangeSymbol:     domain.ExchangeSymbol(sym),
			CanonicalBaseAsset: domain.AssetSymbol(asset),
			InstrumentType:     domain.InstrumentLinearUSDTPerpetual,
			SettlementCurrency: "USDT",
			Status:             domain.InstrumentStatusActive,
			ContractMultiplier: decimal.One,
			QtyStep:            decimal.MustFromString("0.001"),
			MinQty:             decimal.MustFromString("0.001"),
			TickSize:           decimal.MustFromString("0.1"),
			FundingIntervalSec: 8 * 3600,
		}
	}
	// Перекос цен и funding по индексу площадки создаёт ненулевой basis + funding-спред
	// (idx=0 — базовая ставка, дальше по возрастанию: max−min спред между любыми двумя биржами).
	base := map[string]string{"BTC": "50000", "ETH": "2500"}
	skew := decimal.One.Add(decimal.FromInt(int64(idx)).Mul(decimal.MustFromString("0.0002")))
	rate := decimal.MustFromString("0.0001").Add(decimal.FromInt(int64(idx)).Mul(decimal.MustFromString("0.00008")))
	var ins []domain.CanonicalInstrument
	for asset, px := range base {
		sym := domain.ExchangeSymbol(asset + "USDT")
		ins = append(ins, mk(string(sym), asset))
		p := decimal.MustFromString(px).Mul(skew)
		spread := p.Mul(decimal.MustFromString("0.0002"))
		m.SetOrderBook(sym,
			[]domain.PriceLevel{{Price: p.Sub(spread), Qty: decimal.FromInt(50)}},
			[]domain.PriceLevel{{Price: p.Add(spread), Qty: decimal.FromInt(50)}},
		)
		m.SetTicker(sym, domain.Ticker{
			LastPrice:      p,
			MarkPrice:      p,
			QuoteVolume24h: decimal.FromInt(5_000_000),
			Timestamp:      time.Now(),
		})
		m.SetFunding(sym, domain.FundingInfo{
			PredictedFundingRate: rate,
			RealizedFundingRate:  rate,
			RateType:             domain.FundingRatePredicted,
			Confidence:           domain.ConfidenceHigh,
			FundingIntervalSec:   8 * 3600,
			NextFundingTime:      time.Now().Add(90 * time.Minute),
			FundingPriceType:     domain.FundingPriceMark,
		})
	}
	m.SetInstruments(ins)
	return m
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
