# Perintah pengembangan.
#
#   make test   seluruh test terhadap PostgreSQL lokal (docker compose)
#   make lint   go vet dan gofmt
#   make db     database lokal saja
#   make down   menghentikan database lokal

COMPOSE := docker compose -f compose.dev.yaml
DEV_DB_PORT ?= 15442
DEV_DATABASE_URL := postgres://app:app@127.0.0.1:$(DEV_DB_PORT)/app?sslmode=disable
export DEV_DB_PORT

.PHONY: test lint db down

test: db
	TEST_DATABASE_URL='$(DEV_DATABASE_URL)' go test -race -count=1 ./...

lint:
	go vet ./...
	@out=$$(gofmt -l .); \
	  if [ -n "$$out" ]; then echo "belum di-gofmt:"; echo "$$out"; exit 1; fi

db:
	$(COMPOSE) up -d --wait db

down:
	$(COMPOSE) down
