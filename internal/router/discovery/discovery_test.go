package discovery

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// memBus, birden cok memTransport'u birbirine baglayan bellek-ici broadcast
// veri yoludur (deterministik test icin).
type memBus struct {
	mu   sync.Mutex
	subs []chan []byte
}

func (b *memBus) attach() *memTransport {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan []byte, 128)
	b.subs = append(b.subs, ch)
	return &memTransport{bus: b, inbox: ch}
}

func (b *memBus) publish(src chan []byte, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		if ch == src {
			continue // kendine gonderme
		}
		cp := make([]byte, len(data))
		copy(cp, data)
		select {
		case ch <- cp:
		default:
		}
	}
}

type memTransport struct {
	bus   *memBus
	inbox chan []byte
}

func (t *memTransport) Broadcast(data []byte) error { t.bus.publish(t.inbox, data); return nil }
func (t *memTransport) Inbox() <-chan []byte        { return t.inbox }
func (t *memTransport) Close() error                { return nil }

// TestAutoDiscoverExit, KABUL KRITERI: istemci dugum (A), agda WAN cikisi olan
// exit node'u (B) MANUEL YAPILANDIRMA OLMADAN otomatik kesfeder.
func TestAutoDiscoverExit(t *testing.T) {
	bus := &memBus{}

	// A: istemci, WAN yok, exit degil.
	svcA := New(bus.attach(), Config{
		Self:     Announce{NodeID: "A", ExitCapable: false, WANHealthy: false},
		Interval: 20 * time.Millisecond,
		TTL:      2 * time.Second,
	})
	// B: exit node, WAN saglikli.
	svcB := New(bus.attach(), Config{
		Self:     Announce{NodeID: "B", RelayAddr: "10.0.0.2:9800", ExitCapable: true, WANHealthy: true},
		Interval: 20 * time.Millisecond,
		TTL:      2 * time.Second,
	})
	go svcA.Run()
	go svcB.Run()
	defer svcA.Close()
	defer svcB.Close()

	// A, B'yi exit olarak otomatik kesfetmeli.
	if !waitFor(t, 2*time.Second, func() bool {
		_, ok := svcA.BestExit()
		return ok
	}) {
		t.Fatal("A, exit node B'yi otomatik kesfedemedi")
	}
	exit, _ := svcA.BestExit()
	if exit.NodeID != "B" || exit.RelayAddr != "10.0.0.2:9800" {
		t.Fatalf("yanlis exit kesfedildi: %+v", exit)
	}
}

// TestUnhealthyExitExcluded, WAN'i saglikli OLMAYAN exit'in secilmedigini
// dogrular.
func TestUnhealthyExitExcluded(t *testing.T) {
	bus := &memBus{}
	svcA := New(bus.attach(), Config{Self: Announce{NodeID: "A"}, Interval: 20 * time.Millisecond})
	// B exit ama WAN'i saglikli degil.
	svcB := New(bus.attach(), Config{
		Self:     Announce{NodeID: "B", RelayAddr: "x:1", ExitCapable: true, WANHealthy: false},
		Interval: 20 * time.Millisecond,
	})
	go svcA.Run()
	go svcB.Run()
	defer svcA.Close()
	defer svcB.Close()

	// B kesfedilmeli (peer) ama exit olarak SECILMEMELI.
	waitFor(t, time.Second, func() bool { return len(svcA.Peers()) > 0 })
	if _, ok := svcA.BestExit(); ok {
		t.Fatal("WAN'i saglikli olmayan exit secilmemeliydi")
	}

	// Simdi B'nin WAN'i saglikli olsun; A onu exit olarak gormeli.
	svcB.SetWANHealthy(true)
	if !waitFor(t, 2*time.Second, func() bool { _, ok := svcA.BestExit(); return ok }) {
		t.Fatal("WAN saglikli olunca exit secilmeliydi")
	}
}

// TestStaleEviction, ilan gondermeyi durduran komsunun TTL sonrasi
// tablodan dustugunu dogrular.
func TestStaleEviction(t *testing.T) {
	bus := &memBus{}
	svcA := New(bus.attach(), Config{Self: Announce{NodeID: "A"}, Interval: time.Hour, TTL: 200 * time.Millisecond})
	go svcA.Run()
	defer svcA.Close()

	// B'nin tek bir ilanini elle enjekte et.
	trB := bus.attach()
	_ = trB.Broadcast(mustJSON(Announce{NodeID: "B", TS: time.Now().UnixNano()}))

	if !waitFor(t, time.Second, func() bool { return len(svcA.Peers()) == 1 }) {
		t.Fatal("B kesfedilmeliydi")
	}
	// TTL sonrasi dusmeli (B artik ilan gondermiyor).
	if !waitFor(t, 2*time.Second, func() bool { return len(svcA.Peers()) == 0 }) {
		t.Fatal("eski komsu TTL sonrasi tablodan dusmeliydi")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func mustJSON(a Announce) []byte {
	b, _ := json.Marshal(a)
	return b
}
