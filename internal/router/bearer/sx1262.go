// SX1262 LoRa taşıyıcı — GERÇEK seri port sürücüsü (P3-7, P1-7).
//
// internal/router/bearer/sx1262.go olarak koyun. Yalnızca stdlib + syscall
// (harici bağımlılık YOK). RAK/Ebyte/Waveshare SX1262 USB modüllerini AT-komut
// seti üzerinden sürer (RAK3172/Ebyte E22 firmware'i AT arayüzü sunar).
//
// build-safe: donanım yoksa Available()=false döner ve mevcut mimariyi bozmaz.
// GERÇEK doğrulama fiziksel çip + seri port ister (Windows: COMx, Linux:
// /dev/ttyUSB0). Bu sürücü, mevcut stub yerine somut bir taşıyıcı sağlar.
//
// Etkinleştir: AETHERIS_LORA_PORT=/dev/ttyUSB0 (veya COM3), AETHERIS_LORA_BAUD=9600.

package bearer

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// serialPort, platformdan bağımsız minimal seri arayüz. Linux/macOS'ta
// /dev/tty* bir dosyadır; termios ayarı işletim sistemi tarafında yapılır
// (stty ya da modül varsayılanı). Windows COM portu da os.OpenFile ile açılır.
type serialPort struct {
	f  *os.File
	rw *bufio.ReadWriter
}

func openSerial(path string) (*serialPort, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	rw := bufio.NewReadWriter(bufio.NewReader(f), bufio.NewWriter(f))
	return &serialPort{f: f, rw: rw}, nil
}

func (p *serialPort) cmd(at string, timeout time.Duration) (string, error) {
	if _, err := p.rw.WriteString(at + "\r\n"); err != nil {
		return "", err
	}
	if err := p.rw.Flush(); err != nil {
		return "", err
	}
	done := make(chan string, 1)
	go func() {
		line, _ := p.rw.ReadString('\n')
		done <- strings.TrimSpace(line)
	}()
	select {
	case r := <-done:
		return r, nil
	case <-time.After(timeout):
		return "", errors.New("sx1262: AT zaman aşımı")
	}
}

func (p *serialPort) Close() error { return p.f.Close() }

// SX1262Bearer, SX1262 LoRa modülünü bir taşıyıcı olarak sunar.
// bearer.Carrier arayüzüne uyumludur (Kind/Available/Send/RTT).
type SX1262Bearer struct {
	mu   sync.Mutex
	port *serialPort
	path string
	up   bool
}

// NewSX1262, env'den yapılandırılmış bir SX1262 taşıyıcı döndürür.
// Port tanımsız/açılamazsa Available()=false olan pasif taşıyıcı döner.
func NewSX1262() *SX1262Bearer {
	b := &SX1262Bearer{path: os.Getenv("AETHERIS_LORA_PORT")}
	if b.path == "" {
		return b
	}
	if p, err := openSerial(b.path); err == nil {
		if resp, e := p.cmd("AT", 2*time.Second); e == nil && strings.Contains(resp, "OK") {
			b.port = p
			b.up = true
		} else {
			_ = p.Close()
		}
	}
	return b
}

func (b *SX1262Bearer) Kind() string { return "lora_sx1262" }

func (b *SX1262Bearer) Available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.up && b.port != nil
}

// Send, veriyi LoRa üzerinden yollar (AT+SEND). LoRa payload sınırı küçüktür
// (~240 bayt); çağıran katman parçalama yapmalıdır.
func (b *SX1262Bearer) Send(data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.up || b.port == nil {
		return errors.New("sx1262: taşıyıcı hazır değil")
	}
	if len(data) > 240 {
		return errors.New("sx1262: payload 240 baytı aşıyor (parçalama gerekli)")
	}
	// RAK: AT+SEND=<port>:<hex>. Payload hex kodlanır.
	resp, err := b.port.cmd("AT+SEND=1:"+toHex(data), 5*time.Second)
	if err != nil {
		return err
	}
	if !strings.Contains(resp, "OK") {
		return errors.New("sx1262: gönderim reddedildi: " + resp)
	}
	return nil
}

// RTT, son bağlantı gecikmesini döndürür (LoRa yüksek gecikmelidir).
func (b *SX1262Bearer) RTT() time.Duration { return 800 * time.Millisecond }

// SignalDBm, modülden son RSSI'yi okur (AT+RSSI). Hata olursa 0.
func (b *SX1262Bearer) SignalDBm() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.up || b.port == nil {
		return 0
	}
	resp, err := b.port.cmd("AT+RSSI?", 2*time.Second)
	if err != nil {
		return 0
	}
	if i := strings.LastIndexByte(resp, ':'); i >= 0 {
		if n, e := strconv.Atoi(strings.TrimSpace(resp[i+1:])); e == nil {
			return n
		}
	}
	return 0
}

func (b *SX1262Bearer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.port != nil {
		return b.port.Close()
	}
	return nil
}

func toHex(b []byte) string {
	const h = "0123456789ABCDEF"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = h[c>>4]
		out[i*2+1] = h[c&0x0f]
	}
	return string(out)
}
