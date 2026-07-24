// Package gossip, merkezi sunucu/DNS olmadan calisan otonom dugum kesfi
// ve epidemik (anti-entropy) veri yayilimi saglar.
//
// # TASARIM
//
// Her dugum bir Set tutar: icerik-adresli (SHA-256) kayitlarin kumesi.
// Dugumler periyodik olarak komsulariyla DIGEST (elimdeki ID listesi)
// degis-tokus eder. Push-pull anti-entropy ile eksikler tek turda
// kapanir:
//
//	A --digest(A.IDs)--> B
//	B: missing = A.IDs \ B.IDs  -> B --pull(missing)--> A
//	B: extra   = B.IDs \ A.IDs  -> B --push(extra)-->   A
//	A --push(pull edilenler)--> B
//
// Digest'e digest ile YANIT VERILMEZ; bu, sonsuz ping-pong'u onler.
// Sonuc: baglanti kopup birlestiginde (MADDE 4) iki tarafin kayitlari
// SIFIR KAYIPLA birlesir.
//
// # DURUSTLUK NOTU
//
// Kesif iki katmanlidir. Yerel agda UDP broadcast beacon'i GERCEK bir
// soket kullanir (UDPBeacon). Cevrimdisi/donanim senaryosunda (BLE/LoRa
// beaconing) ayni Beacon arayuzu, LoRa HAL'in mock ortami veya gercek
// modem uzerinden surulur — bu paket beacon'in ALTINDAKI tasiyiciya
// bakmaz, yalnizca "komsu duyuruldu" olayini tuketir. Boylece ayni
// gossip mantigi hem Wi-Fi/LAN hem LoRa uzerinde degismeden calisir.
package gossip

import "sync"

// Record, gossip ile cogaltilan tek bir veri ogesidir.
// ID, Data'nin icerik ozetidir (SHA-256 hex); ayni veri her dugumde ayni
// ID'yi uretir, bu da tekilligi (idempotency) ucretsiz saglar.
type Record struct {
	ID   string `json:"id"`
	Data []byte `json:"data"`
}

// Set, bir dugumun tuttugu kayitlarin es-zamanli guvenli kumesidir.
type Set struct {
	mu    sync.RWMutex
	items map[string]Record
}

// NewSet, bos bir kume olusturur.
func NewSet() *Set {
	return &Set{items: make(map[string]Record)}
}

// Add, kaydi ekler. Kayit YENIyse true doner (ilk kez goruldu).
func (s *Set) Add(r Record) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[r.ID]; ok {
		return false
	}
	// Data'nin kopyasini tut; cagiran tamponu degistirse bile kume bozulmasin.
	cp := make([]byte, len(r.Data))
	copy(cp, r.Data)
	s.items[r.ID] = Record{ID: r.ID, Data: cp}
	return true
}

// Has, verilen ID'nin kumede olup olmadigini bildirir.
func (s *Set) Has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.items[id]
	return ok
}

// Get, ID'ye karsilik gelen kaydi dondurur.
func (s *Set) Get(id string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.items[id]
	return r, ok
}

// IDs, kumedeki tum ID'leri dondurur (digest icin).
func (s *Set) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.items))
	for id := range s.items {
		out = append(out, id)
	}
	return out
}

// Len, kumedeki kayit sayisi.
func (s *Set) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// missing, verilen ID listesinden kumede OLMAYANLARI dondurur.
func (s *Set) missing(ids []string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, id := range ids {
		if _, ok := s.items[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// extra, kumede olup verilen ID listesinde OLMAYAN kayitlari dondurur.
func (s *Set) extra(theirIDs []string) []Record {
	their := make(map[string]struct{}, len(theirIDs))
	for _, id := range theirIDs {
		their[id] = struct{}{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Record
	for id, r := range s.items {
		if _, ok := their[id]; !ok {
			out = append(out, r)
		}
	}
	return out
}

// collect, verilen ID'lere karsilik gelen kayitlari dondurur (pull yaniti).
func (s *Set) collect(ids []string) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(ids))
	for _, id := range ids {
		if r, ok := s.items[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

// Kind, gossip mesaj turudur.
type Kind uint8

const (
	// KindBeacon, "buradayim" duyurusu (kesif).
	KindBeacon Kind = iota
	// KindDigest, gonderenin elindeki ID listesi.
	KindDigest
	// KindPull, istenen ID'ler.
	KindPull
	// KindPush, gonderilen kayitlar.
	KindPush
)

// Message, dugumler arasi gossip mesajidir.
type Message struct {
	Kind Kind     `json:"kind"`
	From string   `json:"from"` // dugum kimligi
	Addr string   `json:"addr"` // dugum adresi (beacon icin)
	IDs  []string `json:"ids,omitempty"`
	Recs []Record `json:"recs,omitempty"`
}
