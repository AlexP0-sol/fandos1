# Операционный Runbook — fundarbitrage

Документ описывает операционные процедуры для системы fundarbitrage, развёрнутой
на одной машине с Docker Compose (раздел 20 промпта v2).

---

## 1. Старт и остановка

### Запуск

```bash
# Скопируйте шаблон переменных окружения и заполните секреты.
cp .env.example .env
$EDITOR .env

# Запустите инфраструктуру в фоне.
docker compose -f deploy/docker-compose.yml up -d

# Проверьте статус всех сервисов.
docker compose -f deploy/docker-compose.yml ps
```

Порядок старта автоматический: postgres → migrate → worker + server.

### Graceful shutdown

```bash
# Остановить всё (контейнеры сохраняются, данные в volume остаются).
docker compose -f deploy/docker-compose.yml down

# Остановить только worker (без остановки postgres и server).
docker compose -f deploy/docker-compose.yml stop worker
```

Таймаут graceful shutdown контролируется переменной `SHUTDOWN_TIMEOUT` (по умолчанию 30s).
При получении SIGTERM worker завершает in-flight операции и закрывает соединения.

### Просмотр логов

```bash
# Следить за логами worker в реальном времени.
docker compose -f deploy/docker-compose.yml logs -f worker

# Просмотр последних 100 строк server.
docker compose -f deploy/docker-compose.yml logs --tail=100 server
```

---

## 2. Проверка здоровья системы

### Health endpoints

| Endpoint     | Назначение                                             | Ожидаемый ответ        |
|--------------|--------------------------------------------------------|------------------------|
| `/healthz`   | Liveness: процесс жив                                  | 200 OK всегда          |
| `/readyz`    | Readiness: все критические проверки OK                 | 200 OK / 503 при сбое  |
| `/metrics`   | Prometheus text-format метрики                         | 200 OK + text/plain    |

```bash
# Liveness check (всегда 200, если контейнер запущен).
curl -s http://localhost:8080/healthz | jq .

# Readiness check (200 = готов принимать трафик, 503 = деградация).
curl -s http://localhost:8080/readyz | jq .

# Метрики Prometheus (порт из PROM_ADDR, по умолчанию :9090).
curl -s http://localhost:9090/metrics | grep -E "^(ws_|order_|circuit_|outbox_|clock_)"
```

Пример ответа `/readyz`:

```json
{
  "status": "ok",
  "checks": [
    {"name": "db",         "ok": true,  "critical": true,  "latency_ms": 1.2},
    {"name": "master_key", "ok": true,  "critical": true,  "latency_ms": 0.1},
    {"name": "clock",      "ok": true,  "critical": true,  "latency_ms": 15.4},
    {"name": "adapters",   "ok": true,  "critical": true,  "latency_ms": 8.7},
    {"name": "private_ws", "ok": true,  "critical": true,  "latency_ms": 0.5},
    {"name": "incidents",  "ok": true,  "critical": true,  "latency_ms": 2.1},
    {"name": "breakers",   "ok": true,  "critical": false, "latency_ms": 0.3}
  ]
}
```

### Ключевые метрики для мониторинга

```bash
# Задержка WS-сообщений от биржи (должна быть < 100ms в 99-м перцентиле).
curl -s http://localhost:9090/metrics | grep ws_message_lag_ms

# Clock offset (должен быть < MAX_CLOCK_OFFSET_MS, по умолчанию 500ms).
curl -s http://localhost:9090/metrics | grep clock_offset_ms

# Необработанные записи в outbox (должны стремиться к 0).
curl -s http://localhost:9090/metrics | grep outbox_unprocessed

# Срабатывания circuit breaker (рост = проблема).
curl -s http://localhost:9090/metrics | grep circuit_breaker_trips_total
```

---

## 3. Применение миграций

Миграции применяются автоматически при `docker compose up` через сервис `migrate`.
Для ручного применения:

```bash
# Через Makefile (требует DATABASE_URL в окружении).
DATABASE_URL="postgres://user:pass@host/db" make migrate

# Или напрямую через psql.
for f in fandos1/migrations/*.sql; do
  echo "Applying $f..."
  psql "$DATABASE_URL" -f "$f"
done
```

**Важно:** миграции применяются в порядке нумерации (0001, 0002, 0003, ...).
Никогда не изменяйте уже применённые миграции — только добавляйте новые файлы.

---

## 4. Ротация ключей

Краткая процедура (подробности в разделе 27 промпта v2):

1. Сгенерируйте новый мастер-ключ: `openssl rand -base64 32`.
2. Обновите `MASTER_KEY_B64` в `.env` и в секретном хранилище (Vault / AWS Secrets Manager).
3. Запустите процедуру re-encrypt credential'ов (инструмент появится в `cmd/rekey`).
4. Перезапустите worker и server: `docker compose restart worker server`.
5. Убедитесь, что `/readyz` возвращает `"master_key": {"ok": true}`.

---

## 5. SAFE_HALT: диагностика и выход

### Что такое SAFE_HALT

SAFE_HALT — защитный режим, при котором система запрещает новые входы в позиции.
Активируется автоматически при:
- Превышении дневного лимита убытков (`MaxDailyLossUSDT`).
- Недоступности базы данных (раздел 17.4).
- Clock offset > `MAX_CLOCK_OFFSET_MS`.
- Блокирующем recovery incident.

### Как обнаружить SAFE_HALT

```bash
# Проверить статус через readyz.
curl -s http://localhost:8080/readyz | jq '.checks[] | select(.name=="incidents")'

# Поиск в логах.
docker compose logs worker | grep -i "SAFE_HALT\|safe_halt"
```

### Выход из SAFE_HALT

1. Устраните причину (восстановите БД, синхронизируйте часы, закройте incident).
2. Убедитесь, что `/readyz` вернул `"status": "ok"`.
3. Сбросьте флаг через API (эндпоинт появится в app-пакете): `POST /admin/resume`.
4. Убедитесь, что `RUN_MODE=dry_run` перед возвратом в `live`.

---

## 6. Бэкапы PostgreSQL

### Базовый снимок (pg_dump)

```bash
# Создать сжатый дамп базы.
docker compose exec postgres \
  pg_dump -U appuser -d fundarbitrage -Fc \
  > backups/fundarbitrage_$(date +%Y%m%d_%H%M%S).dump

# Восстановление из дампа.
pg_restore -U appuser -d fundarbitrage_restore backups/fundarbitrage_YYYYMMDD.dump
```

### WAL-архивирование (для Point-in-Time Recovery)

Для production рекомендуется настроить `archive_mode = on` и `archive_command`
в `postgresql.conf` с отправкой WAL-сегментов в S3 или сетевой диск.

**RTO/RPO (раздел 16.2):**
- RPO (Recovery Point Objective) без WAL-архива: время между pg_dump снимками.
- RPO с WAL-архивом: до последнего WAL-сегмента (~секунды/минуты).
- RTO (Recovery Time Objective): зависит от размера БД; для < 10GB обычно < 15 минут.

### Расписание бэкапов

Рекомендуется cron на хосте:

```cron
# Ежедневно в 03:00 UTC
0 3 * * * docker compose -f /opt/fundarbitrage/deploy/docker-compose.yml exec -T postgres \
  pg_dump -U appuser -d fundarbitrage -Fc \
  > /opt/backups/fundarbitrage_$(date +\%Y\%m\%d).dump
```

---

## 7. DRY_RUN чек-лист перед переключением в LIVE

Выполните все пункты перед установкой `RUN_MODE=live`.

- [ ] Все unit-тесты зелёные: `make test-race`
- [ ] `go vet ./...` без предупреждений: `make vet`
- [ ] `/readyz` возвращает `"status": "ok"` со всеми `critical: true` проверками
- [ ] Clock offset в норме: `clock_offset_ms` < `MAX_CLOCK_OFFSET_MS`
- [ ] Тестовый план ребалансировки в режиме plan-only выполнен без ошибок
- [ ] Лимиты риска выставлены: `MaxDailyLossUSDT`, `MaxPositionLossUSDT`, `JointSlippageCapBps`
- [ ] Kill switch проверен: `SAFE_HALT` активируется и снимается корректно
- [ ] Все биржевые адаптеры отвечают: `/readyz` → `"adapters": {"ok": true}`
- [ ] Private WebSocket соединения живы: `"private_ws": {"ok": true}`
- [ ] Нет блокирующих incidents: `"incidents": {"ok": true}`
- [ ] Circuit breakers не активны: `"breakers": {"ok": true}`
- [ ] Telegram Mini App успешно прошла валидацию initData на backend
- [ ] Reconciliation после рестарта выполнена: нет «blind» торговли
- [ ] Бэкап БД создан и проверен перед переходом в live
- [ ] Логирование настроено: `LOG_LEVEL=info`, секреты redacted в `/healthz` логах
