package wimax

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestWiGigSendReceive(t *testing.T) {
	medium := NewSharedMedium()
	a := NewMockWiGig(DefaultConfig("a"), medium)
	b := NewMockWiGig(DefaultConfig("b"), medium)
	ctx := context.Background()
	_ = a.Open(ctx)
	_ = b.Open(ctx)

	payload := []byte("WiGig 60GHz 2.31 Gbps testi")
	if err := a.Send(ctx, Frame{Payload: payload}); err != nil {
		t.Fatalf("gönderim hatası: %v", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	f, err := b.Receive(rctx)
	if err != nil {
		t.Fatalf("alım hatası: %v", err)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Fatal("payload bozulmuş")
	}
}

func TestWiGigDataRates(t *testing.T) {
	for mcs := 0; mcs <= 7; mcs++ {
		rate := MaxDataRateGbps(mcs)
		if rate <= 0 {
			t.Fatalf("MCS%d negatif hız: %.3f", mcs, rate)
		}
	}
	if MaxDataRateGbps(7) <= MaxDataRateGbps(0) {
		t.Fatal("yüksek MCS daha hızlı olmalı")
	}
}

func TestMeshNode(t *testing.T) {
	n := NewMeshNode("node-1", "aetheris-mesh")
	n.AddNeighbor("node-2")
	n.AddNeighbor("node-3")
	if n.NeighborCount() != 2 {
		t.Fatalf("2 komşu beklenir: %d", n.NeighborCount())
	}
}

func TestMTU(t *testing.T) {
	medium := NewSharedMedium()
	d := NewMockWiGig(DefaultConfig("a"), medium)
	_ = d.Open(context.Background())
	if err := d.Send(context.Background(), Frame{Payload: make([]byte, MTU+1)}); err != ErrFrameSize {
		t.Fatalf("MTU aşımı reddedilmeli: %v", err)
	}
}

func TestRangeSummary(t *testing.T) {
	s := RangeSummary(DefaultConfig("x"))
	if len(s) == 0 {
		t.Fatal("özet boş olmamalı")
	}
	t.Log(s)
}
