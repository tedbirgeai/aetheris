#!/usr/bin/env bash
# P3 yük testi — gateway'in eşzamanlı yük altında stabilitesini ölçer.
# stdlib-only (curl + xargs); harici araç gerektirmez. k6 varsa alttaki notu kullan.
#
# Kullanım:  bash loadtest.sh https://127.0.0.1:18080 benim-tokenim 200 20
#   $1 base URL, $2 admin token, $3 toplam istek, $4 eşzamanlılık
set -euo pipefail
BASE="${1:-https://127.0.0.1:18080}"
TOKEN="${2:-benim-tokenim}"
TOTAL="${3:-200}"
CONC="${4:-20}"

echo "==> Yük testi: $TOTAL istek, $CONC eşzamanlı → $BASE"
start=$(date +%s.%N)

seq 1 "$TOTAL" | xargs -P "$CONC" -I{} curl -sk -o /dev/null -w "%{http_code}\n" \
  "$BASE/readyz" > /tmp/aetheris_load.out

end=$(date +%s.%N)
dur=$(echo "$end - $start" | bc)
ok=$(grep -c '^200$' /tmp/aetheris_load.out || true)
echo "==> Süre: ${dur}s | 200 OK: $ok/$TOTAL"
echo "==> İstek/sn: $(echo "scale=1; $TOTAL / $dur" | bc)"

# DTN kuyruğu backpressure testi (P1-11 doğrulama):
echo "==> DTN backpressure: 50 bundle enqueue..."
seq 1 50 | xargs -P 10 -I{} curl -sk -o /dev/null -w "%{http_code} " \
  "$BASE/admin/dtn/test?token=$TOKEN"
echo ""
echo "   (kuyruk dolunca 429 görülür — bu beklenen davranıştır)"

# --- k6 alternatifi (kuruluysa, daha zengin rapor) ---
# cat > /tmp/k6.js <<'EOF'
# import http from 'k6/http';
# export const options = { vus: 50, duration: '30s' };
# export default function () { http.get(`${__ENV.BASE}/readyz`, {insecureSkipTLSVerify:true}); }
# EOF
# BASE=$BASE k6 run /tmp/k6.js
