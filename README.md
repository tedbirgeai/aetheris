# Aetheris Enterprise Gateway v0.3a

Taşıyıcı-bağımsız (PHY-agnostic), sıfır-bilgi (zero-knowledge) bir tünel geçidi:
bayt bazlı ölçüm, kalıcı faturalama defteri, dayanıklı kuyruk, dağıtık hız
sınırlama ve failover'lı yönlendirme.

**Doğrulama durumu**

| Kontrol | Sonuç |
|---|---|
| `gofmt -l .` | temiz |
| `go vet ./...` | temiz |
| `go test -race -count=1 ./...` | 7/7 paket geçti |
| Entegrasyon (canlı PostgreSQL 16) | geçti |
| Redis testleri (canlı Redis 7) | 7/7 geçti |
| WAL → PostgreSQL uçtan uca | 100 kayıt, sıfır kayıp |
| Eşzamanlı yazma (gerçek DB) | sıfır bayt kaybı |
| Test kapsamı | ~%70 (statements) |

Canlı doğrulama çıktısı (`store: wal+postgres`, Redis limiter aktif):

```
{"msg":"kalici defter aktif","store":"postgres"}
{"msg":"WAL dayanikli kuyruk aktif","dir":"/wal"}
{"msg":"dagitik Redis rate limiter aktif","addr":"redis:6379"}
{"store":"wal+postgres","status":"ok"}
```

---

## v0.3a'da yeni olanlar

### 1. WAL — Dayanıklı Defter Kuyruğu (`internal/store/wal.go`)

**Sorun:** v0.2 fail-closed çalışıyordu — Postgres yavaşlar veya düşerse tünel
istekleri 503 alıyordu. Faturalama bütünlüğü için doğru, kullanılabilirlik için sert.

**Çözüm:** WAL araya girer. Her `Record` çağrısı:
1. Önce diske WAL satırı yazar (`fsync` ile) — hızlı, yerel, dayanıklı
2. Kaydı bellek kuyruğuna koyar
3. Arka plan flusher kuyruğu asenkron olarak Postgres'e basar

Sonuç: veritabanı kesintiye uğrasa dahi tünel istekleri **bloklanmaz**, hiçbir
kayıt **kaybolmaz**. Süreç çökerse yeniden başlatmada WAL'den kurtarma yapılır.

**Takas — açıkça bilinmeli:** Bu, fail-closed'dan fail-open'a geçiştir. Kısa bir
pencerede (WAL yazıldı, Postgres'e henüz basılmadı) kayıt "commit edilmiş" sayılır
ama ana defterde değildir. Disk WAL bu boşluğu dayanıklı kılar; makine diski
tamamen kaybolursa o penceredeki kayıtlar gider.

`AETHERIS_WAL_ENABLED=false` ile v0.2'nin fail-closed davranışına dönülebilir.

### 2. Dağıtık Rate Limiter (`internal/middleware/redis_limiter.go`)

Redis tabanlı token bucket. Kritik nokta: **Lua betiği** kullanır, çünkü dağıtık
ortamda "oku-hesapla-yaz" üç ayrı komut olursa iki düğüm aynı token'ı harcar
(yarış koşulu). Lua, Redis'te tek işlem olarak çalışır — tüm düğümler için atomik.

`TestRedisLimiterSharedAcrossInstances` iki ayrı limiter örneğinin (iki düğüm gibi)
aynı Redis'i paylaştığında limitin **ortak** olduğunu kanıtlar.

**Fallback:** Redis erişilemezse istekler sessizce reddedilmez; yerel bellek
sınırlayıcısına düşülür ve arka planda Redis yoklanır. Kesinti bitince otomatik
dağıtık moda dönülür. Takas: kısa kesintide bir istemci teorik olarak düğüm sayısı
kadar fazla istek atabilir, ama **hizmet ayakta kalır**.

### 3. Rota Failover ve Sağlık Kontrolü (`internal/router/failover.go`)

Arka planda her rotaya periyodik `/healthz` yoklaması. Bir rota art arda N kez
başarısız olursa "sağlıksız" işaretlenir ve trafik:
1. `Backup` tanımlıysa yedek rotaya,
2. yoksa `direct` türünde bir rotaya düşürülür.

Rota tekrar yanıt verince otomatik sağlıklı olur. `/healthz` çıktısında
`route_health` alanı tüm rotaların anlık durumunu gösterir.

Yapılandırma (`AETHERIS_ROUTES`):

```
primary=edge@https://a.example;backup=secondary;health=/status,secondary=peering@https://b.example
```

`backup` adı tanımlı bir rotaya işaret etmelidir; etmiyorsa sunucu **açılışta
hata verir**. Sessizce çalışmayan bir failover, kesinti anında keşfedilirdi.

Canlı doğrulama (gerçek çıktı):

```
{"msg":"rota sagliksiz isaretlendi, failover aktif","route":"primary"}
{"msg":"failover: yedek rotaya yonlendiriliyor","from":"primary","to":"secondary"}
{"route_name":"secondary","upstream_status":200}
```

### 4. Üretim Yığını (`docker-compose.yml`)

PostgreSQL 16 + Redis 7 + Gateway. Sağlık kontrolleri, kalıcı volume'ler
(pgdata, redisdata, wal), `restart: unless-stopped`.

---

## Sıfır-Bilgi Sözleşmesi

Bu sunucu, taşıdığı veriyi çözebilecek hiçbir anahtara **sahip değildir**.

- İstemci veriyi **kendi tarafında** AES-256-GCM ile şifreler
- Sunucuya base64 kodlanmış opak blok gider
- Sunucu yalnızca boyutu ölçer, SHA-256 özetini alır, bloğu olduğu gibi iletir
- Sunucu tarafında şifre çözme kodu **bilinçli olarak yoktur**

Sözleşme her katmanda geçerlidir: log gövdesi yazmaz, veritabanı yük saklamaz
(bir test `payload`/`ciphertext`/`body`/`content` sütunu olmadığını doğrular),
**WAL yalnızca metadata ve özet tutar**, router içeriği çözmez.

---

## Hukuki Kapsam — Okumadan Kullanmayın

`internal/carrier/carrier.go` içindeki taşıyıcı listesi **bilinçli olarak kısıtlıdır**.

**İzin verilenler:** `standard_internet`, `mesh_wifi` (ISM, lisanssız),
`lora_ism` (868/915 MHz, duty-cycle sınırlı), `optical_li_fi` (spektrum lisansı
gerekmez), `satellite_licensed` (**aboneliği bulunan** terminaller).

**Bilerek dışarıda bırakılanlar:**
- Üçüncü taraf uydu sinyallerinin izinsiz dinlenmesi — 5809 sayılı Kanun ve
  TCK 132–140 kapsamında suçtur
- AM/FM veya lisanslı spektrumda veri yayını — BTK lisansı olmadan kaçak
  telsiz istasyonu işletmektir

Koruma üç katmanda test edilir (carrier, router, HTTP).
`TestNormalizeRejectsIllegalCarriers` **silinmemelidir**.

---

## Egress Maliyeti Hakkında Dürüst Not

Bir proxy, bulut sağlayıcısının egress maliyetini **düşürmez** — bir bayt önce
geçide gelir (ingress), sonra hedefe gider (egress). Geçit bulut içindeyse toplam
egress azalmaz, bir kat daha eklenir.

Gerçek tasarruf topolojiktir: `edge` (bulut dışı kenar düğümü, önbellekleme),
`peering` (doğrudan eşleşme, ölçülü egress devreye girmez). Kod tasarrufu
yaratmaz, o topolojiyi **uygulanabilir kılar**.

---

## Mimari

```
cmd/gateway/          Giriş noktası, store/limiter/prober seçimi
internal/config/      Ortam okuma + başlangıç doğrulaması
internal/carrier/     Yasal taşıyıcı allowlist'i
internal/store/       Store arayüzü + Memory + Postgres + WAL + migration
internal/meter/       Ölçüm cephesi
internal/middleware/  Auth, rate limit (bellek + Redis), log, kurtarma
internal/router/      Yönlendirme + failover/healthcheck
internal/tunnel/      HTTP uçları
```

`Store` ve `Limiter` arayüzleri sayesinde implementasyon seçimi tek bir ortam
değişkenidir; uygulamanın geri kalanı hangisinin aktif olduğunu bilmez.

---

## Uçlar

| Metot | Yol | Kimlik | Açıklama |
|---|---|---|---|
| GET | `/healthz` | — | Sağlık, store türü, rotalar, `route_health` |
| POST | `/api/v1/tunnel` | Bearer | Ölç, (varsa) ilet, imzalı makbuz dön |
| GET | `/api/v1/meter/me` | Bearer | **Yalnızca kendi** tüketim defteriniz |

---

## Kurulum

### Docker Compose (üretim)

```bash
cp .env.example .env
openssl rand -hex 24   # AETHERIS_API_KEYS icin
openssl rand -hex 32   # AETHERIS_RECEIPT_SECRET icin
docker compose up --build
```

PostgreSQL, Redis, migration ve WAL otomatik gelir.

### Yerel geliştirme

```bash
cp .env.example .env      # doldurun
set -a; source .env; set +a
make run
```

## Test

```bash
make race          # yaris kosulu denetimiyle
make check         # fmt + vet + race

# Canli servis gerektirenler:
AETHERIS_TEST_DSN="postgres://..." make integration
AETHERIS_TEST_REDIS="127.0.0.1:6379" make integration-redis
```

Entegrasyon testleri `//go:build integration` etiketiyle ayrılmıştır; normal
`go test ./...` çalıştırması veritabanı gerektirmez. Redis testleri
`AETHERIS_TEST_REDIS` tanımsızsa atlanır.

---

## Bilinen sınırlar

1. **WAL tek dosyadır.** Tam batch başarılı olunca truncate edilir. Yüksek
   hacimde segment-bazlı rotasyon gerekir.
2. **Bir WAL dizini = bir süreç.** İki Aetheris örneği aynı `AETHERIS_WAL_DIR`
   değerini paylaşırsa aynı kayıtlar iki kez işlenir — **çift faturalama**.
   Çok düğümlü dağıtımda her düğüm kendi WAL dizinini kullanmalıdır.
   Windows'ta ikinci örnek açılışta anlamlı bir hata ile durur; Linux'ta
   dosya kilidi olmadığı için bu koruma yoktur, konfigürasyona dikkat edin.
3. **WAL fail-open'dır.** Yukarıdaki takas notunu okuyun.
4. **mTLS yok.** Kurumsal istemci sertifikaları v0.3b konusudur.
5. **TLS sonlandırma yok.** Üretimde önüne reverse proxy (nginx/Caddy) veya
   yük dengeleyici konmalı — şu an düz HTTP dinler.
6. **Dedup ve QoS probe yok.** v0.3b'de, dürüst versiyonlarıyla:
   dedup **istemci tarafında** (sunucu şifreli veride tekrar göremez —
   AES-GCM rastgele nonce kullanır), QoS probe pazarlama vaadi olmadan
   gerçek latency/packet-loss ölçümü.
7. **Faturalama köprüsü yok.** Stripe/e-Fatura entegrasyonu v0.3b.
