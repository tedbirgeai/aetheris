package qos

import (
	"context"
	"math"
	"net"
	"time"
)

// ProbeUDPJitter, hedefteki bir UDP echo ucuna 'count' datagram gonderir,
// her birinin gidis-donus suresini olcer ve jitter (ardisik RTT farklarinin
// ortalama mutlak degeri) ile GERCEK paket kaybini hesaplar.
//
// UDP echo yetkisiz calisir (ham soket gerektirmez); bu yuzden ICMP'nin
// aksine CAP_NET_RAW olmayan konteynerde de gercek olcum verir. Etiket:
// probe_type="udp_jitter".
//
// Hedefin datagram'lari geri yansitan bir echo ucu olmasi beklenir
// (RFC 862 echo veya kendi echo servisimiz — StartUDPEcho'ya bakin).
func (p *RawProber) ProbeUDPJitter(ctx context.Context, target string, count int) (Result, error) {
	if count <= 0 {
		count = 5
	}
	raddr, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		return Result{Type: ProbeUDPJitter, Target: target}, err
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return Result{Type: ProbeUDPJitter, Target: target}, err
	}
	defer conn.Close()

	perProbeTimeout := 500 * time.Millisecond
	rtts := make([]time.Duration, 0, count)
	sent, recv := 0, 0
	buf := make([]byte, 64)

	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			break
		}
		payload := []byte{byte(i), 'q', 'o', 's'}
		_ = conn.SetWriteDeadline(time.Now().Add(perProbeTimeout))
		start := time.Now()
		if _, err := conn.Write(payload); err != nil {
			sent++
			continue // gonderim hatasi = kayip
		}
		sent++

		_ = conn.SetReadDeadline(time.Now().Add(perProbeTimeout))
		if _, err := conn.Read(buf); err != nil {
			continue // zaman asimi = kayip; RTT eklenmez
		}
		recv++
		rtts = append(rtts, time.Since(start))
	}

	res := Result{Type: ProbeUDPJitter, Target: target, Sent: sent, Recv: recv}
	if sent > 0 {
		res.LossRatio = float64(sent-recv) / float64(sent)
	}
	if len(rtts) > 0 {
		var sum time.Duration
		for _, r := range rtts {
			sum += r
		}
		res.RTT = sum / time.Duration(len(rtts))
	}
	if len(rtts) > 1 {
		var diffSum float64
		for i := 1; i < len(rtts); i++ {
			diffSum += math.Abs(float64(rtts[i] - rtts[i-1]))
		}
		res.Jitter = time.Duration(diffSum / float64(len(rtts)-1))
	}
	return res, nil
}

// UDPEcho, test ve saha kullanimi icin basit bir UDP echo sunucusudur.
// Aldigi her datagram'i geldigi adrese geri yansitir.
type UDPEcho struct {
	conn *net.UDPConn
	done chan struct{}
}

// StartUDPEcho, verilen adreste (ornegin "127.0.0.1:0") bir echo sunucu
// baslatir ve dinlenen adresi dondurur.
func StartUDPEcho(listen string) (*UDPEcho, string, error) {
	addr, err := net.ResolveUDPAddr("udp4", listen)
	if err != nil {
		return nil, "", err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, "", err
	}
	e := &UDPEcho{conn: conn, done: make(chan struct{})}
	go e.loop()
	return e, conn.LocalAddr().String(), nil
}

func (e *UDPEcho) loop() {
	buf := make([]byte, 2048)
	for {
		select {
		case <-e.done:
			return
		default:
		}
		_ = e.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, from, err := e.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		_, _ = e.conn.WriteToUDP(buf[:n], from)
	}
}

// Close, echo sunucusunu durdurur.
func (e *UDPEcho) Close() error {
	close(e.done)
	return e.conn.Close()
}
