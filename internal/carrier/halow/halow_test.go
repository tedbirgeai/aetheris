package halow

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestMockHaLowSendReceive(t *testing.T) {
	medium := NewSharedMedium()
	a := NewMockHaLow(DefaultConfig("node-A"), medium)
	b := NewMockHaLow(DefaultConfig("node-B"), medium)

	ctx := context.Background()
	if err := a.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Open(ctx); err != nil {
		t.Fatal(err)
	}

	payload := []byte("HaLow 863MHz off-grid test mesaji")
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
	if f.RSSI == 0 {
		t.Fatal("RSSI set edilmiş olmalı")
	}
}

func TestMockHaLowStats(t *testing.T) {
	medium := NewSharedMedium()
	a := NewMockHaLow(DefaultConfig("a"), medium)
	b := NewMockHaLow(DefaultConfig("b"), medium)
	ctx := context.Background()
	_ = a.Open(ctx)
	_ = b.Open(ctx)

	_ = a.Send(ctx, Frame{Payload: []byte("x")})
	_ = a.Send(ctx, Frame{Payload: []byte("y")})

	rctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, _ = b.Receive(rctx)
	_, _ = b.Receive(rctx)

	if a.Stats().TX != 2 {
		t.Fatalf("a TX 2 olmalı: %d", a.Stats().TX)
	}
	if b.Stats().RX != 2 {
		t.Fatalf("b RX 2 olmalı: %d", b.Stats().RX)
	}
}

func TestMTUEnforcement(t *testing.T) {
	medium := NewSharedMedium()
	a := NewMockHaLow(DefaultConfig("a"), medium)
	ctx := context.Background()
	_ = a.Open(ctx)

	big := make([]byte, MTU+1)
	if err := a.Send(ctx, Frame{Payload: big}); err != ErrFrameTooLarge {
		t.Fatalf("MTU aşımı reddedilmeli: %v", err)
	}
}

func TestOpenHALReturnsMock(t *testing.T) {
	drv, isHW := OpenHAL(DefaultConfig("x"), nil)
	if isHW {
		t.Fatal("donanım yokken mock dönmeli")
	}
	if drv == nil {
		t.Fatal("sürücü nil olamaz")
	}
	if !drv.Available() {
		t.Fatal("mock her zaman available olmalı")
	}
}

func TestBandwidthAndRange(t *testing.T) {
	if BandwidthKbps(MCS0) != 150 {
		t.Fatalf("MCS0 150kbps olmalı: %d", BandwidthKbps(MCS0))
	}
	if RangeMeter(MCS0) != 2000 {
		t.Fatalf("MCS0 2000m olmalı: %d", RangeMeter(MCS0))
	}
	if BandwidthKbps(MCS9) <= BandwidthKbps(MCS0) {
		t.Fatal("yüksek MCS daha hızlı olmalı")
	}
	if RangeMeter(MCS9) >= RangeMeter(MCS0) {
		t.Fatal("yüksek MCS daha kısa menzil olmalı")
	}
}

func TestNotOpenSendFails(t *testing.T) {
	medium := NewSharedMedium()
	a := NewMockHaLow(DefaultConfig("a"), medium)
	if err := a.Send(context.Background(), Frame{Payload: []byte("x")}); err != ErrNotOpen {
		t.Fatalf("açılmadan gönderim reddedilmeli: %v", err)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig("node-test")
	if cfg.Band != Band863MHz {
		t.Fatal("varsayılan band 863MHz olmalı")
	}
	if cfg.TxPowerDBm > 14 {
		t.Fatal("çıkış gücü BTK limitini aşmamalı (14dBm)")
	}
	if cfg.MCS != MCS0 {
		t.Fatal("varsayılan MCS0 (maksimum menzil) olmalı")
	}
}
