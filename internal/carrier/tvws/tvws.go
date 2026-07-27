// Package tvws, TV White Space (TVWS) taşıyıcı katmanını implement eder.
//
// TVWS, dijital TV'ye geçiş sonrası boşta kalan VHF/UHF frekanslarını
// (470-790 MHz, Avrupa/Türkiye) lisanssız ikincil kullanım için açar.
// LoRa'dan 5-10× daha uzun menzil, binalardan ve dağlardan geçiş yeteneği.
//
// IEEE 802.11af (White-Fi / Super-WiFi) standardına dayalı.
//
// Gerçek donanım: RTL-SDR dongle (~200₺) + SDR yazılımı.
// Mock: SharedSpectrum simülasyonu — kanal yarışması ve çakışma modeli.
package tvws

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
)

// Türkiye/Avrupa TVWS parametreleri (ETSI EN 301 598).
const (
	FreqMinMHz     = 470                                      // UHF band alt sınır
	FreqMaxMHz     = 790                                      // UHF band üst sınır
	ChannelBWMHz   = 8                                        // Kanal genişliği (8 MHz, DVB-T uyumlu)
	MaxTxPowerDBm  = 36                                       // 4W ERP — ETSI maks.
	DefaultTxPower = 30                                       // 1W ERP — güvenli varsayılan
	ChannelCount   = (FreqMaxMHz - FreqMinMHz) / ChannelBWMHz // 40 kanal
)

var (
	ErrNoChannel     = errors.New("tvws: müsait kanal yok")
	ErrNotOpen       = errors.New("tvws: adaptör açılmadı")
	ErrFrameTooLarge = errors.New("tvws: çerçeve MTU aşıyor")
	ErrChannelBusy   = errors.New("tvws: kanal meşgul (birincil kullanıcı)")
)

// ChannelState, bir TV kanalının durumunu bildirir.
type ChannelState int

const (
	ChannelFree    ChannelState = iota // Boş — kullanılabilir
	ChannelPrimary                     // Birincil TV kullanıcısı var — yasak
	ChannelBusy                        // Diğer TVWS cihazı kullanıyor
)

// Channel, tek bir TVWS kanalını temsil eder.
type Channel struct {
	Index   int // 0-39
	FreqMHz int // 470, 478, 486 ... 782 MHz
	State   ChannelState
	RSSI    float64 // alım gücü (dBm)
	Users   int     // bu kanalı kullanan TVWS cihaz sayısı
}

// FreqMHz, kanal indeksinden frekansı hesaplar.
func ChanFreq(idx int) int { return FreqMinMHz + idx*ChannelBWMHz }

// BandwidthMbps, kanal başına teorik maksimum bant genişliği.
// TVWS 802.11af: 8 MHz kanalda ~2.4-18 Mbps (OFDM modülasyonuna göre).
func BandwidthMbps(modulation string) float64 {
	switch modulation {
	case "BPSK":
		return 2.4
	case "QPSK":
		return 4.8
	case "16QAM":
		return 9.6
	case "64QAM":
		return 18.0
	default:
		return 4.8
	}
}

// Frame, TVWS üzerinden gönderilen veri çerçevesidir.
type Frame struct {
	Src     net.HardwareAddr
	Dst     net.HardwareAddr
	Channel int // kullanılan kanal indeksi
	Payload []byte
	RSSI    float64
}

// MTU, maksimum çerçeve boyutu (802.11af payload).
const MTU = 7991

// Config, TVWS adaptör yapılandırmasıdır.
type Config struct {
	NodeID            string
	TxPowerDBm        int
	PreferredChannels []int // boşsa otomatik seç
	UseDatabase       bool  // coğrafi konum veritabanı kullan (gerçek modda)
}

// DefaultConfig, güvenli varsayılan TVWS yapılandırması.
func DefaultConfig(nodeID string) Config {
	return Config{
		NodeID:      nodeID,
		TxPowerDBm:  DefaultTxPower,
		UseDatabase: false,
	}
}

// TVWSDriver, bir TVWS adaptörünün soyutlamasıdır.
type TVWSDriver interface {
	Open(ctx context.Context) error
	ScanChannels(ctx context.Context) ([]Channel, error)
	SelectChannel(ctx context.Context) (Channel, error)
	Send(ctx context.Context, f Frame) error
	Receive(ctx context.Context) (Frame, error)
	ActiveChannel() int
	SignalDBm() float64
	Available() bool
	Close() error
}

// --- Spektrum Veritabanı (Coğrafi Konum Tabanlı) ---

// SpectrumDB, konuma göre müsait kanalları belirler.
// Gerçek modda BTK/ETSI veritabanına sorgu yapar.
type SpectrumDB struct {
	// occupied: kanal indeksi → birincil kullanıcı var mı
	occupied map[int]bool
}

// NewMockSpectrumDB, test amaçlı rastgele kanal müsaitliği üretir.
// Gerçekte: BTK/Ofcom API'ye HTTP sorgusu yapılır.
func NewMockSpectrumDB(occupiedRatio float64) *SpectrumDB {
	db := &SpectrumDB{occupied: make(map[int]bool)}
	for i := 0; i < ChannelCount; i++ {
		if randFloat() < occupiedRatio {
			db.occupied[i] = true
		}
	}
	return db
}

func (db *SpectrumDB) IsAvailable(ch int) bool {
	return !db.occupied[ch]
}

func (db *SpectrumDB) FreeChannels() []int {
	var out []int
	for i := 0; i < ChannelCount; i++ {
		if !db.occupied[i] {
			out = append(out, i)
		}
	}
	return out
}

// --- SharedSpectrum: Mock RF Ortam Simülasyonu ---

// SharedSpectrum, birden fazla MockTVWS'nin paylaştığı sanal RF ortamıdır.
type SharedSpectrum struct {
	mu       sync.Mutex
	devices  []*MockTVWS
	channels [ChannelCount]ChannelState
}

func NewSharedSpectrum() *SharedSpectrum {
	s := &SharedSpectrum{}
	// Bazı kanalları "birincil TV" olarak işaretle (%30)
	for i := range s.channels {
		if randFloat() < 0.30 {
			s.channels[i] = ChannelPrimary
		}
	}
	return s
}

func (s *SharedSpectrum) attach(d *MockTVWS) {
	s.mu.Lock()
	s.devices = append(s.devices, d)
	s.mu.Unlock()
}

func (s *SharedSpectrum) detach(d *MockTVWS) {
	s.mu.Lock()
	out := s.devices[:0]
	for _, dev := range s.devices {
		if dev != d {
			out = append(out, dev)
		}
	}
	s.devices = out
	s.mu.Unlock()
}

func (s *SharedSpectrum) channelState(idx int) ChannelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channels[idx]
}

func (s *SharedSpectrum) broadcast(src *MockTVWS, f Frame) {
	s.mu.Lock()
	devs := append([]*MockTVWS(nil), s.devices...)
	s.mu.Unlock()
	for _, d := range devs {
		if d == src || !d.opened.Load() || d.activeCh != f.Channel {
			continue
		}
		f.RSSI = -75 + randFloat()*10 // simüle RSSI
		select {
		case d.inbox <- f:
		default:
		}
	}
}

// --- Mock TVWS Sürücüsü ---

type MockTVWS struct {
	cfg      Config
	spectrum *SharedSpectrum
	db       *SpectrumDB
	activeCh int
	inbox    chan Frame
	opened   atomic.Bool
	mu       sync.RWMutex
	tx, rx   atomic.Uint64
	rssi     float64
	logger   *slog.Logger
}

func NewMockTVWS(cfg Config, spectrum *SharedSpectrum, db *SpectrumDB, logger *slog.Logger) *MockTVWS {
	if logger == nil {
		logger = slog.Default()
	}
	if db == nil {
		db = NewMockSpectrumDB(0.30)
	}
	d := &MockTVWS{
		cfg:      cfg,
		spectrum: spectrum,
		db:       db,
		inbox:    make(chan Frame, 128),
		activeCh: -1,
		rssi:     -80,
		logger:   logger,
	}
	if spectrum != nil {
		spectrum.attach(d)
	}
	return d
}

func (d *MockTVWS) Available() bool { return true }
func (d *MockTVWS) SignalDBm() float64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.rssi
}
func (d *MockTVWS) ActiveChannel() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.activeCh
}

func (d *MockTVWS) Open(_ context.Context) error {
	d.opened.Store(true)
	return nil
}

func (d *MockTVWS) ScanChannels(_ context.Context) ([]Channel, error) {
	if !d.opened.Load() {
		return nil, ErrNotOpen
	}
	var channels []Channel
	for i := 0; i < ChannelCount; i++ {
		state := ChannelFree
		if d.spectrum != nil {
			state = d.spectrum.channelState(i)
		}
		if !d.db.IsAvailable(i) {
			state = ChannelPrimary
		}
		rssi := -90 + randFloat()*30
		channels = append(channels, Channel{
			Index:   i,
			FreqMHz: ChanFreq(i),
			State:   state,
			RSSI:    rssi,
		})
	}
	return channels, nil
}

func (d *MockTVWS) SelectChannel(ctx context.Context) (Channel, error) {
	channels, err := d.ScanChannels(ctx)
	if err != nil {
		return Channel{}, err
	}
	// En iyi sinyal gücüne sahip boş kanalı seç.
	best := Channel{Index: -1, RSSI: math.Inf(-1)}
	for _, ch := range channels {
		if ch.State == ChannelFree && ch.RSSI > best.RSSI {
			best = ch
		}
	}
	if best.Index == -1 {
		return Channel{}, ErrNoChannel
	}
	d.mu.Lock()
	d.activeCh = best.Index
	d.rssi = best.RSSI
	d.mu.Unlock()
	d.logger.Info("TVWS kanal seçildi",
		"kanal", best.Index,
		"freq_mhz", best.FreqMHz,
		"rssi_dbm", fmt.Sprintf("%.1f", best.RSSI))
	return best, nil
}

func (d *MockTVWS) Send(_ context.Context, f Frame) error {
	if !d.opened.Load() {
		return ErrNotOpen
	}
	if d.activeCh < 0 {
		return ErrNoChannel
	}
	if len(f.Payload) > MTU {
		return ErrFrameTooLarge
	}
	// Birincil kullanıcı tespiti simülasyonu.
	if d.spectrum != nil && d.spectrum.channelState(d.activeCh) == ChannelPrimary {
		return ErrChannelBusy
	}
	f.Channel = d.activeCh
	d.tx.Add(1)
	if d.spectrum != nil {
		d.spectrum.broadcast(d, f)
	}
	return nil
}

func (d *MockTVWS) Receive(ctx context.Context) (Frame, error) {
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case f := <-d.inbox:
		d.rx.Add(1)
		return f, nil
	}
}

func (d *MockTVWS) Close() error {
	d.opened.Store(false)
	if d.spectrum != nil {
		d.spectrum.detach(d)
	}
	return nil
}

type TVWSStats struct{ TX, RX uint64 }

func (d *MockTVWS) Stats() TVWSStats {
	return TVWSStats{TX: d.tx.Load(), RX: d.rx.Load()}
}

// OpenHAL, TVWS sürücüsünü açar.
// Gerçek donanım: RTL-SDR USB (~200₺) + GNU Radio / rtl_power.
// Mock: SharedSpectrum simülasyonu.
func OpenHAL(cfg Config, spectrum *SharedSpectrum, db *SpectrumDB) (TVWSDriver, bool) {
	return NewMockTVWS(cfg, spectrum, db, nil), false
}

func randFloat() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.LittleEndian.Uint64(b[:])) / float64(^uint64(0))
}
