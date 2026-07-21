package meter

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tedbirge-labs/aetheris-gateway/internal/store"
)

// TestMeterDelegatesToStore, meter'in kendi kalicilik mantigi
// tutmadigini, her seyi store'a devrettigini dogrular.
func TestMeterDelegatesToStore(t *testing.T) {
	st := store.NewMemory()
	m := New(st)
	ctx := context.Background()

	if err := m.Record(ctx, store.Usage{
		ClientID:    "acme",
		CarrierType: "mesh_wifi",
		BytesIn:     100,
		BytesOut:    20,
	}); err != nil {
		t.Fatal(err)
	}

	// Store'a dogrudan sorulan cevap ile meter uzerinden gelen ayni olmali.
	direct, err := st.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	viaMeter, err := m.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if direct.BytesIn != viaMeter.BytesIn || direct.Requests != viaMeter.Requests {
		t.Fatal("meter store ile ayni veriyi dondurmuyor")
	}
}

func TestMeterKindReflectsStore(t *testing.T) {
	m := New(store.NewMemory())
	if m.Kind() != "memory" {
		t.Fatalf("Kind() = %q, beklenen memory", m.Kind())
	}
}

// TestActiveTunnelsIsSymmetric, acilan her tunelin kapandigini ve
// gostergenin sifira dondugunu dogrular. -race ile calistirilmalidir.
func TestActiveTunnelsIsSymmetric(t *testing.T) {
	m := New(store.NewMemory())

	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.TunnelOpened()
			m.TunnelClosed()
		}()
	}
	wg.Wait()

	if got := m.ActiveTunnels(); got != 0 {
		t.Fatalf("ActiveTunnels = %d, beklenen 0", got)
	}
}

func TestActiveTunnelsCountsOpen(t *testing.T) {
	m := New(store.NewMemory())
	m.TunnelOpened()
	m.TunnelOpened()
	if got := m.ActiveTunnels(); got != 2 {
		t.Fatalf("ActiveTunnels = %d, beklenen 2", got)
	}
	m.TunnelClosed()
	if got := m.ActiveTunnels(); got != 1 {
		t.Fatalf("ActiveTunnels = %d, beklenen 1", got)
	}
}

func TestMeterPropagatesNotFound(t *testing.T) {
	m := New(store.NewMemory())
	if _, err := m.ClientUsage(context.Background(), "yok"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ErrNotFound bekleniyordu, alinan: %v", err)
	}
}

func TestMeterUptimeIsNonNegative(t *testing.T) {
	m := New(store.NewMemory())
	if m.UptimeSeconds() < 0 {
		t.Fatal("negatif calisma suresi")
	}
}
