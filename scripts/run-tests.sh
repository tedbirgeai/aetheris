#!/usr/bin/env bash
# Aetheris test kosucusu - host tarafi sarmalayici.
#
# Tek komutla: Linux konteynerinde, CGO acik, gercek Postgres 16 ve
# Redis 7 esliginde tum test suite'ini -race ile kosar.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! docker info >/dev/null 2>&1; then
  echo "HATA: Docker daemon calismiyor." >&2
  echo "Docker Desktop'i baslatip tekrar deneyin." >&2
  exit 1
fi

echo "Test yigini baslatiliyor (Postgres 16 + Redis 7 + Go runner)..."
echo ""

# --abort-on-container-exit: runner bitince digerlerini de durdur
# --exit-code-from runner : runner'in cikis kodunu betige tasi
docker compose -f docker-compose.test.yml up \
  --abort-on-container-exit \
  --exit-code-from runner \
  --quiet-pull
CODE=$?

echo ""
echo "Temizlik..."
docker compose -f docker-compose.test.yml down -v --remove-orphans >/dev/null 2>&1 || true

if [ $CODE -eq 0 ]; then
  echo "SONUC: BASARILI - tum testler -race altinda yesil"
else
  echo "SONUC: BASARISIZ (cikis kodu $CODE)"
fi
exit $CODE
