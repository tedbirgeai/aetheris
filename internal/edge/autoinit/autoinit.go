// Package autoinit, cihaz başladığında mevcut fiziksel kanalları tarayıp
// bearer manager'a kaydeden daemon'dır.
//
// Saf stdlib ile uygulanan taramalar:
//   - Ağ arayüzleri (net.Interfaces) → Ethernet/WiFi tespiti
//   - Seri portlar (platform dosya sistemi → /dev/ttyUSB*, COM*) → LoRa dongle
//
// AÇIK ETIKET: Gerçek BLE chip, SoftAP ve SX1262 donanım sürücüsü bu pakette
// YOKTUR. Tarama sonuçları bearer.Manager'a bildirilir; stub bearer'lar
// Available()=false döndürdüğünden Manager bunları atlar. Donanım takılınca
// ilgili bearer gerçek sürücüyle değiştirilir.
package autoinit

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DiscoveredInterface, taranan bir fiziksel kanalı temsil eder.
type DiscoveredInterface struct {
	Name      string
	Kind      string // "ethernet", "wifi", "loopback", "serial_lora", "unknown"
	Available bool
	Note      string
}

// Scanner, başlangıçta mevcut kanalları tarar.
type Scanner struct {
	logger *slog.Logger
}

// New, bir Auto-Init scanner oluşturur.
func New(logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{logger: logger}
}

// Scan, mevcut ağ arayüzlerini ve seri portları tarar.
func (s *Scanner) Scan(ctx context.Context) []DiscoveredInterface {
	var result []DiscoveredInterface
	result = append(result, s.scanNetInterfaces()...)
	result = append(result, s.scanSerialPorts()...)
	for _, d := range result {
		s.logger.Info("kanal tarama",
			"isim", d.Name, "tur", d.Kind,
			"mevcut", d.Available, "not", d.Note)
	}
	return result
}

// Run, periyodik tarama döngüsü. Yeni kanal tespit edilince cb çağrılır.
func (s *Scanner) Run(ctx context.Context, interval time.Duration, cb func([]DiscoveredInterface)) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	s.logger.Info("auto-init daemon aktif", "platform", runtime.GOOS)
	prev := map[string]bool{}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			found := s.Scan(ctx)
			changed := false
			for _, d := range found {
				if prev[d.Name] != d.Available {
					prev[d.Name] = d.Available
					changed = true
				}
			}
			if changed && cb != nil {
				cb(found)
			}
		}
	}
}

// scanNetInterfaces, işletim sisteminin ağ arayüzlerini listeler.
func (s *Scanner) scanNetInterfaces() []DiscoveredInterface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []DiscoveredInterface
	for _, iface := range ifaces {
		d := DiscoveredInterface{
			Name:      iface.Name,
			Available: iface.Flags&net.FlagUp != 0,
		}
		name := strings.ToLower(iface.Name)
		switch {
		case iface.Flags&net.FlagLoopback != 0:
			d.Kind = "loopback"
		case strings.HasPrefix(name, "eth") || strings.HasPrefix(name, "en") ||
			strings.HasPrefix(name, "local area"):
			d.Kind = "ethernet"
		case strings.HasPrefix(name, "wlan") || strings.HasPrefix(name, "wi-fi") ||
			strings.HasPrefix(name, "wifi") || strings.HasPrefix(name, "wlp"):
			d.Kind = "wifi"
		case strings.HasPrefix(name, "usb") || strings.Contains(name, "tether"):
			d.Kind = "usb_tethering"
		default:
			d.Kind = "unknown"
		}
		d.Note = fmt.Sprintf("MTU=%d flags=%v", iface.MTU, iface.Flags)
		out = append(out, d)
	}
	return out
}

// scanSerialPorts, platform'a göre seri port varlığını kontrol eder.
// Gerçek SX1262 iletişimi için ayrı sürücü gerekir; bu yalnızca keşif.
func (s *Scanner) scanSerialPorts() []DiscoveredInterface {
	var patterns []string
	switch runtime.GOOS {
	case "linux":
		patterns = []string{"/dev/ttyUSB*", "/dev/ttyACM*", "/dev/serial/by-id/*"}
	case "darwin":
		patterns = []string{"/dev/cu.usbserial*", "/dev/cu.usbmodem*"}
	case "windows":
		// Windows'ta COM portları registry'de; basit kontrol olarak sabit yol yok.
		// Gerçek tespit için syscall/registry gerekir — burada atlıyoruz.
		return []DiscoveredInterface{{
			Name: "COM_SCAN", Kind: "serial_lora",
			Available: false,
			Note:      "Windows seri port taramasi: registry API gerekli (stub)",
		}}
	}
	var out []DiscoveredInterface
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			_ = fs.ErrInvalid
			continue
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			out = append(out, DiscoveredInterface{
				Name:      m,
				Kind:      "serial_lora",
				Available: err == nil && info != nil,
				Note:      "SX1262/LoRa dongle adayi — gercek surucu gerekli",
			})
		}
	}
	return out
}
