#!/usr/bin/env bash
# fandos — интерактивный гид по настройке для VS Code.
# Запуск: Terminal → Run Task → "🚀 fandos: гид по настройке" (или ./scripts/setup.sh).
# Скрипт НИЧЕГО не делает с биржами и не включает LIVE. Только локальная подготовка.
set -uo pipefail

# ── цвета ──────────────────────────────────────────────────────────────────
if [ -t 1 ]; then
  B="\033[1m"; G="\033[32m"; Y="\033[33m"; R="\033[31m"; C="\033[36m"; D="\033[2m"; N="\033[0m"
else B=""; G=""; Y=""; R=""; C=""; D=""; N=""; fi
ok()   { printf "  ${G}✔${N} %s\n" "$1"; }
warn() { printf "  ${Y}▲${N} %s\n" "$1"; }
err()  { printf "  ${R}✗${N} %s\n" "$1"; }
info() { printf "  ${C}ℹ${N} %s\n" "$1"; }
hr()   { printf "${D}────────────────────────────────────────────────────────────${N}\n"; }
section() { printf "\n${B}%s${N}\n" "$1"; hr; }

# ── всегда работаем из корня Go-модуля (папка с go.mod) ────────────────────
cd "$(dirname "$0")/.." || exit 1
ROOT="$(pwd)"

printf "\n${B}🚀 fandos — гид по настройке${N}\n"
printf "${D}Папка модуля: %s${N}\n" "$ROOT"

if [ ! -f go.mod ]; then
  err "Здесь нет go.mod. В VS Code откройте ВЛОЖЕННУЮ папку fandos1/fandos1 (см. docs/VSCODE.md), затем запустите снова."
  exit 1
fi
ok "go.mod найден — папка открыта правильно."

# ══════════════════════════════════════════════════════════════════════════
section "1/6  Проверка инструментов"
MISSING=0
if command -v go >/dev/null 2>&1; then ok "Go: $(go version | awk '{print $3}')";
else err "Go не найден. Установите Go ≥1.26 с https://go.dev/dl/ и перезапустите VS Code."; MISSING=1; fi
if command -v psql >/dev/null 2>&1; then ok "psql: $(psql --version | awk '{print $3}')";
else warn "psql не найден. Нужна PostgreSQL (или Docker — см. docs/USAGE.md, раздел 13)."; fi
if command -v git >/dev/null 2>&1; then ok "git: $(git --version | awk '{print $3}')"; else warn "git не найден."; fi
if command -v docker >/dev/null 2>&1; then ok "docker: есть (альтернатива установке БД)"; fi
if [ "$MISSING" = "1" ]; then
  err "Go обязателен. Установите его и запустите гид заново."; exit 1
fi

# ══════════════════════════════════════════════════════════════════════════
section "2/6  Файл окружения .env"
if [ -f .env ]; then
  ok ".env уже существует — не трогаю."
else
  cp .env.example .env
  ok "Создал .env из .env.example."
fi

# генерируем MASTER_KEY, если он ещё плейсхолдер
if grep -q "ЗАМЕНИТЕ_НА_base64_32_байта" .env 2>/dev/null; then
  KEY=""
  if command -v openssl >/dev/null 2>&1; then KEY="$(openssl rand -base64 32)";
  elif [ -r /dev/urandom ]; then KEY="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"; fi
  if [ -n "$KEY" ]; then
    # безопасная подстановка литерала (ключ передаём через env, чтобы не мучиться с / и +)
    if command -v python3 >/dev/null 2>&1; then
      MK="$KEY" python3 -c 'import os; p=".env"; s=open(p,encoding="utf-8").read(); open(p,"w",encoding="utf-8").write(s.replace("ЗАМЕНИТЕ_НА_base64_32_байта", os.environ["MK"]))'
    else
      MK="$KEY" awk '{ gsub(/ЗАМЕНИТЕ_НА_base64_32_байта/, ENVIRON["MK"]); print }' .env > .env.tmp && mv .env.tmp .env
    fi
    ok "Сгенерировал MASTER_KEY (32 байта) и записал в .env."
    warn "Сохраните MASTER_KEY из .env в надёжном месте: без него не расшифровать ключи бирж."
  else
    warn "Не смог сгенерировать MASTER_KEY автоматически — впишите вручную в .env."
  fi
else
  ok "MASTER_KEY в .env уже задан."
fi

# читаем DATABASE_URL из .env (срезаем пробелы и кавычки)
DBURL="$(grep -E '^DATABASE_URL=' .env | head -1 | sed -E 's/^DATABASE_URL=//; s/^["'"'"']//; s/["'"'"']$//' | tr -d '[:space:]')"
info "DATABASE_URL = ${DBURL:-<не задан>}"
printf "  ${D}Открыть .env для правки: code .env${N}\n"

# ══════════════════════════════════════════════════════════════════════════
section "3/6  База данных и миграции"
DB_OK=0
if command -v psql >/dev/null 2>&1 && [ -n "${DBURL:-}" ]; then
  if psql "$DBURL" -c "SELECT 1" >/dev/null 2>&1; then
    ok "Подключение к БД работает."
    DB_OK=1
    printf "  Применить миграции сейчас? ${B}[y/N]${N} "; read -r ANS
    if [ "${ANS:-N}" = "y" ] || [ "${ANS:-N}" = "Y" ]; then
      for f in migrations/000*_*.sql; do
        printf "    → %s\n" "$f"
        psql "$DBURL" -v ON_ERROR_STOP=1 -f "$f" >/dev/null 2>&1 && ok "применено" || { err "ошибка в $f (возможно, уже применена)"; }
      done
    else
      info "Пропущено. Позже: Run Task → «db: применить миграции»."
    fi
  else
    warn "К БД по DATABASE_URL подключиться не удалось."
    info "Создайте БД (пример):"
    printf "    ${D}psql -U postgres -c \"CREATE DATABASE fandos;\"${N}\n"
    printf "    ${D}psql -U postgres -c \"CREATE USER fandos WITH PASSWORD 'ПАРОЛЬ';\"${N}\n"
    printf "    ${D}psql -U postgres -c \"ALTER DATABASE fandos OWNER TO fandos;\"${N}\n"
    info "И поправьте DATABASE_URL в .env. Затем запустите гид снова."
  fi
else
  warn "psql или DATABASE_URL недоступны — шаг БД пропущен (см. docs/USAGE.md)."
fi

# ══════════════════════════════════════════════════════════════════════════
section "4/6  Сборка проекта"
if go build ./... 2>/tmp/fandos_build.err; then
  ok "go build ./... — успешно."
else
  err "Ошибка сборки:"; sed 's/^/    /' /tmp/fandos_build.err | head -20
fi

# ══════════════════════════════════════════════════════════════════════════
section "5/6  Быстрый тест (без БД-пакетов)"
if go test ./internal/engine/ ./internal/decimal/ ./internal/scanner/ -count=1 >/tmp/fandos_test.out 2>&1; then
  ok "Ключевые пакеты (engine/decimal/scanner) — тесты зелёные."
else
  warn "Часть тестов не прошла (детали ниже) — обычно из-за окружения:"; tail -8 /tmp/fandos_test.out | sed 's/^/    /'
fi

# ══════════════════════════════════════════════════════════════════════════
section "6/6  Что делать дальше в VS Code"
cat <<'NEXT'
  Всё локально готово. Дальше — только через VS Code:

  ЗАПУСТИТЬ БОТА В БЕЗОПАСНОМ РЕЖИМЕ (без реальных денег):
    1. Слева нажмите значок "Run and Debug" (или Ctrl/Cmd+Shift+D).
    2. Вверху выберите конфигурацию:
         «worker: DRY_RUN + демо-ордера (полный цикл на mock)»
    3. Нажмите F5. В консоли пойдут строки:
         eligible candidate ... → position opened ...
       Это движок сам открыл дельта-нейтральную позицию на mock-биржах.

  ОТКРЫТЬ ИНТЕРФЕЙС (Mini App):
    4. Ещё раз Run and Debug → «server: API + Telegram Mini App» → F5.
    5. Откройте в браузере http://localhost:8080/ — увидите дашборд (demo-режим).

  БОЕВОЙ РЕЖИМ (реальные биржи) — только после:
    • Telegram-бота (@BotFather) и вашего id в TELEGRAM_ADMIN_IDS (в .env);
    • ввода API-ключей ≥2 бирж в Mini App;
    • прохождения чек-листа безопасности.
    Пошагово: docs/USAGE.md (установка/ключи/LIVE), docs/MANUAL.md (все функции).

NEXT
printf "  ${B}Документация:${N} docs/VSCODE.md · docs/USAGE.md · docs/MANUAL.md · docs/RUNBOOK.md\n\n"
printf "${G}${B}Готово. Приятной работы!${N}\n\n"
