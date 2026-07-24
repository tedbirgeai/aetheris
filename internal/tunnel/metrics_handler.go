package tunnel

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/tedbirgeai/aetheris/internal/metrics"
)

// MetricsHandler, Prometheus text exposition formatinda metrik dondurur.
//
// GUVENLIK: Metrikler musteri kimliklerini ve kullanim hacimlerini
// icerir — bunlar ticari olarak hassastir. Bu yuzden:
//   - Token tanimliysa Bearer dogrulamasi yapilir
//   - Token tanimli DEGILSE uc nokta hic acilmaz (main.go'da kontrol)
//
// Prometheus'un kendisi Bearer token destekler (scrape_config'de
// authorization bolumu).
type MetricsHandler struct {
	Handler *Handler
	// Token bos ise bu handler hic kaydedilmemelidir.
	Token string
}

func (m *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "yalnizca GET", http.StatusMethodNotAllowed)
		return
	}

	// Sabit zamanli token kontrolu.
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(m.Token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="aetheris-metrics"`)
		http.Error(w, "yetkisiz", http.StatusUnauthorized)
		return
	}

	samples := m.collect(r.Context())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := metrics.Write(w, samples); err != nil {
		m.Handler.Logger.Error("metrik yazilamadi", "err", err)
	}
}

func (m *MetricsHandler) collect(ctx context.Context) []metrics.Sample {
	h := m.Handler
	var out []metrics.Sample

	// --- Gecit durumu ---
	out = append(out,
		metrics.Sample{
			Name: "aetheris_uptime_seconds", Type: "counter",
			Help:  "Gecidin calisma suresi.",
			Value: float64(h.Meter.UptimeSeconds()),
		},
		metrics.Sample{
			Name: "aetheris_active_tunnels", Type: "gauge",
			Help:  "Su anda acik tunel sayisi.",
			Value: float64(h.Meter.ActiveTunnels()),
		},
	)

	// --- Kullanim defteri ---
	if snap, err := h.Meter.Snapshot(ctx); err == nil {
		out = append(out,
			metrics.Sample{
				Name: "aetheris_bytes_in_total", Type: "counter",
				Help:  "Olculen toplam gelen bayt.",
				Value: float64(snap.TotalBytesIn),
			},
			metrics.Sample{
				Name: "aetheris_bytes_out_total", Type: "counter",
				Help:  "Olculen toplam giden bayt.",
				Value: float64(snap.TotalBytesOut),
			},
			metrics.Sample{
				Name: "aetheris_requests_total", Type: "counter",
				Help:  "Olculen toplam istek sayisi.",
				Value: float64(snap.TotalRequests),
			},
		)
		for clientID, e := range snap.Clients {
			lbl := map[string]string{"client_id": clientID}
			out = append(out,
				metrics.Sample{
					Name: "aetheris_client_bytes_in_total", Type: "counter",
					Help: "Istemci basina gelen bayt.", Labels: lbl,
					Value: float64(e.BytesIn),
				},
				metrics.Sample{
					Name: "aetheris_client_bytes_out_total", Type: "counter",
					Help: "Istemci basina giden bayt.", Labels: lbl,
					Value: float64(e.BytesOut),
				},
				metrics.Sample{
					Name: "aetheris_client_requests_total", Type: "counter",
					Help: "Istemci basina istek sayisi.", Labels: lbl,
					Value: float64(e.Requests),
				},
			)
		}
	}

	// --- QoS metrikleri (DURUST ADLANDIRMA) ---
	if h.Prober != nil {
		for _, q := range h.Prober.QoS() {
			lbl := map[string]string{"route": q.RouteName}
			healthy := 0.0
			if q.Healthy {
				healthy = 1.0
			}
			out = append(out,
				metrics.Sample{
					Name: "aetheris_route_healthy", Type: "gauge",
					Help: "Rota saglikli mi (1=evet).", Labels: lbl, Value: healthy,
				},
				metrics.Sample{
					Name: "aetheris_route_rtt_milliseconds", Type: "gauge",
					Help:   "Uygulama katmani gidis-donus suresi (ICMP ping DEGIL).",
					Labels: lbl, Value: q.RTTAvgMS,
				},
				metrics.Sample{
					Name: "aetheris_route_rtt_p95_milliseconds", Type: "gauge",
					Help: "RTT 95. yuzdelik.", Labels: lbl, Value: q.RTTP95MS,
				},
				metrics.Sample{
					Name: "aetheris_route_jitter_milliseconds", Type: "gauge",
					Help:   "Ardisik RTT farklarinin ortalamasi.",
					Labels: lbl, Value: q.JitterMS,
				},
				metrics.Sample{
					Name: "aetheris_route_probe_failure_ratio", Type: "gauge",
					// ADLANDIRMA NOTU: Bu PAKET KAYBI DEGILDIR. HTTP
					// yoklamalarinin basarisizlik oranidir. Gercek paket
					// kaybi ICMP/UDP seviyesinde olculur.
					Help:   "Basarisiz saglik yoklamalarinin orani (paket kaybi DEGIL).",
					Labels: lbl, Value: q.ProbeFailureRatio,
				},
				metrics.Sample{
					Name: "aetheris_route_probes_total", Type: "counter",
					Help: "Toplam saglik yoklamasi.", Labels: lbl,
					Value: float64(q.ProbesTotal),
				},
			)
		}
	}

	// --- Faturalama koprusu ---
	if h.Billing != nil {
		st := h.Billing.Stats()
		out = append(out,
			metrics.Sample{
				Name: "aetheris_billing_events_enqueued_total", Type: "counter",
				Help: "Kuyruga alinan fatura olayi.", Value: float64(st.Enqueued),
			},
			metrics.Sample{
				Name: "aetheris_billing_events_delivered_total", Type: "counter",
				Help: "Dis sisteme iletilen fatura olayi.", Value: float64(st.Delivered),
			},
			metrics.Sample{
				Name: "aetheris_billing_events_failed_total", Type: "counter",
				Help: "Iletilemeyen fatura olayi.", Value: float64(st.Failed),
			},
			metrics.Sample{
				Name: "aetheris_billing_events_dropped_total", Type: "counter",
				Help:  "Kuyruk dolulugu nedeniyle dusurulen olay.",
				Value: float64(st.Dropped),
			},
			metrics.Sample{
				Name: "aetheris_billing_queue_depth", Type: "gauge",
				Help: "Fatura kuyrugunda bekleyen olay.", Value: float64(st.Pending),
			},
		)
	}

	return out
}
