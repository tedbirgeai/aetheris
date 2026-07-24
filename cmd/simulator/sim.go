// Command simulator, Docker/hermetik ortamda N sanal mesh dugumu ayaga
// kaldirir, aralarindaki baglantiyi yapay olarak keser (split-brain),
// bolunme sirasinda WAL'a yazilan kayitlarin ag birlestiginde (re-convergence)
// SIFIR KAYIPLA senkronize oldugunu KANITLAR.
//
// # NASIL CALISIR
//
// Her dugum:
//   - Bir gossip.Node (kesif + anti-entropy) tutar.
//   - KENDI WAL dizinine sahip bir store.WALStore tutar (dayaniklilik).
//
// Bir dugum "kullanim kaydeder" oldugunda:
//  1. Kayit KENDI WAL'ine yazilir (disk, dayanikli).
//  2. Ayni kayit gossip kumesine eklenir (mesh'e yayilir).
//
// Gossip ile BASKA bir dugumden yeni kayit geldiginde, OnRecord kancasi
// o kaydi ALICININ WAL'ine de yazar (store-and-forward). Boylece ag kopsa
// bile kayitlar yerelde durur, ilk komsu yakalandiginda aktarilir.
//
// # SIFIR KAYIP TANIMI
//
// Senaryo sonunda: her dugumun gossip kumesi TUM benzersiz kayitlari
// icermeli VE her dugumun WAL'i (memory backend) ayni sayida kayda ulasmali.
// Bolunme sirasinda uretilen hicbir kayit kaybolmamalidir.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tedbirgeai/aetheris/internal/router/gossip"
	"github.com/tedbirgeai/aetheris/internal/store"
)

// simNode, tek bir simulasyon dugumudur.
type simNode struct {
	id     string
	g      *gossip.Node
	wal    *store.WALStore
	mem    store.Store
	walDir string

	// walSeen, bu dugumun WAL'ine yazilmis kayit ID'leri (cift yazim onlemi).
	mu      sync.Mutex
	walSeen map[string]struct{}
}

// Sim, tum senaryoyu yoneten simulatordur.
type Sim struct {
	sw     *gossip.MemSwitch
	reg    *gossip.MemRegistry
	nodes  []*simNode
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	tmp    string
}

// SimConfig, simulator ayarlaridir.
type SimConfig struct {
	Nodes    int
	GossipMS int
	Logger   *slog.Logger
	// TmpDir, WAL dizinlerinin kok klasoru. Bos ise gecici dizin olusturulur.
	TmpDir string
}

// NewSim, N dugumlu bir simulasyon kurar (henuz baslatmaz).
func NewSim(cfg SimConfig) (*Sim, error) {
	if cfg.Nodes <= 0 {
		cfg.Nodes = 5
	}
	if cfg.GossipMS <= 0 {
		cfg.GossipMS = 30
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tmp := cfg.TmpDir
	if tmp == "" {
		d, err := os.MkdirTemp("", "aetheris-sim-")
		if err != nil {
			return nil, err
		}
		tmp = d
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Sim{
		sw:     gossip.NewMemSwitch(),
		reg:    gossip.NewMemRegistry(),
		logger: cfg.Logger,
		ctx:    ctx,
		cancel: cancel,
		tmp:    tmp,
	}

	for i := 0; i < cfg.Nodes; i++ {
		id := fmt.Sprintf("node-%d", i)
		walDir := filepath.Join(tmp, id)
		mem := store.NewMemory()
		wal, err := store.NewWAL(ctx, mem, store.WALConfig{
			Dir:           walDir,
			QueueSize:     1024,
			BatchSize:     32,
			FlushInterval: 20 * time.Millisecond,
			Logger:        cfg.Logger,
		})
		if err != nil {
			cancel()
			return nil, fmt.Errorf("dugum %s WAL kurulamadi: %w", id, err)
		}

		sn := &simNode{
			id:      id,
			wal:     wal,
			mem:     mem,
			walDir:  walDir,
			walSeen: make(map[string]struct{}),
		}
		// Gossip ile gelen yeni kaydi bu dugumun WAL'ine de yaz (store-and-forward).
		onRecord := func(r gossip.Record) { sn.persist(ctx, r) }

		g := gossip.NewNode(s.sw.Transport(id), gossip.Config{
			ID:             id,
			Beacon:         s.reg.Beacon(),
			GossipInterval: time.Duration(cfg.GossipMS) * time.Millisecond,
			Seed:           int64(i + 1),
			Logger:         cfg.Logger,
			OnRecord:       onRecord,
		})
		sn.g = g
		s.nodes = append(s.nodes, sn)
	}
	return s, nil
}

// persist, bir gossip kaydini dugumun WAL'ine yazar (idempotent).
func (sn *simNode) persist(ctx context.Context, r gossip.Record) {
	sn.mu.Lock()
	if _, ok := sn.walSeen[r.ID]; ok {
		sn.mu.Unlock()
		return
	}
	sn.walSeen[r.ID] = struct{}{}
	sn.mu.Unlock()

	var u store.Usage
	if err := json.Unmarshal(r.Data, &u); err != nil {
		return // bozuk kayit, atla
	}
	_ = sn.wal.Record(ctx, u)
}

// RecordUsage, bir dugumun kullanim uretmesini modeller: WAL'e yazar VE
// gossip kumesine ekler. Kayit ID'si icerik ozeti oldugundan benzersizdir.
func (s *Sim) RecordUsage(nodeIdx int, u store.Usage) {
	sn := s.nodes[nodeIdx]
	data, _ := json.Marshal(u)
	rec := gossip.NewRecord(data)

	sn.persist(s.ctx, rec) // kendi WAL'ine yaz
	sn.g.Set().Add(rec)    // mesh'e yayilsin
}

// Start, tum dugumlerin arka plan dongulerini baslatir.
func (s *Sim) Start() {
	for _, sn := range s.nodes {
		go sn.g.Run(s.ctx)
	}
}

// Partition, iki dugum grubunu (indeks) birbirinden CIFT YONLU izole eder.
func (s *Sim) Partition(groupA, groupB []int) {
	for _, a := range groupA {
		for _, b := range groupB {
			s.sw.PartitionPair(s.nodes[a].id, s.nodes[b].id)
		}
	}
}

// Heal, iki grup arasindaki tum kesintileri kaldirir.
func (s *Sim) Heal(groupA, groupB []int) {
	for _, a := range groupA {
		for _, b := range groupB {
			s.sw.HealPair(s.nodes[a].id, s.nodes[b].id)
		}
	}
}

// GossipLens, her dugumun gossip kumesindeki kayit sayilarini dondurur.
func (s *Sim) GossipLens() []int {
	out := make([]int, len(s.nodes))
	for i, sn := range s.nodes {
		out[i] = sn.g.Set().Len()
	}
	return out
}

// WALLens, her dugumun WAL backend'indeki kayit sayilarini dondurur.
func (s *Sim) WALLens() []int {
	out := make([]int, len(s.nodes))
	for i, sn := range s.nodes {
		snap, err := sn.mem.Snapshot(s.ctx)
		if err != nil {
			out[i] = -1
			continue
		}
		out[i] = int(snap.TotalRequests)
	}
	return out
}

// WaitConverged, tum dugumlerin gossip kumesi 'want' kayda ulasana kadar
// (veya zaman asimina kadar) bekler. Yakinsama saglandiysa true doner.
func (s *Sim) WaitConverged(want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		all := true
		for _, n := range s.GossipLens() {
			if n != want {
				all = false
				break
			}
		}
		if all {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return false
}

// WaitPeers, her dugum en az 'min' komsu kesfedene kadar bekler.
func (s *Sim) WaitPeers(min int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		all := true
		for _, sn := range s.nodes {
			if sn.g.Peers() < min {
				all = false
				break
			}
		}
		if all {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return false
}

// WaitWALDrained, tum dugumlerin WAL backend'i 'want' kayda ulasana kadar
// bekler. WAL asenkron flush ettigi icin gossip yakinsamasindan biraz sonra
// tamamlanir.
func (s *Sim) WaitWALDrained(want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		all := true
		for _, n := range s.WALLens() {
			if n != want {
				all = false
				break
			}
		}
		if all {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return false
}

// Close, tum dugumleri kapatir ve gecici WAL dizinlerini temizler.
func (s *Sim) Close() {
	s.cancel()
	for _, sn := range s.nodes {
		_ = sn.g.Close()
		_ = sn.wal.Close()
	}
	if s.tmp != "" {
		_ = os.RemoveAll(s.tmp)
	}
}
