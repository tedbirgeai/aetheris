GO ?= go

.PHONY: build test race lint fmt vet cover check run docker up down integration integration-redis clean

build:
	$(GO) build -trimpath -ldflags="-s -w" -o bin/aetheris ./cmd/gateway

fmt:
	gofmt -w .

vet:
	$(GO) vet ./...

test:
	$(GO) test -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

# Canli PostgreSQL gerektirir: AETHERIS_TEST_DSN tanimli olmali.
integration:
	$(GO) test -race -count=1 -tags=integration ./internal/store/

# Canli Redis gerektirir: AETHERIS_TEST_REDIS tanimli olmali.
integration-redis:
	$(GO) test -race -count=1 ./internal/middleware/

cover:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	$(GO) tool cover -func=coverage.out | tail -1

check: fmt vet race

run: build
	./bin/aetheris

docker:
	docker build -t aetheris-gateway:latest .

up:
	docker compose up --build

down:
	docker compose down

clean:
	rm -rf bin coverage.out coverage.html wal
