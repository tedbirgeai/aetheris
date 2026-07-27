// Package wimax, WiGig 60GHz (IEEE 802.11ad/ay) taşıyıcı katmanını implement
// eder. 57-66 GHz lisanssız bandında çalışır; bina içi ve yakın mesafe
// (100-300m) ultra hızlı (1-10 Gbps) bağlantı sağlar.
//
// Kullanım alanları:
//   - Bina içi son 100 metre — fiber hızında wireless
//   - İki bina arasında kısa mesafe backbone
//   - HaLow/LoRa'dan gelen trafiğin LAN'a aktarımı
//
// Gerçek donanım: Intel Wi-Fi 6E/7 çipleri, Qualcomm QCA6430.
package wimax

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNotOpen   = errors.New("wimax: adaptör açılmadı")
	ErrNoLine    = errors.New("wimax: 60GHz görüş hattı yok (oda içi engel)")
	ErrFrameSize = errors.New("wimax: çerçeve çok büyük")
)

// Channel, 60GHz kanalıdır (57.24 - 65.88 GHz, 4 kanal).
type Channel int

const (
	Ch1 Channel = 1 // 58.32 GHz
	Ch2 Channel = 2 // 60.48 GHz
	Ch3 Channel = 3 // 62.64 GHz
	Ch4 Channel = 4 // 64.80 GHz
)

// MaxDataRateGbps, MCS seviyesine göre maksimum hız.
func MaxDataRateGbps(mcs int) float64 {
	rates := []float64{0.385, 0.770, 1.155, 1.251, 1.502, 1.540, 1.925, 2.310}
	if mcs < 0 || mcs >= len(rates) {
		return 0.385
	}
	return rates[mcs]
}

const (
	MaxRangeM = 300  // açık alan
	IndoorM   = 10   // duvar geçemez
	MTU       = 7920 // 802.11ad MSDU
)

// Config, WiGig adaptör yapılandırmasıdır.
type Config struct {
	NodeID  string
	Channel Channel
	MCS     int
}

// DefaultConfig, güvenli varsayılan WiGig yapılandırması.
func DefaultConfig(nodeID string) Config {
	return Config{NodeID: nodeID, Channel: Ch2, MCS: 4}
}

// Frame, WiGig çerçevesidir.
type Frame struct {
	Payload  []byte
	RSSI_DBm float64
}

// WiGigDriver arayüzü.
type WiGigDriver interface {
	Open(ctx context.Context) error
	Send(ctx context.Context, f Frame) error
	Receive(ctx context.Context) (Frame, error)
	DataRateGbps() float64
	Available() bool
	Close() error
}

// SharedMedium, mock WiGig ortamı.
type SharedMedium struct {
	mu      sync.Mutex
	devices []*MockWiGig
}

func NewSharedMedium() *SharedMedium { return &SharedMedium{} }

func (m *SharedMedium) attach(d *MockWiGig) {
	m.mu.Lock()
	m.devices = append(m.devices, d)
	m.mu.Unlock()
}
func (m *SharedMedium) detach(d *MockWiGig) {
	m.mu.Lock()
	out := m.devices[:0]
	for _, dev := range m.devices {
		if dev != d {
			out = append(out, dev)
		}
	}
	m.devices = out
	m.mu.Unlock()
}
func (m *SharedMedium) broadcast(src *MockWiGig, f Frame) {
	m.mu.Lock()
	devs := append([]*MockWiGig(nil), m.devices...)
	m.mu.Unlock()
	for _, d := range devs {
		if d == src || !d.opened.Load() || d.cfg.Channel != src.cfg.Channel {
			continue
		}
		f.RSSI_DBm = -55
		select {
		case d.inbox <- f:
		default:
		}
	}
}

type MockWiGig struct {
	cfg    Config
	medium *SharedMedium
	inbox  chan Frame
	opened atomic.Bool
	tx, rx atomic.Uint64
}

func NewMockWiGig(cfg Config, medium *SharedMedium) *MockWiGig {
	d := &MockWiGig{cfg: cfg, medium: medium, inbox: make(chan Frame, 256)}
	if medium != nil {
		medium.attach(d)
	}
	return d
}

func (d *MockWiGig) Available() bool       { return true }
func (d *MockWiGig) DataRateGbps() float64 { return MaxDataRateGbps(d.cfg.MCS) }

func (d *MockWiGig) Open(_ context.Context) error {
	d.opened.Store(true)
	return nil
}

func (d *MockWiGig) Send(_ context.Context, f Frame) error {
	if !d.opened.Load() {
		return ErrNotOpen
	}
	if len(f.Payload) > MTU {
		return ErrFrameSize
	}
	d.tx.Add(1)
	if d.medium != nil {
		d.medium.broadcast(d, f)
	}
	return nil
}

func (d *MockWiGig) Receive(ctx context.Context) (Frame, error) {
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case f := <-d.inbox:
		d.rx.Add(1)
		return f, nil
	}
}

func (d *MockWiGig) Close() error {
	d.opened.Store(false)
	if d.medium != nil {
		d.medium.detach(d)
	}
	return nil
}

func OpenHAL(cfg Config, medium *SharedMedium) (WiGigDriver, bool) {
	return NewMockWiGig(cfg, medium), false
}

// RangeSummary, özet bilgi.
func RangeSummary(cfg Config) string {
	return fmt.Sprintf("WiGig 60GHz Ch%d | MCS%d | %.2f Gbps | maks %dm | lisanssız",
		int(cfg.Channel), cfg.MCS, MaxDataRateGbps(cfg.MCS), MaxRangeM)
}

// --- WiFi Mesh 802.11s ---

// MeshNode, 802.11s topluluk WiFi mesh düğümüdür.
// Freifunk/Guifi.net modeli: herkese açık, topluluk sahipli.
type MeshNode struct {
	NodeID    string
	IP        string
	Neighbors []string
	Channel   int    // 2.4/5/6 GHz WiFi kanalı
	SSID      string // mesh ağ adı
	mu        sync.RWMutex
	active    bool
}

// NewMeshNode, yeni bir 802.11s mesh düğümü oluşturur.
func NewMeshNode(nodeID, ssid string) *MeshNode {
	return &MeshNode{
		NodeID:  nodeID,
		SSID:    ssid,
		Channel: 6, // 2.4GHz varsayılan
	}
}

// AddNeighbor, bir komşu düğüm ekler.
func (n *MeshNode) AddNeighbor(nodeID string) {
	n.mu.Lock()
	n.Neighbors = append(n.Neighbors, nodeID)
	n.mu.Unlock()
}

// Neighbors sayısı.
func (n *MeshNode) NeighborCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.Neighbors)
}

// MeshStats, mesh düğüm istatistikleri.
type MeshStats struct {
	NodeID    string
	Neighbors int
	Uptime    time.Duration
	TxBytes   uint64
	RxBytes   uint64
}
