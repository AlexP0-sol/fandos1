# fandos — интерактивный гид по настройке (Windows PowerShell).
# Запуск: Terminal → Run Task → "fandos: гид по настройке", либо  ./scripts/setup.ps1
# Ничего не делает с биржами и не включает LIVE. Только локальная подготовка.
$ErrorActionPreference = "Continue"
function Ok($m)   { Write-Host "  [OK]  $m" -ForegroundColor Green }
function Warn($m) { Write-Host "  [!]   $m" -ForegroundColor Yellow }
function Err($m)  { Write-Host "  [x]   $m" -ForegroundColor Red }
function Info($m) { Write-Host "  [i]   $m" -ForegroundColor Cyan }
function Section($m) { Write-Host ""; Write-Host $m -ForegroundColor White;
  Write-Host "------------------------------------------------------------" -ForegroundColor DarkGray }

# корень модуля (папка с go.mod) = на уровень выше scripts/
Set-Location (Join-Path $PSScriptRoot "..")
$root = (Get-Location).Path
Write-Host ""
Write-Host "fandos — гид по настройке" -ForegroundColor White
Write-Host "Папка модуля: $root" -ForegroundColor DarkGray

if (-not (Test-Path "go.mod")) {
  Err "Здесь нет go.mod. Откройте в VS Code ВЛОЖЕННУЮ папку fandos1\fandos1 (см. docs/VSCODE.md)."; exit 1
}
Ok "go.mod найден — папка открыта правильно."

Section "1/6  Проверка инструментов"
$missing = $false
if (Get-Command go -ErrorAction SilentlyContinue)   { Ok ("Go: " + ((go version) -split ' ')[2]) } else { Err "Go не найден: https://go.dev/dl/ , перезапустите VS Code"; $missing = $true }
if (Get-Command psql -ErrorAction SilentlyContinue) { Ok "psql: найден" } else { Warn "psql не найден (нужна PostgreSQL или Docker)" }
if (Get-Command git -ErrorAction SilentlyContinue)  { Ok "git: найден" } else { Warn "git не найден" }
if ($missing) { Err "Go обязателен. Установите и запустите гид снова."; exit 1 }

Section "2/6  Файл окружения .env"
if (Test-Path ".env") { Ok ".env уже существует — не трогаю." }
else { Copy-Item ".env.example" ".env"; Ok "Создал .env из .env.example." }

$envtext = Get-Content ".env" -Raw
if ($envtext -match "ЗАМЕНИТЕ_НА_base64_32_байта") {
  $bytes = New-Object 'System.Byte[]' 32
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  $key = [Convert]::ToBase64String($bytes)
  # простая замена литерала (без regex — base64 содержит / и +)
  (Get-Content ".env" -Raw).Replace("ЗАМЕНИТЕ_НА_base64_32_байта", $key) | Set-Content ".env" -NoNewline
  Ok "Сгенерировал MASTER_KEY (32 байта) и записал в .env."
  Warn "Сохраните MASTER_KEY из .env в надёжном месте."
} else { Ok "MASTER_KEY в .env уже задан." }

$dbline = (Select-String -Path ".env" -Pattern '^DATABASE_URL=' | Select-Object -First 1).Line
$dburl = if ($dbline) { ($dbline -replace '^DATABASE_URL=', '').Trim().Trim('"').Trim("'") } else { "" }
Info "DATABASE_URL = $dburl"
Write-Host "  Открыть .env: code .env" -ForegroundColor DarkGray

Section "3/6  База данных и миграции"
if ((Get-Command psql -ErrorAction SilentlyContinue) -and $dburl) {
  & psql $dburl -c "SELECT 1" *> $null
  if ($LASTEXITCODE -eq 0) {
    Ok "Подключение к БД работает."
    $ans = Read-Host "  Применить миграции сейчас? [y/N]"
    if ($ans -eq "y" -or $ans -eq "Y") {
      Get-ChildItem "migrations" -Filter "000*_*.sql" | Sort-Object Name | ForEach-Object {
        Write-Host "    -> $($_.Name)"
        & psql $dburl -v ON_ERROR_STOP=1 -f $_.FullName *> $null
        if ($LASTEXITCODE -eq 0) { Ok "применено" } else { Err "ошибка в $($_.Name) (возможно, уже применена)" }
      }
    } else { Info "Пропущено. Позже: Run Task -> «db: применить миграции»." }
  } else {
    Warn "К БД подключиться не удалось. Создайте БД fandos и поправьте DATABASE_URL в .env (см. docs/USAGE.md)."
  }
} else { Warn "psql или DATABASE_URL недоступны — шаг БД пропущен." }

Section "4/6  Сборка проекта"
& go build ./...
if ($LASTEXITCODE -eq 0) { Ok "go build ./... — успешно." } else { Err "Ошибка сборки (см. вывод выше)." }

Section "5/6  Быстрый тест"
& go test ./internal/engine/ ./internal/decimal/ ./internal/scanner/ -count=1 *> $null
if ($LASTEXITCODE -eq 0) { Ok "Ключевые пакеты — тесты зелёные." } else { Warn "Часть тестов не прошла (обычно из-за окружения)." }

Section "6/6  Что делать дальше в VS Code"
@"
  Всё локально готово. Дальше — через VS Code:

  БЕЗОПАСНЫЙ ЗАПУСК (без реальных денег):
    1. Слева — значок "Run and Debug" (Ctrl+Shift+D).
    2. Вверху выберите: «worker: DRY_RUN + демо-ордера (полный цикл на mock)».
    3. F5. В консоли: eligible candidate ... -> position opened ...

  ИНТЕРФЕЙС:
    4. Run and Debug -> «server: API + Telegram Mini App» -> F5.
    5. Браузер: http://localhost:8080/  (Mini App, demo-режим).

  БОЕВОЙ РЕЖИМ — только после Telegram-бота, ввода ключей >=2 бирж и чек-листа.
  Пошагово: docs/USAGE.md, docs/MANUAL.md.
"@ | Write-Host
Write-Host "  Документация: docs/VSCODE.md - docs/USAGE.md - docs/MANUAL.md - docs/RUNBOOK.md" -ForegroundColor White
Write-Host ""
Write-Host "Готово. Приятной работы!" -ForegroundColor Green
