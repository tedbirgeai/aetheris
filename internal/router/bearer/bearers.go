// Package bearer — tasiyici kayit tablosu + gercek mock surucu adaptorleri.
//
// Bu dosya internal/router/bearer/bearers.go'nun YERINE gecer (tek dosya, 0 surtunme).
//
// DEGISIKLIK: Lisanssiz ve mock surucusu MEVCUT olan tasiyicilar (WiGig 60GHz,
// FSO lazer, Wi-Fi HaLow) artik internal/carrier/* altindaki gercek mock
// suruculere baglidir (Available()=true, Probe gercek LinkQuality okur).
// Boylece Manager bunlari secebilir ve failover zinciri gercek calisir.
//
// HUKUKI SINIR (bilincli olarak stub birakilanlar):
//   - TVWS 470-790MHz  → BTK kanal veritabani + koordinasyon izni gerekir (5809).
//   - LoRa USB / BLE / SoftAP / USB tethering → fiziksel cip + OS surucusu gerekir.
// Bu tasiyicilar Available()=false doner; ilgili donanim/lisans geldiginde
// adaptorleri asagidaki lisanssiz ornekler gibi yazilir.

package bearer

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tedbirgeai/aetheris/internal/carrier/fso"
	"github.com/tedbirgeai/aetheris/internal/carrier/halow"
	"github.com/tedbirgeai/aetheris/internal/carrier/wimax"
)

// ============================================================
//  TCP tabanli tasiyicilar (Ethernet / WiFi WAN)
// ============================================================

// TCPBearer, bir TCP endpoint'e baglanarak saglik olcer.
type TCPBearer struct {
	kind    Kind
	targets []string
	timeout time.Duration
}

func NewTCPBearer(kind Kind, targets []string) *TCPBearer {
	return &TCPBearer{kind: kind, targets: targets, timeout: 2 * time.Second}
}

func (b *TCPBearer) Kind() Kind      { return b.kind }
func (b *TCPBearer) Available() bool { return true }
func (b *TCPBearer) Probe(ctx context.Context) (float64, error) {
	for _, target := range b.targets {
		start := time.Now()
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", target)
		if err == nil {
			_ = conn.Close()
			return float64(time.Since(start).Microseconds()) / 1000.0, nil
		}
	}
	return 0, fmt.Errorf("hicbir hedefe ulasilamadi: %v", b.targets)
}

// ============================================================
//  Donanim stub'i (gercek surucu YOK — lisans/donanim bekler)
// ============================================================

type HardwareStubBearer struct {
	kind Kind
	note string
}

func NewHardwareStub(kind Kind, note string) *HardwareStubBearer {
	return &HardwareStubBearer{kind: kind, note: note}
}
func (b *HardwareStubBearer) Kind() Kind      { return b.kind }
func (b *HardwareStubBearer) Available() bool { return false }
func (b *HardwareStubBearer) Probe(_ context.Context) (float64, error) {
	return 0, fmt.Errorf("stub: %s", b.note)
}

// ============================================================
//  Gercek mock surucu adaptorleri (lisanssiz tasiyicilar)
//  internal/carrier/* mock'larini bearer.Bearer arayuzune baglar.
// ============================================================

// WiGigBearer, WiGig 60GHz (57-66 GHz lisanssiz) mock surucusunu sarar.
type WiGigBearer struct {
	drv    wimax.WiGigDriver
	once   sync.Once
	opened bool
}

func NewWiGigBearer(nodeID string) *WiGigBearer {
	drv, _ := wimax.OpenHAL(wimax.DefaultConfig(nodeID), wimax.NewSharedMedium())
	return &WiGigBearer{drv: drv}
}
func (b *WiGigBearer) Kind() Kind      { return KindWiGig60GHz }
func (b *WiGigBearer) Available() bool { return b.drv != nil && b.drv.Available() }
func (b *WiGigBearer) Probe(ctx context.Context) (float64, error) {
	b.once.Do(func() { b.opened = b.drv.Open(ctx) == nil })
	if !b.opened {
		return 0, fmt.Errorf("wigig: adaptor acilamadi")
	}
	if rate := b.drv.DataRateGbps(); rate <= 0 {
		return 0, fmt.Errorf("wigig: baglanti yok (gorus hatti engeli)")
	}
	// 60GHz kisa mesafe: ~0.5ms tipik gecikme.
	return 0.5, nil
}

// FSOBearer, FSO lazer/optik (isik spektrumu, lisanssiz) mock surucusunu sarar.
type FSOBearer struct {
	drv    fso.FSODriver
	once   sync.Once
	opened bool
}

func NewFSOBearer(nodeID string) *FSOBearer {
	drv, _ := fso.OpenHAL(fso.DefaultConfig(nodeID), fso.NewSharedOptical(fso.WeatherClear))
	return &FSOBearer{drv: drv}
}
func (b *FSOBearer) Kind() Kind      { return KindFSO }
func (b *FSOBearer) Available() bool { return b.drv != nil && b.drv.Available() }
func (b *FSOBearer) Probe(ctx context.Context) (float64, error) {
	b.once.Do(func() { b.opened = b.drv.Open(ctx) == nil })
	if !b.opened {
		return 0, fmt.Errorf("fso: adaptor acilamadi")
	}
	q := b.drv.LinkQuality()
	if !q.LinkOK {
		return 0, fmt.Errorf("fso: baglanti yok (hava: %s)", q.Weather.String())
	}
	// Isik hizi, kisa mesafe: ~1ms tipik (isleme dahil).
	return 1.0, nil
}

// HaLowBearer, Wi-Fi HaLow 802.11ah (863-868 MHz SRD, lisanssiz) mock'unu sarar.
type HaLowBearer struct {
	drv    halow.HaLowDriver
	once   sync.Once
	opened bool
}

func NewHaLowBearer(nodeID string) *HaLowBearer {
	drv, _ := halow.OpenHAL(halow.DefaultConfig(nodeID), halow.NewSharedMedium())
	return &HaLowBearer{drv: drv}
}
func (b *HaLowBearer) Kind() Kind      { return KindHaLow }
func (b *HaLowBearer) Available() bool { return b.drv != nil && b.drv.Available() }
func (b *HaLowBearer) Probe(ctx context.Context) (float64, error) {
	b.once.Do(func() { b.opened = b.drv.Open(ctx) == nil })
	if !b.opened {
		return 0, fmt.Errorf("halow: adaptor acilamadi")
	}
	// Sub-GHz uzun menzil: RSSI negatif olmali; ~15ms tipik gecikme.
	if b.drv.RSSI() >= 0 {
		return 0, fmt.Errorf("halow: gecerli sinyal yok")
	}
	return 15.0, nil
}

// ============================================================
//  Varsayilan tasiyici listesi (oncelik sirasi)
// ============================================================

// DefaultBearers, standart oncelik sirasiyla hazir bearer listesi dondurur.
// Lisanssiz + mock surucusu olanlar gercek adaptorle (Available=true) doner;
// lisans/donanim bekleyenler stub (Available=false) kalir ve Manager atlar.
func DefaultBearers(wanTargets []string) []Bearer {
	const nodeID = "aetheris-local"
	return []Bearer{
		// 1. Ethernet GbE — en hizli, en kararli (TCP probe)
		NewTCPBearer(KindEthernet, wanTargets),
		// 2. WiFi WAN — standart kablosuz (TCP probe)
		NewTCPBearer(KindWiFiWAN, wanTargets),
		// 3. WiGig 60GHz — lisanssiz, mock surucu AKTIF
		NewWiGigBearer(nodeID),
		// 4. FSO lazer — lisanssiz (isik), mock surucu AKTIF
		NewFSOBearer(nodeID),
		// 5. Wi-Fi HaLow 802.11ah — lisanssiz SRD, mock surucu AKTIF
		NewHaLowBearer(nodeID),
		// 6. TVWS 470-790MHz — BTK koordinasyon/kanal DB gerekir (stub)
		NewHardwareStub(KindTVWS, "TVWS: BTK kanal veritabani + koordinasyon izni gerekli (5809)"),
		// 7. LoRa USB — SX1262 dongle + seri surucu gerekir (stub)
		NewHardwareStub(KindLoRaUSB, "LoRa USB: SX1262 dongle + seri surucu gerekli"),
		// 8. SoftAP Mesh — Wi-Fi cip + OS ag API gerekir (stub)
		NewHardwareStub(KindSoftAPMesh, "SoftAP: Wi-Fi cip surucu + OS ag API gerekli"),
		// 9. USB Tethering — platform-spesifik ag API gerekir (stub)
		NewHardwareStub(KindUSBTethering, "USB tethering: platform-spesifik ag API gerekli"),
		// 10. BLE Mesh — Bluetooth cip surucu gerekir (stub)
		NewHardwareStub(KindBLEMesh, "BLE Mesh: Bluetooth cip surucu gerekli"),
	}
}
