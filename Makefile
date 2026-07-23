# Makefile для проекта fundarbitrage (раздел 20: развёртывание на одной машине).
# Все рецепты работают из корня репозитория; подкаталог fandos1 содержит Go-модуль.
# Используйте DATABASE_URL=... make migrate для применения миграций.

.PHONY: build test test-race vet fmt-check migrate run-worker-dry run-server \
        docker-build docker-up clean

# Бинарники собираются в fandos1/bin/ (директория создаётся автоматически).
BUILD_DIR := fandos1/bin

## build — компилирует оба бинарника (server + worker) в fandos1/bin/.
build:
	cd fandos1 && \
	  mkdir -p bin && \
	  CGO_ENABLED=0 go build -o bin/server ./cmd/server && \
	  CGO_ENABLED=0 go build -o bin/worker ./cmd/worker

## test — запускает все unit-тесты (без гонок).
test:
	cd fandos1 && go test ./... -count=1

## test-race — запускает тесты с детектором гонок (CGO_ENABLED=1 обязателен).
test-race:
	cd fandos1 && CGO_ENABLED=1 go test -race ./... -count=1

## vet — статический анализ Go-кода.
vet:
	cd fandos1 && go vet ./...

## fmt-check — проверяет форматирование (gofmt -l). CI-рецепт, не меняет файлы.
fmt-check:
	cd fandos1 && test -z "$$(gofmt -l .)"

## migrate — применяет SQL-миграции в порядке нумерации (требует DATABASE_URL).
# Пример: DATABASE_URL=postgres://user:pass@localhost/db make migrate
migrate:
	@if [ -z "$$DATABASE_URL" ]; then \
	  echo "ERROR: DATABASE_URL не задан"; exit 1; \
	fi
	@for f in $$(ls fandos1/migrations/*.sql | sort); do \
	  echo "Применяем миграцию: $$f"; \
	  psql "$$DATABASE_URL" -f "$$f"; \
	done

## run-worker-dry — запускает worker в dry-run режиме (безопасно: торговые ордера не выставляются).
run-worker-dry:
	@if [ -z "$$DATABASE_URL" ]; then \
	  echo "ERROR: DATABASE_URL не задан"; exit 1; \
	fi
	cd fandos1 && RUN_MODE=dry_run ./bin/worker

## run-server — запускает HTTP-сервер (API + Mini App).
run-server:
	@if [ -z "$$DATABASE_URL" ]; then \
	  echo "ERROR: DATABASE_URL не задан"; exit 1; \
	fi
	cd fandos1 && ./bin/server

## docker-build — собирает Docker-образ (требует Docker).
docker-build:
	docker build -f deploy/Dockerfile -t fundarbitrage:latest .

## docker-up — запускает всю инфраструктуру через docker compose.
docker-up:
	docker compose -f deploy/docker-compose.yml up -d

## clean — удаляет скомпилированные бинарники.
clean:
	rm -rf $(BUILD_DIR)
