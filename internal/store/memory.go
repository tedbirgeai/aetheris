package store

import (
	"context"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"
)

const shardCount = 32

type shard struct {
	mu     sync.RWMutex
	ledger map[string]*Entry
}

// MemoryStore, bellek içi defterdir.
//
// UYARI: Süreç yeniden başladığında tüm veri kaybolur. Geliştirme,
// test ve faturalama gerektirmeyen dağıtımlar içindir. Üretimde
// PostgresStore kullanın.
//
// TASARIM: Tek bir global mutex yüksek istek hacminde tüm yazmaları
// serileştirir. Defter, istemci kimliğinin FNV-1a özetine göre 32
// shard'a bölünmüştür; farklı istemciler birbirini bloklamaz.
type MemoryStore struct {
	totalBytesIn  atomic.Uint64
	totalBytesOut atomic.Uint64
	totalRequests atomic.Uint64
	shards        [shardCount]*shard
}

// NewMemory, bellek içi defter oluşturur.
func NewMemory() *MemoryStore {
	m := &MemoryStore{}
	for i := range m.shards {
		m.shards[i] = &shard{ledger: make(map[string]*Entry)}
	}
	return m
}

func (m *MemoryStore) shardFor(clientID string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(clientID))
	return m.shards[h.Sum32()%shardCount]
}

func (m *MemoryStore) Record(_ context.Context, u Usage) error {
	if u.OccurredAt.IsZero() {
		u.OccurredAt = time.Now().UTC()
	}

	m.totalBytesIn.Add(u.BytesIn)
	m.totalBytesOut.Add(u.BytesOut)
	m.totalRequests.Add(1)

	s := m.shardFor(u.ClientID)
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.ledger[u.ClientID]
	if !ok {
		e = &Entry{
			ByCarrier: make(map[string]uint64),
			FirstSeen: u.OccurredAt,
		}
		s.ledger[u.ClientID] = e
	}
	e.BytesIn += u.BytesIn
	e.BytesOut += u.BytesOut
	e.Requests++
	e.ByCarrier[u.CarrierType] += u.BytesIn + u.BytesOut
	e.LastSeen = u.OccurredAt
	return nil
}

func (m *MemoryStore) ClientUsage(_ context.Context, clientID string) (*Entry, error) {
	s := m.shardFor(clientID)
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.ledger[clientID]
	if !ok {
		return nil, ErrNotFound
	}
	return e.Clone(), nil
}

func (m *MemoryStore) Snapshot(_ context.Context) (Snapshot, error) {
	snap := Snapshot{
		TotalBytesIn:  m.totalBytesIn.Load(),
		TotalBytesOut: m.totalBytesOut.Load(),
		TotalRequests: m.totalRequests.Load(),
		Clients:       make(map[string]*Entry),
		GeneratedAt:   time.Now().UTC(),
	}
	for _, s := range m.shards {
		s.mu.RLock()
		for id, e := range s.ledger {
			snap.Clients[id] = e.Clone()
		}
		s.mu.RUnlock()
	}
	return snap, nil
}

func (m *MemoryStore) Kind() string { return "memory" }

func (m *MemoryStore) Close() error { return nil }
