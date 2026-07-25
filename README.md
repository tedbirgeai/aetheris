# AETHERIS PROTOCOL — Evrensel Mimari (v0.6a-turnkey)

Taşıyıcı-bağımsız (PHY-agnostic), sıfır-bilgi (zero-knowledge) tünel geçidi ve
mesh SDK'sı. Afrika/Arabistan çölündeki **off-grid** saha cihazından metropoldeki
**kurumsal veri merkezine** kadar, sıfır dış bağımlılıkla çalışan anahtar-teslim
bir sistem.

Tek statik binary. Node.js yok, dış CDN yok, çalışması için internet gerekmez.

---

## Çelik İlkeler

1. **Mutlak Sıfır-Altyapı (Off-Grid Native).** İnternet, GSM veya merkezi hiçbir
   servis olmadan; ağ kendi çok-sıçramalı yönlendirmesini, düğüm keşfini ve
   off-grid bakiye muhasebesini kendisi yapar.
2. **Donanım ve Platform Agnostic.** Tek komutla Linux (amd64, arm64), Windows
   (amd64) ve macOS (amd64, arm64) için cross-compiled binary üretilir.
3. **Ağ İçi Sıfır-Bilgi Güvenliği.** Sybil saldırıları, replay (tekrar oynatma)
   ve sahte bakiye girişimleri Ed25519 imzaları, Proof-of-Work ve nonce kayan
   penceresi ile matematiksel olarak engellenir. Taşınan yükün içeriği asla
   saklanmaz; yalnızca payload SHA-256 ve bayt sayımı ölçülür.

---

## Mimari Evrim (v0.1a → v0.6a)

| Sürüm | Kod adı | Getirdikleri |
|---|---|---|
| v0.1a | temel geçit | Bayt bazlı ölçüm, PHY-agnostic taşıyıcı soyutlaması |
| v0.2a | dayanıklılık | Kalıcı faturalama defteri, failover'lı yönlendirme |
| v0.3a | ölçek | WAL dayanıklı kuyruk, Redis dağıtık hız sınırlama |
| v0.3b | ticari | mTLS, dedup, QoS probu, faturalama köprüsü, hermetik test |
| v0.4a | mesh | LoRa HAL, gossip keşif/anti-entropy, ham ICMP/UDP QoS, split-brain WAL simülatörü, Stripe/e-Fatura harness |
| v0.5a | enterprise | Gömülü web dashboard, canlı TCP/UDP tünel proxy motoru, Windows WAL sertleştirme, üretim paketi |
| **v0.6a** | **turnkey** | **Çok-sıçramalı Dijkstra yönlendirme, Sybil/replay kalkanı, off-grid Ed25519 fiş defteri, çapraz-platform CLI SDK, canlı mesh topolojisi** |

---

## Mimari Şeması

```
                        ┌──────────────────────────────────────────┐
                        │              AETHERIS NODE                │
                        │            (tek statik binary)            │
                        ├──────────────────────────────────────────┤
   İstemci ─TCP/UDP─►   │  Tünel Proxy Motoru (AES-256-GCM chunk)   │
                        │      zero-knowledge: SHA-256 + bayt       │
                        ├──────────────────────────────────────────┤
                        │  Mesh Router (Dijkstra, çok-sıçramalı)    │──► komşu B
                        │   TTL · loop-prevention · taşıyıcı seçimi │──► komşu C
                        ├──────────────────────────────────────────┤
                        │  Güvenlik Kalkanı (guard)                 │
                        │   Ed25519 kimlik · PoW · replay penceresi │
                        ├──────────────────────────────────────────┤
                        │  Gossip (keşif + anti-entropy, merkezsiz) │
                        ├──────────────────────────────────────────┤
                        │  Off-Grid Ledger (Ed25519 fiş/voucher)    │
                        ├──────────────────────────────────────────┤
                        │  WAL (atomik-swap, çapraz-platform)       │
                        ├──────────────────────────────────────────┤
                        │  /admin Dashboard (go:embed, offline)     │
                        │   canlı topoloji · WebSocket telemetri    │
                        └──────────────────────────────────────────┘
```

Taşıyıcılar: **LoRa (ISM)**, **Wi-Fi**, **Ethernet**. Router en düşük
`RTT × taşıyıcı_ağırlığı` maliyetli yolu seçer; A→C doğrudan yoksa B üzerinden
hop-by-hop aktarır.

---

## Off-Grid Kullanım Kılavuzu

İnternet, DNS veya merkezi sunucu **olmadan** üç sahra düğümü:

```bash
# Düğüm A (sahra röle noktası)
AETHERIS_MESH=true AETHERIS_MESH_NODE_ID=saha-A \
AETHERIS_MESH_ADDR=:7946 aetheris-gateway

# Düğüm B (ara röle) — A'yı tohum komşu olarak alır
AETHERIS_MESH=true AETHERIS_MESH_NODE_ID=saha-B \
AETHERIS_MESH_ADDR=:7946 AETHERIS_MESH_SEEDS=10.0.0.1:7946 aetheris-gateway
```

- **Keşif:** UDP broadcast beacon ile komşular merkezi sunucu olmadan bulunur.
- **Yönlendirme:** A, C'ye doğrudan erişemezse paket B üzerinden otomatik gider.
- **Muhasebe:** B, A'nın taşıdığı baytlar için Ed25519 imzalı fiş keser; A bu
  fişleri biriktirir ve röle kredisi (Relay Credit) kazanır. İnternet sıfır olsa
  dahi kredi matematiksel olarak kanıtlanır.

CLI ile hızlı gösterim:

```bash
aetheris-cli mesh-demo         # 3 düğümlü kayıpsız çok-sıçramalı teslim
aetheris-cli route -links "A-B:10:ethernet,B-C:10:ethernet" -from A -to C
aetheris-cli keygen            # Ed25519 düğüm kimliği
```

---

## Kurumsal (Enterprise) Kullanım Kılavuzu

```bash
# Üretim: Postgres + Redis + mTLS + panel + metrikler
AETHERIS_STORE=postgres \
AETHERIS_DATABASE_DSN="postgres://...:5432/aetheris" \
AETHERIS_WAL_ENABLED=true \
AETHERIS_METRICS=true AETHERIS_METRICS_TOKEN=<gizli> \
AETHERIS_ADMIN=true AETHERIS_ADMIN_TOKEN=<gizli> \
AETHERIS_MESH=true AETHERIS_MESH_ADDR=:7946 \
aetheris-gateway
```

- **Dashboard:** `https://<host>/admin?token=<AETHERIS_ADMIN_TOKEN>` — canlı mesh
  topolojisi, WAL derinliği, geçiş hızı, kredi dökümü. Tamamen offline (gömülü).
- **Metrikler:** `/metrics` (Prometheus). Hazır Grafana panosu:
  `deploy/grafana-dashboard.json`.
- **Dağıtım:** `deploy/aetheris.service` (systemd, sertleştirilmiş),
  `deploy/docker-compose.prod.yml`.

---

## Derleme ve Çalıştırma

```bash
# Yerel binary
make build            # bin/aetheris (gateway)
make cli              # bin/aetheris-cli

# Tüm platformlar için release (Linux amd64/arm64, Windows, macOS amd64/arm64)
make release          # dist/ altına 2 uygulama × 5 platform

# Testler
make test-race        # hermetik Docker, -race, canlı Postgres/Redis
```

Cross-compilation CGO gerektirmez; her hedef tek statik binary olarak üretilir.

---

## Güvenlik Modeli (Ağ İçi Sıfır-Bilgi)

| Tehdit | Savunma |
|---|---|
| Sahte kimlik (spoofing) | Ed25519 düğüm kimliği; her düğüm NodeID'sini imzalar |
| Sybil (ağı sahte düğümle domine etme) | Proof-of-Work: her düğüm katılım için hesaplama maliyeti öder |
| Replay (paket tekrar oynatma) | Nonce kayan penceresi + zaman damgası pencere doğrulaması |
| Sahte bakiye / çift harcama | Ed25519 imzalı fiş/voucher; nonce ile tek-kullanım |
| Yük gözetimi | Zero-knowledge: içerik saklanmaz, yalnızca SHA-256 + bayt |

---

## Doğrulama Durumu (v0.6a)

| Kontrol | Sonuç |
|---|---|
| `gofmt -l .` | temiz |
| `go vet ./...` | temiz |
| `go test -race -count=1 ./...` | tüm paketler geçti (5 yeni sütun dahil) |
| Çok-sıçramalı 3 düğüm (B üzerinden C) | kayıpsız teslim kanıtlandı |
| Entegrasyon (canlı PostgreSQL 16) | geçti |
| Cross-compile (5 platform × 2 uygulama) | 10/10 binary üretildi |
| `/admin` tek binary + WebSocket | canlı topoloji + telemetri |

Tüm mimari evrim ve sürüm notları için `CHANGELOG.md` dosyasına bakın.

---

## Lisans

MIT — bkz. `LICENSE`.
