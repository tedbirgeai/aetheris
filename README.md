# Aetheris Enterprise Gateway v0.3b

Taşıyıcı-bağımsız (PHY-agnostic), sıfır-bilgi (zero-knowledge) tünel geçidi:
bayt bazlı ölçüm, kalıcı faturalama defteri, dayanıklı kuyruk, dağıtık hız
sınırlama, failover'lı yönlendirme, mTLS, istemci taraflı tekilleştirme ve
ticari faturalama köprüsü.

**Doğrulama durumu**

| Kontrol | Sonuç |
|---|---|
| `gofmt -l .` | temiz |
| `go vet ./...` | temiz |
| `go test -race -count=1 ./...` | **11/11 paket geçti** |
| Entegrasyon (canlı PostgreSQL 16) | geçti |
| Redis testleri (canlı Redis 7) | geçti |
| mTLS (gerçek sertifikalarla) | geçti |
| Dedup uçtan uca | %95.4 ölçülen tasarruf |
| Faturalama webhook | imza geçerli, yük sızıntısı yok |

---

## v0.3b'de yeni olanlar

### 1. Hermetik Test Koşucusu (`scripts/run-tests.sh`)

**Sorun:** Windows'ta `gcc` yok, dolayısıyla `go test -race` çalışmıyor. Yarış
koşulu denetimi geliştiricinin işletim sistemine bağımlı kalıyordu.

**Çözüm:** Testler CGO açık bir Linux konteynerinde, gerçek PostgreSQL 16 ve
Redis 7 eşliğinde koşar:

```bash
make test-race
```

Tek komut: konteynerleri kaldırır, `gofmt` + `vet` + `-race` + entegrasyon
testlerini koşar, temizler. Çıkış kodu betiğe taşınır (CI uyumlu).

### 2. İstemci Taraflı Dedup + Dürüst Manifest Doğrulama

**Neden istemci tarafında?** Sunucu şifreli veriyi görür. AES-256-GCM her
şifrelemede rastgele nonce kullanır; aynı düz metin her seferinde **farklı**
ciphertext üretir. Sunucu tekrar eden parçayı **göremez**, dolayısıyla
tekilleştiremez. "Sunucu %80 dedup yapıyor" iddiası ancak sunucu düz metni
görürse doğru olabilirdi — o da tüm sıfır-bilgi mimarisini geçersiz kılardı.

**Sunucu neyi doğrular:**
- Manifest yapısal geçerliliği (parça sayısı, boyutlar)
- Beyan edilen boyut ile gövdedeki gerçek boyutun **eşleşmesi**
- Gönderilen parça sayısının manifestteki "yeni" sayıyla tutarlılığı

**Sunucu neyi doğrulayamaz (ve doğruladığını iddia etmez):**
- Parça özetinin gerçekten o düz metnin özeti olduğu
- İstemcinin "bunu daha önce gönderdim" beyanının doğruluğu

`VerificationResult.ContentVerified` **her zaman `false`**. Bir test bunu
kilitler ve silinmemelidir.

**Faturalama kuralı:** Telde **gerçekten akan** bayta göre. İstemci yalan
söylerse veri hedefte eksik olur — kendi zararı, sunucunun gelirine zarar
veremez.

Ölçülen gerçek sonuçlar (uçtan uca, 17 KB log verisi):

```
Tur 1 | parça=5 (yeni=5 önbellek=0) | TELDE=17290 B | tasarruf= 0.0%
Tur 2 | parça=5 (yeni=1 önbellek=4) | TELDE=  794 B | tasarruf=95.4%
Tur 3 | parça=5 (yeni=1 önbellek=4) | TELDE=  794 B | tasarruf=95.4%
```

Rastgele veride tasarruf **sıfırdır** — bunu doğrulayan ayrı bir test var
(`TestDedupSavesNothingOnRandomData`). Tekilleştirme yalnızca tekrar eden
veride işe yarar; kütüphane oranı **ölçer**, vaat etmez.

### 3. mTLS ve TLS Sonlandırma (`internal/security`)

- HTTPS dinleme, TLS 1.2 alt sınır, ileri gizlilik sağlayan şifre takımları
- **Sertifika rotasyonu:** dosya değişince süreç yeniden başlatılmadan devreye
  girer (`GetCertificate` her el sıkışmada okur)
- **mTLS:** `require` modunda sertifikasız istemci reddedilir, yabancı CA'dan
  imzalı sertifika reddedilir
- Süresi dolmuş sertifikayla açılış **engellenir** (aksi halde geçit
  "çalışıyor" görünür ama her el sıkışma başarısız olur)
- Bozuk dosya yazılırsa **eski sertifika korunur** (yarım yazılmış dosya
  yüzünden hizmet düşmez)

Bunların hepsi gerçek sertifikalar üretilerek test edildi.

### 4. Dürüst QoS Probe (`internal/router/qos.go`)

Gerçek ölçümler: RTT (ortalama, min, max, p50, p95) ve jitter.

**Terminoloji uyarısı — kodda da yazılı:**

`probe_failure_ratio` **paket kaybı değildir.** Gerçek paket kaybı ICMP veya
UDP seviyesinde tekil paketler sayılarak ölçülür ve ham soket yetkisi
(`CAP_NET_RAW`) gerektirir. HTTP yoklamasının başarısız olması paket kaybına
işaret *edebilir*, ama sunucu aşırı yükü, TLS hatası veya uygulama hatası da
olabilir. Bu değeri "packet loss" diye sunmak müşteriyi yanıltmak olurdu.

Aynı şekilde RTT, uygulama katmanı gidiş-dönüş süresidir; TLS handshake ve
sunucu işleme süresi dahildir, ICMP ping değildir.

### 5. Faturalama Köprüsü (`internal/billing`)

Asenkron olay dağıtıcı: `ReceiptGenerated`, `CreditEarned`,
`UsageThresholdExceeded`.

- **Asla bloklamaz** — Stripe yavaşlarsa müşteri isteği etkilenmez
- Üstel geri çekilmeli yeniden deneme, idempotency anahtarı
- **Webhook:** gövde HMAC-SHA256 ile imzalanır, alıcı doğrulayabilir
- **Röle kredileri:** kripto/token yok — bir sonraki faturadan düşülecek
  ticari iskonto. Kendi trafiğini röle etmek kredi kazandırmaz; dönem başına
  tavan uygulanır
- **Eşik izleyici:** her eşik için yalnızca bir kez olay

**Yük içeriği asla gönderilmez** — sadece SHA-256 özeti. Bir test bunu
doğrular.

**Doğrulanmamış olanlar:** Stripe ve e-Fatura emitter'ları canlı API'lere
karşı test edilmedi (gerçek hesap kimlik bilgisi gerekir). Kodda uyarı
olarak yazılı. Türkiye'de tek bir standart e-Fatura API'si yoktur; her
entegratörün (Logo, Uyumsoft, Paraşüt) sözleşmesi farklıdır — o uç nokta
kendi adaptör servisinize işaret etmelidir.

### 6. Mobil Saha Testi (`make mobile-test`)

Telefondan test etmek için dört adım otomatik:

1. Geçidin localhost'ta çalıştığını doğrular
2. LAN IP adreslerini bulur (Docker/WSL sanal adapterlerini eler)
3. LAN üzerinden **gerçekten erişilebildiğini** doğrular — erişilemiyorsa
   sebebini söyler (güvenlik duvarı komutunu hazır verir)
4. Telefonla taranabilir QR kodu terminale basar

Go ile yazıldı, bash değil: Windows'ta `ip`/`ifconfig` yok.

**Tünel:** Varsayılan LAN erişimidir (aynı WiFi, güvenli). `--tunnel` bayrağı
public tünel açar ve geçidi **tüm internete** açar; geçerli API anahtarlarınız
olduğu için opt-in ve açık uyarılıdır.

**3. adım hakkında dürüst not:** Bu kontrol geçidin LAN adresine *kendi
makinenizden* istek atar. O trafik işletim sisteminin ağ yığınında kalır ve
**güvenlik duvarının gelen kuralına takılmaz** — yani "yanıt verdi" dese bile
telefon bağlanamayabilir. Araç bunu açıkça söyler ve güvenlik duvarı
komutlarını önden basar. İlk sürümde bu adım sessizce "OK" diyordu ve
geliştiriciyi körlemesine denemeye itiyordu; saha testinde ortaya çıktı ve
düzeltildi.

**Windows'ta en sık tuzak:** Docker Desktop ilk açılışta güvenlik duvarı izni
sorar. "İptal" denirse Windows o program için bir **engelleme kuralı** oluşturur
ve engelleme kuralları, sonradan eklenen izin kurallarından **önceliklidir**.
Port kuralınız doğru olsa bile bağlantı kurulmaz. Araç bu kuralı nasıl bulup
sileceğinizi de yazdırır.

---

## Sıfır-Bilgi Sözleşmesi

Bu sunucu, taşıdığı veriyi çözebilecek hiçbir anahtara **sahip değildir**.

Sözleşme her katmanda geçerlidir:
- **Log:** istek/yanıt gövdesi asla loglanmaz
- **Veritabanı:** yük saklanmaz, yalnızca SHA-256 özeti (bir test tabloda
  `payload`/`ciphertext`/`body`/`content` sütunu olmadığını doğrular)
- **WAL:** yalnızca metadata ve özet
- **Router:** içeriği çözmez, değiştirmez
- **Faturalama:** olaylarda yük içeriği yok, yalnızca özet
- **Dedup:** tekilleştirme istemcide; sunucu manifest yapısını doğrular,
  içeriği değil

---

## Hukuki Kapsam

`internal/carrier/carrier.go` taşıyıcı listesi **bilinçli olarak kısıtlıdır**.

**İzin verilenler:** `standard_internet`, `mesh_wifi` (ISM, lisanssız),
`lora_ism` (duty-cycle sınırlı), `optical_li_fi`, `satellite_licensed`
(**aboneliği bulunan** terminaller).

**Bilerek dışarıda:** üçüncü taraf uydu sinyallerinin izinsiz dinlenmesi
(5809 sayılı Kanun, TCK 132–140) ve lisanslı spektrumda veri yayını (BTK
lisansı olmadan kaçak telsiz istasyonu).

`TestNormalizeRejectsIllegalCarriers` **silinmemelidir**.

---

## Egress Maliyeti Hakkında Dürüst Not

Bir proxy, bulut sağlayıcısının egress maliyetini **düşürmez** — bayt önce
geçide gelir (ingress), sonra hedefe gider (egress). Geçit bulut içindeyse
toplam egress azalmaz, bir kat daha eklenir.

Gerçek tasarruf iki yerden gelir:
1. **Topoloji:** `edge` (bulut dışı önbellekleme), `peering` (ölçülü egress
   devreye girmez)
2. **Dedup:** tekrar eden veride ölçülen gerçek tasarruf (yukarıdaki %95.4
   örneği), ama rastgele veride sıfır

---

## Mimari

```
cmd/gateway/          Giriş noktası
cmd/mobiletest/       Mobil saha testi (LAN IP + QR)
internal/config/      Ortam okuma + başlangıç doğrulaması
internal/carrier/     Yasal taşıyıcı allowlist'i
internal/store/       Store arayüzü + Memory + Postgres + WAL
internal/meter/       Ölçüm cephesi
internal/middleware/  Auth, rate limit (bellek + Redis), log, kurtarma
internal/router/      Yönlendirme + failover + QoS probe
internal/dedup/       Manifest tipleri + sunucu doğrulama
internal/security/    TLS / mTLS + sertifika rotasyonu
internal/billing/     Olay köprüsü + emitter'lar + kredi motoru
internal/metrics/     Prometheus text format
internal/tunnel/      HTTP uçları
pkg/client/           İstemci SDK: chunking, dedup, şifreleme
```

`pkg/client` dış kullanıma açıktır; `internal/*` Go kuralı gereği dışarıdan
import edilemez.

---

## Uçlar

| Metot | Yol | Kimlik | Açıklama |
|---|---|---|---|
| GET | `/healthz` | — | Sağlık, store, rotalar, QoS, TLS, billing |
| POST | `/api/v1/tunnel` | Bearer | Ölç, ilet, imzalı makbuz |
| POST | `/api/v1/tunnel/chunked` | Bearer | Parçalı (dedup) geçiş |
| GET | `/api/v1/meter/me` | Bearer | Yalnızca kendi defteriniz |
| GET | `/metrics` | Bearer (ayrı token) | Prometheus formatı |

`/metrics` müşteri kimlikleri ve hacimleri içerir — **ticari olarak
hassastır**. Token tanımlı değilse uç nokta hiç açılmaz; geçit açılışta hata
verir.

---

## Kurulum

```bash
cp .env.example .env
openssl rand -hex 24   # AETHERIS_API_KEYS
openssl rand -hex 32   # AETHERIS_RECEIPT_SECRET
docker compose up --build
```

## Test

```bash
make test         # yerel, -race yok (Windows uyumlu)
make test-race    # HERMETIK: Linux konteynerinde -race + gerçek DB
make check        # fmt + vet + test
make mobile-test  # telefon erişimi + QR
```

---

## Bilinen sınırlar

1. **WAL tek dosyadır.** Yüksek hacimde segment-bazlı rotasyon gerekir.
2. **Bir WAL dizini = bir süreç.** İki örnek aynı dizini paylaşırsa çift
   faturalama olur; Windows'ta ikinci örnek hata ile durur, Linux'ta bu
   koruma yoktur.
3. **WAL fail-open'dır.** Kısa bir pencerede kayıt WAL'de ama Postgres'te
   değildir.
4. **Stripe/e-Fatura canlı doğrulanmadı.** Kimlik bilgisi gerekir.
5. **Dedup istemciler arası paylaşımlı değildir.** Paylaşımlı dedup,
   özetlerin sunucuya açıklanmasını gerektirir ve içerik hakkında bilgi
   sızdırır (confirmation-of-file saldırısı). Bilinçli olarak yapılmamıştır.
6. **Rate limiter Redis'siz tek düğümlüdür.** `AETHERIS_REDIS_ADDR`
   tanımlıysa dağıtık, değilse düğüm-yereldir.
7. **Gerçek paket kaybı ölçülmez.** ICMP prober ayrı bir bileşen gerektirir.
