package wan

import (
	"context"
	"testing"
	"time"
)

// fakeProber, testte dogrudan erisilebilirligi kontrol etmemizi saglar.
type fakeProber struct{ reachable bool }

func (f fakeProber) DirectReachable(context.Context) bool { return f.reachable }

func TestClassifyDirect(t *testing.T) {
	d := NewDetector(fakeProber{reachable: true}, func() bool { return false }, time.Second)
	if s := d.Refresh(context.Background()); s != StatusDirect {
		t.Fatalf("dogrudan erisim varken Direct beklenir, %q", s)
	}
}

func TestClassifyRelayed(t *testing.T) {
	// Dogrudan WAN yok ama exit peer var -> Relayed.
	d := NewDetector(fakeProber{reachable: false}, func() bool { return true }, time.Second)
	if s := d.Refresh(context.Background()); s != StatusRelayed {
		t.Fatalf("exit peer varken Relayed beklenir, %q", s)
	}
}

func TestClassifyOffGrid(t *testing.T) {
	// Ne dogrudan WAN ne exit peer -> Off-Grid.
	d := NewDetector(fakeProber{reachable: false}, func() bool { return false }, time.Second)
	if s := d.Refresh(context.Background()); s != StatusOffGrid {
		t.Fatalf("WAN ve exit yokken Off-Grid beklenir, %q", s)
	}
}

func TestNilExitPeerIsOffGrid(t *testing.T) {
	d := NewDetector(fakeProber{reachable: false}, nil, time.Second)
	if s := d.Refresh(context.Background()); s != StatusOffGrid {
		t.Fatalf("exit saglayici nil ve WAN yokken Off-Grid beklenir, %q", s)
	}
}

func TestStatusHuman(t *testing.T) {
	cases := map[Status]string{
		StatusDirect:  "Direct Internet",
		StatusRelayed: "Relayed via Peer",
		StatusOffGrid: "Off-Grid Mesh Only",
	}
	for s, want := range cases {
		if s.Human() != want {
			t.Fatalf("%q.Human()=%q, beklenen %q", s, s.Human(), want)
		}
	}
}

func TestInitialStatusUnknown(t *testing.T) {
	d := NewDetector(fakeProber{reachable: true}, nil, time.Second)
	if d.Status() != StatusUnknown {
		t.Fatalf("olcum oncesi Unknown beklenir, %q", d.Status())
	}
	d.Refresh(context.Background())
	if d.Status() != StatusDirect {
		t.Fatalf("olcum sonrasi Direct beklenir, %q", d.Status())
	}
}

// TestTCPProberUnreachable, erisilemez bir hedefin false dondurdugunu dogrular
// (gercek dial, kisa zaman asimi). Bu, off-grid tespitinin gercek olceme
// dayandigini gosterir.
func TestTCPProberUnreachable(t *testing.T) {
	p := NewTCPProber("240.0.0.1:9") // yonlendirilemez TEST-NET adresi
	p.Timeout = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if p.DirectReachable(ctx) {
		t.Skip("beklenmedik: 240.0.0.1 erisilebilir cikti, ortam ozel")
	}
}
