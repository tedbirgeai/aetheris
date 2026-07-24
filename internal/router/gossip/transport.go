package gossip

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// Envelope, gelen bir mesaji kaynak adresiyle sarar.
type Envelope struct {
	FromAddr string
	Msg      Message
}

// Transport, dugumler arasi adresli (unicast) mesaj tasima sozlesmesidir.
type Transport interface {
	// Addr, bu tasiyicinin yerel adresi.
	Addr() string
	// Send, mesaji hedef adrese iletir.
	Send(dstAddr string, msg Message) error
	// Inbox, gelen mesajlarin okundugu kanal.
	Inbox() <-chan Envelope
	// Close, tasiyiciyi kapatir.
	Close() error
}

// --- Bellek-ici tasima (test ve simulator icin) ---

// MemSwitch, birden cok MemTransport'u baglayan bellek-ici yonlendiricidir.
// Ag bolunmesini (partition) modellemek icin adresler arasi iletimi
// kesebilir — MADDE 4 split-brain simulasyonunun cekirdegidir.
type MemSwitch struct {
	mu          sync.RWMutex
	ports       map[string]*MemTransport
	partitioned map[string]map[string]bool // src -> dst -> kesik mi
}

// NewMemSwitch, bos bir anahtar olusturur.
func NewMemSwitch() *MemSwitch {
	return &MemSwitch{
		ports:       make(map[string]*MemTransport),
		partitioned: make(map[string]map[string]bool),
	}
}

// Transport, verilen adres icin yeni bir MemTransport olusturur ve baglar.
func (s *MemSwitch) Transport(addr string) *MemTransport {
	t := &MemTransport{addr: addr, sw: s, inbox: make(chan Envelope, 1024)}
	s.mu.Lock()
	s.ports[addr] = t
	s.mu.Unlock()
	return t
}

// Partition, src->dst yonunde iletimi keser (tek yonlu). Cift yon icin
// iki kez cagirin.
func (s *MemSwitch) Partition(src, dst string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.partitioned[src] == nil {
		s.partitioned[src] = make(map[string]bool)
	}
	s.partitioned[src][dst] = true
}

// Heal, src->dst kesintisini kaldirir.
func (s *MemSwitch) Heal(src, dst string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.partitioned[src] != nil {
		delete(s.partitioned[src], dst)
	}
}

// PartitionPair, iki dugum arasindaki iletimi CIFT YONLU keser.
func (s *MemSwitch) PartitionPair(a, b string) {
	s.Partition(a, b)
	s.Partition(b, a)
}

// HealPair, iki dugum arasini cift yonlu iyilestirir.
func (s *MemSwitch) HealPair(a, b string) {
	s.Heal(a, b)
	s.Heal(b, a)
}

func (s *MemSwitch) route(src, dst string, msg Message) error {
	s.mu.RLock()
	if s.partitioned[src] != nil && s.partitioned[src][dst] {
		s.mu.RUnlock()
		// Bolunme: mesaj sessizce dusrer (gercek agda paket kaybi gibi).
		return nil
	}
	port, ok := s.ports[dst]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("gossip memswitch: bilinmeyen hedef %q", dst)
	}
	select {
	case port.inbox <- Envelope{FromAddr: src, Msg: msg}:
	default:
		// Alici kutusu dolu: mesaj dusrer, anti-entropy sonraki turda kapatir.
	}
	return nil
}

// MemTransport, bellek-ici tasiyicidir.
type MemTransport struct {
	addr  string
	sw    *MemSwitch
	inbox chan Envelope
	once  sync.Once
}

func (t *MemTransport) Addr() string           { return t.addr }
func (t *MemTransport) Inbox() <-chan Envelope { return t.inbox }
func (t *MemTransport) Send(dst string, m Message) error {
	return t.sw.route(t.addr, dst, m)
}
func (t *MemTransport) Close() error {
	t.sw.mu.Lock()
	delete(t.sw.ports, t.addr)
	t.sw.mu.Unlock()
	return nil
}

// --- UDP tasima (gercek LAN icin) ---

// UDPTransport, mesajlari JSON olarak UDP unicast ile tasir. Yerel agda
// altyapi (DNS, broker) olmadan dugumler arasi dogrudan iletisim saglar.
type UDPTransport struct {
	conn  *net.UDPConn
	addr  string
	inbox chan Envelope
	done  chan struct{}
	once  sync.Once
}

// NewUDPTransport, verilen UDP portunu dinler.
func NewUDPTransport(listen string) (*UDPTransport, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", listen)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, err
	}
	t := &UDPTransport{
		conn:  conn,
		addr:  conn.LocalAddr().String(),
		inbox: make(chan Envelope, 1024),
		done:  make(chan struct{}),
	}
	go t.readLoop()
	return t, nil
}

func (t *UDPTransport) Addr() string           { return t.addr }
func (t *UDPTransport) Inbox() <-chan Envelope { return t.inbox }

func (t *UDPTransport) Send(dst string, m Message) error {
	udpAddr, err := net.ResolveUDPAddr("udp4", dst)
	if err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_ = t.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = t.conn.WriteToUDP(b, udpAddr)
	return err
}

func (t *UDPTransport) readLoop() {
	buf := make([]byte, 65536)
	for {
		select {
		case <-t.done:
			return
		default:
		}
		_ = t.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		var m Message
		if err := json.Unmarshal(buf[:n], &m); err != nil {
			continue // bozuk datagram, yut
		}
		select {
		case t.inbox <- Envelope{FromAddr: from.String(), Msg: m}:
		default:
		}
	}
}

func (t *UDPTransport) Close() error {
	t.once.Do(func() { close(t.done) })
	return t.conn.Close()
}
