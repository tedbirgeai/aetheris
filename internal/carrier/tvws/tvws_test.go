package tvws

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestChannelScan(t *testing.T) {
	spectrum := NewSharedSpectrum()
	db := NewMockSpectrumDB(0.30)
	d := NewMockTVWS(DefaultConfig("node-A"), spectrum, db, nil)
	ctx := context.Background()
	if err := d.Open(ctx); err != nil {
		t.Fatal(err)
	}
	channels, err := d.ScanChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != ChannelCount {
		t.Fatalf("%d kanal beklenir: %d", ChannelCount, len(channels))
	}
	// Boş kanal sayısı >0 olmalı.
	free := 0
	for _, ch := range channels {
		if ch.State == ChannelFree {
			free++
		}
	}
	if free == 0 {
		t.Fatal("en az bir boş kanal olmalı")
	}
	t.Logf("toplam %d kanal, %d boş, %d birincil", ChannelCount, free, ChannelCount-free)
}

func TestChannelSelect(t *testing.T) {
	spectrum := NewSharedSpectrum()
	db := NewMockSpectrumDB(0.20) // %20 meşgul
	d := NewMockTVWS(DefaultConfig("node-A"), spectrum, db, nil)
	ctx := context.Background()
	_ = d.Open(ctx)
	ch, err := d.SelectChannel(ctx)
	if err != nil {
		t.Fatalf("kanal seçimi başarısız: %v", err)
	}
	if ch.State != ChannelFree {
		t.Fatal("seçilen kanal boş olmalı")
	}
	if d.ActiveChannel() != ch.Index {
		t.Fatal("aktif kanal güncellenmeli")
	}
	t.Logf("seçilen kanal: %d (%d MHz), RSSI: %.1f dBm", ch.Index, ch.FreqMHz, ch.RSSI)
}

func TestSendReceive(t *testing.T) {
	spectrum := NewSharedSpectrum()
	db := NewMockSpectrumDB(0.0) // tüm kanallar boş
	a := NewMockTVWS(DefaultConfig("node-A"), spectrum, db, nil)
	b := NewMockTVWS(DefaultConfig("node-B"), spectrum, db, nil)
	ctx := context.Background()
	_ = a.Open(ctx)
	_ = b.Open(ctx)
	ch, _ := a.SelectChannel(ctx)
	// B aynı kanala geç
	b.mu.Lock()
	b.activeCh = ch.Index
	b.mu.Unlock()

	payload := []byte("TVWS 470-790MHz Super-WiFi testi")
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

func TestMTUEnforcement(t *testing.T) {
	d := NewMockTVWS(DefaultConfig("a"), nil, NewMockSpectrumDB(0), nil)
	_ = d.Open(context.Background())
	d.mu.Lock()
	d.activeCh = 0
	d.mu.Unlock()
	if err := d.Send(context.Background(), Frame{Payload: make([]byte, MTU+1)}); err != ErrFrameTooLarge {
		t.Fatalf("MTU aşımı reddedilmeli: %v", err)
	}
}

func TestBandwidthValues(t *testing.T) {
	if BandwidthMbps("BPSK") >= BandwidthMbps("64QAM") {
		t.Fatal("yüksek modülasyon daha fazla bant genişliği vermeli")
	}
	if BandwidthMbps("64QAM") != 18.0 {
		t.Fatalf("64QAM 18 Mbps olmalı: %.1f", BandwidthMbps("64QAM"))
	}
}

func TestSpectrumDB(t *testing.T) {
	db := NewMockSpectrumDB(0.0) // hepsi boş
	free := db.FreeChannels()
	if len(free) != ChannelCount {
		t.Fatalf("tümü boş: %d", len(free))
	}
	db2 := NewMockSpectrumDB(1.0) // hepsi meşgul
	if len(db2.FreeChannels()) != 0 {
		t.Fatal("hepsi meşgul olmalı")
	}
}

func TestFreqCalculation(t *testing.T) {
	if ChanFreq(0) != FreqMinMHz {
		t.Fatalf("kanal 0 = %d MHz olmalı", FreqMinMHz)
	}
	if ChanFreq(1) != FreqMinMHz+ChannelBWMHz {
		t.Fatalf("kanal 1 = %d MHz olmalı", FreqMinMHz+ChannelBWMHz)
	}
}
