package router

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tedbirgeai/aetheris/internal/carrier"
)

// HealthProber, rotalarin sagligini periyodik olarak yoklar ve
// sagliksiz olanlar icin Forward'in yedek rotaya dusmesini saglar.
//
// MANTIK:
//   - Her rota icin arka planda saglik yoklamasi calisir.
//   - Bir rota art arda N kez basarisiz olursa "sagliksiz" isaretlenir.
//   - Sagliksiz rotaya gelen istek:
//     1. Route.Backup tanimliysa ona yonlendirilir,
//     2. degilse "direct" turunde bir rota varsa ona dusulur,
//     3. o da yoksa hata doner (upstream gercekten erisilemez).
//   - Rota tekrar yanit vermeye baslayinca otomatik "saglikli" olur.
type HealthProber struct {
	router   *Router
	logger   *slog.Logger
	client   *http.Client
	interval time.Duration

	// failThreshold: bu kadar art arda basarisizlikta sagliksiz say.
	failThreshold int

	mu       sync.RWMutex
	health   map[string]*routeHealth
	qos      map[string]*qosTracker
	directRt string // ilk "direct" turundeki rotanin adi (fallback hedefi)

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

type routeHealth struct {
	healthy   atomic.Bool
	failCount atomic.Int32
	lastCheck atomic.Int64 // unix nano
}

// ProberConfig, HealthProber parametreleridir.
type ProberConfig struct {
	Interval      time.Duration
	Timeout       time.Duration
	FailThreshold int
	Logger        *slog.Logger
}

// NewHealthProber, prober'i kurar ama BASLATMAZ. Start() cagrilmalidir.
func NewHealthProber(r *Router, cfg ProberConfig) *HealthProber {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = 3
	}

	hp := &HealthProber{
		router:        r,
		logger:        cfg.Logger,
		client:        &http.Client{Timeout: cfg.Timeout},
		interval:      cfg.Interval,
		failThreshold: cfg.FailThreshold,
		health:        make(map[string]*routeHealth),
		qos:           make(map[string]*qosTracker),
		stop:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}

	for name, rt := range r.routes {
		rh := &routeHealth{}
		rh.healthy.Store(true) // baslangicta saglikli varsay
		hp.health[name] = rh
		hp.qos[name] = newQoSTracker()
		if rt.Kind == "direct" && hp.directRt == "" {
			hp.directRt = name
		}
	}
	return hp
}

// Start, arka plan yoklamasini baslatir.
func (hp *HealthProber) Start() {
	go hp.loop()
}

func (hp *HealthProber) loop() {
	defer close(hp.stopped)
	ticker := time.NewTicker(hp.interval)
	defer ticker.Stop()

	// Baslangicta bir kez hemen yokla.
	hp.probeAll()

	for {
		select {
		case <-hp.stop:
			return
		case <-ticker.C:
			hp.probeAll()
		}
	}
}

// probeAll, tum rotalari es zamanli yoklar.
func (hp *HealthProber) probeAll() {
	routes := hp.snapshotRoutes()
	var wg sync.WaitGroup
	for _, rt := range routes {
		wg.Add(1)
		go func(rt Route) {
			defer wg.Done()
			hp.probeOne(rt)
		}(rt)
	}
	wg.Wait()
}

func (hp *HealthProber) snapshotRoutes() []Route {
	out := make([]Route, 0, len(hp.router.routes))
	for _, rt := range hp.router.routes {
		out = append(out, rt)
	}
	return out
}

// probeOne, tek bir rotaya saglik istegi atar ve durumu gunceller.
func (hp *HealthProber) probeOne(rt Route) {
	hp.mu.RLock()
	rh := hp.health[rt.Name]
	hp.mu.RUnlock()
	if rh == nil {
		return
	}
	rh.lastCheck.Store(time.Now().UnixNano())

	healthPath := rt.HealthPath
	if healthPath == "" {
		healthPath = "/healthz"
	}
	probeURL := *rt.Upstream
	probeURL.Path = healthPath

	ctx, cancel := context.WithTimeout(context.Background(), hp.client.Timeout)
	defer cancel()

	hp.mu.RLock()
	tr := hp.qos[rt.Name]
	hp.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL.String(), nil)
	if err != nil {
		hp.markResult(rt.Name, rh, false)
		if tr != nil {
			tr.record(0, false)
		}
		return
	}

	// RTT olcumu: istek gonderiminden yanit basliklarinin alinmasina kadar.
	// Govde okuma suresi DAHIL DEGILDIR (saglik yanitlari kucuktur, ama
	// olcumun tutarli olmasi icin sinir net tutulur).
	start := time.Now()
	resp, err := hp.client.Do(req)
	rtt := time.Since(start)

	if err != nil {
		hp.markResult(rt.Name, rh, false)
		if tr != nil {
			tr.record(rtt, false)
		}
		return
	}
	_ = resp.Body.Close()

	ok := resp.StatusCode < 500
	hp.markResult(rt.Name, rh, ok)
	if tr != nil {
		tr.record(rtt, ok)
	}
}

// markResult, saglik durumunu esik mantigina gore gunceller.
func (hp *HealthProber) markResult(name string, rh *routeHealth, ok bool) {
	if ok {
		if !rh.healthy.Load() {
			hp.logger.Info("rota yeniden saglikli", "route", name)
		}
		rh.failCount.Store(0)
		rh.healthy.Store(true)
		return
	}
	if rh.failCount.Add(1) >= int32(hp.failThreshold) {
		if rh.healthy.CompareAndSwap(true, false) {
			hp.logger.Warn("rota sagliksiz isaretlendi, failover aktif", "route", name)
		}
	}
}

// IsHealthy, bir rotanin saglikli olup olmadigini bildirir.
func (hp *HealthProber) IsHealthy(name string) bool {
	hp.mu.RLock()
	rh := hp.health[name]
	hp.mu.RUnlock()
	if rh == nil {
		return true // bilinmeyen rota icin engel olma
	}
	return rh.healthy.Load()
}

// ResolveTarget, istenen hedef sagliksizsa yedek/direct'e dusurerek
// gercekte kullanilacak rota adini dondurur.
//
// Sira:
//  1. Hedef saglikliysa: hedefin kendisi.
//  2. Sagliksiz + Backup tanimli + Backup saglikli: Backup.
//  3. Sagliksiz + direct rota var + direct saglikli: direct.
//  4. Hicbiri: hedefin kendisi (son care, muhtemelen hata donecek).
func (hp *HealthProber) ResolveTarget(destination string) (string, bool) {
	if hp.IsHealthy(destination) {
		return destination, false
	}

	hp.mu.RLock()
	rt, exists := hp.router.routes[destination]
	hp.mu.RUnlock()
	if !exists {
		return destination, false
	}

	if rt.Backup != "" && hp.IsHealthy(rt.Backup) {
		hp.logger.Info("failover: yedek rotaya yonlendiriliyor",
			"from", destination, "to", rt.Backup)
		return rt.Backup, true
	}

	if hp.directRt != "" && hp.directRt != destination && hp.IsHealthy(hp.directRt) {
		hp.logger.Info("failover: direct hatta dusuluyor",
			"from", destination, "to", hp.directRt)
		return hp.directRt, true
	}

	return destination, false
}

// Status, tum rotalarin saglik durumunu dondurur (health ucu icin).
func (hp *HealthProber) Status() map[string]bool {
	hp.mu.RLock()
	defer hp.mu.RUnlock()
	out := make(map[string]bool, len(hp.health))
	for name, rh := range hp.health {
		out[name] = rh.healthy.Load()
	}
	return out
}

// Stop, prober'i durdurur.
func (hp *HealthProber) Stop() {
	hp.stopOnce.Do(func() { close(hp.stop) })
	<-hp.stopped
}

// ForwardWithFailover, ResolveTarget ile hedefi cozup Forward'i cagirir.
// Router.Forward'in failover-farkinda sarmalayicisi.
func (hp *HealthProber) ForwardWithFailover(
	ctx context.Context,
	destination string,
	clientID string,
	carrierType carrier.Type,
	payload []byte,
) (*Result, bool, error) {
	target, failedOver := hp.ResolveTarget(destination)
	res, err := hp.router.Forward(ctx, target, clientID, carrierType, payload)
	return res, failedOver, err
}
