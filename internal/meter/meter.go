package meter

import (
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

type Entry struct {
	BytesIn   uint64            `json:"bytes_in"`
	BytesOut  uint64            `json:"bytes_out"`
	Requests  uint64            `json:"requests"`
	ByCarrier map[string]uint64 `json:"by_carrier"`
	FirstSeen time.Time         `json:"first_seen"`
	LastSeen  time.Time         `json:"last_seen"`
}

type Meter struct {
	totalBytesIn  atomic.Uint64
	totalBytesOut atomic.Uint64
	totalRequests atomic.Uint64
	activeTunnels atomic.Int64
	shards        [shardCount]*shard
	startAt       time.Time
}

func New() *Meter {
	m := &Meter{startAt: time.Now().UTC()}
	for i := range m.shards {
		m.shards[i] = &shard{ledger: make(map[string]*Entry)}
	}
	return m
}

func (m *Meter) shardFor(clientID string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(clientID))
	return m.shards[h.Sum32()%shardCount]
}

func (m *Meter) TunnelOpened() { m.activeTunnels.Add(1) }
func (m *Meter) TunnelClosed() { m.activeTunnels.Add(-1) }

func (m *Meter) Record(clientID, carrierType string, bytesIn, bytesOut uint64) {
	m.totalBytesIn.Add(bytesIn)
	m.totalBytesOut.Add(bytesOut)
	m.totalRequests.Add(1)

	now := time.Now().UTC()
	s := m.shardFor(clientID)
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.ledger[clientID]
	if !ok {
		e = &Entry{ByCarrier: make(map[string]uint64), FirstSeen: now}
		s.ledger[clientID] = e
	}
	e.BytesIn += bytesIn
	e.BytesOut += bytesOut
	e.Requests++
	e.ByCarrier[carrierType] += bytesIn + bytesOut
	e.LastSeen = now
}

type Snapshot struct {
	TotalBytesIn  uint64            `json:"total_bytes_in"`
	TotalBytesOut uint64            `json:"total_bytes_out"`
	TotalRequests uint64            `json:"total_requests"`
	ActiveTunnels int64             `json:"active_tunnels"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Clients       map[string]*Entry `json:"clients"`
	GeneratedAt   time.Time         `json:"generated_at"`
}

func copyEntry(e *Entry) *Entry {
	cp := &Entry{
		BytesIn:   e.BytesIn,
		BytesOut:  e.BytesOut,
		Requests:  e.Requests,
		ByCarrier: make(map[string]uint64, len(e.ByCarrier)),
		FirstSeen: e.FirstSeen,
		LastSeen:  e.LastSeen,
	}
	for k, v := range e.ByCarrier {
		cp.ByCarrier[k] = v
	}
	return cp
}

func (m *Meter) Snapshot() Snapshot {
	snap := Snapshot{
		TotalBytesIn:  m.totalBytesIn.Load(),
		TotalBytesOut: m.totalBytesOut.Load(),
		TotalRequests: m.totalRequests.Load(),
		ActiveTunnels: m.activeTunnels.Load(),
		UptimeSeconds: int64(time.Since(m.startAt).Seconds()),
		Clients:       make(map[string]*Entry),
		GeneratedAt:   time.Now().UTC(),
	}
	for _, s := range m.shards {
		s.mu.RLock()
		for id, e := range s.ledger {
			snap.Clients[id] = copyEntry(e)
		}
		s.mu.RUnlock()
	}
	return snap
}

func (m *Meter) ClientSnapshot(clientID string) (*Entry, bool) {
	s := m.shardFor(clientID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.ledger[clientID]
	if !ok {
		return nil, false
	}
	return copyEntry(e), true
}
