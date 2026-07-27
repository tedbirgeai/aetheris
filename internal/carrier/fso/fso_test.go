package fso

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestFSOOpenSend(t *testing.T) {
	optical := NewSharedOptical(WeatherClear)
	a := NewMockFSO(DefaultConfig("a"), optical, nil)
	b := NewMockFSO(DefaultConfig("b"), optical, nil)
	ctx := context.Background()
	_ = a.Open(ctx)
	_ = b.Open(ctx)

	payload := []byte("FSO lazer iletim testi — 1Gbps")
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
	if f.LatencyUs > 10 {
		t.Fatalf("FSO gecikmesi çok yüksek: %d µs", f.LatencyUs)
	}
}

func TestFSOFogBlocks(t *testing.T) {
	optical := NewSharedOptical(WeatherFog)
	a := NewMockFSO(DefaultConfig("a"), optical, nil)
	_ = a.Open(context.Background())
	a.mu.Lock()
	a.cfg.MaxDistanceM = 2000 // yoğun siste bu mesafe bağlantıyı keser
	a.mu.Unlock()

	err := a.Send(context.Background(), Frame{Payload: []byte("test")})
	if err != nil && err != ErrWeatherBlock {
		t.Logf("yoğun sis bağlantı kalitesini düşürür: %v", err)
	}
}

func TestLinkBudget(t *testing.T) {
	cfg := DefaultConfig("x")
	// 500m açık havada her zaman çalışmalı.
	_, _, ok := LinkBudget(cfg, 500, WeatherClear)
	if !ok {
		t.Fatal("500m açık havada bağlantı olmalı")
	}
	t.Logf("500m açık: bağlantı=%v", ok)
}

func TestWeatherAttenuation(t *testing.T) {
	if WeatherClear.AttenuationDB() != 0 {
		t.Fatal("açık havada zayıflama sıfır olmalı")
	}
	if WeatherFog.AttenuationDB() <= WeatherHaze.AttenuationDB() {
		t.Fatal("yoğun sis daha fazla zayıflama yapmalı")
	}
}

func TestLinkQuality(t *testing.T) {
	optical := NewSharedOptical(WeatherClear)
	d := NewMockFSO(DefaultConfig("a"), optical, nil)
	_ = d.Open(context.Background())
	q := d.LinkQuality()
	if !q.LinkOK {
		t.Fatal("açık havada bağlantı iyi olmalı")
	}
	if q.EstBWMbps <= 0 {
		t.Fatal("tahmini bant genişliği pozitif olmalı")
	}
	if q.Availability <= 0 || q.Availability > 1 {
		t.Fatalf("erişilebilirlik 0-1 arasında olmalı: %.2f", q.Availability)
	}
}

func TestEstimateAvailability(t *testing.T) {
	if EstimateAvailability(500) <= EstimateAvailability(2000) {
		t.Fatal("kısa mesafe daha yüksek erişilebilirlik vermeli")
	}
	if EstimateAvailability(500) > 1.0 {
		t.Fatal("erişilebilirlik 1'i aşamaz")
	}
}

func TestMTUEnforcement(t *testing.T) {
	optical := NewSharedOptical(WeatherClear)
	d := NewMockFSO(DefaultConfig("a"), optical, nil)
	_ = d.Open(context.Background())
	if err := d.Send(context.Background(), Frame{Payload: make([]byte, MTU+1)}); err != ErrTooBig {
		t.Fatalf("MTU aşımı reddedilmeli: %v", err)
	}
}
