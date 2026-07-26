// Package pipeline, Snapshot → DTN Store → LoRa iletim hattını uçtan uca
// bağlar.
//
// Tam akış:
//
//	[Kamera/Mock]
//	     ↓ Frame yakalama (her N saniyede bir)
//	[Snapshot Daemon]
//	     ↓ AES-256-GCM şifreleme + JPEG sıkıştırma (<50KB)
//	[DTN Store] ← disk-kalıcı bundle kuyruğu
//	     ↓ Taşıyıcı (LoRa) mevcut olduğunda
//	[LoRa Driver] → RF iletim (868MHz / mock)
//	     ↓ Alıcı düğüm
//	[Bundle deşifrele + işle]
//
// Gerçek LoRa donanımı olmadan MockDriver ile test edilir; OpenHAL() gerçek
// seri port takılınca otomatik devreye girer.
package pipeline

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tedbirgeai/aetheris/internal/carrier/lora"
	"github.com/tedbirgeai/aetheris/internal/dtn"
	"github.com/tedbirgeai/aetheris/internal/edge/snapshot"
)

// LoRACarrier, lora.Driver'ı dtn.Carrier arayüzüne uyarlar.
type LoRACarrier struct {
	drv    lora.Driver
	nodeID byte // hedef düğüm adresi
	// MTU sınırı: LoRa 222 bayttır (SX1262 payload - header).
	// Büyük snapshot'lar parcalanır.
	chunkSize int
}

// NewLoRACarrier, bir LoRa sürücüsünü DTN taşıyıcısına çevirir.
func NewLoRACarrier(drv lora.Driver, dst byte, chunkSize int) *LoRACarrier {
	if chunkSize <= 0 {
		chunkSize = 200
	}
	return &LoRACarrier{drv: drv, nodeID: dst, chunkSize: chunkSize}
}

func (c *LoRACarrier) Available() bool { return true }

// Send, bundle payload'ını LoRa MTU'ya sığacak şekilde parçalara bölerek iletir.
func (c *LoRACarrier) Send(ctx context.Context, b *dtn.Bundle) error {
	data, err := bundleToWire(b)
	if err != nil {
		return err
	}
	// Parçalı gönderim.
	total := len(data)
	nChunks := (total + c.chunkSize - 1) / c.chunkSize
	for i := 0; i < nChunks; i++ {
		start := i * c.chunkSize
		end := start + c.chunkSize
		if end > total {
			end = total
		}
		chunk := makeChunk(b.ID, i, nChunks, data[start:end])
		if err := c.drv.Send(ctx, chunk); err != nil {
			return fmt.Errorf("LoRa chunk %d/%d: %w", i+1, nChunks, err)
		}
	}
	return nil
}

// chunkHeader, [id(16)][chunkIdx(2)][totalChunks(2)] = 20 bayt meta header.
func makeChunk(id string, idx, total int, payload []byte) []byte {
	hdr := make([]byte, 20)
	copy(hdr[:16], []byte(id))
	hdr[16] = byte(idx >> 8)
	hdr[17] = byte(idx)
	hdr[18] = byte(total >> 8)
	hdr[19] = byte(total)
	return append(hdr, payload...)
}

// bundleToWire, bundle'ı tel formatına çevirir (JSON meta + payload).
type wireBundle struct {
	ID      string `json:"id"`
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Payload []byte `json:"payload"`
}

func bundleToWire(b *dtn.Bundle) ([]byte, error) {
	return json.Marshal(wireBundle{ID: b.ID, Src: b.Src, Dst: b.Dst, Payload: b.Payload})
}

// Pipeline, tam Snapshot→DTN→LoRa hattını yönetir.
type Pipeline struct {
	snap    *snapshot.Daemon
	store   *dtn.Store
	fwd     *dtn.Forwarder
	nodeID  string
	logger  *slog.Logger
	bundled atomic.Uint64 // toplam bundle'lanan snapshot
}

// Config, pipeline yapılandırmasıdır.
type Config struct {
	NodeID          string
	SnapshotKey     []byte // AES-256 (nil = şifreleme atla)
	CaptureInterval time.Duration
	ForwardInterval time.Duration
	DTNDir          string
	LoRaDrv         lora.Driver // nil = MockDriver kullanılır
	LoRaDstAddr     byte
	Logger          *slog.Logger
}

// New, pipeline'ı oluşturur. Config.LoRaDrv nil ise mock LoRa kullanılır.
func New(cap snapshot.Capture, cfg Config) (*Pipeline, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.DTNDir == "" {
		return nil, fmt.Errorf("pipeline: DTNDir boş olamaz")
	}
	store, err := dtn.NewStore(cfg.DTNDir)
	if err != nil {
		return nil, fmt.Errorf("pipeline: DTN depo: %w", err)
	}
	// LoRa sürücüsü.
	drv := cfg.LoRaDrv
	if drv == nil {
		medium := lora.NewMockMedium()
		drv = lora.NewMockDriver(0x01, medium, cfg.Logger)
		cfg.Logger.Info("pipeline: gerçek LoRa yok, MockDriver aktif")
	}
	loraCar := NewLoRACarrier(drv, cfg.LoRaDstAddr, 200)
	fwd := dtn.NewForwarder(store, []dtn.Carrier{loraCar}, cfg.Logger)

	p := &Pipeline{store: store, fwd: fwd, nodeID: cfg.NodeID, logger: cfg.Logger}

	// Snapshot daemon'ı: her frame gelince DTN store'a yaz.
	p.snap = snapshot.New(cap, cfg.SnapshotKey, cfg.NodeID, cfg.Logger,
		func(f snapshot.Frame) {
			b, err := p.frameToBundle(f)
			if err != nil {
				cfg.Logger.Warn("pipeline: bundle oluşturulamadı", "err", err)
				return
			}
			if err := store.Put(b); err != nil {
				cfg.Logger.Warn("pipeline: DTN store hatası", "err", err)
				return
			}
			p.bundled.Add(1)
			cfg.Logger.Info("pipeline: snapshot → DTN store",
				"bundle_id", b.ID,
				"boyut_kb", f.SizeKB,
				"toplam_bekleyen", store.Size())
		})

	return p, nil
}

func (p *Pipeline) frameToBundle(f snapshot.Frame) (*dtn.Bundle, error) {
	id := fmt.Sprintf("%s-%d", p.nodeID, f.CapturedAt.UnixNano())
	return &dtn.Bundle{
		ID:        id,
		Src:       p.nodeID,
		Dst:       "base-station",
		Priority:  dtn.PriorityNormal,
		CreatedAt: f.CapturedAt,
		ExpiresAt: f.CapturedAt.Add(24 * time.Hour),
		Payload:   f.Encrypted,
	}, nil
}

// Run, pipeline'ın tüm bileşenlerini başlatır.
func (p *Pipeline) Run(ctx context.Context, capInterval, fwdInterval time.Duration) {
	go p.snap.Run(ctx, capInterval)
	p.fwd.Run(ctx, fwdInterval)
}

// Stats, pipeline sayaçlarını döndürür.
type Stats struct {
	Bundled uint64 // DTN store'a yazılan snapshot sayısı
	Pending int    // DTN'de bekleyen bundle sayısı
}

func (p *Pipeline) Stats() Stats {
	return Stats{
		Bundled: p.bundled.Load(),
		Pending: p.store.Size(),
	}
}

// --- AES-256-GCM deşifre (alıcı tarafta kullanılır) ---

// DecryptPayload, pipeline'ın şifrelediği payload'ı çözer.
func DecryptPayload(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := aead.NonceSize()
	if len(ciphertext) < ns {
		return nil, fmt.Errorf("şifreli metin çok kısa")
	}
	return aead.Open(nil, ciphertext[:ns], ciphertext[ns:], nil)
}
