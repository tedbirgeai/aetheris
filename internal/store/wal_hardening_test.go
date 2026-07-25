package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWALTruncateAtomicSwap, v0.5a sertlestirmesini dogrular: bircok kez
// truncate (atomik temp-swap) tetiklendiginde dosya tutarli kalir, gecici
// dosya artigi birakilmaz ve store yazmaya devam edebilir. Bu test, Windows
// dosya-kilidi uyarisinin kaynagi olan yolu (rotateToEmpty) dogrudan zorlar.
func TestWALTruncateAtomicSwap(t *testing.T) {
	ctx := context.Background()
	backend := NewMemory()
	dir := t.TempDir()

	wal, err := NewWAL(ctx, backend, WALConfig{
		Dir:           dir,
		QueueSize:     256,
		BatchSize:     4,
		FlushInterval: 10 * time.Millisecond,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cok sayida kayit yaz: her basarili batch bir truncate (swap) tetikler.
	const total = 200
	for i := 0; i < total; i++ {
		if err := wal.Record(ctx, Usage{
			ClientID:   "c",
			PayloadSHA: sha(i),
			BytesIn:    uint64(i),
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
		if i%20 == 0 {
			time.Sleep(5 * time.Millisecond) // flusher'a swap sansi ver
		}
	}

	// Tum kayitlar alt store'a ulasana kadar bekle.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := backend.Snapshot(ctx)
		if snap.TotalRequests >= total {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := backend.Snapshot(ctx)
	if snap.TotalRequests != total {
		t.Fatalf("beklenen %d kayit, alt store'da %d", total, snap.TotalRequests)
	}

	// Asil WAL dosyasi hala acilabilir/yazilabilir olmali (handle saglikli).
	if err := wal.Record(ctx, Usage{ClientID: "c", PayloadSHA: "son", BytesIn: 1}); err != nil {
		t.Fatalf("swap sonrasi yazma basarisiz: %v", err)
	}

	// WAL'i KAPAT: bu, arka plandaki flusher'i durdurur ve son rotasyonu
	// tamamlar. Ancak bundan sonra gecici .tmp dosyasi kesin olarak kalmamis
	// olmalidir (calisirken .tmp rotasyon sirasinda gecici olarak var olabilir;
	// bu yuzden kontrol yalnizca kapanistan SONRA anlamlidir).
	if err := wal.Close(); err != nil {
		t.Fatalf("WAL kapatilamadi: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "aetheris.wal.tmp")); !os.IsNotExist(err) {
		t.Fatalf("kapanis sonrasi gecici swap dosyasi temizlenmemis: %v", err)
	}
}

// TestWALSurvivesRepeatedRotation, art arda dogrudan rotateToEmpty
// cagrilarinin (yogun swap) veri kaybina veya bozuk handle'a yol acmadigini
// dogrular.
func TestWALSurvivesRepeatedRotation(t *testing.T) {
	ctx := context.Background()
	wal, err := NewWAL(ctx, NewMemory(), WALConfig{
		Dir:           t.TempDir(),
		QueueSize:     64,
		BatchSize:     1000, // batch tetiklenmesin; swap'i elle cagiracagiz
		FlushInterval: time.Hour,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	for r := 0; r < 50; r++ {
		if err := wal.Record(ctx, Usage{ClientID: "x", PayloadSHA: sha(r)}); err != nil {
			t.Fatalf("round %d Record: %v", r, err)
		}
		// Dogrudan swap yolunu zorla (Windows'ta eskiden "Erisim engellendi").
		wal.walMu.Lock()
		err := wal.rotateToEmpty()
		wal.walMu.Unlock()
		if err != nil {
			t.Fatalf("round %d rotateToEmpty: %v", r, err)
		}
	}
}

func sha(i int) string {
	const hexdig = "0123456789abcdef"
	b := make([]byte, 8)
	for j := 0; j < 8; j++ {
		b[j] = hexdig[(i>>(j*4))&0xF]
	}
	return string(b)
}
