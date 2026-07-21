# Aetheris Enterprise Gateway v0.2

Taşıyıcı-bağımsız (PHY-agnostic), sıfır-bilgi (zero-knowledge) bir tünel geçidi:
bayt bazlı ölçüm, kalıcı faturalama defteri ve esnek yönlendirme.

**Doğrulama durumu**

| Kontrol | Sonuç |
|---|---|
| `gofmt -l .` | temiz |
| `go vet ./...` | temiz |
| `go test -race -count=1 ./...` | 7/7 paket geçti |
| Entegrasyon (canlı PostgreSQL 16) | 7/7 test geçti |
| Eşzamanlı yazma (500 goroutine, gerçek DB) | sıfır bayt kaybı |
| Test kapsamı | %73.4 (statements) |
| Yeniden başlatma sonrası defter | korundu (uçtan uca doğrulandı) |

---

## Sıfır-Bilgi Sözleşmesi

Bu sunucu, taşıdığı veriyi çözebilecek hiçbir anahtara **sahip değildir**.

- İstemci veriyi **kendi tarafında** AES-256-GCM ile şifreler.
- Sunucuya base64 kodlanmış opak bir blok (`ciphertext`) gider.
- Sunucu yalnızca boyutu ölçer, SHA-256 özetini alır, bloğu olduğu gibi iletir.
- Sunucu tarafında şifre çözme kodu **bilinçli olarak yoktur**.

Bu, "veriyi okumuyoruz" iddiasını bir taahhüt değil, **mimari bir imkânsızlık**
hâline getirir. KVKK/GDPR denetiminde kritik fark budur: "okumuyoruz" yerine
"okuyamayız" diyebilmek.

Sözleşme her katmanda geçerlidir:
- **Log katmanı:** istek/yanıt gövdesi asla loglanmaz, yalnızca metadata.
- **Veritabanı:** yük saklanmaz, yalnızca SHA-256 özeti. Bir test bu tabloda
  `payload`/`ciphertext`/`body`/`content` adlı sütun olmadığını doğrular.
- **Yönlendirme:** router içeriği çözmez, ayrıştırmaz, değiştirmez.

---

## Hukuki Kapsam — Okumadan Kullanmayın

`internal/carrier/carrier.go` içindeki taşıyıcı listesi **bilinçli olarak kısıtlıdır**.

**İzin verilenler:**

| Taşıyıcı | Dayanak |
|---|---|
| `standard_internet` | Mevcut TCP/IP hatları |
| `mesh_wifi` | 2.4/5 GHz ISM, lisanssız |
| `lora_ism` | 868/915 MHz ISM, duty-cycle sınırlı |
| `optical_li_fi` | Görünür ışık/kızılötesi, spektrum lisansı gerekmez |
| `satellite_licensed` | **Aboneliği bulunan** uydu terminalleri |

**Bilerek dışarıda bırakılanlar:**

- Üçüncü taraf uydu sinyallerinin izinsiz dinlenmesi/çözülmesi — 5809 sayılı
  Elektronik Haberleşme Kanunu ve TCK 132–140 kapsamında suçtur.
- AM/FM veya diğer lisanslı spektrumda veri yayını — BTK lisansı olmadan
  kaçak telsiz istasyonu işletmektir.

Bu koruma iki katmanda test edilir (`carrier` ve `router`), ayrıca HTTP
seviyesinde de doğrulanır. `TestNormalizeRejectsIllegalCarriers` **silinmemelidir**.

---

## Egress Maliyeti Hakkında Dürüst Not

Bir proxy, bulut sağlayıcısının egress maliyetini **düşürmez**. Bir bayt önce
geçide gelir (ingress), sonra hedefe gider (egress). Geçit bulut içinde
çalışıyorsa toplam egress azalmaz, **bir kat daha eklenir**.

Gerçek tasarrufun tek yolu topolojiktir — kod değil, rotaların nereye işaret
ettiği belirler:

| Rota türü | Ne yapar |
|---|---|
| `direct` | Optimizasyon yok, düz iletim. Ölçüm ve test için. |
| `edge` | Bulut dışındaki kenar düğümüne yönlendirir; tekrar eden içerik orada önbelleklenirse kaynaktan çıkan bayt azalır. |
| `peering` | Doğrudan eşleşme bağlantısı; sağlayıcının ölçülü egress'i devreye girmez. |

Bu paket tasarrufu yaratmaz, o topolojiyi **uygulanabilir kılar**.

---

## Mimari

```
cmd/gateway/          Giriş noktası, store seçimi, zarif kapatma
internal/config/      Ortam okuma + başlangıç doğrulaması (rota ve DSN dahil)
internal/carrier/     Yasal taşıyıcı allowlist'i
internal/store/       Store arayüzü + MemoryStore + PostgresStore + migration
internal/meter/       Ölçüm cephesi (store'u sarar, açık tünel göstergesi)
internal/middleware/  Auth (sabit zamanlı), rate limit, log, panic kurtarma
internal/router/      Yönlendirme motoru (direct/edge/peering)
internal/tunnel/      HTTP uçları
```

**Store arayüzü.** Bellek ve PostgreSQL implementasyonları aynı sözleşmeyi
uygular; geçiş tek bir ortam değişkenidir. Uygulamanın geri kalanı hangisinin
aktif olduğunu bilmez. Derleme zamanında `var _ Store = (*PostgresStore)(nil)`
ile uyumluluk garanti edilir.

**Neden sharded bellek defteri?** Tek global mutex, yüksek hacimde tüm
yazmaları serileştirir. Defter, istemci kimliğinin FNV-1a özetine göre 32
shard'a bölünmüştür.

**Neden append-only olay defteri?** Birikimli toplam, faturalama itirazında
"bu rakam nereden çıktı" sorusunu yanıtlayamaz. `aetheris_ledger_events`
her geçişi ayrı satır olarak, makbuz özeti ve imzasıyla saklar. Bu satırlar
hiçbir zaman güncellenmez veya silinmez.

**Fail-closed ölçüm.** Defter yazılamıyorsa istek **reddedilir** (503).
Ölçülemeyen trafik faturalanamaz; sessizce hizmet vermek doğrudan gelir
kaybıdır. Bu, kullanılabilirliği veritabanına bağlayan bilinçli bir takastır —
alternatifi dayanıklı kuyruk + asenkron yazmadır ve v0.3'e bırakılmıştır.
`TestTunnelFailsClosedWhenLedgerUnavailable` bu davranışı kilitler.

**Loglama zinciri.** `Recover → Logging → Auth → RateLimit`. Logging'in Auth'un
*dışında* olması, başarısız kimlik denemelerinin (401) de loglanmasını sağlar —
anahtar deneme saldırılarını görmek için şart. İstemci kimliği Logging'e
holder deseniyle ulaşır; Auth `r.WithContext()` ile yeni bir request ürettiği
için naif bir zincirde `client_id` boş kalırdı.

---

## Uçlar

| Metot | Yol | Kimlik | Açıklama |
|---|---|---|---|
| GET | `/healthz` | — | Sağlık, aktif store, rotalar, taşıyıcılar |
| POST | `/api/v1/tunnel` | Bearer | Ölç, (varsa) ilet, imzalı makbuz dön |
| GET | `/api/v1/meter/me` | Bearer | **Yalnızca kendi** tüketim defteriniz |

İstek:

```json
{
  "carrier_type": "optical_li_fi",
  "ciphertext": "<base64(nonce || AES-256-GCM ciphertext || tag)>",
  "destination": "edge-1"
}
```

Yanıt (gerçek çıktı):

```json
{
  "status": "routed",
  "protocol": "Aetheris/1.1",
  "client_id": "acme",
  "carrier_used": "optical_li_fi",
  "metered_bytes": 96,
  "payload_sha256": "8b8f3a2a9b24d4959381812be419624a...",
  "timestamp": 1784674524,
  "route": {
    "route_name": "edge-1",
    "route_kind": "edge",
    "upstream_status": 200,
    "bytes_sent": 96,
    "bytes_received": 4,
    "duration_ms": 2
  },
  "signature": "fd9603e693783f3cbb41c9d80f91dbd7..."
}
```

`signature`, `Signature` alanı boşken alınan makbuzun HMAC-SHA256 özetidir.
İstemci faturasının değiştirilmediğini bağımsız olarak doğrulayabilir —
`TestReceiptSignatureIsVerifiable` bunu gösterir.

`destination` boşsa yönlendirme yapılmaz, `status` `"metered"` olur.

---

## Kurulum

### Docker Compose (önerilen)

```bash
cp .env.example .env
openssl rand -hex 24   # AETHERIS_API_KEYS icin
openssl rand -hex 32   # AETHERIS_RECEIPT_SECRET icin
docker compose up --build
```

PostgreSQL, sağlık kontrolü ve migration otomatik gelir.

### Yerel

```bash
docker run -d --name aetheris-pg \
  -e POSTGRES_USER=aetheris -e POSTGRES_PASSWORD=aetheris -e POSTGRES_DB=aetheris \
  -p 5432:5432 postgres:16-alpine

cp .env.example .env      # doldurun
set -a; source .env; set +a
make run
```

Konfigürasyon geçersizse sunucu **başlamaz**. `AETHERIS_STORE=postgres` iken
DSN yoksa, anahtar 32 karakterden kısaysa, rota URL'i geçersizse açılışta hata verir.

## Test

```bash
make race          # yaris kosulu denetimiyle
make cover         # kapsam raporu -> coverage.html
make check         # fmt + vet + race

# Canli veritabani gerektirir:
AETHERIS_TEST_DSN="postgres://aetheris:aetheris@localhost:5432/aetheris?sslmode=disable" \
  make integration
```

Entegrasyon testleri `//go:build integration` etiketiyle ayrılmıştır; normal
`go test ./...` çalıştırması veritabanı gerektirmez.

---

## Veri Modeli

```sql
aetheris_ledgers          -- istemci basina birikimli toplamlar
aetheris_ledger_carriers  -- tasiyici kirilimi
aetheris_ledger_events    -- SALT-EKLEME olay defteri (denetim izi)
aetheris_schema_migrations-- uygulanan sema surumleri
```

Migration'lar açılışta otomatik uygulanır, idempotenttir ve her sürüm kendi
transaction'ında çalışır. `Record` üç tabloyu **tek transaction**'da günceller —
ya hepsi ya hiçbiri. Rollback davranışı sqlmock ile test edilmiştir.

---

## Bilinen sınırlar

1. **Rate limiter tek düğümlüdür.** Çok düğümlü dağıtımda Redis tabanlı ortak
   sayaca geçilmeli, aksi halde her düğüm kendi kotasını uygular.
2. **Fail-closed, kullanılabilirliği DB'ye bağlar.** Postgres düşerse geçit
   503 döner. Dayanıklı kuyruk (WAL + asenkron flush) v0.3 konusudur.
3. **mTLS yok.** Kurumsal istemciler için karşılıklı sertifika doğrulaması
   henüz eklenmedi.
4. **Rota seçimi statiktir.** Sağlık durumuna göre otomatik yeniden yönlendirme
   (failover) yok; hedef düşerse istek 502 alır.
5. **Test kapsamı %73.4.** `cmd/gateway` ve bazı hata dalları kapsam dışı.
