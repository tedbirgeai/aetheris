# CHANGELOG

Aetheris Protocol — sürüm geçmişi. Tarihler sürüm mühürleme anını yansıtır.

## [v0.6b-turnkey-production] — Otonom Ağ: Otomatik Exit Routing, Failover, Zero-KVKK & B2B Panel

Demo/parçalı yapılar üretim seviyesine çıkarıldı. "İnternet fişini çektim, ağdaki
Aetheris cihaz üzerinden sıfır-konfigürasyonla otomatik dış dünyaya çıktım"
senaryosu uçtan uca çalışır hale getirildi.

### Eklendi
- **Zero-Conf otomatik keşif** (`internal/router/discovery/`): UDP broadcast ile
  eş ve exit node otomatik keşfi; SO_REUSEPORT ile aynı-host çok-düğüm. Manuel
  `AETHERIS_EXIT_PEER` zorunluluğu kaldırıldı.
- **Canlı exit relay** (`internal/relay/`): gerçek TCP üzerinden A→B(exit)→WAN
  yönlendirme, AES-256-GCM. Gateway veri düzlemine canlı bağlandı.
- **Canlı link-sağlık monitörü + failover** (`internal/router/health/`):
  RTT/heartbeat/EWMA ölçümü, down tespiti, otomatik yeniden yönlendirme.
- **Zero-KVKK ephemeral framing** (`internal/carrier/ephemeral/`): RF/Seri
  katmanında IP/MAC baypas; 1B Magic + 8B dönen hedef hash + 12B nonce +
  AES-256-GCM. Epoch rotasyonuyla ilişkilendirilemezlik.
- **BLE/SoftAP HAL soyutlaması** (`internal/transport/driver/`): yerel donanım
  sürücüleri için mimari zemin + registry + stub.
- **Enterprise B2B dashboard** (`/admin`): Datadog/Cloudflare tarzı Slate
  (#0f172a) tema; WAN rozeti "Relayed via [Peer-ID]", aktif taşıyıcı/RTT/tünel/
  bant genişliği canlı; go:embed, sıfır CDN.
- **Yasal/taşıyıcı denetimi** (`docs/TRANSPORT_AUDIT.md`): ISM/BTK KET, KVKK/GDPR
  matrisi, BTK 5651 sorumluluk, Tampere/TAMP afet muafiyetleri.
- **Otonom doğrulama betiği** (`scripts/test-mesh-relay.sh`): tek komutla çift
  düğümlü A→B→WAN exit + "Relayed" panel kanıtı.
- Gateway: uzun ömürlü `bgCtx` (önceki 30sn arka plan timeout hatası düzeltildi).

### Değişti
- `cmd/gateway`: discovery/relay/health/LoRa ana akışa bağlandı; otomatik exit
  yönlendirici; WAN dedektörü keşfi danışıyor.
- `internal/config`: `AETHERIS_DISCOVERY*`, `RELAY_*`, `FORWARD_*`,
  `HEALTH_INTERVAL`, `LORA*` ortam değişkenleri.
- WAN etiketleri: "Direct WAN" / "Relayed via Peer" / "Isolated Mesh Only".

## [v0.6a] — Off-Grid Saha Testi: WAN Durumu & Exit Node (nokta güncelleme)

### Eklendi
- **WAN durumu tespiti** (`internal/wan/`): dürüst erişilebilirlik ölçümüyle
  `Direct` / `Relayed` / `Off-Grid` sınıflandırması.
- **Panel WAN göstergesi:** `/admin` üst barında düğümün internet durumunu anlık
  gösteren rozet (Direct Internet / Relayed via Peer / Off-Grid Mesh Only).
- **Exit Node & WAN köprü demosu** (`aetheris-cli exit-demo`): internetsiz Düğüm
  A'nın, WAN'ı olan Düğüm B üzerinden çok-sıçramalı olarak dış dünyaya eriştiğini
  kanıtlar.
- **0-WAN P2P demosu** (`aetheris-cli p2p-demo`): hiçbir düğümde internet yokken
  iki yerel düğümün mesh üzerinden mesaj + dosya takas ettiğini kanıtlar.
- `AETHERIS_WAN_CHECK`, `AETHERIS_WAN_TARGETS`, `AETHERIS_EXIT_PEER`,
  `AETHERIS_EXIT_NODE` ortam değişkenleri.

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
