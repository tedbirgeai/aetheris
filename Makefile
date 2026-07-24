GO ?= go

.PHONY: build test race test-race check fmt vet cover run docker up down integration mobile-test clean

build:
	$(GO) build -trimpath -ldflags="-s -w" -o bin/aetheris ./cmd/gateway

fmt:
	gofmt -w .

vet:
	$(GO) vet ./...

# Yerel testler. Windows`da -race calismaz (CGO/gcc yok) -> test-race kullanin.
test:
	$(GO) test -count=1 ./...

# Yerel -race. CGO gerektirir; Windows`ta calismaz.
race:
	$(GO) test -race -count=1 ./...

# HERMETIK -race: Linux konteynerinde, CGO acik, gercek Postgres+Redis ile.
# Windows dahil her platformda calisir. EMIR MADDE 1.
test-race:
	bash scripts/run-tests.sh

integration:
	$(GO) test -race -count=1 -tags=integration ./internal/store/

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html
	$(GO) tool cover -func=coverage.out | tail -1

check: fmt vet test

run: build
	./bin/aetheris

# Telefondan test: LAN IP bulur, erisimi dogrular, QR basar. EMIR MADDE 6.
mobile-test:
	$(GO) run ./cmd/mobiletest

# Public tunel ile (INTERNETE ACAR - dikkat)
mobile-test-tunnel:
	$(GO) run ./cmd/mobiletest --tunnel

docker:
	docker build -t aetheris-gateway:latest .

up:
	docker compose up --build

down:
	docker compose down

clean:
	rm -rf bin coverage.out coverage.html wal
