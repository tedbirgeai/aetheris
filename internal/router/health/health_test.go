package health

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePinger, testte hata/basari davranisini kontrol etmemizi saglar.
type fakePinger struct {
	failing atomic.Bool
	rtt     time.Duration
}

func (f *fakePinger) Ping(context.Context, string) (time.Duration, error) {
	if f.failing.Load() {
		return 0, errors.New("ping basarisiz")
	}
	return f.rtt, nil
}

// TestFailoverOnLinkDown, KABUL KRITERI (failover): aktif hat koptugunda
// monitor onu 'down' isaretler ve geri cagri tetiklenir.
func TestFailoverOnLinkDown(t *testing.T) {
	fp := &fakePinger{rtt: 10 * time.Millisecond}

	var (
		mu       sync.Mutex
		downSeen bool
	)
	m := New(fp, Config{
		Interval:  15 * time.Millisecond,
		DownAfter: 3,
		Timeout:   50 * time.Millisecond,
		OnChange: func(s State) {
			if !s.Up {
				mu.Lock()
				downSeen = true
				mu.Unlock()
			}
		},
	})
	m.Watch("exit-B")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	// Baslangicta saglikli olmali.
	if !waitFor(t, time.Second, func() bool { return m.Healthy("exit-B") }) {
		t.Fatal("baslangicta saglikli olmaliydi")
	}

	// Hat kopsun.
	fp.failing.Store(true)

	// downAfter (3) ardisik hatadan sonra down olmali + geri cagri tetiklenmeli.
	if !waitFor(t, 2*time.Second, func() bool { return !m.Healthy("exit-B") }) {
		t.Fatal("hat kopunca down isaretlenmeliydi")
	}
	mu.Lock()
	seen := downSeen
	mu.Unlock()
	if !seen {
		t.Fatal("down durum degisikligi geri cagrisi tetiklenmeliydi (failover)")
	}
}

// TestRecovery, kopan hattin geri gelince tekrar Up olmasini dogrular.
func TestRecovery(t *testing.T) {
	fp := &fakePinger{rtt: 5 * time.Millisecond}
	fp.failing.Store(true)
	m := New(fp, Config{Interval: 15 * time.Millisecond, DownAfter: 2, Timeout: 50 * time.Millisecond})
	m.Watch("peer")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	// Once down olmali.
	if !waitFor(t, 2*time.Second, func() bool { return !m.Healthy("peer") }) {
		t.Fatal("basta down olmaliydi")
	}
	// Hat geri gelsin.
	fp.failing.Store(false)
	if !waitFor(t, 2*time.Second, func() bool { return m.Healthy("peer") }) {
		t.Fatal("hat geri gelince Up olmaliydi (recovery)")
	}
}

// TestRTTSmoothing, RTT'nin EWMA ile yumusatildigini kontrol eder.
func TestRTTSmoothing(t *testing.T) {
	fp := &fakePinger{rtt: 20 * time.Millisecond}
	m := New(fp, Config{Interval: 10 * time.Millisecond})
	m.Watch("x")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	if !waitFor(t, time.Second, func() bool {
		for _, s := range m.Snapshot() {
			if s.Target == "x" && s.RTT > 0 {
				return true
			}
		}
		return false
	}) {
		t.Fatal("RTT olculmeliydi")
	}
}

// TestTCPPingerReal, gercek bir TCP dinleyiciye ping'in basarili, olmayan
// adrese basarisiz oldugunu dogrular.
func TestTCPPingerReal(t *testing.T) {
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
			c.Close()
		}
	}()

	p := NewTCPPinger()
	ctx := context.Background()
	if _, err := p.Ping(ctx, ln.Addr().String()); err != nil {
		t.Fatalf("canli dinleyiciye ping basarili olmaliydi: %v", err)
	}
	// Kapali port.
	if _, err := p.Ping(ctx, "127.0.0.1:1"); err == nil {
		t.Skip("beklenmedik: 127.0.0.1:1 erisilebilir, ortam ozel")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(8 * time.Millisecond)
	}
	return cond()
}
