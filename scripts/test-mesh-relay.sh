#!/usr/bin/env bash
# AETHERIS v0.6b — Cift dugumlu OTONOM exit yonlendirme dogrulamasi.
#
# Tek komutla:
#   1. "WAN" yerine gecen bir echo sunucusu baslatir (exit'in dis dunyasi).
#   2. Dugum B'yi EXIT NODE olarak baslatir (WAN cikisi sunar).
#   3. Dugum A'yi INTERNETSIZ istemci olarak baslatir; A, B'yi OTOMATIK
#      (Zero-Conf) kesfeder ve trafigini B uzerinden WAN'a yonlendirir.
#   4. A'nin yerel forward portuna baglanip baytlarin B uzerinden gidip
#      kayipsiz dondugunu ve A panelinin "Relayed via [nodeB]" gosterdigini
#      dogrular.
#
# Kullanim:  bash scripts/test-mesh-relay.sh
# Cikis kodu 0 = basari.
set -uo pipefail

GW="${GW:-/tmp/aetheris-gw}"
DP="${DP:-47860}"          # kesif broadcast portu
ECHO_PORT="${ECHO_PORT:-9910}"
RELAY_PORT="${RELAY_PORT:-9800}"
FWD_PORT="${FWD_PORT:-10800}"
ADMIN_A="${ADMIN_A:-18092}"
ADMIN_B="${ADMIN_B:-18091}"
KEY="acme:0123456789abcdef0123456789abcdef"
SECRET="mesh-relay-test"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# Tasinabilir Python komutu bul (Linux: python3; Windows Git Bash: python veya py).
PY_BIN=""
for c in py python python3; do
  if command -v "$c" >/dev/null 2>&1 && "$c" -c "" >/dev/null 2>&1; then PY_BIN="$c"; break; fi
done
if [ -z "$PY_BIN" ]; then
  echo "HATA: Python bulunamadi (python3/python/py). Test dogrulayicisi Python gerektirir."
  exit 1
fi
echo "==> Python komutu: $PY_BIN"

echo "==> Gateway derleniyor..."
go build -o "$GW" ./cmd/gateway || { echo "BUILD HATASI"; exit 1; }

PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null; done
  wait 2>/dev/null
}
trap cleanup EXIT

# 1) "WAN" echo sunucusu
echo "==> WAN echo sunucusu (:$ECHO_PORT) baslatiliyor..."
$PY_BIN - "$ECHO_PORT" <<'PY' &
import socket,threading,sys
port=int(sys.argv[1])
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind(("127.0.0.1",port));s.listen()
def h(c):
    while True:
        d=c.recv(4096)
        if not d: break
        c.sendall(d)
    c.close()
while True:
    try: c,_=s.accept()
    except OSError: break
    threading.Thread(target=h,args=(c,),daemon=True).start()
PY
PIDS+=($!)
sleep 0.5

# 2) Dugum B — EXIT NODE (WAN hedefi = echo, yani B "internete" cikabilir)
echo "==> Dugum B (EXIT NODE) baslatiliyor..."
AETHERIS_LISTEN=127.0.0.1:$ADMIN_B AETHERIS_ADMIN=true AETHERIS_ADMIN_TOKEN=tb \
  AETHERIS_DISCOVERY=true AETHERIS_DISCOVERY_PORT=$DP AETHERIS_EXIT_NODE=true \
  AETHERIS_RELAY_ADDR=127.0.0.1:$RELAY_PORT AETHERIS_RELAY_SECRET=$SECRET \
  AETHERIS_MESH_NODE_ID=nodeB AETHERIS_WAN_TARGETS=127.0.0.1:$ECHO_PORT \
  AETHERIS_API_KEYS="$KEY" "$GW" >/tmp/nodeB.log 2>&1 &
PIDS+=($!)

# 3) Dugum A — istemci (dogrudan WAN yok; forward hedefi = echo, B uzerinden)
echo "==> Dugum A (INTERNETSIZ istemci) baslatiliyor..."
AETHERIS_LISTEN=127.0.0.1:$ADMIN_A AETHERIS_ADMIN=true AETHERIS_ADMIN_TOKEN=ta \
  AETHERIS_DISCOVERY=true AETHERIS_DISCOVERY_PORT=$DP AETHERIS_EXIT_NODE=false \
  AETHERIS_RELAY_SECRET=$SECRET AETHERIS_FORWARD_ADDR=127.0.0.1:$FWD_PORT \
  AETHERIS_FORWARD_TARGET=127.0.0.1:$ECHO_PORT AETHERIS_WAN_TARGETS=240.0.0.1:9 \
  AETHERIS_MESH_NODE_ID=nodeA \
  AETHERIS_API_KEYS="$KEY" "$GW" >/tmp/nodeA.log 2>&1 &
PIDS+=($!)

echo "==> Otomatik kesif + WAN dedektoru bekleniyor (~20sn)..."
sleep 20

if grep -q "OTOMATIK EXIT yonlendirme aktif" /tmp/nodeA.log; then
  echo "    [OK] A, exit node'u OTOMATIK kesfetti ve yonlendirmeyi baslatti:"
  grep "OTOMATIK EXIT" /tmp/nodeA.log | tail -1 | sed 's/^/        /'
else
  echo "    [HATA] A otomatik yonlendirmeyi baslatmadi. A logu:"
  tail -5 /tmp/nodeA.log | sed 's/^/        /'
  exit 1
fi

# 4a) Bayt turu: A'nin forward'ina baglan -> B uzerinden WAN echo -> geri
echo "==> Uctan uca bayt turu dogrulaniyor (A -> B -> WAN -> geri)..."
BYTE_OK=$($PY_BIN - "$FWD_PORT" <<'PY'
import socket,sys
port=int(sys.argv[1])
msg=b"internet fisini cektim, agdaki Aetheris uzerinden ciktim! "*80
try:
    s=socket.create_connection(("127.0.0.1",port),timeout=8)
    s.sendall(msg); s.shutdown(socket.SHUT_WR)
    got=b""
    while len(got)<len(msg):
        d=s.recv(4096)
        if not d: break
        got+=d
    s.close()
    print("OK" if got==msg else "BOZUK")
except Exception as e:
    print("HATA:"+str(e))
PY
)
if [ "$BYTE_OK" = "OK" ]; then
  echo "    [OK] Baytlar B'nin WAN'i uzerinden KAYIPSIZ gidip dondu."
else
  echo "    [HATA] Bayt turu basarisiz: $BYTE_OK"
  exit 1
fi

# 4b) Panel telemetri: A "Relayed via nodeB" gostermeli
echo "==> A paneli WAN durumu dogrulaniyor..."
WAN_OK=$($PY_BIN - "$ADMIN_A" <<'PY'
import socket,base64,os,struct,json,sys
port=int(sys.argv[1])
try:
    s=socket.create_connection(("127.0.0.1",port),timeout=5)
    s.sendall(b"GET /api/v1/ws/telemetry?token=ta HTTP/1.1\r\nHost:x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: "+base64.b64encode(os.urandom(16))+b"\r\nSec-WebSocket-Version: 13\r\n\r\n")
    d=s.recv(4096);_,_,rest=d.partition(b"\r\n\r\n")
    b=rest if rest else s.recv(4096);ln=b[1]&0x7f;off=2
    if ln==126: ln=struct.unpack(">H",b[off:off+2])[0];off+=2
    p=b[off:off+ln]
    while len(p)<ln: p+=s.recv(ln-len(p))
    t=json.loads(p);s.close()
    print("%s|%s"%(t.get("wan_status"),t.get("exit_peer")))
except Exception as e:
    print("HATA:"+str(e))
PY
)
echo "    A telemetri: wan_status|exit_peer = $WAN_OK"
if [ "$WAN_OK" = "relayed|nodeB" ]; then
  echo "    [OK] Panel 'Relayed via nodeB' gosteriyor."
else
  echo "    [HATA] Beklenen 'relayed|nodeB', gelen '$WAN_OK'"
  exit 1
fi

echo ""
echo "============================================================"
echo " SONUC: BASARILI — Dugum A, Dugum B uzerinden OTOMATIK ve"
echo " SIFIR-KONFIGURASYONLA internete (WAN) cikti. Panel Relayed."
echo "============================================================"
