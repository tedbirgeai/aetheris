// Package discovery, SIFIR-KONFIGURASYONLU (Zero-Conf) otomatik eş ve exit
// node kesfi saglar. Dugumler ayni yerel agda (Wi-Fi, Hotspot, LAN) acildigi
// an, UDP local broadcast ile birbirini ve WAN cikisi olan EXIT node'lari
// otomatik kesfeder. Manuel AETHERIS_EXIT_PEER girme zorunlulugu ortadan
// kalkar: istemci BestExit() cagirir ve nereye rele edecegini ogrenir.
package discovery

import (
	"context"
	"encoding/json"
	"net"
	"sort"
	"sync"
	"time"
)

// Announce, bir dugumun agda yaptigi kimlik/yetenek ilanidir.
type Announce struct {
	NodeID      string `json:"node_id"`
	RelayAddr   string `json:"relay_addr"`   // exit ise relay dinleme adresi
	ExitCapable bool   `json:"exit_capable"` // WAN cikisi sunuyor mu
	WANHealthy  bool   `json:"wan_healthy"`  // WAN'i su an saglikli mi
	TS          int64  `json:"ts"`           // unix nano (tazelik)
}

// Peer, kesfedilen bir komsu (son gorulme zamaniyla).
type Peer struct {
	Announce
	LastSeen time.Time
}

// Transport, ilanlari yayinlayan/dinleyen tasima katmanidir. UDP broadcast
// (gercek) veya bellek-ici (test) uygulanabilir.
type Transport interface {
	Broadcast(data []byte) error
	// Inbox, gelen ilan baytlarini dondurur.
	Inbox() <-chan []byte
	Close() error
}

// Service, otomatik kesif servisidir. Periyodik olarak kendi ilanini yayinlar,
// gelen ilanlari isleyip eş tablosunu (TTL'li) tutar.
type Service struct {
	self     Announce
	tr       Transport
	interval time.Duration
	ttl      time.Duration

	mu    sync.RWMutex
	peers map[string]*Peer

	stop chan struct{}
	once sync.Once
}

// Config, kesif servisi ayarlaridir.
type Config struct {
	Self     Announce
	Interval time.Duration // ilan araligi (varsayilan 2sn)
	TTL      time.Duration // eş suresi (varsayilan 10sn)
}

// New, bir kesif servisi olusturur.
func New(tr Transport, cfg Config) *Service {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 10 * time.Second
	}
	return &Service{
		self:     cfg.Self,
		tr:       tr,
		interval: cfg.Interval,
		ttl:      cfg.TTL,
		peers:    make(map[string]*Peer),
		stop:     make(chan struct{}),
	}
}

// Run, servisi baslatir: periyodik ilan + gelen ilan isleme. ctx yerine
// Close() ile durdurulur; ayri goroutine'de cagrilabilir.
func (s *Service) Run() {
	_ = s.announce() // hemen bir ilan
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	gc := time.NewTicker(s.ttl / 2)
	defer gc.Stop()

	inbox := s.tr.Inbox()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			_ = s.announce()
		case <-gc.C:
			s.evictStale()
		case data, ok := <-inbox:
			if !ok {
				return
			}
			s.ingest(data)
		}
	}
}

func (s *Service) announce() error {
	s.mu.Lock()
	s.self.TS = time.Now().UnixNano()
	self := s.self // kilit altinda kopyala
	s.mu.Unlock()
	data, err := json.Marshal(self)
	if err != nil {
		return err
	}
	return s.tr.Broadcast(data)
}

func (s *Service) ingest(data []byte) {
	var a Announce
	if err := json.Unmarshal(data, &a); err != nil {
		return
	}
	if a.NodeID == "" || a.NodeID == s.self.NodeID {
		return // kendi ilanimizi yok say
	}
	s.mu.Lock()
	s.peers[a.NodeID] = &Peer{Announce: a, LastSeen: time.Now()}
	s.mu.Unlock()
}

func (s *Service) evictStale() {
	cutoff := time.Now().Add(-s.ttl)
	s.mu.Lock()
	for id, p := range s.peers {
		if p.LastSeen.Before(cutoff) {
			delete(s.peers, id)
		}
	}
	s.mu.Unlock()
}

// Peers, o an bilinen tum komsulari dondurur.
func (s *Service) Peers() []Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// ExitNodes, WAN cikisi sunan ve WAN'i saglikli olan komsulari dondurur.
func (s *Service) ExitNodes() []Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Peer
	for _, p := range s.peers {
		if p.ExitCapable && p.WANHealthy && p.RelayAddr != "" {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// BestExit, kullanilabilecek en uygun exit node'u dondurur (deterministik:
// en kucuk NodeID). Bulunamazsa ok=false.
func (s *Service) BestExit() (Peer, bool) {
	exits := s.ExitNodes()
	if len(exits) == 0 {
		return Peer{}, false
	}
	return exits[0], true
}

// SetWANHealthy, bu dugumun WAN saglik durumunu gunceller (health monitor'dan).
func (s *Service) SetWANHealthy(healthy bool) {
	s.mu.Lock()
	s.self.WANHealthy = healthy
	s.mu.Unlock()
}

// Close, servisi durdurur.
func (s *Service) Close() error {
	s.once.Do(func() { close(s.stop) })
	return s.tr.Close()
}

// --- UDP broadcast tasima (gercek LAN) ---

// UDPTransport, 255.255.255.255:port uzerinden ilan yayinlar/dinler.
type UDPTransport struct {
	port  int
	conn  *net.UDPConn
	inbox chan []byte
	stop  chan struct{}
	once  sync.Once
}

// NewUDPTransport, verilen broadcast portunu dinlemeye baslar. Ayni host'ta
// birden cok dugumun ayni portu paylasabilmesi icin SO_REUSEADDR/REUSEPORT
// kullanilir (test/tek-makine cok-dugum senaryolari).
func NewUDPTransport(port int) (*UDPTransport, error) {
	lc := net.ListenConfig{Control: reusePortControl}
	pc, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort("0.0.0.0", itoa(port)))
	if err != nil {
		return nil, err
	}
	conn := pc.(*net.UDPConn)
	t := &UDPTransport{
		port:  port,
		conn:  conn,
		inbox: make(chan []byte, 64),
		stop:  make(chan struct{}),
	}
	go t.readLoop()
	return t, nil
}

func (t *UDPTransport) readLoop() {
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		_ = t.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		cp := make([]byte, n)
		copy(cp, buf[:n])
		select {
		case t.inbox <- cp:
		default:
		}
	}
}

func (t *UDPTransport) Broadcast(data []byte) error {
	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: t.port}
	_, err := t.conn.WriteToUDP(data, dst)
	return err
}

func (t *UDPTransport) Inbox() <-chan []byte { return t.inbox }

func (t *UDPTransport) Close() error {
	t.once.Do(func() { close(t.stop) })
	return t.conn.Close()
}
