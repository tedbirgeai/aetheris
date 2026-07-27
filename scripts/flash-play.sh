#!/usr/bin/env bash
# AETHERIS Flash & Play — Zero-Touch Provisioning imaj oluşturucu.
#
# Bu betik, bir USB flash diske tak-çalıştır Aetheris edge node imajı yazar.
# Flash disk takıldığında hedef cihaz:
#   1. node.key'den Ed25519 kimliğini yükler (yoksa otomatik üretir)
#   2. bootstrap dosyasındaki node'lardan yapılandırma çeker
#   3. Hiçbir konfigürasyon gerekmeden mesh'e katılır
#
# Kullanım:
#   bash scripts/flash-play.sh /dev/sdX [bootstrap_addr] [exit_node=true]
#   bash scripts/flash-play.sh flash_dir              # dizine yaz (test)
#
# Örnek:
#   bash scripts/flash-play.sh /tmp/flash-test 192.168.1.100:7948
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

TARGET="${1:-/tmp/aetheris-flash}"
BOOTSTRAP="${2:-}"
EXIT_NODE="${3:-false}"
VERSION="v1.0.0-enterprise"

echo "=== AETHERIS Flash & Play ==="
echo "    Hedef  : $TARGET"
echo "    Bootstrap: ${BOOTSTRAP:-'(broadcast keşif)'}"
echo "    Exit node: $EXIT_NODE"
echo ""

# Hedef dizin hazırla
mkdir -p "$TARGET"

# Binary derle (yoksa)
GW_BIN="$ROOT/bin/aetheris-gateway"
CLI_BIN="$ROOT/bin/aetheris-cli"
if [ ! -f "$GW_BIN" ] || [ ! -f "$CLI_BIN" ]; then
  echo "==> Binary'ler derleniyor..."
  mkdir -p "$ROOT/bin"
  go build -o "$GW_BIN" ./cmd/gateway 2>/dev/null || {
    echo "[HATA] Gateway derlenemedi"; exit 1
  }
  go build -o "$CLI_BIN" ./cmd/aetheris-cli 2>/dev/null || {
    echo "[HATA] CLI derlenemedi"; exit 1
  }
fi

# Binary'leri kopyala
cp "$GW_BIN" "$TARGET/aetheris-gateway"
cp "$CLI_BIN" "$TARGET/aetheris-cli"
chmod +x "$TARGET/aetheris-gateway" "$TARGET/aetheris-cli"

# bootstrap dosyası
if [ -n "$BOOTSTRAP" ]; then
  echo "$BOOTSTRAP" > "$TARGET/bootstrap"
  echo "    bootstrap dosyası yazıldı: $BOOTSTRAP"
fi

# Başlatma betiği
# start.sh icerigini olustur
NODE_ID="edge-$(hostname 2>/dev/null || echo node)-$$"
ADMIN_TOKEN=$(openssl rand -hex 16 2>/dev/null || cat /dev/urandom 2>/dev/null | head -c 16 | od -A n -t x1 | tr -d ' \n' || echo "flashtoken0000ab")
cat > "$TARGET/start.sh" <<STARTEOF
#!/usr/bin/env bash
# AETHERIS Zero-Touch baslama - hicbir konfigurasyon gerekmez.
DIR="\$(cd "\$(dirname "\$0")" && pwd)"
export AETHERIS_FLASH_DIR="\$DIR"
export AETHERIS_LISTEN="0.0.0.0:18080"
export AETHERIS_ADMIN=true
export AETHERIS_ADMIN_TOKEN="${ADMIN_TOKEN}"
export AETHERIS_MESH=true
export AETHERIS_MESH_NODE_ID="${NODE_ID}"
export AETHERIS_MESH_ADDR="0.0.0.0:7946"
export AETHERIS_DISCOVERY=true
export AETHERIS_DISCOVERY_PORT=17947
export AETHERIS_WAN_CHECK=true
export AETHERIS_API_KEYS="flash:${ADMIN_TOKEN}0000000000000000"
if [ "$EXIT_NODE" = "true" ]; then
  export AETHERIS_EXIT_NODE=true
fi
echo "============================================"
echo " AETHERIS ${VERSION} - Zero-Touch Aktif"
echo " Panel : http://127.0.0.1:18080/admin?token=${ADMIN_TOKEN}"
echo " Tenant: http://127.0.0.1:18080/tenant?key=flash:${ADMIN_TOKEN}0000000000000000"
echo " NodeID: ${NODE_ID}"
echo "============================================"
"\$DIR/aetheris-gateway"
STARTEOF
chmod +x "$TARGET/start.sh"

# README
cat > "$TARGET/README.txt" <<READMEEOF
AETHERIS Flash & Play — v${VERSION}
======================================
Kullanım:
  1. Bu USB'yi hedef cihaza tak
  2. bash start.sh
  3. Tarayıcıda: http://CIHAZ_IP:18080/admin

Zero-Touch: node.key yoksa otomatik üretilir, bootstrap varsa oradan
yapılandırma çekilir, yoksa sıfır-konfigürasyon modda çalışır.

Dosyalar:
  aetheris-gateway  Ana binary
  aetheris-cli      Komut satırı aracı
  start.sh          Başlatma betiği
  node.key          Ed25519 kimlik (otomatik oluşturulur)
  bootstrap         Bootstrap node adresleri (opsiyonel)
READMEEOF

echo "=== Flash imajı hazır ==="
echo ""
ls -lh "$TARGET/"
echo ""
echo "Çalıştır: bash $TARGET/start.sh"
