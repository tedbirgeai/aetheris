# TEDBİRGE PROTOKOL

**Off-Grid ve Kesintili Ağlar İçin Gecikmeye Dayanıklı (DTN) Hibrit Mesh Mimarisi ve Yerel Defter Standardı**

* **Sürüm:** v1.0.0-enterprise
* **Tarih:** Temmuz 2026
* **Yazar / Sistem Mimarı:** Mehmet DİNÇ
* **Lisans:** Proprietary / B2B Enterprise

---

## 1. MİMARIN MANİFESTOSU

*"1998 yılında Adıyaman Teknik Lisesi Yazılım Bölümü’nde ilk kodlarımızı derlediğimizde, ekranın karartısı karşısında yalnız ama tam anlamıyla özgürdük. Hazır kütüphanelerin, devasa bulut katmanlarının ve her hareketimizi kısıtlayan merkezi tekel yapıların arkasına sığınmamıştık. Bellek yönetimiyle, donanımın ham gücüyle ve ağın gerçekçi, tavizsiz çıplaklığıyla yüzleşmek zorundaydık; o günlerde yazılan her satır kod, bir keşif ve bir hayatta kalma mücadelesiydi.

Bugünün dijital dünyası ise 'her an kesintisiz internet var' varsayımına dayanan, son derece kırılgan ve bağımlı sistemlerle örülü. Merkezi omurgalara ve altyapı tüccarlarının insafına bırakılmış bu düzen; bir kablo koptuğunda, bir merkezi bulut sunucusu çöktüğünde veya coğrafi sınırlar daraldığında bütünüyle felç oluyor. Sahra Çölü'ndeki bir çobanın, kriz anındaki bir saha tesisinin veya kendi kaderini tayin etmek isteyen bağımsız bir işletmenin verisi bu dayatmacı çarklar arasında kaybolup gidiyor.

Tedbirge Protokolü; işte bu kırılganlığa, merkezi köleliğe ve tek merkezli ağ bağımlılığına karşı, Adıyaman’ın o ilk günkü kodlama ruhuyla yakılmış bir meşale, tavizsiz bir başkaldırıdır. 'İnternet bir lükstür, yerel dayanıklılık ve tam otonomi ise vazgeçilmez bir haktır' şiarıyla doğdu. Tedbirge; fiber hatlar koptuğunda, omurgalar çöktüğünde veya bulutlar tamamen yıkıldığında bile durmaz. Veriyi yerelde WAL defteriyle diske kazır, radyo dalgalarıyla en zorlu coğrafyaları aşarak taşır ve ilk çıkış kapısını (gateway / exit relay) bulduğu an dış dünyayla sıfır sürtünmeyle yeniden kenetlenir.

Bu sadece bir yazılım veya protokol mimarisi değil; yılların birikimiyle yazılmış, bağımsızlığın ve adanmışlığın dijital manifestosudur."*

— Mehmet DİNÇ, Protokol Mimarı

---

## 2. PROBLEM TANIMI

Geleneksel B2B ve endüstriyel ağ mimarileri 3 ana zafiyet barındırır:

1. **Merkezi Bulut Bağımlılığı (Single Point of Failure):** İnternet bağlantısı kesildiğinde yerel cihazlar birbiriyle konuşsa dahi işlem yapamaz hale gelir.
2. **Yüksek Gecikme ve Veri Kaybı (RTT & Packet Loss):** Zayıf kapsama alanlarında veya kesintili hatlarda klasik TCP/IP el sıkışmaları başarısız olur.
3. **Çift Harcama ve Veri Tutarsızlığı (Double Spending / Desync):** İnternetsiz ortamda üretilen verilerin/fatura fişlerinin merkeze aktarılırken kaybolması veya mükerrer işlenmesi.

---

## 3. TEDBİRGE GATEWAY MİMARİSİ VE ÇEKİRDEK BİLEŞENLER

Tedbirge Protokolü, bu sorunları **3 Katmanlı Otonom Mimari** ile çözer:

### Katman 1: Yerel Mesh, Gossip Keşfi ve Exit Relay Köprülemesi
* **Gossip Protokolü ve Zero-Conf:** Cihazlar merkezi bir DNS veya DHCP sunucusuna ihtiyaç duymadan, yerel ağ (LAN/Wi-Fi) veya LoRa/Wi-Fi HaLow radyo frekansları üzerinden birbirini otomatik olarak keşfeder.
* **Exit Relay (Çıkış Düğümü):** Bölgesel internet kesintilerinde, ağ üzerindeki herhangi bir düğümün aktif internet bağlantısı (uydu, 4G veya yedek hat) varsa, diğer düğümler tüm dış dünya trafiğini bu düğüm üzerinden güvenli tünellerle (`Exit Relay`) dış dünyaya aktarır.

### Katman 2: WAL (Write-Ahead Log) ve DTN (Store-and-Forward) Motoru
* **Anlık Disk Mühürleme (WAL):** Dış dünya ile bağ koptuğu an, gitmek isteyip de gidemeyen tüm paketler havaya uçmaz; milisaniyeler içinde yerel diske Write-Ahead Log defteriyle güvenle kazınır.
* **Gecikmeye Dayanıklı Ağ (DTN):** Veriler `PENDING` statüsünde DTN demetleri (`Bundles`) halinde kuyruklanır. İnternet hattı veya uygun bir taşıyıcı (kurye node / uydu) bulunur bulunmaz, kuyruk sıfır veri kaybıyla karşı tarafa fırlatılır (`Seamless Handoff`).

### Katman 3: Çift Arayüz Katmanı ve Ticari Entegrasyonlar (v1.0.0-enterprise)
* **Tedbirge Off-Grid Control Plane (`/admin`):** WebSocket tabanlı canlı telemetri akışı; aktif mesh düğümleri, RTT gecikmeleri, WAL kuyruk derinliği, disk kullanımı ve SOCKS5 tünel durumlarının anlık görselleştirilmesi. Otonom düğüm konfigürasyon paketi (`POST /admin/deploy`) ile tek hamlede node üretimi.
* **Tedbirge Loop Kiracı (Tenant) Paneli (`/kiracı`):** Çok kiracılı (multi-tenant) mimari ile her API anahtarı için anlık bant genişliği takibi, WAL geçmişi ve kota izolasyonu.
* **Tedbirge Pay & e-Fatura / e-Arşiv Modülü (`/billing` / `pkg/billing`):** Türkiye yasal mevzuatına uygun (VKN/TCKN, %20 KDV, matrah hesaplama) e-Fatura taslak üreticisi ve Stripe webhook / Checkout entegrasyonu.
* **Ed25519 Voucher Kontör Sistemi (`pkg/voucher`):** Çöllerde veya tamamen off-grid ortamlarda internet olmasa bile şifreli, imzalı kontör kodlarıyla (`TEDB-XXXX-...`) çevrimdışı ödeme ve lisans doğrulaması; işlemlerin Zero-Knowledge prensibiyle WAL ledger'a işlenmesi.

---

## 4. GÜVENLİK VE SIFIR-KVKK STANDARTLARI

* **Payload Şifrelemesi:** Tüm veri iletimi uçtan uca şifrelenir; sistem yalnızca payload SHA-256 özetini ve bayt sayımını tutar.
* **Fail-Closed Mimarisi:** Doğrulama ve güvenlik katmanları varsayılan olarak kapalı (fail-closed) gelir; token veya API anahtarı olmadan hiçbir tünel veya admin uç noktası aktifleşmez.

---

## 5. SONUÇ

Tedbirge Protokolü; geleneksel telekomünikasyon altyapılarına ve merkezi bulut sağlayıcılarına bağımlılığı ortadan kaldıran, internet kesintilerinde dahi WAL ve DTN motorlarıyla kesintisiz çalışan, ticarileşmeye ve enterprise ölçeğe hazır yeni nesil otonom ağ standardıdır.
