package router

import (
	"math"
	"sort"
	"sync"
	"time"
)

// QoS metrikleri — DURUSTLUK NOTU
//
// Bu paket HTTP saglik yoklamalarindan turetilen metrikleri raporlar.
// Adlandirmalar bilincli olarak dikkatlidir:
//
//   RTT      : Istek gonderiminden yanit alinmasina kadar gecen sure.
//              Uygulama katmani gidis-donus suresidir; ICMP ping degildir.
//              TLS handshake, sunucu isleme suresi ve kuyruk gecikmesi
//              bu degere DAHILDIR.
//
//   Jitter   : Ardisik RTT olcumleri arasindaki ortalama mutlak fark
//              (RFC 3550'deki gibi ustel yumusatma degil, basit ortalama).
//              Ag kararliliginin gostergesidir.
//
//   ProbeFailureRatio : Basarisiz yoklamalarin toplam yoklamaya orani.
//
// BU "PAKET KAYBI" DEGILDIR ve oyle adlandirilmamistir.
// Gercek paket kaybi olcumu ICMP veya UDP seviyesinde, tekil paketleri
// sayarak yapilir. Bir HTTP yoklamasinin basarisiz olmasi paket kaybina
// isaret EDEBILIR, ama ayni zamanda sunucu asiri yuku, TLS hatasi,
// zaman asimi veya uygulama hatasi da olabilir. Bu degeri "packet loss"
// diye sunmak musteriyi yaniltmak olurdu.
//
// Gercek paket kaybi olcumu isteniyorsa ayri bir ICMP prober gerekir;
// bu, ham soket yetkisi (Linux'ta CAP_NET_RAW) ister ve konteynerde
// varsayilan olarak mevcut degildir.

// qosWindow, hareketli pencerede tutulan azami olcum sayisi.
const qosWindow = 50

// QoSMetrics, tek bir rota icin olculen ag kalitesi degerleridir.
type QoSMetrics struct {
	RouteName string `json:"route_name"`

	// Samples, penceredeki olcum sayisi.
	Samples int `json:"samples"`
	// ProbesTotal / ProbesFailed, kumulatif sayaclardir.
	ProbesTotal  uint64 `json:"probes_total"`
	ProbesFailed uint64 `json:"probes_failed"`

	// RTT degerleri milisaniye cinsinden.
	RTTLastMS float64 `json:"rtt_last_ms"`
	RTTAvgMS  float64 `json:"rtt_avg_ms"`
	RTTMinMS  float64 `json:"rtt_min_ms"`
	RTTMaxMS  float64 `json:"rtt_max_ms"`
	RTTP50MS  float64 `json:"rtt_p50_ms"`
	RTTP95MS  float64 `json:"rtt_p95_ms"`

	// JitterMS, ardisik RTT farklarinin ortalamasi.
	JitterMS float64 `json:"jitter_ms"`

	// ProbeFailureRatio, [0,1]. "Paket kaybi" DEGILDIR — yukaridaki nota bakin.
	ProbeFailureRatio float64 `json:"probe_failure_ratio"`

	// Healthy, failover mantiginin gordugu saglik durumu.
	Healthy bool `json:"healthy"`

	LastProbeUnix int64 `json:"last_probe_unix"`
}

// qosTracker, tek bir rotanin olcumlerini biriktirir.
type qosTracker struct {
	mu       sync.Mutex
	rtts     []time.Duration // hareketli pencere
	total    uint64
	failed   uint64
	lastRTT  time.Duration
	lastTime time.Time
}

func newQoSTracker() *qosTracker {
	return &qosTracker{rtts: make([]time.Duration, 0, qosWindow)}
}

// record, tek bir yoklamanin sonucunu isler.
func (q *qosTracker) record(rtt time.Duration, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.total++
	q.lastTime = time.Now()

	if !ok {
		q.failed++
		// Basarisiz yoklamanin RTT'si anlamsizdir (zaman asimi suresi
		// olurdu ve ortalamayi carpitirdi); pencereye EKLENMEZ.
		return
	}

	q.lastRTT = rtt
	q.rtts = append(q.rtts, rtt)
	if len(q.rtts) > qosWindow {
		q.rtts = q.rtts[1:]
	}
}

// snapshot, o anki metrikleri hesaplar.
func (q *qosTracker) snapshot(name string, healthy bool) QoSMetrics {
	q.mu.Lock()
	defer q.mu.Unlock()

	m := QoSMetrics{
		RouteName:     name,
		Samples:       len(q.rtts),
		ProbesTotal:   q.total,
		ProbesFailed:  q.failed,
		Healthy:       healthy,
		RTTLastMS:     msOf(q.lastRTT),
		LastProbeUnix: q.lastTime.Unix(),
	}
	if q.total > 0 {
		m.ProbeFailureRatio = float64(q.failed) / float64(q.total)
	}
	if len(q.rtts) == 0 {
		return m
	}

	sorted := make([]time.Duration, len(q.rtts))
	copy(sorted, q.rtts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	m.RTTMinMS = msOf(sorted[0])
	m.RTTMaxMS = msOf(sorted[len(sorted)-1])
	m.RTTP50MS = msOf(percentile(sorted, 0.50))
	m.RTTP95MS = msOf(percentile(sorted, 0.95))

	var sum time.Duration
	for _, d := range q.rtts {
		sum += d
	}
	m.RTTAvgMS = msOf(sum / time.Duration(len(q.rtts)))

	// Jitter: ardisik olcumlerin mutlak farklarinin ortalamasi.
	if len(q.rtts) > 1 {
		var diffSum float64
		for i := 1; i < len(q.rtts); i++ {
			diffSum += math.Abs(float64(q.rtts[i] - q.rtts[i-1]))
		}
		m.JitterMS = diffSum / float64(len(q.rtts)-1) / float64(time.Millisecond)
	}
	return m
}

func msOf(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// percentile, SIRALI dilimden yuzdelik deger dondurur.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// QoS, tum rotalarin metriklerini dondurur.
func (hp *HealthProber) QoS() []QoSMetrics {
	hp.mu.RLock()
	names := make([]string, 0, len(hp.qos))
	for name := range hp.qos {
		names = append(names, name)
	}
	hp.mu.RUnlock()
	sort.Strings(names)

	out := make([]QoSMetrics, 0, len(names))
	for _, name := range names {
		hp.mu.RLock()
		tr := hp.qos[name]
		hp.mu.RUnlock()
		if tr == nil {
			continue
		}
		out = append(out, tr.snapshot(name, hp.IsHealthy(name)))
	}
	return out
}
