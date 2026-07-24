package gossip

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("zaman asimi: %s", msg)
}

func TestSetAddIdempotent(t *testing.T) {
	s := NewSet()
	r := NewRecord([]byte("ayni veri"))
	if !s.Add(r) {
		t.Fatal("ilk ekleme yeni olmali")
	}
	if s.Add(r) {
		t.Fatal("ayni ID ikinci kez eklenmemeli")
	}
	if s.Len() != 1 {
		t.Fatalf("uzunluk 1 olmali, %d", s.Len())
	}
	// Ayni icerik ayni ID uretir.
	if NewRecord([]byte("ayni veri")).ID != r.ID {
		t.Fatal("icerik-adresli ID deterministik olmali")
	}
}

func TestTwoNodesConverge(t *testing.T) {
	sw := NewMemSwitch()
	reg := NewMemRegistry()

	a := NewNode(sw.Transport("A"), Config{ID: "A", Beacon: reg.Beacon(), GossipInterval: 30 * time.Millisecond, Seed: 1})
	b := NewNode(sw.Transport("B"), Config{ID: "B", Beacon: reg.Beacon(), GossipInterval: 30 * time.Millisecond, Seed: 2})

	// A'ya 50, B'ye baska 50 kayit koy; kesisim yok.
	for i := 0; i < 50; i++ {
		a.Set().Add(NewRecord([]byte(fmt.Sprintf("A-%d", i))))
		b.Set().Add(NewRecord([]byte(fmt.Sprintf("B-%d", i))))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	go b.Run(ctx)
	defer a.Close()
	defer b.Close()

	waitFor(t, 5*time.Second, func() bool {
		return a.Set().Len() == 100 && b.Set().Len() == 100
	}, "iki dugum 100 kayda yakinsamali")
}

func TestDiscoveryFindsPeers(t *testing.T) {
	sw := NewMemSwitch()
	reg := NewMemRegistry()
	nodes := make([]*Node, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range nodes {
		id := fmt.Sprintf("N%d", i)
		nodes[i] = NewNode(sw.Transport(id), Config{ID: id, Beacon: reg.Beacon(), GossipInterval: 30 * time.Millisecond, Seed: int64(i + 1)})
		go nodes[i].Run(ctx)
	}
	defer func() {
		for _, n := range nodes {
			n.Close()
		}
	}()

	// Her dugum diger 3'u kesfetmeli (merkezi sunucu YOK).
	waitFor(t, 5*time.Second, func() bool {
		for _, n := range nodes {
			if n.Peers() < 3 {
				return false
			}
		}
		return true
	}, "her dugum 3 komsu kesfetmeli")
}

func TestPartitionThenHealConverges(t *testing.T) {
	sw := NewMemSwitch()
	reg := NewMemRegistry()

	a := NewNode(sw.Transport("A"), Config{ID: "A", Beacon: reg.Beacon(), GossipInterval: 25 * time.Millisecond, Seed: 1})
	b := NewNode(sw.Transport("B"), Config{ID: "B", Beacon: reg.Beacon(), GossipInterval: 25 * time.Millisecond, Seed: 2})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	go b.Run(ctx)
	defer a.Close()
	defer b.Close()

	// Once komsu kesfini bekle.
	waitFor(t, 3*time.Second, func() bool { return a.Peers() >= 1 && b.Peers() >= 1 }, "kesif")

	// BOL: A<->B iletisimini cift yonlu kes.
	sw.PartitionPair("A", "B")

	// Bolunme sirasinda her iki tarafa da kayit yaz.
	for i := 0; i < 20; i++ {
		a.Set().Add(NewRecord([]byte(fmt.Sprintf("partA-%d", i))))
		b.Set().Add(NewRecord([]byte(fmt.Sprintf("partB-%d", i))))
	}
	time.Sleep(300 * time.Millisecond)
	// Bolunme sirasinda yakinsama OLMAMALI.
	if a.Set().Len() == 40 || b.Set().Len() == 40 {
		t.Fatal("bolunme sirasinda senkronize olmamaliydi")
	}

	// IYILESTIR.
	sw.HealPair("A", "B")

	// Simdi sifir kayipla birlesmeli.
	waitFor(t, 5*time.Second, func() bool {
		return a.Set().Len() == 40 && b.Set().Len() == 40
	}, "iyilesme sonrasi 40 kayda yakinsamali (sifir kayip)")
}

func TestUDPTransportLoopback(t *testing.T) {
	ta, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Skipf("UDP tasima kurulamadi (ortam kisiti): %v", err)
	}
	defer ta.Close()
	tb, err := NewUDPTransport("127.0.0.1:0")
	if err != nil {
		t.Skipf("UDP tasima kurulamadi: %v", err)
	}
	defer tb.Close()

	msg := Message{Kind: KindPush, From: "A", Recs: []Record{NewRecord([]byte("udp yuku"))}}
	if err := ta.Send(tb.Addr(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case env := <-tb.Inbox():
		if env.Msg.From != "A" || len(env.Msg.Recs) != 1 {
			t.Fatalf("beklenmeyen mesaj: %+v", env.Msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UDP mesaji zamaninda gelmedi")
	}
}
