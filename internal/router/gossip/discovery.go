package gossip

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"
)

// PeerAnnounce, bir dugumun kesif duyurusudur.
type PeerAnnounce struct {
	NodeID string `json:"node_id"`
	Addr   string `json:"addr"` // gossip Transport adresi (UDP host:port)
}

// Beacon, komsu kesif sozlesmesidir. Yerel agda UDP broadcast, cevrimdisi
// senaryoda BLE/LoRa beaconing ayni arayuzu uygular; gossip mantigi
// altindaki tasiyiciyi bilmez.
type Beacon interface {
	// Announce, kendi varligini periyodik yayinlar (ctx iptaline kadar).
	Announce(ctx context.Context, self PeerAnnounce)
	// Discovered, kesfedilen komsularin okundugu kanal.
	Discovered() <-chan PeerAnnounce
	// Close, beacon'i kapatir.
	Close() error
}

// --- Bellek-ici beacon (test/simulator) ---

// MemRegistry, bellek-ici bir kesif veriyoludur: bir dugumun Announce'i
// kayitli tum dinleyicilere ulasir. Gercek broadcast'i taklit eder.
type MemRegistry struct {
	mu        sync.Mutex
	listeners map[*MemBeacon]struct{}
}

// NewMemRegistry, bos bir kesif veriyolu olusturur.
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{listeners: make(map[*MemBeacon]struct{})}
}

// Beacon, bu veriyoluna bagli yeni bir MemBeacon dondurur.
func (r *MemRegistry) Beacon() *MemBeacon {
	b := &MemBeacon{reg: r, out: make(chan PeerAnnounce, 64), done: make(chan struct{})}
	r.mu.Lock()
	r.listeners[b] = struct{}{}
	r.mu.Unlock()
	return b
}

func (r *MemRegistry) broadcast(src *MemBeacon, a PeerAnnounce) {
	r.mu.Lock()
	targets := make([]*MemBeacon, 0, len(r.listeners))
	for b := range r.listeners {
		if b != src {
			targets = append(targets, b)
		}
	}
	r.mu.Unlock()
	for _, b := range targets {
		select {
		case b.out <- a:
		default:
		}
	}
}

// MemBeacon, MemRegistry uzerinden calisan beacon'dir.
type MemBeacon struct {
	reg  *MemRegistry
	out  chan PeerAnnounce
	done chan struct{}
	once sync.Once
}

func (b *MemBeacon) Announce(ctx context.Context, self PeerAnnounce) {
	// Ilk duyuruyu hemen yap, sonra periyodik tekrarla.
	b.reg.broadcast(b, self)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		case <-ticker.C:
			b.reg.broadcast(b, self)
		}
	}
}

func (b *MemBeacon) Discovered() <-chan PeerAnnounce { return b.out }

func (b *MemBeacon) Close() error {
	b.once.Do(func() {
		close(b.done)
		b.reg.mu.Lock()
		delete(b.reg.listeners, b)
		b.reg.mu.Unlock()
	})
	return nil
}

// --- UDP broadcast beacon (gercek LAN) ---

// UDPBeacon, yerel agda UDP broadcast ile komsu kesfi yapar. Merkezi sunucu
// veya mDNS daemon'una ihtiyac duymaz; dogrudan 255.255.255.255:port'a yayin
// yapar ve ayni portu dinler.
type UDPBeacon struct {
	port int
	conn *net.UDPConn
	out  chan PeerAnnounce
	done chan struct{}
	once sync.Once
}

// NewUDPBeacon, verilen broadcast portunu dinlemeye baslar.
func NewUDPBeacon(port int) (*UDPBeacon, error) {
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: port}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return nil, err
	}
	b := &UDPBeacon{
		port: port,
		conn: conn,
		out:  make(chan PeerAnnounce, 64),
		done: make(chan struct{}),
	}
	go b.listenLoop()
	return b, nil
}

func (b *UDPBeacon) Announce(ctx context.Context, self PeerAnnounce) {
	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: b.port}
	payload, _ := json.Marshal(self)
	send := func() {
		_ = b.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = b.conn.WriteToUDP(payload, dst)
	}
	send()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.done:
			return
		case <-ticker.C:
			send()
		}
	}
}

func (b *UDPBeacon) Discovered() <-chan PeerAnnounce { return b.out }

func (b *UDPBeacon) listenLoop() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-b.done:
			return
		default:
		}
		_ = b.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		var a PeerAnnounce
		if err := json.Unmarshal(buf[:n], &a); err != nil {
			continue
		}
		select {
		case b.out <- a:
		default:
		}
	}
}

func (b *UDPBeacon) Close() error {
	b.once.Do(func() { close(b.done) })
	return b.conn.Close()
}
