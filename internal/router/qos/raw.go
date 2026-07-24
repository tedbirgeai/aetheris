// Package qos, GERCEK ag katmani olcumleri saglar: ham ICMP echo ve UDP
// jitter. Bu, internal/router icindeki HTTP-yoklamali QoS'un tamamlayicisidir.
//
// # DURUSTLUK SOZLESMESI (Faz 2)
//
// internal/router/qos.go bilincli olarak "paket kaybi" iddia etmez; cunku
// HTTP yoklamasi uygulama katmanindadir. Bu paket, ham soket yetkisi
// (Linux'ta CAP_NET_RAW) VARSA gercek ICMP Echo ve UDP jitter olcer ve
// probe_type="icmp_echo" / "udp_jitter" etiketler.
//
// Yetki YOKSA sistem YALAN METRIK URETMEZ. Ham soket acilamazsa Prober,
// probe_type="http_fallback" etiketiyle acikca fallback moduna gecer ve
// cagirani mevcut HTTP prober'a yonlendirir. Sahte bir "0ms ICMP RTT"
// asla raporlanmaz.
//
// # NEDEN AYRI PAKET
//
// Ham soket kodu, konteynerde varsayilan olarak CALISMAZ (yetki yok).
// Ayri paket olmasi, testlerin fallback yolunu yetkiden BAGIMSIZ
// dogrulamasini ve gercek ICMP testlerinin yetki oldugunda kosmasini
// saglar.
package qos

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// ProbeType, bir olcumun HANGI yontemle alindigini dururstce etiketler.
type ProbeType string

const (
	// ProbeICMPEcho, gercek ICMP Echo (ham soket, CAP_NET_RAW gerektirir).
	ProbeICMPEcho ProbeType = "icmp_echo"
	// ProbeUDPJitter, gercek UDP gidis-donus jitter olcumu.
	ProbeUDPJitter ProbeType = "udp_jitter"
	// ProbeHTTPFallback, ham soket yetkisi yokken dururst geri cekilme.
	ProbeHTTPFallback ProbeType = "http_fallback"
)

// ErrNoRawSocket, ham soket acilamadiginda (yetki yok) doner. Cagiran taraf
// bunu gorunce HTTP prober'a gecmelidir.
var ErrNoRawSocket = errors.New("qos: ham soket yetkisi yok (CAP_NET_RAW gerekli)")

// Result, tek bir olcumun sonucudur.
type Result struct {
	Type   ProbeType     `json:"probe_type"`
	Target string        `json:"target"`
	RTT    time.Duration `json:"rtt"`
	Jitter time.Duration `json:"jitter,omitempty"`
	Sent   int           `json:"sent"`
	Recv   int           `json:"recv"`
	// LossRatio, GERCEK paket kaybi oranidir [0,1] — yalnizca icmp_echo ve
	// udp_jitter icin anlamlidir. http_fallback'te 0 birakilir cunku HTTP
	// yoklamasi paket kaybi olcemez (bkz. dururstluk notu).
	LossRatio float64 `json:"loss_ratio"`
}

// RawProber, ham ICMP/UDP olcumlerini yurutur.
type RawProber struct {
	// icmpAvailable, kurulusta yapilan yetki denemesinin sonucu.
	icmpAvailable bool
	seq           atomic.Uint32
	id            int
}

// NewRawProber, ham soket yetkisini BIR KEZ dener ve sonucu saklar.
// Yetki yoksa hata DONMEZ; Prober fallback moduna hazir olur. Boylece
// yetkisiz ortamda da sorunsuz kurulur; dururstluk ProbeICMP cagrisinda
// probe_type ile saglanir.
func NewRawProber() *RawProber {
	p := &RawProber{id: os.Getpid() & 0xffff}
	// Yetki testi: ham ICMP soketi acilabiliyor mu?
	if c, err := net.ListenPacket("ip4:icmp", "0.0.0.0"); err == nil {
		_ = c.Close()
		p.icmpAvailable = true
	}
	return p
}

// ICMPAvailable, gercek ICMP olcumunun mumkun olup olmadigini bildirir.
func (p *RawProber) ICMPAvailable() bool { return p.icmpAvailable }

// ProbeICMP, hedefe tek bir ICMP Echo gonderir ve RTT olcer.
//
// Yetki yoksa: RTT=0, Type=http_fallback ve ErrNoRawSocket doner. Cagiran
// bunu gorunce HTTP prober kullanmalidir. HICBIR sahte metrik uretilmez.
func (p *RawProber) ProbeICMP(ctx context.Context, target string) (Result, error) {
	if !p.icmpAvailable {
		return Result{Type: ProbeHTTPFallback, Target: target}, ErrNoRawSocket
	}

	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		// Kurulusta acilabiliyordu ama simdi acilamiyor: yine de dururst
		// fallback dondur, yalan metrik verme.
		return Result{Type: ProbeHTTPFallback, Target: target}, fmt.Errorf("%w: %v", ErrNoRawSocket, err)
	}
	defer conn.Close()

	dst, err := net.ResolveIPAddr("ip4", target)
	if err != nil {
		return Result{Type: ProbeICMPEcho, Target: target}, fmt.Errorf("qos: hedef cozulemedi: %w", err)
	}

	seq := int(p.seq.Add(1) & 0xffff)
	req := encodeICMPEcho(p.id, seq, []byte("aetheris-qos"))

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	}

	start := time.Now()
	if _, err := conn.WriteTo(req, dst); err != nil {
		return Result{Type: ProbeICMPEcho, Target: target, Sent: 1}, fmt.Errorf("qos: ICMP gonderilemedi: %w", err)
	}

	reply := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(reply)
		if err != nil {
			// Zaman asimi = paket kaybi. Dururst: kayip say, RTT verme.
			return Result{Type: ProbeICMPEcho, Target: target, Sent: 1, Recv: 0, LossRatio: 1}, nil
		}
		rtt := time.Since(start)
		gotID, gotSeq, ok := parseICMPEchoReply(reply[:n])
		if !ok || gotID != p.id || gotSeq != seq {
			// Baska bir ICMP paketi (baska bir ping'in yaniti olabilir);
			// dogru seq gelene kadar okumaya devam et.
			if ctx.Err() != nil {
				return Result{Type: ProbeICMPEcho, Target: target, Sent: 1, LossRatio: 1}, nil
			}
			continue
		}
		_ = peer
		return Result{
			Type:      ProbeICMPEcho,
			Target:    target,
			RTT:       rtt,
			Sent:      1,
			Recv:      1,
			LossRatio: 0,
		}, nil
	}
}

// icmpEchoRequest tur/kod sabitleri (IPv4).
const (
	icmpTypeEchoRequest = 8
	icmpTypeEchoReply   = 0
)

// encodeICMPEcho, bir ICMP Echo Request paketi olusturur (checksum dahil).
func encodeICMPEcho(id, seq int, payload []byte) []byte {
	pkt := make([]byte, 8+len(payload))
	pkt[0] = icmpTypeEchoRequest
	pkt[1] = 0 // kod
	// [2:4] checksum sonra doldurulur
	binary.BigEndian.PutUint16(pkt[4:6], uint16(id))
	binary.BigEndian.PutUint16(pkt[6:8], uint16(seq))
	copy(pkt[8:], payload)

	cs := checksum(pkt)
	binary.BigEndian.PutUint16(pkt[2:4], cs)
	return pkt
}

// parseICMPEchoReply, gelen ICMP mesajini cozer. net paketi ip4:icmp'te
// IP basligini SIYIRIR; gelen baytlar dogrudan ICMP mesajidir.
func parseICMPEchoReply(msg []byte) (id, seq int, ok bool) {
	if len(msg) < 8 {
		return 0, 0, false
	}
	if msg[0] != icmpTypeEchoReply {
		return 0, 0, false
	}
	id = int(binary.BigEndian.Uint16(msg[4:6]))
	seq = int(binary.BigEndian.Uint16(msg[6:8]))
	return id, seq, true
}

// checksum, RFC 1071 internet saglama toplami.
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return uint16(^sum)
}
