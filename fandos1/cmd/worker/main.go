// cmd/worker — торговый движок (раздел 15): market data, scanner, risk loop,
// outbox dispatcher, clock sync, DB watchdog. Ордеры НЕ отправляются в dry_run.
//
// Режимы (раздел 19): dry_run (по умолчанию) — полный цикл оценки без ордеров;
// live — требует настроенного владельца, master key и учётных данных бирж.
package main

import (
	"context"
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
	"github.com/thecd/fundarbitrage/internal/exchange"
	mockexchange "github.com/thecd/fundarbitrage/internal/exchange/mock"
	"github.com/thecd/fundarbitrage/internal/instrument"
	"github.com/thecd/fundarbitrage/internal/lifecycle"
	"github.com/thecd/fundarbitrage/internal/marketdata"
	"github.com/thecd/fundarbitrage/internal/outbox"
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
	if errs := lifecycle.CheckPreconditions(ctx, boot.Preconditions()); len(errs) > 0 {
		for _, e := range errs {
			log.Error("precondition failed", "err", e.Error())
		}
		if boot.Cold.RunMode == domain.RunModeLive {
			return fmt.Errorf("live mode blocked: %d precondition(s) failed", len(errs))
		}
		log.Warn("продолжаем в наблюдательном режиме (dry_run): торговля недоступна до устранения предусловий")
	}

	// Биржи: в dry_run без учётных данных — mock-биржи с демо-данными,
	// чтобы полный контур scanner→ranking был виден вживую.
	adapters := map[domain.ExchangeID]exchange.ExchangeAdapter{}
	if boot.Cold.RunMode == domain.RunModeLive {
		return fmt.Errorf("live wiring требует учётных данных бирж (ввод через Mini App — этап 12); используйте RUN_MODE=dry_run")
	}
	for _, ex := range []domain.ExchangeID{domain.ExchangeBinance, domain.ExchangeBybit} {
		adapters[ex] = seededMock(ex)
	}
	log.Info("dry_run: mock-биржи с демо-инструментами", "exchanges", len(adapters))

	registry := instrument.New()
	cache := marketdata.New()
	scan := scanner.New(registry, cache)

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
				log.Info("outbox event", "topic", ev.Topic, "kind", ev.Kind)
				return nil
			}, 2*time.Second)
			return nil
		}},
		{Name: "market-refresh", Run: marketRefreshLoop(boot, adapters, registry, cache)},
		{Name: "scanner", Run: scannerLoop(boot, scan)},
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

// scannerLoop — Level 3: оценка кандидатов; в dry_run только логирование+метрики.
func scannerLoop(boot *app.Bootstrap, scan *scanner.Scanner) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		cfg := scanner.Config{
			MinQuoteVolume24h:       decimal.FromInt(1000),
			MinOrderBookDepthUSDT:   decimal.FromInt(100),
			MaxDataAgeMs:            30_000,
			MinConfidenceLevel:      domain.ConfidenceLow,
			MinSecondsBeforeFunding: 60,
			MinExpectedNetPnL:       decimal.MustFromString("0.5"),
			TargetQty:               decimal.MustFromString("0.05"),
			FeeRateBps:              decimal.MustFromString("5"),
			Horizon:                 24 * time.Hour,
		}
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				if halted, reason := boot.Halter.IsHalted(); halted {
					boot.Log.Warn("scanner paused: SAFE_HALT", "reason", reason)
					continue
				}
				cands := scan.Scan(cfg, time.Now())
				eligible := scanner.EligibleCount(cands)
				boot.App.ScannerEligibleCandidates.Set(float64(eligible))
				for _, c := range scanner.Top(cands, 3) {
					boot.Log.Info("candidate",
						"asset", string(c.Asset),
						"long", string(c.LongExchange), "short", string(c.ShortExchange),
						"net_pnl_usdt", c.PnLBreakdown.Net.String(),
						"score", c.CompositeScore.StringFixed(3),
						"secs_to_funding", c.SecondsToFunding,
					)
				}
				if eligible == 0 && len(cands) > 0 {
					boot.Log.Debug("no eligible candidates", "evaluated", len(cands), "top_reason", cands[0].Reason)
				}
			}
		}
	}
}

// seededMock — mock-биржа с демо-инструментами, стаканами и funding-данными
// (только dry_run: полный контур scanner→ranking виден без реальных бирж).
func seededMock(ex domain.ExchangeID) *mockexchange.Mock {
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
	// Перекос цен и funding между биржами создаёт ненулевой basis + funding-спред.
	base := map[string]string{"BTC": "50000", "ETH": "2500"}
	skew, rate := decimal.One, decimal.MustFromString("0.0001")
	if ex == domain.ExchangeBybit {
		skew = decimal.MustFromString("1.0006")
		rate = decimal.MustFromString("0.0005") // short на bybit получает больший funding
	}
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
