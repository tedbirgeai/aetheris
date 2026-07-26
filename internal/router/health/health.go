// Package health, dugumler/hatlar arasi CANLI link sagligini izler: gecikme
// (RTT), ardisik hata (paket kaybi gostergesi) ve heartbeat. Aktif hat veya
// exit node koptugunda/kalitesi dustugunde durum degisikligi bir geri cagri
// ile bildirilir; boylece mesh yonlendirme (Dijkstra) veya exit secimi
// otomatik olarak alternatife kayar (failover).
package health

import (
	"context"
	"net"
	"sync"
	"time"
)

// Pinger, bir hedefe canli olcum yapar (heartbeat). Basari = RTT, hata = kayip.
// Testlerde sahte pinger enjekte edilir; uretimde TCPPinger.
type Pinger interface {
	Ping(ctx context.Context, target string) (time.Duration, error)
}

// State, bir hedefin anlik saglik durumudur.
type State struct {
	Target    string
	Up        bool
	RTT       time.Duration // EWMA yumusatilmis
	Fails     int           // ardisik hata sayisi
	LastCheck time.Time
}

// Monitor, kayitli hedefleri periyodik izler.
type Monitor struct {
	pinger   Pinger
	interval time.Duration
	// downAfter, bir hedefin "down" sayilmasi icin gereken ardisik hata.
	downAfter int
	// timeout, tek bir ping'in azami suresi.
	timeout time.Duration

	mu       sync.RWMutex
	targets  map[string]*State
	onChange func(State) // Up durumu degisince cagrilir

	stop chan struct{}
	once sync.Once
}

// Config, monitor ayarlaridir.
type Config struct {
	Interval  time.Duration // olcum araligi (varsayilan 2sn)
	DownAfter int           // kac ardisik hata sonrasi down (varsayilan 3)
	Timeout   time.Duration // ping zaman asimi (varsayilan 2sn)
	OnChange  func(State)   // durum degisikligi geri cagrisi
}

// New, bir saglik monitoru olusturur.
func New(pinger Pinger, cfg Config) *Monitor {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.DownAfter <= 0 {
		cfg.DownAfter = 3
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	return &Monitor{
		pinger:    pinger,
		interval:  cfg.Interval,
		downAfter: cfg.DownAfter,
		timeout:   cfg.Timeout,
		targets:   make(map[string]*State),
		onChange:  cfg.OnChange,
		stop:      make(chan struct{}),
	}
}

// Watch, bir hedefi izleme listesine ekler (baslangicta Up kabul edilir).
func (m *Monitor) Watch(target string) {
	m.mu.Lock()
	if _, ok := m.targets[target]; !ok {
		m.targets[target] = &State{Target: target, Up: true}
	}
	m.mu.Unlock()
}

// Unwatch, bir hedefi izlemeden cikarir.
func (m *Monitor) Unwatch(target string) {
	m.mu.Lock()
	delete(m.targets, target)
	m.mu.Unlock()
}

// Healthy, bir hedefin su an saglikli (Up) olup olmadigini dondurur.
func (m *Monitor) Healthy(target string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.targets[target]
	return ok && s.Up
}

// Snapshot, tum hedeflerin durum kopyasini dondurur.
func (m *Monitor) Snapshot() []State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]State, 0, len(m.targets))
	for _, s := range m.targets {
		out = append(out, *s)
	}
	return out
}

// Run, izlemeyi baslatir. ctx iptaline veya Close()'a kadar bloklar.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	m.probeAll(ctx) // hemen bir tur
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case <-ticker.C:
			m.probeAll(ctx)
		}
	}
}

func (m *Monitor) probeAll(ctx context.Context) {
	// Hedef listesini kilit altinda kopyala.
	m.mu.RLock()
	targets := make([]string, 0, len(m.targets))
	for t := range m.targets {
		targets = append(targets, t)
	}
	m.mu.RUnlock()

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			m.probeOne(ctx, target)
		}(t)
	}
	wg.Wait()
}

func (m *Monitor) probeOne(ctx context.Context, target string) {
	pctx, cancel := context.WithTimeout(ctx, m.timeout)
	rtt, err := m.pinger.Ping(pctx, target)
	cancel()

	m.mu.Lock()
	s, ok := m.targets[target]
	if !ok {
		m.mu.Unlock()
		return
	}
	prevUp := s.Up
	s.LastCheck = time.Now()
	if err != nil {
		s.Fails++
		if s.Fails >= m.downAfter {
			s.Up = false
		}
	} else {
		s.Fails = 0
		s.Up = true
		// EWMA: yeni_rtt = 0.7*eski + 0.3*olcum (ilk olcumde dogrudan).
		if s.RTT == 0 {
			s.RTT = rtt
		} else {
			s.RTT = (s.RTT*7 + rtt*3) / 10
		}
	}
	changed := prevUp != s.Up
	snapshot := *s
	cb := m.onChange
	m.mu.Unlock()

	if changed && cb != nil {
		cb(snapshot) // failover tetikleyicisi
	}
}

// Close, monitoru durdurur.
func (m *Monitor) Close() {
	m.once.Do(func() { close(m.stop) })
}

// --- Gercek pinger (TCP dial ile RTT olcumu) ---

// TCPPinger, hedefe TCP baglanti kurma suresini RTT olarak olcer. Heartbeat
// icin hafif ve tasiyici-bagimsizdir (LoRa/UDP hatlarinda da hedef bir
// dinleyiciyse calisir).
type TCPPinger struct {
	dialer net.Dialer
}

// NewTCPPinger, bir TCP pinger olusturur.
func NewTCPPinger() *TCPPinger { return &TCPPinger{} }

// Ping, hedefe baglanip RTT'yi olcer.
func (p *TCPPinger) Ping(ctx context.Context, target string) (time.Duration, error) {
	start := time.Now()
	conn, err := p.dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return time.Since(start), nil
}
