# CHANGELOG

Aetheris Protocol — sürüm geçmişi. Tarihler sürüm mühürleme anını yansıtır.

## [v0.6a-turnkey] — Evrensel Mimari (Turnkey Release)

Sistem parçalı yapılardan çıkarılıp; off-grid sahra cihazından kurumsal veri
merkezine kadar sıfır bağımlılıkla çalışan anahtar-teslim standartlara yükseltildi.

### Eklendi
- **Çok-Sıçramalı Dinamik Yönlendirme** (`internal/router/mesh/`): Dijkstra tabanlı
  en düşük maliyetli yol (RTT × taşıyıcı ağırlığı). Hop-by-hop aktarım, TTL ve
  döngü engelleme. A→C doğrudan yoksa B üzerinden otomatik yönlendirme.
- **Sybil & Replay Kalkanı** (`internal/security/guard.go`): Ed25519 düğüm kimliği,
  anti-Sybil Proof-of-Work, nonce kayan penceresi + zaman damgası ile replay
  koruması. Tümü internetsiz, yerel doğrulama.
- **Off-Grid Fiş Defteri** (`internal/billing/ledger/`): Ed25519 imzalı yerel
  fişler (Local Signed Receipts) ve devredilebilir voucher'lar. Röle kredisi
  otomatik hesaplanır; çift-harcama ve sahte imza reddedilir. Off-grid takas için
  serileştirme.
- **Çapraz-Platform CLI SDK** (`cmd/aetheris-cli/`): keygen, route, receipt,
  mesh-demo, serve alt komutları. `scripts/build-release.sh` ile Linux
  (amd64/arm64), Windows (amd64), macOS (amd64/arm64) hedeflerine tek binary.
- **Canlı Mesh Topolojisi:** Gossip düğümü gateway'e entegre edildi;
  `AETHERIS_MESH=true` ile `/admin` paneli keşfedilen komşuları canlı gösterir.
- `README.md` ve bu `CHANGELOG.md` v0.1a→v0.6a mimari evrimiyle güncellendi.

### Değişti
- `internal/router/gossip`: `PeerList()` ile komşu listesi dışa açıldı (dashboard).
- `internal/config`: `AETHERIS_MESH*` ortam değişkenleri.
- `cmd/gateway`: gossip düğümü başlatma + telemetriye topoloji beslemesi.
- `Makefile`: `cli` ve `release` hedefleri.

## [v0.5a-enterprise]

### Eklendi
- Gömülü web dashboard (`internal/dashboard/`): `go:embed`, offline-first, sıfır
  dış CDN, stdlib-only WebSocket telemetri, cookie tabanlı admin oturumu.
- Canlı TCP/UDP tünel proxy motoru (`internal/tunnel/proxy.go`): AES-256-GCM
  chunk şifreleme, zero-knowledge muhasebe (yalnızca SHA-256 + bayt).
- Üretim paketi (`deploy/`): sertleştirilmiş systemd unit, Grafana panosu,
  üretim docker-compose.

### Düzeltildi
- Windows WAL dosya kilidi: `Truncate(0)` yerine atomik temp-swap; hem Linux
  hem Windows'ta sıfır uyarı.

## [v0.4a-mesh]

### Eklendi
- LoRa HAL (`internal/carrier/lora/`): SX1262/RadioHead çerçeveleme, mock'a
  otomatik düşüş, 222-bayt MTU fragmentasyon.
- Gossip keşif/anti-entropy (`internal/router/gossip/`): merkezi sunucusuz
  push-pull.
- Ham ICMP/UDP QoS (`internal/router/qos/`): gerçek probe; yetki yoksa sahte
  metrik üretmeden http_fallback.
- Split-brain WAL simülatörü (`cmd/simulator/`): 5 düğüm, %0 veri kaybı.
- Stripe/e-Fatura test harness (`internal/billing/harness/`): kimlik-bilgisiz.

## [v0.3b]

### Eklendi
- Hermetik test koşucusu, mTLS, istemci taraflı dedup, QoS probu, Prometheus
  metrikleri, ticari faturalama köprüsü (Stripe/e-Fatura/webhook).

## [v0.3a]

### Eklendi
- WAL dayanıklı kuyruk, Redis dağıtık hız sınırlama.

## [v0.1a — v0.2a]

### Eklendi
- Temel PHY-agnostic geçit, bayt bazlı ölçüm, kalıcı faturalama defteri,
  failover'lı yönlendirme.
