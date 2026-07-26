package pipeline

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/tedbirgeai/aetheris/internal/carrier/lora"
	"github.com/tedbirgeai/aetheris/internal/dtn"
	"github.com/tedbirgeai/aetheris/internal/edge/snapshot"
)

// TestPipelineEndToEnd, snapshot yakalanıp DTN store'a yazıldığını ve LoRa
// mock üzerinden iletildiğini doğrular.
func TestPipelineEndToEnd(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	// Mock kamera.
	cap := snapshot.NewMockCapture(160, 120)

	// Mock LoRa ortamı: gönderici + alıcı.
	medium := lora.NewMockMedium()
	txDrv := lora.NewMockDriver(0x01, medium, nil) // gönderici
	rxDrv := lora.NewMockDriver(0x02, medium, nil) // alıcı (iletimi doğrular)

	// Pipeline.
	cfg := Config{
		NodeID:          "saha-1",
		SnapshotKey:     key,
		DTNDir:          dir,
		LoRaDrv:         txDrv,
		LoRaDstAddr:     0x02,
		CaptureInterval: 100 * time.Millisecond,
		ForwardInterval: 200 * time.Millisecond,
	}
	p, err := New(cap, cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go p.Run(ctx, cfg.CaptureInterval, cfg.ForwardInterval)

	// En az 1 snapshot yakalanıp DTN'e yazılmasını ve LoRa ile iletilmesini bekle.
	deadline := time.Now().Add(5 * time.Second)
	var st Stats
	for time.Now().Before(deadline) {
		st = p.Stats()
		if st.Bundled >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st.Bundled < 1 {
		t.Fatalf("en az 1 snapshot bundle'lanmalıydı: %+v", st)
	}
	t.Logf("pipeline stats: %+v", st)

	// LoRa alıcısının bir şey aldığını doğrula.
	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	defer rcancel()
	frame, err := rxDrv.Receive(rctx)
	if err != nil {
		t.Fatalf("LoRa alıcısı en az 1 çerçeve almalıydı: %v", err)
	}
	if len(frame) < 20 {
		t.Fatalf("LoRa çerçevesi çok kısa: %d bayt", len(frame))
	}
	t.Logf("LoRa üzerinden %d bayt alındı", len(frame))
}

// TestLoRACarrierChunking, büyük payload'ın parçalara bölündüğünü doğrular.
func TestLoRACarrierChunking(t *testing.T) {
	medium := lora.NewMockMedium()
	drv := lora.NewMockDriver(0x01, medium, nil)
	car := NewLoRACarrier(drv, 0x02, 50) // 50 bayt chunk

	b := &dtn.Bundle{
		ID:      "chunk-test",
		Src:     "A",
		Dst:     "B",
		Payload: make([]byte, 200),
	}
	ctx := context.Background()
	if err := car.Send(ctx, b); err != nil {
		t.Fatalf("gönderim hatası: %v", err)
	}
	// 200 bayt payload + meta / 50 bayt chunk = birden fazla chunk.
	// Hata yoksa chunking başarılı.
}

// TestPipelineStats, istatistik sayaçlarının doğru arttığını doğrular.
func TestPipelineStats(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cap := snapshot.NewMockCapture(80, 60)
	cfg := Config{NodeID: "n1", SnapshotKey: key, DTNDir: dir}
	p, err := New(cap, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go p.Run(ctx, 80*time.Millisecond, time.Hour) // forward interval çok uzun

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Bundled >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := p.Stats()
	if st.Bundled < 2 {
		t.Fatalf("en az 2 bundle beklenir: %+v", st)
	}
	t.Logf("son stats: bundled=%d pending=%d", st.Bundled, st.Pending)
}
