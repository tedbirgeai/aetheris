package mesh

import (
	"bytes"
	"sync"
	"testing"
)

// TestShortestPathDirect, dogrudan komsu icin tek-hop yol.
func TestShortestPathDirect(t *testing.T) {
	g := NewGraph()
	_ = g.AddLink("A", "B", 10, CarrierEthernet)
	res, err := g.ShortestPath("A", "B")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hops) != 2 || res.Hops[0] != "A" || res.Hops[1] != "B" {
		t.Fatalf("beklenen [A B], gelen %v", res.Hops)
	}
}

// TestMultiHopViaB, A-C dogrudan baglantisi YOKKEN yolun B uzerinden
// gectigini dogrular (kabul kriteri 3'un topoloji tarafi).
func TestMultiHopViaB(t *testing.T) {
	g := NewGraph()
	g.AddLink("A", "B", 10, CarrierEthernet)
	g.AddLink("B", "C", 10, CarrierEthernet)
	// A-C yok.
	res, err := g.ShortestPath("A", "C")
	if err != nil {
		t.Fatalf("yol bulunmaliydi: %v", err)
	}
	want := []string{"A", "B", "C"}
	if len(res.Hops) != 3 {
		t.Fatalf("3 hop beklenir, gelen %v", res.Hops)
	}
	for i := range want {
		if res.Hops[i] != want[i] {
			t.Fatalf("yol %v olmali, gelen %v", want, res.Hops)
		}
	}
	next, _ := g.NextHop("A", "C")
	if next != "B" {
		t.Fatalf("A'nin C icin sonraki hop'u B olmali, %q", next)
	}
}

// TestPrefersLowerCost, dogrudan LoRa (pahali) yerine iki-hop Ethernet (ucuz)
// yolun secildigini dogrular: tasiyici agirligi yonlendirmeyi etkiler.
func TestPrefersLowerCost(t *testing.T) {
	g := NewGraph()
	// Dogrudan A-C ama LoRa: 30ms * 4.0 = 120 maliyet.
	g.AddLink("A", "C", 30, CarrierLoRa)
	// A-B-C Ethernet: (10*1)+(10*1) = 20 maliyet.
	g.AddLink("A", "B", 10, CarrierEthernet)
	g.AddLink("B", "C", 10, CarrierEthernet)

	res, err := g.ShortestPath("A", "C")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hops) != 3 || res.Hops[1] != "B" {
		t.Fatalf("dusuk maliyetli B yolu secilmeliydi, gelen %v (maliyet %.1f)", res.Hops, res.Cost)
	}
}

// TestNoRoute, ayrik dugume yol olmadigini dogrular.
func TestNoRoute(t *testing.T) {
	g := NewGraph()
	g.AddLink("A", "B", 10, CarrierEthernet)
	if _, err := g.ShortestPath("A", "Z"); err != ErrNoRoute {
		t.Fatalf("ErrNoRoute beklenir, %v", err)
	}
}

// TestThreeNodeForwardingLossless, KABUL KRITERI 3: 3 dugumlu gercek
// forwarding'de A'dan gonderilen paket, A-C dogrudan baglantisi olmadan
// B uzerinden C'ye KAYIPSIZ ulasir.
func TestThreeNodeForwardingLossless(t *testing.T) {
	g := NewGraph()
	g.AddLink("A", "B", 10, CarrierWiFi)
	g.AddLink("B", "C", 10, CarrierEthernet)

	A := NewRouter("A")
	B := NewRouter("B")
	C := NewRouter("C")
	for _, r := range []*Router{A, B, C} {
		r.SetGraph(g)
	}
	// Fiziksel baglantilar: A-B ve B-C (A-C YOK).
	Wire(A, B)
	Wire(B, C)

	var (
		mu       sync.Mutex
		received [][]byte
	)
	C.OnDeliver(func(p Packet) {
		mu.Lock()
		received = append(received, append([]byte(nil), p.Payload...))
		mu.Unlock()
	})

	payload := []byte("cok-sicramali off-grid paket: A -> B -> C")
	if err := A.Send("C", "pkt-1", payload); err != nil {
		t.Fatalf("gonderim hatasi: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("C tam olarak 1 paket almaliydi, aldi %d", len(received))
	}
	if !bytes.Equal(received[0], payload) {
		t.Fatal("payload bozuldu (kayipli aktarim)")
	}
	// B gercekten aktarmis olmali (forwarded=1).
	if B.Stats().Forwarded != 1 {
		t.Fatalf("B 1 paket aktarmaliydi, %d", B.Stats().Forwarded)
	}
	// C teslim almis olmali.
	if C.Stats().Delivered != 1 {
		t.Fatalf("C 1 paket teslim almaliydi, %d", C.Stats().Delivered)
	}
}

// TestTTLPreventsInfinite, TTL=0 paketin dusuruldugunu dogrular.
func TestTTLPreventsInfinite(t *testing.T) {
	g := NewGraph()
	g.AddLink("A", "B", 10, CarrierEthernet)
	g.AddLink("B", "C", 10, CarrierEthernet)
	A := NewRouter("A")
	B := NewRouter("B")
	C := NewRouter("C")
	for _, r := range []*Router{A, B, C} {
		r.SetGraph(g)
	}
	Wire(A, B)
	Wire(B, C)

	// TTL=1 ile A'dan C'ye: A->B (TTL 1->0), B->C denemesi TTL 0 -> duser.
	p := Packet{ID: "x", Src: "A", Dst: "C", TTL: 1}
	err := A.Deliver(p)
	if err != ErrTTLExceeded {
		t.Fatalf("TTL tukenmesi beklenir, %v", err)
	}
}

// TestLoopPrevention, yolda tekrar gorunen dugumun paketi dusurdugunu
// dogrular.
func TestLoopPrevention(t *testing.T) {
	B := NewRouter("B")
	g := NewGraph()
	g.AddLink("B", "C", 10, CarrierEthernet)
	B.SetGraph(g)

	// Path zaten B iceriyor: dongu.
	p := Packet{ID: "y", Src: "A", Dst: "C", TTL: 10, Path: []string{"A", "B"}}
	if err := B.Deliver(p); err != ErrLoop {
		t.Fatalf("dongu tespiti beklenir, %v", err)
	}
}

// TestFourNodeReroute, bir link koptugunda alternatif yolun secildigini
// dogrular (dinamik yeniden yonlendirme).
func TestFourNodeReroute(t *testing.T) {
	// A-B-D ve A-C-D iki ayri yol. B-D daha pahali olursa C uzerinden gider.
	g := NewGraph()
	g.AddLink("A", "B", 5, CarrierEthernet)
	g.AddLink("B", "D", 50, CarrierLoRa) // pahali
	g.AddLink("A", "C", 5, CarrierEthernet)
	g.AddLink("C", "D", 5, CarrierEthernet) // ucuz

	res, err := g.ShortestPath("A", "D")
	if err != nil {
		t.Fatal(err)
	}
	if res.Hops[1] != "C" {
		t.Fatalf("ucuz C yolu secilmeliydi, gelen %v", res.Hops)
	}
}
