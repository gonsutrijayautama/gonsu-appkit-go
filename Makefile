# Perintah pengembangan.
#
#   make test   seluruh test terhadap PostgreSQL dan S3 lokal (docker compose)
#   make lint   go vet dan gofmt
#   make db     database dan S3 lokal saja
#   make down   menghentikan keduanya

COMPOSE := docker compose -f compose.dev.yaml
DEV_DB_PORT ?= 15442
DEV_S3_PORT ?= 15480
DEV_DATABASE_URL := postgres://app:app@127.0.0.1:$(DEV_DB_PORT)/app?sslmode=disable
DEV_S3_ENDPOINT := http://127.0.0.1:$(DEV_S3_PORT)
export DEV_DB_PORT DEV_S3_PORT

.PHONY: test lint db down

test: db
	TEST_DATABASE_URL='$(DEV_DATABASE_URL)' TEST_S3_ENDPOINT='$(DEV_S3_ENDPOINT)' \
	  go test -race -count=1 ./...

lint:
	go vet ./...
	@out=$$(gofmt -l .); \
	  if [ -n "$$out" ]; then echo "belum di-gofmt:"; echo "$$out"; exit 1; fi

db:
	$(COMPOSE) up -d --wait db s3

down:
	$(COMPOSE) down
