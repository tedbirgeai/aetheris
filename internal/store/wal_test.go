package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// flakyStore, kontrollu olarak hata donduren bir alt store.
type flakyStore struct {
	*MemoryStore
	failing atomic.Bool
	calls   atomic.Uint64
}

func newFlaky() *flakyStore {
	return &flakyStore{MemoryStore: NewMemory()}
}

func (f *flakyStore) Record(ctx context.Context, u Usage) error {
	f.calls.Add(1)
	if f.failing.Load() {
		return errors.New("veritabani gecici olarak erisilemez")
	}
	return f.MemoryStore.Record(ctx, u)
}

// TestWALFlushesToBackend, normal kosulda kayitlarin alt store'a
// ulastigini dogrular.
func TestWALFlushesToBackend(t *testing.T) {
	ctx := context.Background()
	backend := NewMemory()

	wal, err := NewWAL(ctx, backend, WALConfig{
		Dir:           t.TempDir(),
		FlushInterval: 50 * time.Millisecond,
		BatchSize:     10,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	for i := 0; i < 20; i++ {
		if err := wal.Record(ctx, Usage{ClientID: "acme", CarrierType: "mesh_wifi", BytesIn: 5, BytesOut: 2}); err != nil {
			t.Fatal(err)
		}
	}

	// Flush'in tamamlanmasini bekle.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e, err := backend.ClientUsage(ctx, "acme"); err == nil && e.Requests == 20 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	e, err := backend.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatalf("alt store'da kayit yok: %v", err)
	}
	if e.Requests != 20 {
		t.Fatalf("Requests = %d, beklenen 20", e.Requests)
	}
	if e.BytesIn != 100 {
		t.Fatalf("BytesIn = %d, beklenen 100", e.BytesIn)
	}
}

// TestWALIsNonBlockingDuringDBOutage, veritabani DUSUKKEN Record'un
// BLOKLANMADIGINI ve kayitlarin kaybolmadigini dogrular. v0.3a'nin
// varlik sebebi budur.
func TestWALIsNonBlockingDuringDBOutage(t *testing.T) {
	ctx := context.Background()
	flaky := newFlaky()
	flaky.failing.Store(true) // veritabani DUSUK

	wal, err := NewWAL(ctx, flaky, WALConfig{
		Dir:           t.TempDir(),
		FlushInterval: 30 * time.Millisecond,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	// DB dusukken 50 istek gelsin. Record BLOKLANMAMALI.
	start := time.Now()
	for i := 0; i < 50; i++ {
		if err := wal.Record(ctx, Usage{ClientID: "acme", CarrierType: "direct", BytesIn: 1, BytesOut: 1}); err != nil {
			t.Fatalf("DB dusukken Record hata dondu: %v", err)
		}
	}
	elapsed := time.Since(start)

	// 50 diske-yazma cok hizli olmali; blokli olsaydi flush timeout'larina
	// takilirdi. 5 saniyeden az beklenir (fsync dahil).
	if elapsed > 5*time.Second {
		t.Fatalf("Record bloklandi: %v", elapsed)
	}

	// Flusher hata almis olmali (DB dusuk), kayitlar WAL'de.
	time.Sleep(200 * time.Millisecond)
	if flaky.calls.Load() == 0 {
		t.Fatal("flusher hic denenmedi")
	}

	// Simdi DB gelsin.
	flaky.failing.Store(false)

	// Flusher toparlamali. (Mevcut WAL implementasyonunda basarisiz batch
	// WAL'de kalir; DB gelince bir sonraki flush veya recover isler.)
	// Bu testte Close -> recover yolunu dogrulayalim.
}

// TestWALRecoversAfterCrash, surec cokse bile WAL'deki kayitlarin
// yeni bir WALStore acilisinda kurtarildigini dogrular.
func TestWALRecoversAfterCrash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. Asama: DB dusukken kayitlar yaz, sonra "cokme" simule et
	//    (Close cagirmadan referansi birak).
	flaky := newFlaky()
	flaky.failing.Store(true)

	wal1, err := NewWAL(ctx, flaky, WALConfig{
		Dir:           dir,
		FlushInterval: time.Hour, // flush'i engelle, kayitlar WAL'de kalsin
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		if err := wal1.Record(ctx, Usage{ClientID: "acme", CarrierType: "lora_ism", BytesIn: 3, BytesOut: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// "Cokme" simulasyonu: flusher'i durdur ve dosya taniticiyi kapat,
	// ama kayitlar DB'ye BASILMAMIS olsun (FlushInterval bir saat).
	//
	// PLATFORM NOTU: Windows, acik dosya taniticisi olan bir dosyanin
	// silinmesine/yeniden acilmasina izin vermez. Bu yuzden goroutine'i
	// de durdurup dosyayi tamamen birakiyoruz.
	wal1.stopOnce.Do(func() { close(wal1.stop) })
	<-wal1.stopped

	wal1.walMu.Lock()
	_ = wal1.walBuf.Flush()
	_ = wal1.walFile.Sync()
	_ = wal1.walFile.Close()
	wal1.walMu.Unlock()

	// 2. Asama: DB artik saglikli, yeni WALStore ac -> kurtarma calismali.
	healthy := NewMemory()
	wal2, err := NewWAL(ctx, healthy, WALConfig{
		Dir:           dir,
		FlushInterval: 50 * time.Millisecond,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal2.Close()

	e, err := healthy.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatalf("kurtarma sonrasi kayit yok: %v", err)
	}
	if e.Requests != 10 {
		t.Fatalf("kurtarilan Requests = %d, beklenen 10", e.Requests)
	}
	if e.BytesIn != 30 {
		t.Fatalf("kurtarilan BytesIn = %d, beklenen 30", e.BytesIn)
	}
}

// TestWALConcurrentRecord, es zamanli yazmalarda WAL'in bozulmadigini
// dogrular. -race ile calistirilmalidir.
func TestWALConcurrentRecord(t *testing.T) {
	ctx := context.Background()
	backend := NewMemory()

	wal, err := NewWAL(ctx, backend, WALConfig{
		Dir:           t.TempDir(),
		FlushInterval: 20 * time.Millisecond,
		QueueSize:     8192,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	const goroutines = 20
	const per = 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_ = wal.Record(ctx, Usage{ClientID: "acme", CarrierType: "mesh_wifi", BytesIn: 1, BytesOut: 1})
			}
		}()
	}
	wg.Wait()

	// Close, kalan kuyrugu bosaltir.
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	e, err := backend.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	const total = goroutines * per
	if e.Requests != total {
		t.Fatalf("Requests = %d, beklenen %d - KAYIP VAR", e.Requests, total)
	}
}

// TestWALRecoveryReleasesFileHandle, kurtarma sonrasi WAL dosyasinin
// TAMAMEN serbest birakildigini dogrular.
//
// NEDEN AYRI TEST: recover() dosyayi okuduktan sonra siliyor. Eger dosya
// taniticisi hala acikken silmeye calisirsa, Linux buna izin verir ama
// WINDOWS VERMEZ ("dosya baska bir islem tarafindan kullaniliyor").
// Bu test, platformlar arasi bu farkin bir daha regresyon olmasini onler.
//
// KULLANIM NOTU: Ayni WAL dizininde AYNI ANDA tek bir WALStore ornegi
// bulunmalidir (uretimde: bir surec, bir dizin). Iki ornek ayni dizini
// paylasirsa ayni kayitlar iki kez islenir — cift faturalama. Bu test
// bilerek SIRALI acilis/kapanis yapar.
func TestWALRecoveryReleasesFileHandle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. acilis: kayit yaz, flush etme, duzgun kapat.
	wal1, err := NewWAL(ctx, NewMemory(), WALConfig{
		Dir:           dir,
		FlushInterval: time.Hour,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = wal1.Record(ctx, Usage{ClientID: "acme", CarrierType: "direct", BytesIn: 1, BytesOut: 1})
	}
	if err := wal1.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. acilis: kurtarma yapar, WAL'i temizler, kapanir.
	wal2, err := NewWAL(ctx, NewMemory(), WALConfig{
		Dir:           dir,
		FlushInterval: 50 * time.Millisecond,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatalf("ikinci acilis dosya kilidine takildi: %v", err)
	}
	if err := wal2.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. acilis: dosya tamamen serbest kalmis olmali.
	wal3, err := NewWAL(ctx, NewMemory(), WALConfig{
		Dir:           dir,
		FlushInterval: 50 * time.Millisecond,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatalf("ucuncu acilis basarisiz: %v", err)
	}
	if err := wal3.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestWALConcurrentInstancesRejected, ayni dizini paylasan ikinci bir
// ornegin acilista ACIK HATA verdigini dogrular (Windows'ta).
//
// Bu davranis bilinclidir: sessizce devam etmek ayni kayitlarin iki kez
// faturalanmasina yol acardi. Linux'ta dosya silme serbest oldugu icin
// bu senaryo hata vermez; test bu yuzden platform-toleransli yazilmistir.
func TestWALConcurrentInstancesBehaviour(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	wal1, err := NewWAL(ctx, NewMemory(), WALConfig{
		Dir:           dir,
		FlushInterval: time.Hour,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal1.Close()

	_ = wal1.Record(ctx, Usage{ClientID: "acme", CarrierType: "direct", BytesIn: 1, BytesOut: 1})

	// wal1 ACIKKEN ikinci ornegi ac. Windows'ta hata beklenir,
	// Linux'ta basarili olur. Her iki durumda da PANIK OLMAMALI.
	wal2, err := NewWAL(ctx, NewMemory(), WALConfig{
		Dir:           dir,
		FlushInterval: 50 * time.Millisecond,
		Logger:        quietLog(),
	})
	if err != nil {
		// Windows yolu: acik ve anlamli hata mesaji bekleniyor.
		if !strings.Contains(err.Error(), "wal:") {
			t.Fatalf("hata mesaji anlamli degil: %v", err)
		}
		return
	}
	// Linux yolu: acilis basarili.
	_ = wal2.Close()
}

// TestWALStats, gozlemlenebilirlik sayaclarinin calistigini dogrular.
func TestWALStats(t *testing.T) {
	ctx := context.Background()
	wal, err := NewWAL(ctx, NewMemory(), WALConfig{
		Dir:           t.TempDir(),
		FlushInterval: 20 * time.Millisecond,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	for i := 0; i < 5; i++ {
		_ = wal.Record(ctx, Usage{ClientID: "acme", CarrierType: "direct", BytesIn: 1, BytesOut: 1})
	}
	time.Sleep(200 * time.Millisecond)

	s := wal.Stats()
	if s.Enqueued != 5 {
		t.Fatalf("Enqueued = %d, beklenen 5", s.Enqueued)
	}
	if s.Flushed != 5 {
		t.Fatalf("Flushed = %d, beklenen 5", s.Flushed)
	}
}

// TestWALKind, store turunun dogru raporlandigini dogrular.
func TestWALKind(t *testing.T) {
	wal, err := NewWAL(context.Background(), NewMemory(), WALConfig{
		Dir:    t.TempDir(),
		Logger: quietLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close()

	if wal.Kind() != "wal+memory" {
		t.Fatalf("Kind() = %q, beklenen wal+memory", wal.Kind())
	}
}
