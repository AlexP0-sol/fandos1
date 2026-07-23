# fandos — руководство по установке, запуску и эксплуатации

Полное практическое руководство: как выгрузить проект на компьютер, собрать, запустить в
безопасном режиме DRY_RUN, ввести API-ключи бирж через Telegram Mini App и перейти в
боевой режим LIVE. Читать целиком до первого реального запуска.

> ⚠️ **Финансовый риск.** Это ПО размещает реальные ордера на криптобиржах в режиме LIVE.
> Никогда не включайте LIVE, не пройдя чек-лист безопасности (раздел 11) и период DRY_RUN.
> Начинайте с минимальных сумм. Автор кода не несёт ответственности за торговые убытки.

---

## Оглавление

1. [Что это и как устроено](#1-что-это)
2. [Как выгрузить репозиторий на компьютер](#2-как-выгрузить-репозиторий)
3. [Предварительные требования](#3-предварительные-требования)
4. [Сборка проекта](#4-сборка)
5. [База данных и миграции](#5-база-данных-и-миграции)
6. [Конфигурация и master-ключ](#6-конфигурация)
7. [Запуск в режиме DRY_RUN](#7-запуск-dry_run)
8. [Telegram-бот и Mini App](#8-telegram-и-mini-app)
9. [Ввод API-ключей бирж](#9-ввод-api-ключей)
10. [Переход в режим LIVE](#10-переход-в-live)
11. [Чек-лист безопасности перед LIVE](#11-чек-лист-безопасности)
12. [Эксплуатация: мониторинг, закрытие, kill switch](#12-эксплуатация)
13. [Запуск через Docker Compose](#13-docker-compose)
14. [Диагностика проблем](#14-диагностика)

---

## 1. Что это

**fandos** — система дельта-нейтрального арбитража funding rate между криптобиржами.
Идея: одновременно открыть **long** на бирже с низкой (или отрицательной) ставкой funding
и **short** на бирже с высокой ставкой по одному и тому же активу. Позиция дельта-нейтральна
(цена актива не важна), а доход — из разницы funding-платежей и курсового базиса за вычетом
комиссий, проскальзывания и резервов.

Поддерживаются **7 бирж** (USDT-перпетуалы): Binance, Bybit, OKX, Bitget, KuCoin, MEXC, Gate.io.

Два процесса:
- **worker** — торговый движок: сбор рыночных данных, сканер кандидатов, открытие/сопровождение/
  закрытие позиций, риск-контроль, ребалансировка.
- **server** — HTTP API + Telegram-бот + раздача Mini App (дашборд, настройки, ввод ключей).

Оба используют одну PostgreSQL как единственный источник истины.

**Режимы работы:**
- `dry_run` (по умолчанию) — полный цикл оценки на mock-биржах с демо-данными **без реальных
  ордеров**. Безопасно для проверки установки.
- `live` — реальные адаптеры бирж, реальные ордера. Требует введённых API-ключей.

---

## 2. Как выгрузить репозиторий

Репозиторий: **https://github.com/AlexP0-sol/fandos1** (публичный).
Код Go-модуля лежит в подпапке `fandos1/` внутри репозитория.

### Вариант А — git clone по HTTPS (рекомендуется)

```bash
# перейдите в папку, где хотите разместить проект
cd ~/projects

# клонируйте репозиторий
git clone https://github.com/AlexP0-sol/fandos1.git

# зайдите в корень репозитория
cd fandos1
```

Внутри вы увидите: `README.md`, `.gitignore` и папку `fandos1/` — это и есть Go-модуль
(`go.mod`, `internal/`, `cmd/`, `migrations/`, `docs/`, `webapp/`).

### Вариант Б — git clone по SSH (если настроен SSH-ключ на GitHub)

```bash
git clone git@github.com:AlexP0-sol/fandos1.git
cd fandos1
```

### Вариант В — скачать ZIP без git

1. Откройте https://github.com/AlexP0-sol/fandos1 в браузере.
2. Нажмите зелёную кнопку **Code** → **Download ZIP**.
3. Распакуйте архив (получится папка `fandos1-main`).
4. В терминале перейдите в неё: `cd путь/к/fandos1-main`.

### Обновление до последней версии (если клонировали через git)

```bash
cd ~/projects/fandos1
git pull origin main
```

### Куда дальше `cd`

Почти все команды ниже выполняются из **корня Go-модуля** — папки `fandos1/fandos1`
(вложенная папка `fandos1` внутри клонированного репозитория). Проверить, что вы в нужном
месте: там должен лежать файл `go.mod`.

```bash
cd fandos1        # из корня репозитория попадаем в Go-модуль
ls go.mod         # должен существовать
```

Если пользуетесь `Makefile` — он лежит в корне репозитория и сам делает `cd fandos1` внутри.

---

## 3. Предварительные требования

| Компонент | Версия | Зачем |
|---|---|---|
| **Go** | ≥ 1.26 | сборка бинарников |
| **PostgreSQL** | ≥ 14 | хранение состояния (истина системы) |
| **Docker** + Docker Compose | любой актуальный | опционально: запуск «в один клик» |
| **git** | любой | выгрузка репозитория |
| **Telegram** | — | бот + Mini App для управления |

### Установка Go

- **macOS:** `brew install go` или скачать с https://go.dev/dl/
- **Linux (Debian/Ubuntu):** скачать tar.gz с https://go.dev/dl/ и распаковать в `/usr/local`:
  ```bash
  curl -sSLo go.tgz https://go.dev/dl/go1.26.5.linux-amd64.tar.gz
  sudo tar -C /usr/local -xzf go.tgz
  export PATH=$PATH:/usr/local/go/bin   # добавьте в ~/.bashrc / ~/.zshrc
  go version                            # проверка
  ```
- **Windows:** установщик MSI с https://go.dev/dl/

### Установка PostgreSQL

- **macOS:** `brew install postgresql@16 && brew services start postgresql@16`
- **Linux:** `sudo apt install postgresql` (или через Docker — см. раздел 13)
- **Windows:** установщик с https://www.postgresql.org/download/

---

## 4. Сборка

Из корня Go-модуля (`fandos1/fandos1`):

```bash
go build ./...            # собрать всё (проверка, что компилируется)
go test ./...             # прогнать тесты (нужна доступная PostgreSQL для repository-тестов)
```

Собрать исполняемые файлы в `bin/`:

```bash
mkdir -p bin
go build -o bin/worker ./cmd/worker
go build -o bin/server ./cmd/server
```

Либо из **корня репозитория** через Makefile:

```bash
make build        # соберёт оба бинарника в fandos1/bin/
make test         # go test ./...
make vet          # go vet ./...
```

---

## 5. База данных и миграции

Создайте базу и пользователя (пример для локальной PostgreSQL):

```bash
sudo -u postgres psql -c "CREATE DATABASE fandos;"
sudo -u postgres psql -c "CREATE USER fandos WITH PASSWORD 'смените_пароль';"
sudo -u postgres psql -c "ALTER DATABASE fandos OWNER TO fandos;"
```

Примените миграции по порядку (0001 → 0002 → 0003):

```bash
export DATABASE_URL="postgres://fandos:смените_пароль@localhost:5432/fandos"

psql "$DATABASE_URL" -f migrations/0001_core.sql
psql "$DATABASE_URL" -f migrations/0002_trading.sql
psql "$DATABASE_URL" -f migrations/0003_transfers.sql
```

Или одной командой из корня репозитория:

```bash
make migrate DATABASE_URL="postgres://fandos:смените_пароль@localhost:5432/fandos"
```

Что создаётся: идентичность и секреты (0001), торговые сущности — позиции/ордера/fills/funding
(0002), трансферы и circuit breaker (0003). Денежные поля — тип `NUMERIC` (без float).

---

## 6. Конфигурация

Конфигурация делится на **COLD** (задаётся через переменные окружения, неизменна после старта)
и **HOT** (стратегия/риск — правится на лету через Mini App, хранится в БД).

Скопируйте пример env-файла (лежит в корне репозитория) и заполните:

```bash
cp ../.env.example .env       # из папки Go-модуля; файл .env.example — в корне репо
```

Ключевые переменные окружения:

| Переменная | Пример | Описание |
|---|---|---|
| `DATABASE_URL` | `postgres://fandos:pass@localhost:5432/fandos` | строка подключения к БД |
| `RUN_MODE` | `dry_run` \| `live` | режим работы (по умолчанию `dry_run`) |
| `HTTP_ADDR` | `:8080` | адрес HTTP API / Mini App (server) |
| `PROM_ADDR` | `:9090` | адрес метрик/health (worker) |
| `MASTER_KEY` | (base64, 32 байта) | ключ шифрования секретов бирж (см. ниже) |
| `MASTER_KEY_ENV` | `MASTER_KEY` | имя env-переменной с ключом (по умолчанию `MASTER_KEY`) |
| `TELEGRAM_BOT_TOKEN` | `123456:AA...` | токен бота от @BotFather |
| `TELEGRAM_ADMIN_IDS` | `123456789` | ваш Telegram user id (allowlist владельца) |
| `NTP_SERVERS` | `pool.ntp.org` | серверы синхронизации часов (важно для LIVE) |
| `LOG_LEVEL` | `info` | debug \| info \| warn \| error |

### Генерация master-ключа

Секреты бирж хранятся в БД зашифрованными (envelope encryption, AES-256-GCM). Мастер-ключ
**никогда не хранится в БД или репозитории** — только в окружении/KMS. Сгенерируйте 32 байта:

```bash
# Linux/macOS:
export MASTER_KEY="$(head -c 32 /dev/urandom | base64)"
echo "$MASTER_KEY"     # сохраните это значение в надёжном месте (менеджер секретов)
```

> 🔑 **Критично:** потеря master-ключа = невозможность расшифровать введённые API-ключи
> (придётся вводить заново). Компрометация master-ключа = компрометация всех биржевых ключей.
> Храните его отдельно от БД, делайте резервную копию отдельно от бэкапа БД.

---

## 7. Запуск DRY_RUN

DRY_RUN не требует ни ключей, ни владельца — он поднимает mock-биржи с демо-данными и
показывает весь цикл сканера/движка **без единого реального ордера**. Идеально для проверки,
что всё установлено верно.

Терминал 1 — worker (движок):

```bash
export DATABASE_URL="postgres://fandos:pass@localhost:5432/fandos"
export RUN_MODE=dry_run
export NTP_SERVERS=""          # отключить NTP локально, если UDP закрыт
export PROM_ADDR=":9090"
./bin/worker
```

В логах вы увидите: подключение к БД, посев настроек, 7 mock-бирж, старт движка.
Метрики и health доступны на `http://localhost:9090/metrics`, `/healthz`, `/readyz`.

Чтобы **увидеть полный цикл открытия/закрытия позиции** на mock-биржах, включите демо-ордера:

```bash
export FANDOS_DEMO_ORDERS=1    # ТОЛЬКО dry_run: ордера на mock-биржах, реальных бирж нет
./bin/worker
```

Тогда в логах появится `position opened` (движок сам выберет пару бирж с максимальным
funding-спредом и откроет дельта-нейтральную позицию).

Терминал 2 — server (API + Mini App):

```bash
export DATABASE_URL="postgres://fandos:pass@localhost:5432/fandos"
export HTTP_ADDR=":8080"
export WEBAPP_DIR="webapp"
./bin/server
```

Проверка:

```bash
curl http://localhost:8080/healthz            # {"status":...,"checks":[...]}
curl -s http://localhost:8080/ | head         # HTML Mini App
```

Через Makefile (из корня репо): `make run-worker-dry` и `make run-server`.

---

## 8. Telegram и Mini App

Управление системой — через Telegram Mini App (дашборд, настройки, ввод ключей, kill switch).

1. **Создайте бота:** напишите @BotFather в Telegram → `/newbot` → получите
   `TELEGRAM_BOT_TOKEN`. Задайте его в окружении server.
2. **Узнайте свой user id:** напишите @userinfobot → он вернёт ваш числовой id.
   Впишите его в `TELEGRAM_ADMIN_IDS` (allowlist администраторов).
3. **Привяжите Mini App:** в @BotFather → `/newapp` (или Bot Settings → Menu Button / Web App) →
   укажите публичный URL вашего server (Mini App должен открываться по HTTPS; для локальной
   разработки используйте туннель, например `cloudflared` или `ngrok`, указывающий на `:8080`).
4. **Откройте Mini App** из чата с ботом. При первом входе allowlisted-пользователь
   автоматически становится **владельцем** (claim: в таблице `users` его telegram_id
   записывается как владелец единственного tenant).

> Mini App также открывается в браузере в **demo-режиме** (с моковыми данными), если открыт
> не из Telegram — удобно для предпросмотра интерфейса.

---

## 9. Ввод API-ключей

Это тот самый шаг, ради которого всё готовилось: после него система готова к LIVE.

**На каждой бирже** (Binance, Bybit, OKX, Bitget, KuCoin, MEXC, Gate.io), где хотите торговать:

1. Создайте API-ключ в личном кабинете биржи с правами **Futures/Perpetual Trading**
   (и, при необходимости, Wallet/Transfer для ребалансировки). **Вывод (withdrawal)
   включайте только если осознанно планируете авто-ребаланс** — иначе не давайте это право.
2. **Ограничьте ключ по IP** (whitelist IP вашего сервера) — важнейшая мера безопасности.
3. Для **OKX, Bitget, KuCoin** дополнительно понадобится **passphrase** (задаётся при создании
   ключа). Binance, Bybit, MEXC, Gate.io passphrase не используют.

**В Mini App** → раздел **Настройки → API-ключи**:

1. Выберите биржу и тип ключа (`trade`).
2. Вставьте **API key** и **API secret** (и **passphrase** для OKX/Bitget/KuCoin).
3. Нажмите сохранить. Ключи шифруются master-ключом и сохраняются в БД; в интерфейсе
   отображается только «отпечаток» (первые/последние символы). **Секреты никогда не логируются.**

Для арбитража нужны ключи **минимум двух бирж** (long на одной, short на другой).

> Ввод ключей защищён проверкой Telegram initData (подпись) и allowlist администраторов;
> критичные операции могут дополнительно требовать второй фактор (2FA-код).

---

## 10. Переход в LIVE

После ввода ключей минимум двух бирж:

1. Остановите worker (Ctrl+C).
2. Установите `RUN_MODE=live` и убедитесь, что заданы `MASTER_KEY`, `DATABASE_URL`,
   `NTP_SERVERS` (реальные NTP-серверы — синхронизация часов обязательна для подписи запросов).
3. Запустите worker снова:

   ```bash
   export RUN_MODE=live
   export MASTER_KEY="ваш_base64_ключ"
   export NTP_SERVERS="pool.ntp.org,time.google.com"
   ./bin/worker
   ```

При старте LIVE worker:
- проверяет **стартовые предусловия** (владелец настроен, master-ключ читается) — при провале
  в LIVE процесс **не стартует**;
- строит реальные адаптеры **только для тех бирж, где есть активный trade-ключ**;
- требует ≥ 2 бирж с ключами (иначе арбитраж невозможен — ошибка старта).

Настройки стратегии и риска (плечо, `MinExpectedNetPnL`, дневной лимит убытка, режим
semi/auto и т.д.) правятся на лету через Mini App → Настройки (категория HOT, применяются
без рестарта через atomic swap).

---

## 11. Чек-лист безопасности перед LIVE

Не включайте LIVE, пока все пункты не выполнены (раздел 21 мастер-промпта):

- [ ] `go test ./...` и `go test -race ./...` — всё зелёное.
- [ ] Прогон DRY_RUN несколько часов без ошибок в логах.
- [ ] Часы синхронизированы (NTP), `clock_offset_ms` в пределах лимита (метрика на `/metrics`).
- [ ] Ключи бирж ограничены по IP; право на вывод выдано только при осознанной необходимости.
- [ ] Master-ключ сохранён в менеджере секретов, есть отдельная резервная копия.
- [ ] Выставлены риск-лимиты: плечо, `MaxDailyLossUSDT`, `MaxExposurePerExchangeUSDT`.
- [ ] Проверен **kill switch** (Mini App → красная кнопка): переводит систему в SAFE_HALT.
- [ ] Настроены бэкапы PostgreSQL (`pg_dump` + WAL), процедура восстановления отрепетирована.
- [ ] Начинаете с **минимальных** сумм и одной пары бирж.

---

## 12. Эксплуатация

**Дашборд (Mini App):** состояние системы (RUNNING / SAFE_HALT), баланс по биржам, открытые
позиции с дельтой и funding-PnL, список кандидатов сканера с разбивкой ExpectedNetPnL, лента
уведомлений.

**Закрытие позиции вручную:** в карточке позиции → «Закрыть». Запрос идёт через transactional
outbox → worker выполняет координированное закрытие обеих ног синхронно (не независимые TP/SL).

**Kill switch (аварийная остановка):** красная кнопка в Mini App → подтверждение (и 2FA-код,
если включён) → система немедленно переходит в **SAFE_HALT** (новые входы запрещены), отзывает
ключи и фиксирует событие в audit-логе. Снять SAFE_HALT можно только явным действием оператора.

**Мониторинг (worker `/`-эндпоинты на `PROM_ADDR`, server — на `HTTP_ADDR`):**
- `GET /healthz` — liveness (процесс жив), всегда 200 с JSON проверок.
- `GET /readyz` — readiness (503, если провалена критичная проверка: БД, master-key, и т.д.).
- `GET /metrics` — метрики Prometheus: лаг WS, латентность ack ордеров, дельта-рассинхрон,
  число кандидатов, лаг outbox, clock offset, срабатывания circuit breaker и др.

**Логи** — структурированный JSON (slog); секреты и 2FA-коды в логи не попадают (redaction).

**SAFE_HALT срабатывает автоматически** при: недоступности БД (watchdog), превышении
clock offset в LIVE, срабатывании kill switch. Выход — только вручную после устранения причины.

---

## 13. Docker Compose

Если не хотите ставить Go/PostgreSQL вручную — всё поднимается контейнерами. Файлы
`deploy/Dockerfile` и `deploy/docker-compose.yml` — в репозитории; `.env.example` — в корне.

```bash
cd fandos1                       # корень Go-модуля, где deploy/
cp ../.env.example .env          # заполните DATABASE_URL, MASTER_KEY, TELEGRAM_BOT_TOKEN и т.д.

docker compose -f deploy/docker-compose.yml up -d postgres   # сначала БД
docker compose -f deploy/docker-compose.yml run --rm migrate # применить миграции
docker compose -f deploy/docker-compose.yml up -d worker server
```

Сервисы: `postgres` (с healthcheck), одноразовый `migrate`, `worker`, `server` (порт 8080).
Секреты — только через `${ПЕРЕМЕННЫЕ}` из `.env`, в образах и compose их нет.
`RUN_MODE` по умолчанию `dry_run` — для LIVE поменяйте в `.env`.

Полный операционный регламент (старт/стоп, бэкапы, ротация ключей, диагностика SAFE_HALT) —
в [`docs/RUNBOOK.md`](RUNBOOK.md).

---

## 14. Диагностика

| Симптом | Причина / решение |
|---|---|
| worker не стартует в LIVE: «owner telegram_id не настроен» | Войдите в Mini App как allowlisted-пользователь (claim владельца), либо задайте владельца в БД. |
| «нужно ≥2 бирж с trade-ключами» | Введите ключи минимум двух бирж в Mini App. |
| «master key не задан» | Экспортируйте `MASTER_KEY` (base64, 32 байта) перед запуском. |
| `/readyz` возвращает 503 | Смотрите JSON: какая критичная проверка провалена (обычно БД или master-key). |
| Подписи бирж отклоняются (timestamp/recvWindow) | Рассинхрон часов — проверьте NTP и `clock_offset_ms`. |
| Позиция «застряла» в DEGRADED | Неопределённое состояние ноги — требуется ручная сверка (reconciliation) и решение оператора; см. RUNBOOK. |
| Тесты `repository` падают/пропускаются | Нужна доступная PostgreSQL по `DATABASE_URL`; примените миграции. |
| Mini App не открывается из Telegram | Нужен публичный HTTPS-URL (туннель ngrok/cloudflared на `:8080`), привязанный в @BotFather. |

Для WebSocket: у Binance/Bybit потоки реализованы полностью; у OKX/Bitget/KuCoin/MEXC/Gate
рыночные данные идут через REST-поллинг (WebSocket помечен в коде как расширение).
Эндпоинты вывода/переводов у части бирж помечены `TODO:VERIFY` — сверьте с актуальной
документацией биржи перед использованием авто-ребаланса.

---

**Дальнейшее чтение:** [`README.md`](../../README.md) · [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) ·
[`docs/RUNBOOK.md`](RUNBOOK.md) · [`docs/THREAT_MODEL.md`](THREAT_MODEL.md) ·
мастер-спецификация `master_prompt_funding_arbitrage_go_v2.md`.
