# fandos — работа с проектом в VS Code (пошагово, «чтобы всё заработало»)

Максимально подробная инструкция: от установки VS Code до запуска worker и server с отладкой.
В репозитории уже лежат готовые конфиги `.vscode/` (settings, launch, tasks, extensions) —
после правильного открытия папки всё заведётся почти само.

> ⚠️ Самый частый источник проблем — **какую папку открывать**. Читайте раздел 3 внимательно.

---

## 0. Коротко (для нетерпеливых)

```
1. Установить VS Code + расширение "Go" (golang.go) + Go 1.26 + PostgreSQL.
2. git clone https://github.com/AlexP0-sol/fandos1.git
3. Открыть в VS Code ИМЕННО папку с go.mod:  fandos1/fandos1  (вложенную!).
4. Согласиться установить Go-инструменты (gopls, dlv...) когда VS Code предложит.
5. Запустить ГИД: он всё проверит, создаст .env, соберёт проект и покажет шаги (см. ниже).
6. F5 → выбрать "worker: DRY_RUN + демо-ордера" — движок пошёл. Готово.
```

Дальше — то же самое, но с объяснениями и решением проблем.

## 0.1. Скрипт-гид: «запусти — и он всё покажет»

В проекте есть интерактивный гид, который сам проверяет окружение, создаёт `.env` c
автогенерацией `MASTER_KEY`, предлагает применить миграции, собирает проект и печатает
пошаговый план запуска. **Три способа запустить:**

- **Автоматически при открытии папки.** Когда вы впервые открываете папку `fandos1/fandos1`,
  VS Code спросит *«Allow automatic tasks?»* — нажмите **Allow**. Гид запустится сам в
  отдельной панели терминала. (Один раз на папку; потом — по кнопке ниже.)
- **Вручную из меню:** `Terminal → Run Task…` → **🚀 fandos: гид по настройке**.
- **Из терминала:** `bash scripts/setup.sh` (macOS/Linux/WSL/Git-Bash) или
  `powershell -ExecutionPolicy Bypass -File scripts/setup.ps1` (Windows).

Гид ничего не делает с биржами и не включает LIVE — только безопасная локальная подготовка.
Его можно запускать сколько угодно раз (повторный запуск не перезатирает существующий `.env`).

---

## 1. Установка VS Code

- Скачайте с https://code.visualstudio.com/ и установите (Windows/macOS/Linux).
- Запустите.

## 2. Установка внешних инструментов (вне VS Code)

Эти вещи ставятся в систему, а не в редактор:

**Go ≥ 1.26** — https://go.dev/dl/
Проверка в терминале: `go version` → должно быть `go1.26...`.
Если команда не найдена — добавьте Go в `PATH` (обычно `/usr/local/go/bin`, на Windows
установщик делает это сам; перезапустите VS Code после установки, чтобы он увидел `PATH`).

**PostgreSQL ≥ 14** — https://www.postgresql.org/download/
Либо через Docker (проще всего, см. раздел 9). Проверка: `psql --version`.

**git** — https://git-scm.com/ (для клонирования и обновлений).

## 3. Клонирование и — ГЛАВНОЕ — какую папку открывать

Склонируйте репозиторий:

```bash
git clone https://github.com/AlexP0-sol/fandos1.git
```

Структура вложенная:

```
fandos1/                 ← корень РЕПОЗИТОРИЯ (тут README.md, .gitignore)
└── fandos1/             ← корень Go-МОДУЛЯ  ←←← ОТКРЫВАТЬ В VS CODE ИМЕННО ЭТУ ПАПКУ
    ├── go.mod           ← признак модуля
    ├── .vscode/         ← готовые конфиги (уже в репозитории)
    ├── cmd/  internal/  migrations/  docs/  webapp/
    └── .env.example
```

**В VS Code: File → Open Folder… → выберите вложенную `fandos1/fandos1`** (ту, где лежит
`go.mod`). Если открыть внешнюю `fandos1/` (корень репозитория), расширение Go не найдёт
модуль, автодополнение и переходы работать не будут, а `.vscode/`-конфиги не подхватятся.

Проверка, что открыли правильно: в проводнике VS Code на верхнем уровне видны `go.mod`,
`cmd`, `internal`, папка `.vscode`.

> Терминал внутри VS Code (Terminal → New Terminal) при этом сразу оказывается в корне
> модуля — все команды `go ...` можно вводить как есть.

## 4. Расширение Go и его инструменты

- При открытии папки VS Code предложит установить рекомендованные расширения
  (список уже в `.vscode/extensions.json`). Согласитесь — минимум нужен **Go** (`golang.go`).
- После установки Go-расширения снизу справа появится предложение
  **«Install all» Go tools** (gopls, dlv, staticcheck и пр.). Нажмите **Install All**.
  Если предложение не всплыло: `Ctrl/Cmd+Shift+P` → **Go: Install/Update Tools** → отметить все → OK.
- `gopls` (language server) даст автодополнение, переходы (F12), подсветку ошибок.
  `dlv` (delve) нужен для отладки (F5).

Если инструменты не ставятся из-за сети/прокси — откройте встроенный терминал и выполните
`go install golang.org/x/tools/gopls@latest` и `go install github.com/go-delve/delve/cmd/dlv@latest`.

## 5. Настройка окружения (.env)

Готовые launch-конфигурации читают переменные из файла `.env` в корне модуля.

1. В встроенном терминале VS Code:
   ```bash
   cp .env.example .env
   ```
2. Откройте `.env` в редакторе и заполните как минимум:
   - `DATABASE_URL` — строка подключения к вашей PostgreSQL;
   - `MASTER_KEY` — сгенерируйте: в терминале `head -c 32 /dev/urandom | base64`
     (на Windows без WSL: `[Convert]::ToBase64String((1..32|%{Get-Random -Max 256}))` в PowerShell)
     и вставьте результат;
   - для реального Telegram-бота — `TELEGRAM_BOT_TOKEN`, `TELEGRAM_ADMIN_IDS`.

`.env` добавлен в `.gitignore` — он никогда не попадёт в git. Секреты в безопасности.

## 6. База данных и миграции

Создайте БД (терминал VS Code):

```bash
# если PostgreSQL стоит локально:
psql -U postgres -c "CREATE DATABASE fandos;"
psql -U postgres -c "CREATE USER fandos WITH PASSWORD 'ПАРОЛЬ';"
psql -U postgres -c "ALTER DATABASE fandos OWNER TO fandos;"
```

Примените миграции — удобнее через готовую задачу VS Code:
`Ctrl/Cmd+Shift+P` → **Tasks: Run Task** → **db: применить миграции**
(она берёт `DATABASE_URL` из окружения; если задача не видит переменную — просто
выполните в терминале три `psql "$DATABASE_URL" -f migrations/000X_*.sql` по порядку,
предварительно `export DATABASE_URL=...`).

Расширение **PostgreSQL** (ckolkman) позволяет подключиться к БД прямо из VS Code
(иконка слева) и смотреть таблицы `positions`, `orders`, `audit_log` и т.д.

## 7. Сборка и тесты внутри VS Code

- **Сборка:** `Ctrl/Cmd+Shift+B` (задача «go: build all» назначена по умолчанию).
- **Тесты (все):** `Ctrl/Cmd+Shift+P` → **Tasks: Run Task** → «go: test all».
  Есть также «go: test -race» и «go: vet».
- **Тесты по одному:** откройте любой `*_test.go` — над каждой тест-функцией расширение Go
  показывает кликабельные **run test | debug test**. Удобно гонять точечно.
- **Форматирование** происходит автоматически при сохранении (`gofmt`, настроено в
  `settings.json`), плюс авто-упорядочивание импортов.

> Тесты пакета `repository` требуют доступной PostgreSQL (иначе они падают/пропускаются) —
> примените миграции перед запуском. Тесты `-race` требуют компилятора C (gcc/clang):
> на Windows это MinGW; на macOS — Xcode command line tools; на Linux — `build-essential`.

## 8. Запуск и отладка (F5)

Готовые конфигурации уже в `.vscode/launch.json`. Откройте вкладку **Run and Debug**
(значок «play с жучком» слева) и выберите вверху нужную конфигурацию:

| Конфигурация | Что делает |
|---|---|
| **worker: DRY_RUN (mock, без ордеров)** | движок наблюдает, реальных ордеров нет |
| **worker: DRY_RUN + демо-ордера** | полный цикл открытия/закрытия на mock-биржах — рекомендую для знакомства |
| **worker: LIVE (реальные ордера!)** | боевой режим — только после ввода ключей и чек-листа |
| **server: API + Telegram Mini App** | HTTP API и Mini App на `:8080` |
| **Отладка текущего теста** | дебаг теста из открытого файла |
| **worker + server (DRY_RUN)** (compound) | запускает оба процесса разом |

Нажмите **F5** (или зелёный треугольник). Логи — во вкладке **Debug Console** / **Terminal**.
Поставьте точку останова (клик слева от номера строки) в интересном месте — например в
`internal/engine/engine.go` в функции `openPosition` — и увидите пошагово, как движок
открывает позицию.

Остановить — красный квадрат на панели отладки (worker завершится корректно, graceful shutdown).

**Быстрый сценарий «всё заработало»:** выберите **worker: DRY_RUN + демо-ордера** → F5 →
в консоли появятся `eligible candidate` и `position opened`. Параллельно запустите
**server** и откройте `http://localhost:8080/` в браузере — увидите Mini App (demo-режим).

## 9. Альтернатива: всё в Docker (без установки Go/Postgres)

Если не хотите ставить Go и PostgreSQL в систему, при установленном Docker Desktop:

```bash
cp .env.example .env        # заполнить
docker compose -f deploy/docker-compose.yml up -d postgres
docker compose -f deploy/docker-compose.yml run --rm migrate
docker compose -f deploy/docker-compose.yml up -d worker server
```

Расширение **Docker** (ms-azuretools) покажет контейнеры и логи прямо в VS Code.
Для разработки/отладки кода всё же удобнее нативный Go (разделы 4–8), а Docker — для
«запустить и посмотреть» или продакшена.

## 10. Решение частых проблем

| Симптом | Причина / решение |
|---|---|
| Нет автодополнения, «No packages found», красное всё | Открыта не та папка. Закройте и откройте **вложенную** `fandos1/fandos1` (где `go.mod`). |
| «gopls not installed» / инструменты не встали | `Ctrl/Cmd+Shift+P` → **Go: Install/Update Tools** → выбрать все. Проверьте сеть/прокси. |
| `go: command not found` в терминале VS Code | Go не в `PATH`. Добавьте `/usr/local/go/bin`, перезапустите VS Code целиком. |
| F5 пишет «could not launch process: dlv» | Не установлен delve: **Go: Install/Update Tools** → `dlv`. |
| launch не видит переменные | Нет файла `.env` рядом с `go.mod`, либо в нём опечатка. `cp .env.example .env`. |
| worker в LIVE: «owner telegram_id не настроен» | Войдите в Mini App как admin (claim владельца) — см. `docs/USAGE.md`. |
| Тесты `repository` падают | Нужна PostgreSQL по `DATABASE_URL` + применённые миграции. |
| `go test -race` не собирается | Нет C-компилятора: поставьте gcc/clang (см. раздел 7). |
| Формат «прыгает» при сохранении | Это `gofmt` — так и надо (стиль Go). Отключается в `settings.json`, но не рекомендую. |
| Кириллица в комментариях «ломается» | Проверьте кодировку файла = UTF-8 (внизу справа в VS Code). |

## 11. Полезные горячие клавиши

| Действие | Клавиши |
|---|---|
| Палитра команд | `Ctrl/Cmd+Shift+P` |
| Собрать (default build task) | `Ctrl/Cmd+Shift+B` |
| Запустить/отладить | `F5` |
| Перейти к определению | `F12` |
| Найти все ссылки | `Shift+F12` |
| Переименовать символ | `F2` |
| Открыть/закрыть терминал | `` Ctrl+` `` |
| Быстрый поиск файла | `Ctrl/Cmd+P` |
| Поиск по всему проекту | `Ctrl/Cmd+Shift+F` |

---

**Что дальше:** установка и первый запуск целиком — [`USAGE.md`](USAGE.md);
все функции бота и настройки — [`MANUAL.md`](MANUAL.md);
эксплуатация и бэкапы — [`RUNBOOK.md`](RUNBOOK.md).
