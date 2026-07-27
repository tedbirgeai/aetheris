// Package halow, Wi-Fi HaLow (IEEE 802.11ah) sub-GHz uzun menzil kablosuz
// adaptörü için HAL (Hardware Abstraction Layer) sağlar.
//
// Wi-Fi HaLow, 900 MHz bandında çalışan uzun menzilli (1–2 km) düşük güçlü
// Wi-Fi standardıdır. LoRa'dan daha yüksek bant genişliği (~150 kbps–7.8 Mbps),
// standart Wi-Fi'den çok daha uzun menzil sunar. Off-grid IoT ve saha ağları
// için idealdir.
//
// Türkiye/Avrupa: 863–868 MHz ve 902–928 MHz SRD bantları kapsamında.
//
// DURUSTLUK NOTU: Gerçek HaLow donanım sürücüsü (Morse Micro MM6108, Newracom
// NRC7292 vb.) bu pakette YOKTUR — platform-spesifik Linux kernel modülü
// veya vendor SDK gerektirir. Bu paket HAL arayüzü + stub sağlar; gerçek
// donanım takılınca HaLowDriver arayüzünü implement eden sürücü eklenir.
// MockHaLow sürücüsü tüm işlevselliği simüle eder, testler gerçekçidir.
package halow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Band, HaLow çalışma bandıdır.
type Band string

const (
	Band863MHz Band = "863-868MHz" // Avrupa/Türkiye SRD
	Band902MHz Band = "902-928MHz" // ABD ISM
	Band916MHz Band = "916-928MHz" // Avustralya
)

// MCS, HaLow modülasyon ve kodlama şemasıdır (0-10).
// Düşük MCS = düşük hız + uzun menzil.
// Yüksek MCS = yüksek hız + kısa menzil.
type MCS int

const (
	MCS0  MCS = 0 // ~150 kbps, ~2 km menzil — off-grid tercih
	MCS1  MCS = 1
	MCS2  MCS = 2
	MCS3  MCS = 3
	MCS4  MCS = 4 // ~2 Mbps, ~1 km
	MCS5  MCS = 5
	MCS6  MCS = 6
	MCS7  MCS = 7
	MCS8  MCS = 8
	MCS9  MCS = 9 // ~7.8 Mbps, ~200m
	MCS10 MCS = 10
)

var (
	ErrNotAvailable  = errors.New("halow: donanım mevcut değil")
	ErrNotOpen       = errors.New("halow: adaptör açılmamış")
	ErrFrameTooLarge = errors.New("halow: çerçeve MTU'yu aşıyor")
)

// Config, HaLow adaptör yapılandırmasıdır.
type Config struct {
	Band       Band
	MCS        MCS
	TxPowerDBm int    // çıkış gücü (dBm), yasal sınır içinde kalmalı
	SSID       string // HaLow ağ adı
	PSK        string // WPA3 parolası (boşsa açık ağ)
	NodeID     string // bu düğümün HaLow MAC adresi (boşsa otomatik)
}

// DefaultConfig, Türkiye/Avrupa için güvenli varsayılan yapılandırma döndürür.
func DefaultConfig(nodeID string) Config {
	return Config{
		Band:       Band863MHz,
		MCS:        MCS0, // maksimum menzil
		TxPowerDBm: 14,   // 25mW ERP — BTK KET sınırı
		SSID:       "aetheris-mesh",
		NodeID:     nodeID,
	}
}

// Frame, HaLow üzerinden gönderilen/alınan bir veri çerçevesidir.
type Frame struct {
	Src     net.HardwareAddr // kaynak MAC
	Dst     net.HardwareAddr // hedef MAC (nil = yayın)
	Payload []byte           // şifreli içerik
	RSSI    int              // alım sinyal gücü (dBm), sadece Recv'de geçerli
	MCS     MCS              // kullanılan MCS seviyesi
}

// MTU, HaLow maksimum çerçeve boyutudur (MCS0'da).
const MTU = 7991 // IEEE 802.11ah maksimum MPDU boyutu (bayt)

// HaLowDriver, bir HaLow adaptörünün soyutlamasıdır.
type HaLowDriver interface {
	// Config, geçerli yapılandırmayı döndürür.
	Config() Config
	// Open, adaptörü başlatır.
	Open(ctx context.Context) error
	// Send, bir çerçeve gönderir.
	Send(ctx context.Context, f Frame) error
	// Receive, bir çerçeve alır (bloklar).
	Receive(ctx context.Context) (Frame, error)
	// Peers, ağdaki keşfedilen eşleri döndürür.
	Peers() []Peer
	// RSSI, son bağlantının sinyal gücünü döndürür.
	RSSI() int
	// Available, donanımın fiziksel varlığını bildirir.
	Available() bool
	// Close, adaptörü kapatır.
	Close() error
}

// Peer, ağda keşfedilen bir HaLow düğümüdür.
type Peer struct {
	MAC      net.HardwareAddr
	RSSI     int
	LastSeen time.Time
	NodeID   string
}

// --- Mock Sürücü (donanım olmadan simülasyon) ---

// MockHaLow, gerçek HaLow donanımını simüle eden test/geliştirme sürücüsüdür.
// Gerçek RF iletimi yoktur; aynı süreçteki diğer MockHaLow örnekleriyle
// SharedMedium üzerinden haberleşir.
type MockHaLow struct {
	cfg    Config
	medium *SharedMedium
	inbox  chan Frame
	peers  []Peer
	rssi   int
	opened atomic.Bool
	closed atomic.Bool
	mu     sync.RWMutex
	rx     atomic.Uint64
	tx     atomic.Uint64
}

// SharedMedium, birden fazla MockHaLow'un paylaştığı simüle RF ortamıdır.
type SharedMedium struct {
	mu   sync.Mutex
	subs []*MockHaLow
}

// NewSharedMedium, boş bir paylaşımlı ortam oluşturur.
func NewSharedMedium() *SharedMedium { return &SharedMedium{} }

func (m *SharedMedium) attach(d *MockHaLow) {
	m.mu.Lock()
	m.subs = append(m.subs, d)
	m.mu.Unlock()
}

func (m *SharedMedium) detach(d *MockHaLow) {
	m.mu.Lock()
	newSubs := m.subs[:0]
	for _, s := range m.subs {
		if s != d {
			newSubs = append(newSubs, s)
		}
	}
	m.subs = newSubs
	m.mu.Unlock()
}

func (m *SharedMedium) broadcast(src *MockHaLow, f Frame) {
	m.mu.Lock()
	subs := append([]*MockHaLow(nil), m.subs...)
	m.mu.Unlock()
	for _, sub := range subs {
		if sub == src || !sub.opened.Load() {
			continue
		}
		// Hedef filtresi: yayın veya bu düğüme özel.
		if f.Dst != nil && sub.cfg.NodeID != "" {
			dstStr := f.Dst.String()
			if dstStr != "ff:ff:ff:ff:ff:ff" && dstStr != sub.cfg.NodeID {
				continue
			}
		}
		f.RSSI = -65 // simüle sinyal gücü
		select {
		case sub.inbox <- f:
		default: // kuyruk doluysa düşür (gerçek RF davranışı)
		}
	}
}

// NewMockHaLow, verilen yapılandırmayla mock bir HaLow sürücüsü oluşturur.
func NewMockHaLow(cfg Config, medium *SharedMedium) *MockHaLow {
	d := &MockHaLow{
		cfg:    cfg,
		medium: medium,
		inbox:  make(chan Frame, 128),
		rssi:   -65,
	}
	if medium != nil {
		medium.attach(d)
	}
	return d
}

func (d *MockHaLow) Config() Config  { return d.cfg }
func (d *MockHaLow) Available() bool { return true } // mock her zaman mevcut
func (d *MockHaLow) RSSI() int       { return d.rssi }

func (d *MockHaLow) Open(_ context.Context) error {
	d.opened.Store(true)
	return nil
}

func (d *MockHaLow) Send(_ context.Context, f Frame) error {
	if !d.opened.Load() {
		return ErrNotOpen
	}
	if len(f.Payload) > MTU {
		return ErrFrameTooLarge
	}
	d.tx.Add(1)
	if d.medium != nil {
		d.medium.broadcast(d, f)
	}
	return nil
}

func (d *MockHaLow) Receive(ctx context.Context) (Frame, error) {
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case f := <-d.inbox:
		d.rx.Add(1)
		return f, nil
	}
}

func (d *MockHaLow) Peers() []Peer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]Peer(nil), d.peers...)
}

func (d *MockHaLow) Close() error {
	d.closed.Store(true)
	if d.medium != nil {
		d.medium.detach(d)
	}
	return nil
}

// Stats, mock sürücü istatistiklerini döndürür.
type Stats struct{ TX, RX uint64 }

func (d *MockHaLow) Stats() Stats {
	return Stats{TX: d.tx.Load(), RX: d.rx.Load()}
}

// --- HAL Fabrikası ---

// OpenHAL, kullanılabilir HaLow sürücüsünü açar.
// Gerçek donanım (platform sürücüsü) varsa onu açar; yoksa mock döndürür.
// İkinci dönüş değeri true ise gerçek donanım, false ise mock kullanılıyor.
func OpenHAL(cfg Config, medium *SharedMedium) (HaLowDriver, bool) {
	// Gelecekte: /sys/class/net'te halow* arayüzü varsa gerçek sürücüyü dene.
	// Şu an her zaman mock döner (donanım entegrasyonu v0.4+ için).
	return NewMockHaLow(cfg, medium), false
}

// BandwidthKbps, MCS seviyesinin teorik maksimum bant genişliğini döndürür.
func BandwidthKbps(mcs MCS) int {
	table := map[MCS]int{
		MCS0: 150, MCS1: 300, MCS2: 600, MCS3: 900,
		MCS4: 1200, MCS5: 2400, MCS6: 3600, MCS7: 4800,
		MCS8: 6500, MCS9: 7800, MCS10: 8700,
	}
	if v, ok := table[mcs]; ok {
		return v
	}
	return 150
}

// RangeMeter, MCS seviyesinin tipik menzilini döndürür (metre, açık alan).
func RangeMeter(mcs MCS) int {
	table := map[MCS]int{
		MCS0: 2000, MCS1: 1500, MCS2: 1200, MCS3: 900,
		MCS4: 700, MCS5: 500, MCS6: 400, MCS7: 300,
		MCS8: 200, MCS9: 150,
	}
	if v, ok := table[mcs]; ok {
		return v
	}
	return 2000
}

// String, MCS açıklamasını döndürür.
func (m MCS) String() string {
	return fmt.Sprintf("MCS%d (~%d kbps, ~%dm menzil)", int(m), BandwidthKbps(m), RangeMeter(m))
}
