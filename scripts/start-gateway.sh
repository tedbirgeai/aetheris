#!/usr/bin/env bash
# AETHERIS Slate B2B Panel — garantili baslatici.
# 1. :18080 portunu tutan TUM eski surecler oldurulur
# 2. go build ile sifirdan YENI binary derlenir (go:embed yeni HTML bakar)
# 3. Gateway baslatilir, panel URL ve /admin/deploy gosterilir
# Kullanim: bash scripts/start-gateway.sh [TOKEN]
set -uo pipefail
PORT="${PORT:-18080}"
TOKEN="${1:-aetheris-b2b-token}"
KEY="${AETHERIS_API_KEYS:-acme:0123456789abcdef0123456789abcdef}"
GW_BIN="${GW_BIN:-/tmp/aetheris-gw-new}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

echo "==> :$PORT uzerindeki eski surecler sonlandiriliyor..."
if command -v taskkill >/dev/null 2>&1; then
  taskkill //F //IM gw.exe 2>/dev/null || true
  taskkill //F //IM aetheris-gw.exe 2>/dev/null || true
  taskkill //F //IM aetheris-gw-new.exe 2>/dev/null || true
  PID=$(netstat -ano 2>/dev/null | grep ":$PORT" | grep "LISTENING" | awk '{print $NF}' | head -1)
  if [ -n "$PID" ] && [ "$PID" != "0" ]; then
    taskkill //F //PID "$PID" 2>/dev/null || true
    echo "    PID $PID sonlandirildi."
  fi
else
  lsof -ti :"$PORT" 2>/dev/null | xargs -r kill -9 2>/dev/null || true
fi
sleep 0.5

echo "==> Sifirdan derleniyor (go:embed yeni Slate B2B HTML'i bakar)..."
go build -o "$GW_BIN" ./cmd/gateway || { echo "[HATA] derleme basarisiz"; exit 1; }
echo "    Tamam: $GW_BIN"

echo "==> Gateway baslatiliyor (:$PORT)..."
AETHERIS_LISTEN=":$PORT" AETHERIS_ADMIN=true AETHERIS_ADMIN_TOKEN="$TOKEN" \
  AETHERIS_MESH=true AETHERIS_MESH_NODE_ID="edge-node-1" AETHERIS_MESH_ADDR=":7946" \
  AETHERIS_DISCOVERY=true AETHERIS_WAN_CHECK=true AETHERIS_API_KEYS="$KEY" \
  "$GW_BIN" &
GW_PID=$!

for i in $(seq 1 15); do
  curl -s -o /dev/null "http://127.0.0.1:$PORT/healthz" 2>/dev/null && break
  sleep 0.4
done

TITLE=$(curl -s -c /tmp/.gw_cj "http://127.0.0.1:$PORT/admin?token=$TOKEN" | grep -o '<title>[^<]*</title>' || echo "?")
echo ""
echo "============================================================"
echo " AETHERIS Slate B2B Panel AKTIF"
echo " Panel : http://127.0.0.1:$PORT/admin?token=$TOKEN"
echo " Deploy: POST http://127.0.0.1:$PORT/admin/deploy?token=$TOKEN"
echo " Baslik: $TITLE"
echo " PID   : $GW_PID"
echo "============================================================"
wait $GW_PID 2>/dev/null || true
