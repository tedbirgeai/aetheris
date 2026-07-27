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
cat > "$TARGET/start.sh" <<STARTEOF
#!/usr/bin/env bash
# AETHERIS Zero-Touch başlatma betiği — hiçbir konfigürasyon gerekmez.
DIR="\$(cd "\$(dirname "\$0")" && pwd)"
export AETHERIS_FLASH_DIR="\$DIR"
export AETHERIS_ADMIN=true
export AETHERIS_ADMIN_TOKEN="\${AETHERIS_ADMIN_TOKEN:-$(openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | xxd -p)}"
export AETHERIS_MESH=true
export AETHERIS_DISCOVERY=true
export AETHERIS_WAN_CHECK=true
export AETHERIS_API_KEYS="\${AETHERIS_API_KEYS:-flash:\$(cat /dev/urandom | head -c 16 | xxd -p 2>/dev/null || echo 'flashdefault0000')}"
if [ "$EXIT_NODE" = "true" ]; then
  export AETHERIS_EXIT_NODE=true
fi
echo "AETHERIS v${VERSION} - Zero-Touch başlatılıyor..."
echo "Admin token: \$AETHERIS_ADMIN_TOKEN"
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
