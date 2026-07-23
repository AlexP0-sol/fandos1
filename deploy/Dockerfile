# Многоэтапная сборка (раздел 20: развёртывание на одной машине).
#
# Этап 1 (build): компилирует бинарники статически (CGO_ENABLED=0).
#   - WORKDIR /src; копируем только fandos1/ (Go-модуль).
#   - Собираем cmd/server и cmd/worker (появятся в репозитории позже).
#
# Этап 2 (runtime): минимальный distroless-образ без shell и пакетного менеджера.
#   - nonroot-вариант: процессы запускаются от непривилегированного пользователя.
#   - ENTRYPOINT worker по умолчанию; server — через command override в compose.

# ============================================================
# Этап сборки
# ============================================================
FROM golang:1.26-alpine AS build

# Устанавливаем git для go mod download (приватные зависимости через SSH не нужны).
RUN apk add --no-cache git

WORKDIR /src

# Копируем модульные файлы первыми — кэш слоя инвалидируется только при изменении зависимостей.
COPY fandos1/go.mod fandos1/go.sum ./
RUN go mod download

# Копируем весь исходный код Go-модуля.
COPY fandos1/ ./

# Статическая сборка: CGO_ENABLED=0, trimpath для воспроизводимости.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# ============================================================
# Этап runtime
# ============================================================
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# Копируем бинарники из этапа сборки.
COPY --from=build /out/server  /usr/local/bin/server
COPY --from=build /out/worker  /usr/local/bin/worker

# Копируем SQL-миграции (для возможного применения из контейнера через psql в отдельном service).
COPY fandos1/migrations/ /app/migrations/

# Копируем статику веб-приложения (Telegram Mini App).
COPY fandos1/webapp/ /app/webapp/

# Пользователь nonroot (65532) — дефолт distroless:nonroot.
USER nonroot

# По умолчанию запускаем worker; server подключается через command override:
#   command: ["/usr/local/bin/server"]
ENTRYPOINT ["/usr/local/bin/worker"]
