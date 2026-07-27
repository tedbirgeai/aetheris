// Package bearer, aktif ag tasiyicisini yoneten otonom seçim ve failover
// orkestrasyonunu saglar. Ana hat kopunca sistem sirasiyla alternatif
// tasiyicilara milisaniyeler icinde atlar.
//
// Oncelik sirasi (yapilandirilabilir):
//
//	Ethernet → WiFi/WAN → USB Tethering → SoftAP Mesh → USB Serial LoRa → BLE Mesh
//
// DURUSTLUK NOTU: Gercek donanim suruculeri (BLE çip, SX1262 LoRa dongle,
// SoftAP modu) platforma ozel kutuphaneler gerektirir ve bu pakette YOKTUR.
// Bearer.Available() = false olan suruculer otomatik atlanir; aktif donanim
// takildikca suruculer Register() ile eklenir. Mimari zemin hazir.
package bearer

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Kind, tasiyici tur etiketidir.
type Kind string

const (
	KindEthernet     Kind = "ethernet"
	KindWiFiWAN      Kind = "wifi_wan"
	KindUSBTethering Kind = "usb_tethering"
	KindHaLow        Kind = "wifi_halow_802_11ah"
	KindSoftAPMesh   Kind = "softap_mesh"
	KindLoRaUSB      Kind = "lora_usb_serial"
	KindBLEMesh      Kind = "ble_mesh"
)

// Status, bir tasiyicinin anlık durumudur.
type Status struct {
	Kind      Kind
	Available bool
	Healthy   bool
	RTTms     float64
	Note      string // dürüstlük: "donanim yok" gibi
}

// Bearer, tek bir tasiyici kanalinin sozlesmesidir.
type Bearer interface {
	Kind() Kind
	// Available, donanimin fiziksel olarak mevcut oldugunu bildirir.
	// false ise Manager bu tasiyiciyi hicbir zaman secmez.
	Available() bool
	// Probe, kanalin saglik/RTT olcumunu yapar.
	Probe(ctx context.Context) (rttMs float64, err error)
}

// ChangeEvent, aktif tasiyici degisikligini bildirir.
type ChangeEvent struct {
	From Kind
	To   Kind
	RTT  float64
}

// Manager, oncelik sirasina gore aktif tasiyiciyi secen ve failover yapan
// orkestrasyondur.
type Manager struct {
	mu       sync.RWMutex
	bearers  []Bearer // oncelik sirasinda kayitli
	active   Kind
	onChange func(ChangeEvent)
	logger   *slog.Logger
	interval time.Duration
}

// New, bir Bearer Manager olusturur.
func New(logger *slog.Logger, onChange func(ChangeEvent), interval time.Duration) *Manager {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	return &Manager{logger: logger, onChange: onChange, interval: interval}
}

// Register, bir tasiyiciyi oncelik sirasinin SONUNA ekler.
// Oncelik sirasi: ilk eklenen en yuksek onceliktir.
func (m *Manager) Register(b Bearer) {
	m.mu.Lock()
	m.bearers = append(m.bearers, b)
	m.mu.Unlock()
}

// Active, su an secili tasiyicinin turunu dondurur.
func (m *Manager) Active() Kind {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

// Snapshot, tum tasiyicilarin anlık durumunu dondurur (telemetri icin).
func (m *Manager) Snapshot(ctx context.Context) []Status {
	m.mu.RLock()
	bearers := append([]Bearer(nil), m.bearers...)
	m.mu.RUnlock()

	out := make([]Status, 0, len(bearers))
	for _, b := range bearers {
		s := Status{Kind: b.Kind(), Available: b.Available()}
		if b.Available() {
			pctx, cancel := context.WithTimeout(ctx, time.Second)
			rtt, err := b.Probe(pctx)
			cancel()
			s.Healthy = err == nil
			s.RTTms = rtt
			if err != nil {
				s.Note = err.Error()
			}
		} else {
			s.Note = "donanim mevcut degil"
		}
		out = append(out, s)
	}
	return out
}

// Run, periyodik saglik kontrolu + otomatik failover dongusu.
func (m *Manager) Run(ctx context.Context) {
	m.elect(ctx) // hemen bir tur
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.elect(ctx)
		}
	}
}

// elect, oncelik sirasinda saglikli ilk tasiyiciyi secer; degistiyse
// onChange geri cagrisi tetiklenir (failover bildirimi).
func (m *Manager) elect(ctx context.Context) {
	m.mu.RLock()
	bearers := append([]Bearer(nil), m.bearers...)
	prev := m.active
	m.mu.RUnlock()

	for _, b := range bearers {
		if !b.Available() {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		rtt, err := b.Probe(pctx)
		cancel()
		if err != nil {
			m.logger.Debug("bearer sagliksiz", "kind", b.Kind(), "err", err)
			continue
		}
		// Saglikli tasiyici bulundu.
		m.mu.Lock()
		m.active = b.Kind()
		m.mu.Unlock()
		if b.Kind() != prev && m.onChange != nil {
			m.onChange(ChangeEvent{From: prev, To: b.Kind(), RTT: rtt})
		}
		if b.Kind() != prev {
			m.logger.Info("FAILOVER: aktif tasiyici degisti",
				"onceki", prev, "yeni", b.Kind(), "rtt_ms", rtt)
		}
		return
	}
	// Hic saglikli tasiyici yok.
	m.mu.Lock()
	if m.active != "" {
		m.logger.Warn("tum tasiyicilar sagliksiz — izole mesh")
		m.active = ""
	}
	m.mu.Unlock()
}
