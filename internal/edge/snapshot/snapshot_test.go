package snapshot

import (
	"context"
	"crypto/rand"
	"testing"
	"time"
)

func TestMockCaptureProducesFrame(t *testing.T) {
	cap := NewMockCapture(320, 240)
	img, err := cap.Frame(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() != 320 || b.Dy() != 240 {
		t.Fatalf("boyut yanlis: %v", b)
	}
	if cap.Available() {
		t.Fatal("mock Available() false olmali")
	}
}

func TestCompressUnder50KB(t *testing.T) {
	cap := NewMockCapture(640, 480) // buyuk cerceve, daha agir test
	img, _ := cap.Frame(context.Background())
	data, err := compressToSize(img, MaxSizeKB*1024)
	if err != nil {
		t.Fatal(err)
	}
	kb := float64(len(data)) / 1024.0
	if kb > MaxSizeKB {
		t.Fatalf("sikistirilmis boyut %.1f KB > %d KB siniri", kb, MaxSizeKB)
	}
	t.Logf("640x480 mock IR -> %.1f KB JPEG", kb)
}

func TestDaemonCapturesFrame(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	cap := NewMockCapture(320, 240)
	received := make(chan Frame, 1)
	d := New(cap, key, "test-node", nil, func(f Frame) { received <- f })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go d.Run(ctx, 200*time.Millisecond)

	select {
	case f := <-received:
		if f.SizeKB > MaxSizeKB {
			t.Fatalf("frame boyutu siniri asiyor: %.1f KB", f.SizeKB)
		}
		if len(f.Encrypted) == 0 {
			t.Fatal("sifrelenmis payload bos")
		}
		if !f.Simulated {
			t.Fatal("mock frame Simulated=true olmali")
		}
		if f.NodeID != "test-node" {
			t.Fatalf("NodeID yanlis: %q", f.NodeID)
		}
		t.Logf("frame yakalandi: %.1f KB, simulated=%v", f.SizeKB, f.Simulated)
	case <-ctx.Done():
		t.Fatal("frame yakalanmadi (timeout)")
	}
}

func TestFrameMarshal(t *testing.T) {
	f := Frame{
		CapturedAt: time.Now(),
		SizeKB:     12.5,
		NodeID:     "edge-abc",
		Simulated:  true,
		Encrypted:  []byte("payload"),
	}
	data, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("marshal bos sonuc vermemeli")
	}
	t.Logf("marshal: %s", data)
}
