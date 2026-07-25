// Package wan, bir Aetheris dugumunun DIS DUNYA (WAN/Internet) erisim
// durumunu DURUSTCE tespit eder ve siniflandirir:
//
//	Direct    — dugumun kendi dogrudan WAN baglantisi var
//	Relayed   — dogrudan WAN yok ama bir komsu (exit node) uzerinden erisim var
//	OffGrid   — hicbir WAN yok; yalnizca yerel mesh calisiyor
//
// Tespit gercek bir erisilebilirlik denemesine dayanir (sahte durum uretmez).
// WAN yoksa dashboard ve yonlendirme bunu net gorur; boylece kullanici "yerel
// mesh" ile "kuresel internet cikisi" arasindaki farki anlik anlar.
package wan

import (
	"context"
	"net"
	"sync"
	"time"
)

// Status, dugumun WAN erisim durumudur.
type Status string

const (
	StatusDirect  Status = "direct"   // dogrudan internet
	StatusRelayed Status = "relayed"  // komsu exit node uzerinden
	StatusOffGrid Status = "off_grid" // yalnizca yerel mesh
	StatusUnknown Status = "unknown"  // henuz olculmedi
)

// Human, durumun panelde gosterilecek okunabilir karsiligini dondurur.
func (s Status) Human() string {
	switch s {
	case StatusDirect:
		return "Direct Internet"
	case StatusRelayed:
		return "Relayed via Peer"
	case StatusOffGrid:
		return "Off-Grid Mesh Only"
	default:
		return "Unknown"
	}
}

// Prober, dogrudan WAN erisilebilirligini olcer. Testlerde sahte prober
// enjekte edilebilir; uretimde TCPProber kullanilir.
type Prober interface {
	// DirectReachable, dogrudan internet erisimi varsa true doner.
	DirectReachable(ctx context.Context) bool
}

// TCPProber, bilinen bir dis ana bilgisayara kisa zaman asimli TCP baglantisi
// deneyerek dogrudan WAN erisimini olcer. Basari = dogrudan internet var.
type TCPProber struct {
	// Targets, denenecek "host:port" hedefleridir (ilki basarili olursa yeter).
	// Varsayilan olarak yaygin DNS/HTTP uc noktalari.
	Targets []string
	Timeout time.Duration
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewTCPProber, varsayilan hedeflerle bir prober olusturur.
func NewTCPProber(targets ...string) *TCPProber {
	if len(targets) == 0 {
		// Yaygin, yuksek-erisilebilir uc noktalar (DNS/HTTPS).
		targets = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}
	}
	d := &net.Dialer{}
	return &TCPProber{
		Targets: targets,
		Timeout: 2 * time.Second,
		dial:    d.DialContext,
	}
}

// DirectReachable, hedeflerden herhangi birine baglanabiliyorsa true doner.
func (p *TCPProber) DirectReachable(ctx context.Context) bool {
	for _, target := range p.Targets {
		c, cancel := context.WithTimeout(ctx, p.Timeout)
		conn, err := p.dial(c, "tcp", target)
		cancel()
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

// Detector, WAN durumunu belirler ve onbelleğe alir. Dogrudan erisim yoksa,
// bir exit-peer saglayicisina danisarak Relayed mi yoksa OffGrid mi oldugunu
// karar verir.
type Detector struct {
	prober   Prober
	interval time.Duration

	// hasExitPeer, WAN'a cikabilen bir komsu (exit node) bilinip bilinmedigini
	// dondurur. Gateway, gossip'ten kesfedilen exit ilanlarina gore doldurur.
	hasExitPeer func() bool

	mu      sync.RWMutex
	status  Status
	lastRun time.Time
}

// NewDetector, bir prober ve exit-peer saglayicisiyla dedektor olusturur.
// hasExitPeer nil olabilir (o zaman WAN yoksa dogrudan OffGrid).
func NewDetector(prober Prober, hasExitPeer func() bool, interval time.Duration) *Detector {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &Detector{
		prober:      prober,
		interval:    interval,
		hasExitPeer: hasExitPeer,
		status:      StatusUnknown,
	}
}

// Status, en son belirlenen (onbellekli) WAN durumunu dondurur.
func (d *Detector) Status() Status {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.status
}

// Refresh, durumu HEMEN yeniden olcer ve dondurur.
func (d *Detector) Refresh(ctx context.Context) Status {
	s := d.classify(ctx)
	d.mu.Lock()
	d.status = s
	d.lastRun = time.Now()
	d.mu.Unlock()
	return s
}

// classify, tek bir olcumle durumu belirler.
func (d *Detector) classify(ctx context.Context) Status {
	if d.prober != nil && d.prober.DirectReachable(ctx) {
		return StatusDirect
	}
	if d.hasExitPeer != nil && d.hasExitPeer() {
		return StatusRelayed
	}
	return StatusOffGrid
}

// Run, ctx iptaline kadar periyodik olarak durumu tazeler. Ayri goroutine'de
// cagrilmalidir. Ilk olcumu hemen yapar.
func (d *Detector) Run(ctx context.Context) {
	d.Refresh(ctx)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Refresh(ctx)
		}
	}
}
