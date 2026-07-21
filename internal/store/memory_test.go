package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestMemoryConcurrentRecord, 200 goroutine x 500 yazim altinda tek bir
// baytin bile kaybolmadigini dogrular. -race ile calistirilmalidir.
func TestMemoryConcurrentRecord(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	const goroutines = 200
	const perGoroutine = 500
	const bytesEach = 7

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			clientID := fmt.Sprintf("client-%d", g%10)
			for i := 0; i < perGoroutine; i++ {
				if err := m.Record(ctx, Usage{
					ClientID:    clientID,
					CarrierType: "standard_internet",
					BytesIn:     bytesEach,
					BytesOut:    1,
				}); err != nil {
					t.Errorf("Record hata dondu: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	snap, err := m.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	wantIn := uint64(goroutines * perGoroutine * bytesEach)
	if snap.TotalBytesIn != wantIn {
		t.Fatalf("TotalBytesIn = %d, beklenen %d", snap.TotalBytesIn, wantIn)
	}
	if want := uint64(goroutines * perGoroutine); snap.TotalRequests != want {
		t.Fatalf("TotalRequests = %d, beklenen %d", snap.TotalRequests, want)
	}
	if len(snap.Clients) != 10 {
		t.Fatalf("istemci sayisi = %d, beklenen 10", len(snap.Clients))
	}

	var sum uint64
	for _, e := range snap.Clients {
		sum += e.BytesIn
	}
	if sum != wantIn {
		t.Fatalf("defter toplami %d, kuresel sayac %d ile uyusmuyor", sum, wantIn)
	}
}

// TestMemorySnapshotIsDeepCopy, anlik goruntunun canli deftere referans
// tutmadigini dogrular. Faturalama verisinin sonradan degismemesi kritiktir.
func TestMemorySnapshotIsDeepCopy(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	if err := m.Record(ctx, Usage{ClientID: "acme", CarrierType: "mesh_wifi", BytesIn: 100, BytesOut: 10}); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := snap.Clients["acme"].BytesIn

	if err := m.Record(ctx, Usage{ClientID: "acme", CarrierType: "mesh_wifi", BytesIn: 900, BytesOut: 10}); err != nil {
		t.Fatal(err)
	}

	if snap.Clients["acme"].BytesIn != before {
		t.Fatal("anlik goruntu canli defterle birlikte degisti - derin kopya degil")
	}
	if got := snap.Clients["acme"].ByCarrier["mesh_wifi"]; got != 110 {
		t.Fatalf("tasiyici kirilimi bozuldu: %d", got)
	}
}

// TestMemoryClientIsolation, bir istemcinin baskasinin verisini
// goremedigini dogrular.
func TestMemoryClientIsolation(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	_ = m.Record(ctx, Usage{ClientID: "acme", CarrierType: "standard_internet", BytesIn: 500, BytesOut: 20})
	_ = m.Record(ctx, Usage{ClientID: "globex", CarrierType: "standard_internet", BytesIn: 999, BytesOut: 20})

	e, err := m.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatalf("acme defteri okunamadi: %v", err)
	}
	if e.BytesIn != 500 {
		t.Fatalf("acme BytesIn = %d, beklenen 500", e.BytesIn)
	}

	if _, err := m.ClientUsage(ctx, "bilinmeyen"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kayitsiz istemci icin ErrNotFound bekleniyordu, alinan: %v", err)
	}
}

// TestMemoryConcurrentReadWrite, okuma ve yazmanin es zamanli
// calistigini dogrular (RWMutex dogru kullaniliyor mu).
func TestMemoryConcurrentReadWrite(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = m.Snapshot(ctx)
					_, _ = m.ClientUsage(ctx, "acme")
				}
			}
		}()
	}

	for i := 0; i < 1000; i++ {
		if err := m.Record(ctx, Usage{ClientID: "acme", CarrierType: "lora_ism", BytesIn: 1, BytesOut: 1}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()

	e, err := m.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if e.Requests != 1000 {
		t.Fatalf("Requests = %d, beklenen 1000", e.Requests)
	}
}

// TestEntryCloneNil, nil Entry uzerinde Clone'un panik atmadigini dogrular.
func TestEntryCloneNil(t *testing.T) {
	var e *Entry
	if got := e.Clone(); got != nil {
		t.Fatalf("nil Entry icin nil beklendi, alinan %v", got)
	}
}
