package bearer

import (
	"context"
	"fmt"
	"net"
	"time"
)

// --- Gercek tasiyicilar (TCP probe ile test edilebilir) ---

// TCPBearer, bir TCP endpoint'e baglanarak saglik olcer. Ethernet, WiFi WAN
// ve USB tethering icin kullanilir; test edilebilir, donanim surucu gerektirmez.
type TCPBearer struct {
	kind    Kind
	targets []string
	timeout time.Duration
}

// NewTCPBearer, TCP probe tabanli bir bearer olusturur.
func NewTCPBearer(kind Kind, targets []string) *TCPBearer {
	return &TCPBearer{kind: kind, targets: targets, timeout: 2 * time.Second}
}

func (b *TCPBearer) Kind() Kind { return b.kind }
func (b *TCPBearer) Available() bool {
	// TCP bearer her zaman "potansiyel olarak mevcut"; Probe saglik olcer.
	return true
}
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

// --- Donanim stub'lari (gercek surucu YOKTUR — etiketi okuyun) ---

// HardwareStubBearer, gercek donanim suruculeri gelene kadar yer tutan
// stub'dur. Available() DAİMA false doner; Manager bu tasiyiciyi atlar.
// Ekleme amaci: donanim takildiginda gercek surucu bu arayuzu karsilayacak.
type HardwareStubBearer struct {
	kind Kind
	note string
}

// NewHardwareStub, bir donanim yer tutucu bearer olusturur.
func NewHardwareStub(kind Kind, note string) *HardwareStubBearer {
	return &HardwareStubBearer{kind: kind, note: note}
}
func (b *HardwareStubBearer) Kind() Kind      { return b.kind }
func (b *HardwareStubBearer) Available() bool { return false } // donanim yok
func (b *HardwareStubBearer) Probe(_ context.Context) (float64, error) {
	return 0, fmt.Errorf("stub: %s — gercek donanim surucu gerekli", b.note)
}

// DefaultBearers, standart oncelik sirasiyla hazir bearer listesi dondurur.
// Donanim stubları Available()=false oldugundan Manager bunlari atlar;
// gercek donanim takilinca ilgili stub bu arayuzu implement eden gercek
// surucu ile degistir.
func DefaultBearers(wanTargets []string) []Bearer {
	return []Bearer{
		// 1. Ethernet GbE — en hızlı, en kararlı
		NewTCPBearer(KindEthernet, wanTargets),
		// 2. WiFi WAN — standart kablosuz
		NewTCPBearer(KindWiFiWAN, wanTargets),
		// 3. WiGig 60GHz — ultra hız, 100-300m (lisanssız)
		NewHardwareStub(KindWiGig60GHz, "WiGig 60GHz: 802.11ad/ay chip surucu gerekli (Qualcomm/Intel)"),
		// 4. FSO Lazer — bina-bina 1Gbps, lisanssız
		NewHardwareStub(KindFSO, "FSO lazer: optik birim gerekli (Lightpointe/GeoDesy)"),
		// 5. USB Tethering — telefon paylaşımı
		NewHardwareStub(KindUSBTethering, "USB tethering: platform-spesifik ag API gerekli"),
		// 6. HaLow 802.11ah — 1-2km sub-GHz WiFi (lisanssız)
		NewHardwareStub(KindHaLow, "Wi-Fi HaLow 802.11ah: Morse Micro/Newracom chip surucu gerekli"),
		// 7. TVWS 470-790MHz — 10-100km, binalara girer (lisanssız)
		NewHardwareStub(KindTVWS, "TVWS: RTL-SDR dongle + GNU Radio / BTK kanal DB gerekli"),
		// 8. LoRa USB — 2-10km, 150kbps (lisanssız)
		NewHardwareStub(KindLoRaUSB, "LoRa USB: SX1262 dongle + seri surucu gerekli"),
		// 9. SoftAP Mesh — yerel WiFi dağıtımı
		NewHardwareStub(KindSoftAPMesh, "SoftAP: Wi-Fi chip surucu + OS ag API gerekli"),
		// 10. BLE Mesh — son metre, düşük hız
		NewHardwareStub(KindBLEMesh, "BLE Mesh: Bluetooth chip surucu gerekli"),
	}
}
