package router

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tedbirgeai/aetheris/internal/carrier"
)

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func parseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// healthyServer, /healthz'e 200 donen bir upstream.
func healthyServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
}

// toggleServer, saglik durumu calisma aninda degistirilebilen upstream.
func toggleServer(t *testing.T, healthy *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if healthy.Load() {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		// tunel istegi
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ack"))
	}))
}

// TestProberDetectsUnhealthyRoute, prober'in sagliksiz rotayi tespit
// ettigini dogrular.
func TestProberDetectsUnhealthyRoute(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := toggleServer(t, &healthy)
	defer srv.Close()

	r := New([]Route{
		{Name: "edge-1", Kind: "edge", Upstream: parseURL(t, srv.URL)},
	}, 5*time.Second)

	hp := NewHealthProber(r, ProberConfig{
		Interval:      30 * time.Millisecond,
		Timeout:       time.Second,
		FailThreshold: 2,
		Logger:        silentLog(),
	})
	hp.Start()
	defer hp.Stop()

	// Baslangicta saglikli olmali.
	waitFor(t, func() bool { return hp.IsHealthy("edge-1") }, time.Second)

	// Simdi sagliksiz yap.
	healthy.Store(false)
	waitFor(t, func() bool { return !hp.IsHealthy("edge-1") }, 2*time.Second)

	// Tekrar saglikli yap.
	healthy.Store(true)
	waitFor(t, func() bool { return hp.IsHealthy("edge-1") }, 2*time.Second)
}

// TestFailoverToBackup, sagliksiz rotanin tanimli yedegine dustugunu
// dogrular.
func TestFailoverToBackup(t *testing.T) {
	var primaryHealthy atomic.Bool
	primaryHealthy.Store(true)
	primary := toggleServer(t, &primaryHealthy)
	defer primary.Close()

	backup := healthyServer(t)
	defer backup.Close()

	r := New([]Route{
		{Name: "primary", Kind: "edge", Upstream: parseURL(t, primary.URL), Backup: "backup"},
		{Name: "backup", Kind: "edge", Upstream: parseURL(t, backup.URL)},
	}, 5*time.Second)

	hp := NewHealthProber(r, ProberConfig{
		Interval:      30 * time.Millisecond,
		Timeout:       time.Second,
		FailThreshold: 2,
		Logger:        silentLog(),
	})
	hp.Start()
	defer hp.Stop()

	// Primary saglikliyken hedef degismemeli.
	target, failedOver := hp.ResolveTarget("primary")
	if target != "primary" || failedOver {
		t.Fatalf("saglikli primary icin failover olmamali: target=%s failedOver=%v", target, failedOver)
	}

	// Primary'yi dusur.
	primaryHealthy.Store(false)
	waitFor(t, func() bool { return !hp.IsHealthy("primary") }, 2*time.Second)

	// Artik backup'a dusmeli.
	target, failedOver = hp.ResolveTarget("primary")
	if target != "backup" || !failedOver {
		t.Fatalf("failover backup'a dusmedi: target=%s failedOver=%v", target, failedOver)
	}
}

// TestFailoverToDirect, yedegi olmayan sagliksiz rotanin direct hatta
// dustugunu dogrular.
func TestFailoverToDirect(t *testing.T) {
	var edgeHealthy atomic.Bool
	edgeHealthy.Store(true)
	edge := toggleServer(t, &edgeHealthy)
	defer edge.Close()

	direct := healthyServer(t)
	defer direct.Close()

	r := New([]Route{
		{Name: "edge-1", Kind: "edge", Upstream: parseURL(t, edge.URL)},
		{Name: "fallback-direct", Kind: "direct", Upstream: parseURL(t, direct.URL)},
	}, 5*time.Second)

	hp := NewHealthProber(r, ProberConfig{
		Interval:      30 * time.Millisecond,
		Timeout:       time.Second,
		FailThreshold: 2,
		Logger:        silentLog(),
	})
	hp.Start()
	defer hp.Stop()

	edgeHealthy.Store(false)
	waitFor(t, func() bool { return !hp.IsHealthy("edge-1") }, 2*time.Second)

	target, failedOver := hp.ResolveTarget("edge-1")
	if target != "fallback-direct" || !failedOver {
		t.Fatalf("direct'e dusme basarisiz: target=%s failedOver=%v", target, failedOver)
	}
}

// TestForwardWithFailoverDelivers, failover sonrasi istegin gercekten
// yedek upstream'e ulastigini uctan uca dogrular.
func TestForwardWithFailoverDelivers(t *testing.T) {
	var primaryHealthy atomic.Bool
	primaryHealthy.Store(false) // primary bastan dusuk
	primary := toggleServer(t, &primaryHealthy)
	defer primary.Close()

	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		backupHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backup-ack"))
	}))
	defer backup.Close()

	r := New([]Route{
		{Name: "primary", Kind: "edge", Upstream: parseURL(t, primary.URL), Backup: "backup"},
		{Name: "backup", Kind: "edge", Upstream: parseURL(t, backup.URL)},
	}, 5*time.Second)

	hp := NewHealthProber(r, ProberConfig{
		Interval:      30 * time.Millisecond,
		Timeout:       time.Second,
		FailThreshold: 2,
		Logger:        silentLog(),
	})
	hp.Start()
	defer hp.Stop()

	// Primary'nin sagliksiz isaretlenmesini bekle.
	waitFor(t, func() bool { return !hp.IsHealthy("primary") }, 2*time.Second)

	res, failedOver, err := hp.ForwardWithFailover(
		context.Background(), "primary", "acme", carrier.MeshWiFi, []byte("yuk"))
	if err != nil {
		t.Fatalf("failover forward hata dondu: %v", err)
	}
	if !failedOver {
		t.Fatal("failover bayragi false dondu")
	}
	if res.RouteName != "backup" {
		t.Fatalf("istek backup'a gitmedi: %s", res.RouteName)
	}
	if backupHits.Load() != 1 {
		t.Fatalf("backup %d kez cagrildi, beklenen 1", backupHits.Load())
	}
}

// TestResolveTargetUnknownRoute, tanimsiz hedef icin failover
// yapilmadigini dogrular (Forward zaten ErrNoRoute donecek).
func TestResolveTargetUnknownRoute(t *testing.T) {
	r := New([]Route{
		{Name: "edge-1", Kind: "edge", Upstream: parseURL(t, "https://example.com")},
	}, time.Second)
	hp := NewHealthProber(r, ProberConfig{Logger: silentLog()})

	target, failedOver := hp.ResolveTarget("olmayan")
	if target != "olmayan" || failedOver {
		t.Fatalf("tanimsiz hedef degismemeli: target=%s failedOver=%v", target, failedOver)
	}
}

// waitFor, kosul saglanana kadar (veya timeout) bekler.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("kosul %v icinde saglanmadi", timeout)
}

// --- QoS metrikleri ---

// TestQoSMeasuresRealRTT, RTT'nin GERCEKTEN olculdugunu dogrular.
// Yapay gecikmeli bir upstream kullanip olculen degerin gercek
// gecikmeyi yansittigini kontrol eder.
func TestQoSMeasuresRealRTT(t *testing.T) {
	const artificialDelay = 40 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(artificialDelay)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New([]Route{{Name: "slow", Kind: "edge", Upstream: parseURL(t, srv.URL)}}, 5*time.Second)
	hp := NewHealthProber(r, ProberConfig{
		Interval: 30 * time.Millisecond, Timeout: 2 * time.Second, Logger: silentLog(),
	})
	hp.Start()
	defer hp.Stop()

	// Birkac olcum birikmesini bekle.
	waitFor(t, func() bool {
		q := hp.QoS()
		return len(q) > 0 && q[0].Samples >= 3
	}, 3*time.Second)

	q := hp.QoS()[0]
	if q.RTTAvgMS < 30 {
		t.Fatalf("olculen RTT = %.1fms, gercek gecikme %v - olcum gercek degil",
			q.RTTAvgMS, artificialDelay)
	}
	if q.RTTMinMS <= 0 || q.RTTMaxMS <= 0 {
		t.Fatalf("min/max RTT olculmedi: %+v", q)
	}
	if q.RTTP95MS < q.RTTP50MS {
		t.Fatalf("p95 (%.1f) < p50 (%.1f) - yuzdelik hesabi bozuk",
			q.RTTP95MS, q.RTTP50MS)
	}
}

// TestQoSProbeFailureRatioIsNotPacketLoss, basarisiz yoklamalarin
// oraninin dogru hesaplandigini dogrular.
//
// ADLANDIRMA NOTU: Bu deger "paket kaybi" DEGILDIR ve alan adi da oyle
// degildir (ProbeFailureRatio). Bu test, adin degistirilip "packet loss"
// yapilmasi durumunda bile ne olctugunun net kalmasi icin buradadir.
func TestQoSProbeFailureRatioIsNotPacketLoss(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(false) // bastan sagliksiz
	srv := toggleServer(t, &healthy)
	defer srv.Close()

	r := New([]Route{{Name: "flaky", Kind: "edge", Upstream: parseURL(t, srv.URL)}}, 5*time.Second)
	hp := NewHealthProber(r, ProberConfig{
		Interval: 25 * time.Millisecond, Timeout: time.Second,
		FailThreshold: 2, Logger: silentLog(),
	})
	hp.Start()
	defer hp.Stop()

	waitFor(t, func() bool {
		q := hp.QoS()
		return len(q) > 0 && q[0].ProbesTotal >= 4
	}, 3*time.Second)

	q := hp.QoS()[0]
	// Tum yoklamalar basarisiz -> oran 1.0'a yakin olmali.
	if q.ProbeFailureRatio < 0.9 {
		t.Fatalf("ProbeFailureRatio = %.2f, tum yoklamalar basarisizken ~1.0 beklenirdi",
			q.ProbeFailureRatio)
	}
	if q.ProbesFailed == 0 {
		t.Fatal("ProbesFailed sayaci artmadi")
	}
	// Basarisiz yoklamalarin RTT'si ortalamaya KARISMAMALI.
	if q.Samples != 0 {
		t.Fatalf("basarisiz yoklamalar RTT penceresine eklendi: %d ornek", q.Samples)
	}
}

// TestQoSJitterCalculated, jitter'in hesaplandigini dogrular.
func TestQoSJitterCalculated(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Degisken gecikme -> jitter olusur.
		if n.Add(1)%2 == 0 {
			time.Sleep(50 * time.Millisecond)
		} else {
			time.Sleep(5 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := New([]Route{{Name: "jittery", Kind: "edge", Upstream: parseURL(t, srv.URL)}}, 5*time.Second)
	hp := NewHealthProber(r, ProberConfig{
		Interval: 30 * time.Millisecond, Timeout: 2 * time.Second, Logger: silentLog(),
	})
	hp.Start()
	defer hp.Stop()

	waitFor(t, func() bool {
		q := hp.QoS()
		return len(q) > 0 && q[0].Samples >= 4
	}, 4*time.Second)

	q := hp.QoS()[0]
	if q.JitterMS <= 0 {
		t.Fatalf("degisken gecikmede jitter olculmedi: %.2f", q.JitterMS)
	}
}

func TestQoSHealthyFlagMatchesProber(t *testing.T) {
	srv := healthyServer(t)
	defer srv.Close()

	r := New([]Route{{Name: "ok", Kind: "edge", Upstream: parseURL(t, srv.URL)}}, 5*time.Second)
	hp := NewHealthProber(r, ProberConfig{
		Interval: 30 * time.Millisecond, Timeout: time.Second, Logger: silentLog(),
	})
	hp.Start()
	defer hp.Stop()

	waitFor(t, func() bool {
		q := hp.QoS()
		return len(q) > 0 && q[0].ProbesTotal > 0
	}, 2*time.Second)

	q := hp.QoS()[0]
	if q.Healthy != hp.IsHealthy("ok") {
		t.Fatal("QoS Healthy alani prober ile uyusmuyor")
	}
}
