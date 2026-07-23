// cmd/server — HTTP API + Telegram (раздел 15): Mini App backend, health,
// метрики, Telegram bot (long-polling), раздача webapp.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thecd/fundarbitrage/internal/app"
	"github.com/thecd/fundarbitrage/internal/auth"
	"github.com/thecd/fundarbitrage/internal/lifecycle"
	"github.com/thecd/fundarbitrage/internal/telegram"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "server: fatal:", err)
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
	log := boot.Log.With("proc", "server")

	if err := boot.EnsureSettingsSeeded(ctx); err != nil {
		return err
	}

	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	allowlist := allowlistFromEnv(os.Getenv("TELEGRAM_ADMIN_IDS"))
	webAppDir := os.Getenv("WEBAPP_DIR")
	if webAppDir == "" {
		webAppDir = "webapp"
	}

	sessions := telegram.NewSessionManager(telegram.NewMemorySessionStore())
	handler := telegram.NewHandler(
		telegram.APIConfig{
			BotToken:  botToken,
			Allowlist: allowlist,
		},
		telegram.Config{Token: botToken, WebAppDir: webAppDir},
		telegram.HandlerDeps{
			Sessions:   sessions,
			Idem:       telegram.NewMemoryIdemStore(),
			Status:     &app.DBStatusProvider{Boot: boot},
			Candidates: app.EmptyCandidatesProvider{},
			Settings:   &app.DBSettingsProvider{Repo: boot.Settings},
			Closer:     &app.LockingCloseRequester{Boot: boot},
		},
	)

	mux := http.NewServeMux()
	mux.Handle("/", telegram.NewLoggingHandler(handler))
	mux.Handle("/metrics", boot.Metrics.Handler())
	mux.Handle("/healthz", boot.Health.LivenessHandler())
	mux.Handle("/readyz", boot.Health.ReadinessHandler())

	components := []lifecycle.Component{
		{Name: "http", Run: func(ctx context.Context) error {
			srv := &http.Server{Addr: boot.Cold.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
			go func() {
				<-ctx.Done()
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(sctx)
			}()
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		}},
		{Name: "db-watchdog", Run: func(ctx context.Context) error {
			w := &lifecycle.DBWatchdog{Ping: boot.Pool.Ping, Halter: boot.Halter, Threshold: 3, Interval: 5 * time.Second}
			w.Run(ctx)
			return nil
		}},
	}

	// Telegram bot: long-polling только при заданном токене.
	if botToken != "" {
		bot := telegram.NewBot(telegram.Config{Token: botToken})
		components = append(components, lifecycle.Component{Name: "telegram-bot", Run: func(ctx context.Context) error {
			return bot.PollUpdates(ctx, 50, func(ctx context.Context, upd telegram.Update) {
				log.Info("telegram update", "update_id", upd.UpdateID)
			})
		}})
	} else {
		log.Warn("TELEGRAM_BOT_TOKEN не задан — bot и проверка initData ограничены (Mini App в demo-режиме)")
	}

	log.Info("server started", "addr", boot.Cold.HTTPAddr, "webapp", webAppDir)
	sup := &lifecycle.Supervisor{ShutdownTimeout: boot.ShutdownTimeout()}
	err = sup.Run(ctx, components)
	log.Info("server stopped", "err", errStr(err))
	return err
}

// allowlistFromEnv — TELEGRAM_ADMIN_IDS="123,456" → StaticAllowlist.
// Пустая строка → nil (allowlist отключён; допустимо только для dev/dry-run).
func allowlistFromEnv(s string) auth.AdminAllowlist {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	set := auth.StaticAllowlist{}
	for _, part := range strings.Split(s, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil && id > 0 {
			set[id] = true
		}
	}
	return set
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
