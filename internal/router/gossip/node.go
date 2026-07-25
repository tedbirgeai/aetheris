package gossip

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Node, tek bir mesh dugumudur: kesif, anti-entropy senkronizasyon ve
// yerel kayit kumesini bir arada yurutur.
type Node struct {
	ID     string
	tr     Transport
	beacon Beacon
	set    *Set
	logger *slog.Logger

	mu    sync.RWMutex
	peers map[string]string // nodeID -> addr

	gossipIvl time.Duration
	rng       *rand.Rand
	rngMu     sync.Mutex

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	// onRecord, gossip ile GELEN yeni bir kayit ilk kez eklendiginde
	// cagrilir. Store-and-forward icin kullanilir: kayit yerel WAL'a yazilir.
	onRecord func(Record)

	// gozlem sayaclari
	rounds   atomic.Uint64
	recvPush atomic.Uint64
	sentPush atomic.Uint64
}

// Config, Node kurulum parametreleridir.
type Config struct {
	ID     string
	Set    *Set // nil ise yeni bir Set olusturulur
	Beacon Beacon
	// GossipInterval, anti-entropy turlari arasi sure. <=0 ise 250ms.
	GossipInterval time.Duration
	Logger         *slog.Logger
	// Seed, deterministik test icin rastgele tohumu (0 = zaman tabanli).
	Seed int64
	// OnRecord, gossip ile gelen YENI bir kayit ilk kez eklendiginde
	// cagrilir (store-and-forward icin). nil olabilir.
	OnRecord func(Record)
}

// NewNode, bir dugum olusturur (henuz calismaz; Run cagrilmalidir).
func NewNode(tr Transport, cfg Config) *Node {
	if cfg.Set == nil {
		cfg.Set = NewSet()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.GossipInterval <= 0 {
		cfg.GossipInterval = 250 * time.Millisecond
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &Node{
		ID:        cfg.ID,
		tr:        tr,
		beacon:    cfg.Beacon,
		set:       cfg.Set,
		logger:    cfg.Logger,
		peers:     make(map[string]string),
		gossipIvl: cfg.GossipInterval,
		rng:       rand.New(rand.NewSource(seed)),
		onRecord:  cfg.OnRecord,
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
}

// Set, dugumun kayit kumesini dondurur (uygulama kayit eklemek icin).
func (n *Node) Set() *Set { return n.set }

// Addr, dugumun tasima adresi.
func (n *Node) Addr() string { return n.tr.Addr() }

// Peers, o an bilinen komsu sayisi.
func (n *Node) Peers() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.peers)
}

// PeerInfo, bilinen bir komsunun kimlik ve adresidir.
type PeerInfo struct {
	NodeID string
	Addr   string
}

// PeerList, o an bilinen tum komsularin (kimlik, adres) listesini dondurur.
// Dashboard topoloji haritasi bunu kullanir.
func (n *Node) PeerList() []PeerInfo {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]PeerInfo, 0, len(n.peers))
	for id, addr := range n.peers {
		out = append(out, PeerInfo{NodeID: id, Addr: addr})
	}
	return out
}

// Rounds, tamamlanan gossip turu sayisi (gozlem).
func (n *Node) Rounds() uint64 { return n.rounds.Load() }

// AddPeer, bir komsuyu elle ekler (kesif olmadan, statik yapilandirma icin).
func (n *Node) AddPeer(nodeID, addr string) {
	if nodeID == n.ID || addr == n.Addr() {
		return
	}
	n.mu.Lock()
	n.peers[nodeID] = addr
	n.mu.Unlock()
}

// Run, dugumun tum arka plan dongulerini baslatir ve ctx iptaline kadar
// bloklar. Ayri bir goroutine'de cagrilmalidir.
func (n *Node) Run(ctx context.Context) {
	defer close(n.stopped)

	// Beacon: kendini duyur + kesfedilenleri isle.
	if n.beacon != nil {
		go n.beacon.Announce(ctx, PeerAnnounce{NodeID: n.ID, Addr: n.Addr()})
		go n.discoverLoop(ctx)
	}

	// Gossip turlari.
	ticker := time.NewTicker(n.gossipIvl)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stop:
			return
		case env := <-n.tr.Inbox():
			n.handle(env)
		case <-ticker.C:
			n.gossipRound()
		}
	}
}

// discoverLoop, beacon'dan gelen komsu duyurularini peer tablosuna isler.
func (n *Node) discoverLoop(ctx context.Context) {
	ch := n.beacon.Discovered()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stop:
			return
		case a := <-ch:
			if a.NodeID == n.ID || a.NodeID == "" {
				continue
			}
			n.mu.Lock()
			_, known := n.peers[a.NodeID]
			n.peers[a.NodeID] = a.Addr
			n.mu.Unlock()
			if !known {
				n.logger.Debug("gossip: yeni komsu kesfedildi", "node", n.ID, "peer", a.NodeID, "addr", a.Addr)
			}
		}
	}
}

// gossipRound, rastgele bir komsuya kendi digest'ini gonderir.
func (n *Node) gossipRound() {
	n.rounds.Add(1)
	addr := n.randomPeerAddr()
	if addr == "" {
		return
	}
	_ = n.tr.Send(addr, Message{
		Kind: KindDigest,
		From: n.ID,
		Addr: n.Addr(),
		IDs:  n.set.IDs(),
	})
}

func (n *Node) randomPeerAddr() string {
	n.mu.RLock()
	addrs := make([]string, 0, len(n.peers))
	for _, a := range n.peers {
		addrs = append(addrs, a)
	}
	n.mu.RUnlock()
	if len(addrs) == 0 {
		return ""
	}
	n.rngMu.Lock()
	idx := n.rng.Intn(len(addrs))
	n.rngMu.Unlock()
	return addrs[idx]
}

// handle, gelen tek bir mesaji isler.
func (n *Node) handle(env Envelope) {
	m := env.Msg
	// Gonderen adresini peer tablosunda tut (kesif olmasa bile ogren).
	if m.From != "" && m.From != n.ID && m.Addr != "" {
		n.mu.Lock()
		n.peers[m.From] = m.Addr
		n.mu.Unlock()
	}

	switch m.Kind {
	case KindDigest:
		// Push-pull: eksiklerimi cek, fazlalarimi ilet. Digest'e digest
		// ile YANIT VERME (ping-pong onlemi).
		reply := m.Addr
		if reply == "" {
			reply = env.FromAddr
		}
		if miss := n.set.missing(m.IDs); len(miss) > 0 {
			_ = n.tr.Send(reply, Message{Kind: KindPull, From: n.ID, Addr: n.Addr(), IDs: miss})
		}
		if extra := n.set.extra(m.IDs); len(extra) > 0 {
			n.sentPush.Add(uint64(len(extra)))
			_ = n.tr.Send(reply, Message{Kind: KindPush, From: n.ID, Addr: n.Addr(), Recs: extra})
		}

	case KindPull:
		reply := m.Addr
		if reply == "" {
			reply = env.FromAddr
		}
		if recs := n.set.collect(m.IDs); len(recs) > 0 {
			n.sentPush.Add(uint64(len(recs)))
			_ = n.tr.Send(reply, Message{Kind: KindPush, From: n.ID, Addr: n.Addr(), Recs: recs})
		}

	case KindPush:
		for _, r := range m.Recs {
			if n.set.Add(r) {
				n.recvPush.Add(1)
				if n.onRecord != nil {
					n.onRecord(r)
				}
			}
		}
	}
}

// Close, dugumu durdurur.
func (n *Node) Close() error {
	n.stopOnce.Do(func() { close(n.stop) })
	<-n.stopped
	if n.beacon != nil {
		_ = n.beacon.Close()
	}
	return n.tr.Close()
}
