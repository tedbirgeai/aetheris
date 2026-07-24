package qos

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestICMPEncodeChecksumValid(t *testing.T) {
	pkt := encodeICMPEcho(0x1234, 7, []byte("aetheris-qos"))
	// Checksum alani dahil tum paketin saglama toplami 0 olmali.
	if checksum(pkt) != 0 {
		t.Fatalf("gecerli paketin checksum dogrulamasi 0 olmali, %d", checksum(pkt))
	}
	id, seq, ok := parseICMPEchoReply(append([]byte{icmpTypeEchoReply, 0, pkt[2], pkt[3]}, pkt[4:]...))
	if !ok || id != 0x1234 || seq != 7 {
		t.Fatalf("echo reply cozumu yanlis: id=%d seq=%d ok=%v", id, seq, ok)
	}
}

func TestParseRejectsNonEchoReply(t *testing.T) {
	// Tur 3 (destination unreachable) echo reply DEGILDIR.
	if _, _, ok := parseICMPEchoReply([]byte{3, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("echo-olmayan mesaj kabul edilmemeli")
	}
	if _, _, ok := parseICMPEchoReply([]byte{0, 0}); ok {
		t.Fatal("kisa mesaj kabul edilmemeli")
	}
}

func TestProbeICMPHonestFallbackWhenNoPrivilege(t *testing.T) {
	p := NewRawProber()
	if p.ICMPAvailable() {
		// Yetki VAR: 127.0.0.1'e gercek ping at, gercek RTT beklenir.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res, err := p.ProbeICMP(ctx, "127.0.0.1")
		if err != nil {
			t.Fatalf("yetki varken loopback ping hata verdi: %v", err)
		}
		if res.Type != ProbeICMPEcho {
			t.Fatalf("etiket icmp_echo olmali, %q", res.Type)
		}
		if res.Recv == 1 && res.RTT <= 0 {
			t.Fatal("basarili yanitta RTT pozitif olmali")
		}
		t.Logf("gercek ICMP RTT (loopback): %v", res.RTT)
		return
	}

	// Yetki YOK: dururst fallback dogrula. Sahte metrik URETILMEMELI.
	ctx := context.Background()
	res, err := p.ProbeICMP(ctx, "127.0.0.1")
	if !errors.Is(err, ErrNoRawSocket) {
		t.Fatalf("yetkisizken ErrNoRawSocket beklenir, gelen: %v", err)
	}
	if res.Type != ProbeHTTPFallback {
		t.Fatalf("yetkisizken probe_type http_fallback olmali, %q", res.Type)
	}
	if res.RTT != 0 {
		t.Fatal("fallback'te sahte RTT uretilmemeli (0 olmali)")
	}
}

func TestProbeUDPJitterAgainstEcho(t *testing.T) {
	echo, addr, err := StartUDPEcho("127.0.0.1:0")
	if err != nil {
		t.Skipf("UDP echo kurulamadi (ortam kisiti): %v", err)
	}
	defer echo.Close()

	p := NewRawProber()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := p.ProbeUDPJitter(ctx, addr, 8)
	if err != nil {
		t.Fatalf("ProbeUDPJitter: %v", err)
	}
	if res.Type != ProbeUDPJitter {
		t.Fatalf("etiket udp_jitter olmali, %q", res.Type)
	}
	if res.Sent != 8 {
		t.Fatalf("8 gonderim beklenir, %d", res.Sent)
	}
	if res.Recv == 0 {
		t.Fatal("echo sunucusundan en az bir yanit beklenir")
	}
	if res.LossRatio < 0 || res.LossRatio > 1 {
		t.Fatalf("loss_ratio [0,1] disinda: %f", res.LossRatio)
	}
	t.Logf("UDP jitter olcumu: rtt=%v jitter=%v recv=%d/%d loss=%.2f",
		res.RTT, res.Jitter, res.Recv, res.Sent, res.LossRatio)
}

func TestProbeUDPJitterLossOnDeadTarget(t *testing.T) {
	p := NewRawProber()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Kimsenin dinlemedigi bir port: yanit gelmez, kayip yuksek olmali.
	res, _ := p.ProbeUDPJitter(ctx, "127.0.0.1:1", 3)
	if res.Recv != 0 {
		t.Skip("beklenmedik sekilde yanit alindi (ortam bagimli), atlaniyor")
	}
	if res.LossRatio != 1 {
		t.Fatalf("olu hedefte kayip 1.0 olmali, %f", res.LossRatio)
	}
}
