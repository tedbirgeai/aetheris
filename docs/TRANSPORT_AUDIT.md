# AETHERIS — Taşıyıcı Boru Hatları ve Yasal/Teknik Uyum Denetimi

**Sürüm:** v0.6b-turnkey-production
**Kapsam:** Taşıyıcı (transport) envanteri, RF/ISM mevzuatı, KVKK/GDPR zırhı, BTK 5651 sorumluluk matrisi, afet iletişim muafiyetleri.

> **Yasal uyarı:** Bu belge mühendislik-düzenleme referansıdır, **hukuki tavsiye değildir**. Aetheris ekibi avukat değildir. Aşağıdaki çıkış gücü, görev döngüsü ve frekans değerleri genel çerçevedir; nihai olarak **yürürlükteki BTK (ICTA), ETSI/CEPT, FCC** belgeleriyle ve gerektiğinde bir hukukçuyla doğrulanmalıdır. Değerler zamanla değişebilir.

---

## 1. Taşıyıcı Boru Hatları Envanteri (v0.6b durumu)

Durum etiketleri: 🟢 **AKTİF** (kodda, test edildi) · 🟡 **ARAYÜZ HAZIR** (donanım/entegrasyon bekliyor) · ⚪ **ETİKET** (muhasebe enum'u) · 🔴 **YOK**.

### A. IP Tabanlı Taşıyıcılar (ek donanımsız)
| Boru hattı | Durum | Not |
|---|---|---|
| Wi-Fi (IP uplink) | 🟢 AKTİF | Gateway TCP/UDP + otomatik keşif UDP broadcast |
| Ethernet / LAN | 🟢 AKTİF | IP arayüzü olarak tam çalışır |
| Hücresel 4G/5G hotspot (IP) | 🟢 AKTİF | Tethering IP verdiğinde transparan; modem sürücüsü yok |
| Wi-Fi Direct / SoftAP (radyo kontrolü) | 🟡 ARAYÜZ HAZIR | `internal/transport/driver` HAL var; çip sürücüsü v0.6b+ |

### B. RF / Radyo Taşıyıcılar
| Boru hattı | Durum | Not |
|---|---|---|
| LoRa 868/915 MHz ISM | 🟡 ARAYÜZ HAZIR | `lora.Driver` + `SerialDriver` + mock gateway'e bağlı; **Zero-KVKK ephemeral framing** (`internal/carrier/ephemeral`) aktif+test; RF için fiziksel modem gerekir |
| Uydu (Starlink/VSAT Ethernet köprüsü) | 🟢 AKTİF | IP uplink olarak transparan |
| BLE / Bluetooth Mesh | 🟡 ARAYÜZ HAZIR | HAL soyutlaması hazır; çip kütüphanesi yok |
| HF/VHF/UHF Packet Radio (AX.25) | 🔴 YOK | Amatör lisans gerektirir; kodda yok |

### C. Yazılımsal Tünel Taşıyıcılar
| Boru hattı | Durum | Not |
|---|---|---|
| UDP broadcast otomatik keşif | 🟢 AKTİF | `internal/router/discovery` — Zero-Conf, SO_REUSEPORT |
| Şifreli TCP relay (AES-256-GCM) | 🟢 AKTİF | `internal/relay` — exit node WAN köprüsü, uçtan uca test |
| Çok-sıçramalı mesh yönlendirme | 🟢 AKTİF | `internal/router/mesh` — Dijkstra, TTL, döngü engelleme |
| Zero-KVKK RF framing | 🟢 AKTİF | `internal/carrier/ephemeral` — IP/MAC baypas, dönen hash |
| Canlı link-sağlık + failover | 🟢 AKTİF | `internal/router/health` — RTT/heartbeat, otomatik yeniden yönlendirme |
| mDNS / Zero-Conf daemon | 🔴 YOK | Bilinçli olarak UDP broadcast ile ikame edildi |
| Channel bonding / path multiplexing | 🔴 YOK | Tek-yol seçimi; paralel bonding yok |

---

## 2. RF / ISM Frekans Mevzuatı

Lisanssız Kısa Mesafe Erişimli Telsiz (SRD/KET) cihazları coğrafyaya göre farklı bantlarda çalışır. Sub-GHz SRD dört ana bantta bulunur: 315, 433, 868, 915 MHz; hangisinin yasal olduğu bölgeye bağlıdır.

| Bölge | Bant | Çerçeve | Tipik çıkış gücü / kısıt |
|---|---|---|---|
| Avrupa / Türkiye | 868 MHz | ETSI EN 300 220, ERC/REC 70-03 | Genel SRD ~25 mW (14 dBm) ERP; bazı alt bantlarda görev döngüsü kısıtıyla 500 mW'a kadar |
| ABD / Kanada | 902–928 MHz | FCC Part 15 | Yayılı spektrum/frekans atlama, ~1 W'a kadar |
| Küresel | 2.4 / 5.8 GHz | ITU RR 5.150 | Wi-Fi/BT; bölgesel EIRP sınırları |

**Yaklaşım farkı:** Avrupa alt bantlarda **görev döngüsü (duty-cycle)** sınırları uygularken, ABD **frekans atlama** yaklaşımını benimser (ERC/REC 70-03 ve FCC Part 15).

**Türkiye (BTK):** Türkiye CEPT üyesidir ve genel olarak ERC/REC 70-03 uyumlu SRD tahsisi uygular; 868 MHz kullanımı AB çerçevesine benzer. Kesin çıkış gücü, görev döngüsü ve kanal planı için **yürürlükteki BTK "Kısa Mesafe Erişimli Telsiz Cihazları (KET) Yönetmeliği"** esastır. **915 MHz Türkiye/AB'de genel SRD için kullanılmamalıdır** (yanlış bant ihlaldir); LoRa modülü bölgeye uygun banda ayarlı olmalıdır.

---

## 3. KVKK / GDPR Uyum Matrisi (Zero-KVKK RF Katmanı)

Aetheris'in RF/Seri katmanı (`internal/carrier/ephemeral`) **IP ve MAC adresi taşımaz**. Havaya çıkan her çerçeve:

```
[1B Magic/Flags][8B Dönen Hedef Hash][12B AES Nonce][AES-256-GCM şifreli payload]
```

| KVKK/GDPR ilkesi | Aetheris teknik karşılığı |
|---|---|
| Veri minimizasyonu | Telde IP/MAC/sabit kimlik yok; yalnızca dönen hash + şifreli yük |
| İlişkilendirilemezlik (unlinkability) | Hedef hash her epoch'ta döner; aynı düğümün ardışık paketleri dışarıdan **bağlanamaz** |
| Şifreleme | AES-256-GCM; payload içeriği ağdaki aracı düğümlere **kapalı** (zero-knowledge) |
| Bütünlük | GCM auth tag + AAD'ye bağlı başlık; kurcalama reddedilir |

**Önemli sınır:** Bu, *RF/Seri taşıma katmanında* kişisel veri açığa çıkmamasını sağlar. Uygulama katmanında taşınan içerik kişisel veri içeriyorsa, KVKK yükümlülükleri (aydınlatma, rıza, saklama) **uygulama sahibinindir**; taşıma katmanı zırhı bunu ortadan kaldırmaz.

---

## 4. BTK 5651 ve Aracı Sorumluluğu (Local Signed Receipts)

Exit node (WAN çıkışı sunan düğüm), başkalarının trafiğini taşır. Aetheris'in **Ed25519 dijital imzalı yerel fişleri** (`internal/billing/ledger`) bu transiti inkâr-edilemez biçimde kayıt altına alır:

| Gereksinim | Aetheris mekanizması |
|---|---|
| Kim ne kadar trafik üretti? | Origin tarafından imzalı fiş (Local Signed Receipt); kriptografik kanıt |
| İçerik denetimi yapılıyor mu? | Hayır — zero-knowledge; exit yalnızca hedef adresi bilir, içeriği değil (salt taşıyıcı argümanı) |
| İnkâr edilebilirlik | Ed25519 imza sahteciliğe kapalı; fiş origin'in özel anahtarıyla bağlıdır |

**5651 sorumluluk sınırı:** Bu mekanizma **teknik** güvence sağlar (kim, ne kadar, ne zaman). Ancak 5651 kapsamındaki *hukuki* aracı/yer sağlayıcı yükümlülükleri (log tutma süreleri, erişim engelleme, makam taleplerine yanıt) ülke mevzuatına ve işletenin konumuna göre değişir; kod bunları otomatik karşılamaz. Exit node işleten taraf kendi yasal yükümlülüklerini ayrıca değerlendirmelidir.

---

## 5. Afet İletişim Protokolü (Tampere Sözleşmesi & TAMP)

**Tampere Sözleşmesi** (1998'de kabul, 2005'te yürürlük), afet azaltma ve yardım operasyonlarında telekomünikasyon kaynaklarının kullanımı önündeki düzenleyici engelleri azaltır/kaldırır: taraf devletler, **lisans gereklilikleri, belirli ekipman veya radyo-frekans spektrumu kullanım kısıtları ve ekipman ithalat/ihracat engellerini** afet yardımı için azaltmayı taahhüt eder. 2025 sonu itibarıyla 50 taraf devlet vardır.

**Aetheris'in afet konumu:** Off-grid mesh + LoRa omurgası, altyapının çöktüğü afet senaryolarında (deprem, sel) yerel iletişimi ayakta tutmak için tasarlanmıştır. Tampere çerçevesinde, yetkili afet yardım operasyonları kapsamında normalde lisans gerektirebilecek RF kullanımlarına muafiyet tanınabilir — **ancak bu muafiyet otomatik değildir**; ilgili devletin (Türkiye için AFAD/BTK koordinasyonu) onayına ve operasyonun resmi afet yardımı kapsamında olmasına bağlıdır. Rutin/ticari kullanım bu muafiyet kapsamında **değildir** ve normal KET/ISM sınırlarına tabidir.

---

## Özet

v0.6b'de fiilen veri taşıyan ve test edilen taşıyıcılar: IP (Wi-Fi/Ethernet/hücresel/uydu), UDP broadcast keşif, AES-256-GCM TCP relay (exit node), çok-sıçramalı mesh, Zero-KVKK RF framing, canlı sağlık/failover. Arayüzü hazır, donanım bekleyen: LoRa RF, BLE/SoftAP. Kodda olmayan: Packet Radio, mDNS daemon, channel bonding. Yasal çerçeve mühendislik referansıdır; yürürlükteki BTK/ETSI/FCC belgeleri ve hukukçu görüşü esastır.
