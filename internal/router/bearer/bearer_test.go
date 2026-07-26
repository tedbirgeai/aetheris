package bearer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// probBearer, testte saglik/RTT kontrollu bir bearer.
type probeBearer struct {
	kind    Kind
	avail   bool
	healthy atomic.Bool
	rtt     float64
}

func (b *probeBearer) Kind() Kind      { return b.kind }
func (b *probeBearer) Available() bool { return b.avail }
func (b *probeBearer) Probe(_ context.Context) (float64, error) {
	if !b.healthy.Load() {
		return 0, errors.New("sagliksiz")
	}
	return b.rtt, nil
}

// TestBearerPriority, ilk saglikli (yuksek oncelikli) tasiyicinin secildigini
// dogrular.
func TestBearerPriority(t *testing.T) {
	m := New(quietLogger(), nil, 50*time.Millisecond)
	high := &probeBearer{kind: KindEthernet, avail: true, rtt: 5}
	high.healthy.Store(true)
	low := &probeBearer{kind: KindLoRaUSB, avail: true, rtt: 200}
	low.healthy.Store(true)
	m.Register(high) // once eklenen = yuksek oncelik
	m.Register(low)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	if m.Active() != KindEthernet {
		t.Fatalf("yuksek oncelikli Ethernet secilmeli, %q secildi", m.Active())
	}
}

// TestFailoverOnDown, birincil tasiyici copunca bir sonrakine gecildigini
// dogrular.
func TestFailoverOnDown(t *testing.T) {
	var changed atomic.Int32
	m := New(quietLogger(), func(e ChangeEvent) { changed.Add(1) }, 30*time.Millisecond)

	primary := &probeBearer{kind: KindWiFiWAN, avail: true, rtt: 10}
	primary.healthy.Store(true)
	fallback := &probeBearer{kind: KindSoftAPMesh, avail: true, rtt: 50}
	fallback.healthy.Store(true)
	m.Register(primary)
	m.Register(fallback)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	if m.Active() != KindWiFiWAN {
		t.Fatalf("once WiFi secilmeli: %q", m.Active())
	}

	// Birincil copuyor.
	primary.healthy.Store(false)
	time.Sleep(200 * time.Millisecond)

	if m.Active() != KindSoftAPMesh {
		t.Fatalf("failover sonrasi SoftAP secilmeli: %q", m.Active())
	}
	if changed.Load() < 1 {
		t.Fatal("onChange en az bir kez tetiklenmeli (failover bildirimi)")
	}
}

// TestHardwareStubSkipped, Available()=false olan stub tasiyicilerin
// hicbir zaman secilmedigini dogrular.
func TestHardwareStubSkipped(t *testing.T) {
	m := New(quietLogger(), nil, 30*time.Millisecond)
	m.Register(NewHardwareStub(KindLoRaUSB, "test"))
	m.Register(NewHardwareStub(KindBLEMesh, "test"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	if m.Active() != "" {
		t.Fatalf("hic saglikli tasiyici yokken Active bos olmali: %q", m.Active())
	}
}

// TestTCPBearerReal, gercek bir TCP sunucusuna probe'un basarili oldugunu
// dogrular.
func TestTCPBearerReal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	b := NewTCPBearer(KindEthernet, []string{ln.Addr().String()})
	rtt, err := b.Probe(context.Background())
	if err != nil {
		t.Fatalf("gercek TCP probe basarili olmali: %v", err)
	}
	if rtt <= 0 {
		t.Fatalf("RTT pozitif olmali: %f", rtt)
	}
}
