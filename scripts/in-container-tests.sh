#!/usr/bin/env bash
# Konteyner ICINDE calisir. Kaynak salt-okunur baglandigi icin once
# yazilabilir bir dizine kopyalanir (go test cache/build yazar).
set -euo pipefail

echo "=== Aetheris hermetik test kosucusu ==="
echo "Go       : $(go version)"
echo "CGO      : ${CGO_ENABLED}"
echo "Postgres : ${AETHERIS_TEST_DSN%%\?*}"
echo "Redis    : ${AETHERIS_TEST_REDIS}"
echo ""

WORK=/tmp/build
rm -rf "$WORK"
mkdir -p "$WORK"
cp -r /src/. "$WORK"/
cd "$WORK"

echo "--- gofmt ---"
UNFORMATTED=$(gofmt -l . || true)
if [ -n "$UNFORMATTED" ]; then
  echo "HATA: bicimlendirilmemis dosyalar:"
  echo "$UNFORMATTED"
  exit 1
fi
echo "temiz"

echo ""
echo "--- go vet ---"
go vet ./...
echo "temiz"

echo ""
echo "--- go test -race -count=1 ./... ---"
go test -race -count=1 ./...

echo ""
echo "--- go test -race -tags=integration (canli Postgres) ---"
go test -race -count=1 -tags=integration ./internal/store/

echo ""
echo "=== TUM TESTLER YESIL (race detector ACIK) ==="
