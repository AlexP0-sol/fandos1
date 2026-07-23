// cmd/server — HTTP API + Telegram (раздел 15): Mini App backend, health,
// метрики, Telegram bot (long-polling), раздача webapp.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	webAppDir := resolveWebAppDir(os.Getenv("WEBAPP_DIR"), log)

	sessions := telegram.NewSessionManager(telegram.NewMemorySessionStore())

	// Второй фактор для критичных мутаций (kill switch, ввод ключей): включается,
	// если задан FANDOS_2FA_SECRET (общий секрет). Пусто → 2FA отключён (dev).
	var twoFactor telegram.TwoFactorVerifier
	if secret := os.Getenv("FANDOS_2FA_SECRET"); secret != "" {
		twoFactor = &telegram.MemorySharedSecretVerifier{Secret: secret}
		log.Info("2FA включён для критичных мутаций")
	}
	handler := telegram.NewHandler(
		telegram.APIConfig{
			BotToken:  botToken,
			Allowlist: allowlist,
		},
		telegram.Config{Token: botToken, WebAppDir: webAppDir},
		telegram.HandlerDeps{
			Sessions:    sessions,
			Idem:        telegram.NewMemoryIdemStore(),
			Status:      &app.DBStatusProvider{Boot: boot},
			Candidates:  app.EmptyCandidatesProvider{},
			Settings:    &app.DBSettingsProvider{Repo: boot.Settings},
			Closer:      &app.LockingCloseRequester{Boot: boot},
			Credentials: &app.DBCredentialsProvider{Boot: boot, UserID: 1},
			Owner:       &app.DBOwnerClaimer{Boot: boot},
			KillSwitch:  app.NewDBKillSwitchProvider(boot),
			TwoFactor:   twoFactor,
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

// resolveWebAppDir устойчиво находит папку webapp/ независимо от того, откуда
// запущен процесс (терминал, VS Code F5, собранный бинарник). Порядок поиска:
//  1. WEBAPP_DIR из окружения, если задан и содержит index.html;
//  2. "webapp" рядом с текущей рабочей папкой;
//  3. "webapp" рядом с исполняемым файлом;
//  4. поиск вверх от рабочей папки и от бинарника (до 6 уровней) —
//     ищем каталог модуля (go.mod) с подпапкой webapp.
//
// Если ничего не найдено — возвращает "webapp" (сервер отдаст понятную 404).
func resolveWebAppDir(fromEnv string, log *slog.Logger) string {
	has := func(dir string) bool {
		if dir == "" {
			return false
		}
		_, err := os.Stat(filepath.Join(dir, "index.html"))
		return err == nil
	}
	if has(fromEnv) {
		return fromEnv
	}
	candidates := []string{"webapp"}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(exeDir, "webapp"))
	}
	// Поиск вверх от рабочей папки и от каталога бинарника.
	var roots []string
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe))
	}
	for _, root := range roots {
		dir := root
		for i := 0; i < 6; i++ {
			candidates = append(candidates, filepath.Join(dir, "webapp"))
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	for _, c := range candidates {
		if has(c) {
			if c != fromEnv && c != "webapp" {
				log.Info("webapp найден автоматически", "dir", c)
			}
			return c
		}
	}
	log.Warn("webapp/index.html не найден — панель будет отдавать 404; задайте WEBAPP_DIR")
	return "webapp"
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
