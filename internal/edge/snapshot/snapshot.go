// Package snapshot, kenar (edge) kamera yakalama ve sıkıştırma daemon'ını
// implement eder. Gerçek IR kamera olmadığında mock loopback modu aktiftir;
// fiziksel kamera takılınca Capture arayüzü gerçek sürücüyle değiştirilir.
//
// Akış:
//
//	Kamera (gerçek/mock) → Gri-tonlu ham çerçeve
//	                      → JPEG sıkıştırma (%MaxSizeKB altında)
//	                      → AES-256-GCM şifreleme
//	                      → Off-Grid WAL defteri (mühürlü)
//	                      → LoRa/DTN iletim kuyruğu
package snapshot

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"time"
)

const (
	MaxSizeKB  = 50 // LoRa bant genişliğine uygun azami boyut
	DefaultFPS = 1  // saniyede bir çerçeve (off-grid mod)
)

// Capture, bir kamera kaynağını soyutlar.
type Capture interface {
	// Frame, gri-tonlu ham çerçeve döndürür.
	Frame(ctx context.Context) (image.Image, error)
	Available() bool
	Close() error
}

// Frame, mühürlenmiş bir snapshot kaydıdır.
type Frame struct {
	CapturedAt time.Time `json:"captured_at"`
	SizeBytes  int       `json:"size_bytes"`
	SizeKB     float64   `json:"size_kb"`
	JPEG       []byte    `json:"-"`       // sıkıştırılmış ham
	Encrypted  []byte    `json:"payload"` // AES-256-GCM şifreli
	NodeID     string    `json:"node_id"`
	Simulated  bool      `json:"simulated"` // mock mu gerçek mi
}

// Daemon, periyodik kamera yakalama ve WAL mühürleme döngüsüdür.
type Daemon struct {
	cap     Capture
	key     []byte // AES-256 anahtarı
	nodeID  string
	logger  *slog.Logger
	onFrame func(Frame) // WAL/DTN kuyruğuna iletim geri çağrısı
}

// New, bir snapshot daemon'ı oluşturur. key nil ise şifreleme atlanır.
func New(cap Capture, key []byte, nodeID string, logger *slog.Logger, onFrame func(Frame)) *Daemon {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Daemon{cap: cap, key: key, nodeID: nodeID, logger: logger, onFrame: onFrame}
}

// Run, ctx iptaline kadar periyodik frame yakalar.
func (d *Daemon) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Duration(DefaultFPS) * time.Second
	}
	d.logger.Info("edge snapshot daemon aktif",
		"kaynak", fmt.Sprintf("%T", d.cap),
		"simulated", !d.cap.Available(),
		"interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f, err := d.capture(ctx)
			if err != nil {
				d.logger.Warn("frame yakalanamadi", "err", err)
				continue
			}
			d.logger.Info("frame yakalandi",
				"boyut_kb", f.SizeKB,
				"simulated", f.Simulated)
			if d.onFrame != nil {
				d.onFrame(f)
			}
		}
	}
}

func (d *Daemon) capture(ctx context.Context) (Frame, error) {
	img, err := d.cap.Frame(ctx)
	if err != nil {
		return Frame{}, err
	}
	// JPEG sıkıştırma — kaliteyi MaxSizeKB altına düşene kadar düşür.
	jpegData, err := compressToSize(img, MaxSizeKB*1024)
	if err != nil {
		return Frame{}, fmt.Errorf("jpeg sikistirma: %w", err)
	}
	f := Frame{
		CapturedAt: time.Now().UTC(),
		SizeBytes:  len(jpegData),
		SizeKB:     float64(len(jpegData)) / 1024.0,
		JPEG:       jpegData,
		NodeID:     d.nodeID,
		Simulated:  !d.cap.Available(),
	}
	// AES-256-GCM şifreleme (anahtar varsa).
	if len(d.key) == 32 {
		enc, eerr := aesgcmSeal(d.key, jpegData)
		if eerr != nil {
			return Frame{}, fmt.Errorf("sifreleme: %w", eerr)
		}
		f.Encrypted = enc
	} else {
		f.Encrypted = jpegData
	}
	return f, nil
}

// compressToSize, JPEG kalitesini iteratif olarak azaltarak hedef boyutun
// altında bir çıktı üretir.
func compressToSize(img image.Image, maxBytes int) ([]byte, error) {
	for quality := 85; quality >= 10; quality -= 10 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		if buf.Len() <= maxBytes {
			return buf.Bytes(), nil
		}
	}
	// En düşük kalitede bile büyükse kabul et (tek çerçeve, LoRa parçalar).
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 10})
	return buf.Bytes(), nil
}

func aesgcmSeal(key, pt []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, pt, nil), nil
}

// Marshal, frame meta verisini JSON'a çevirir (WAL kaydı için).
func (f Frame) Marshal() ([]byte, error) {
	type wireFrame struct {
		CapturedAt string  `json:"captured_at"`
		SizeKB     float64 `json:"size_kb"`
		NodeID     string  `json:"node_id"`
		Simulated  bool    `json:"simulated"`
		// Şifreli payload Base64 olarak taşınır.
		PayloadLen int `json:"payload_len"`
	}
	return json.Marshal(wireFrame{
		CapturedAt: f.CapturedAt.Format(time.RFC3339),
		SizeKB:     math.Round(f.SizeKB*100) / 100,
		NodeID:     f.NodeID,
		Simulated:  f.Simulated,
		PayloadLen: len(f.Encrypted),
	})
}

// --- Mock kamera (donanım olmadan test/loopback) ---

// MockCapture, sentetik gece-görüş (IR) gri-tonlu görüntü üretir.
// Gerçek kamera olmadan daemon'ı ve sıkıştırma hattını test eder.
type MockCapture struct {
	width, height int
	frame         int
}

// NewMockCapture, W×H boyutlu mock IR kaynağı oluşturur.
func NewMockCapture(w, h int) *MockCapture {
	if w <= 0 {
		w = 320
	}
	if h <= 0 {
		h = 240
	}
	return &MockCapture{width: w, height: h}
}

func (m *MockCapture) Available() bool { return false } // gerçek donanım yok
func (m *MockCapture) Close() error    { return nil }

// Frame, hareket simüle eden sentetik gri-tonlu IR çerçeve üretir.
func (m *MockCapture) Frame(_ context.Context) (image.Image, error) {
	m.frame++
	img := image.NewGray(image.Rect(0, 0, m.width, m.height))
	// Basit gradyan + "hareket nesnesi" (her 5 karede pozisyon kayar).
	objX := (m.frame * 7) % m.width
	objY := (m.frame * 3) % m.height
	for y := 0; y < m.height; y++ {
		for x := 0; x < m.width; x++ {
			// Zemin: hafif gürültülü karanlık
			base := uint8((x + y + m.frame) % 40)
			// Nesne: parlak IR imzası
			if abs(x-objX) < 20 && abs(y-objY) < 20 {
				base = 200 + uint8((x^y)%55)
			}
			img.SetGray(x, y, color.Gray{Y: base})
		}
	}
	return img, nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
