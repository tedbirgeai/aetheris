// P3 metrikleri — additive, stdlib-only Prometheus text exposition.
//
// internal/dashboard/metrics.go olarak koyun. /metrics ucunu (token korumali)
// Prometheus scrape formatinda yayinlar. Harici bagimlilik YOK — Prometheus
// client kutuphanesi gerektirmez; text format elle uretilir.
//
// Kayit: registerHardening'in yanina  s.registerMetrics(mux)  (P3.md'de sed).

package dashboard

import (
	"net/http"
	"os"
	"strconv"
	"time"
)

// MetricsProvider, gateway'in anlik sayaclarini metrige besler (opsiyonel).
// main.go SetMetricsProvider ile canli telemetri kaynagini baglayabilir.
type MetricsProvider func() Telemetry

var metricsProvider MetricsProvider

// SetMetricsProvider, /metrics'in okuyacagi canli telemetri kaynagini ayarlar.
func SetMetricsProvider(p MetricsProvider) { metricsProvider = p }

func (s *Server) registerMetrics(mux *http.ServeMux) {
	// /admin/metrics — dashboard mux'ın gördüğü yol (kök /metrics ana gateway
	// mux'ına gider ve dashboard'a ulaşmaz; /admin/* prefix'i forward edilir).
	mux.HandleFunc("/admin/metrics", func(w http.ResponseWriter, r *http.Request) {
		// Ayri metrik jetonu (yoksa admin jetonu). Prometheus scrape icin.
		tok := os.Getenv("AETHERIS_METRICS_TOKEN")
		if tok == "" {
			tok = s.cfg.AdminToken
		}
		if q := r.URL.Query().Get("token"); q == "" || !constTimeEq(q, tok) {
			if h := r.Header.Get("Authorization"); h != "Bearer "+tok {
				s.deny(w)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		b := metricsBuf{}
		b.help("aetheris_up", "gateway ayakta (1)")
		b.gauge("aetheris_up", 1)
		b.help("aetheris_uptime_seconds", "surec calisma suresi")
		b.gauge("aetheris_uptime_seconds", float64(int64(time.Since(startedAt).Seconds())))
		b.help("aetheris_requests_total", "toplam HTTP istek")
		b.counter("aetheris_requests_total", float64(reqTotal.Load()))
		b.help("aetheris_request_panics_total", "recover edilen panik")
		b.counter("aetheris_request_panics_total", float64(reqPanics.Load()))
		if metricsProvider != nil {
			t := metricsProvider()
			b.help("aetheris_throughput_bytes_per_second", "anlik bant genisligi")
			b.gauge("aetheris_throughput_bytes_per_second", float64(t.ThroughputBps))
			b.help("aetheris_wal_depth", "WAL kuyruk derinligi")
			b.gauge("aetheris_wal_depth", float64(t.WALDepth))
			b.help("aetheris_mesh_nodes", "kesfedilen mesh dugumu")
			b.gauge("aetheris_mesh_nodes", float64(len(t.Nodes)))
			if t.SOCKS5 != nil {
				b.help("aetheris_socks5_active", "aktif SOCKS5 baglanti")
				b.gauge("aetheris_socks5_active", float64(t.SOCKS5.Active))
				b.help("aetheris_socks5_handled_total", "toplam SOCKS5 baglanti")
				b.counter("aetheris_socks5_handled_total", float64(t.SOCKS5.Handled))
			}
			if t.DTN != nil {
				b.help("aetheris_dtn_pending", "bekleyen DTN bundle")
				b.gauge("aetheris_dtn_pending", float64(t.DTN.Pending))
				b.help("aetheris_dtn_delivered_total", "iletilen DTN bundle")
				b.counter("aetheris_dtn_delivered_total", float64(t.DTN.Delivered))
			}
		}
		_, _ = w.Write([]byte(b.s))
	})
}

type metricsBuf struct{ s string }

func (b *metricsBuf) help(name, desc string) {
	b.s += "# HELP " + name + " " + desc + "\n# TYPE " + name + " gauge\n"
}
func (b *metricsBuf) gauge(name string, v float64) {
	b.s += name + " " + strconv.FormatFloat(v, 'f', -1, 64) + "\n"
}
func (b *metricsBuf) counter(name string, v float64) {
	b.s += name + " " + strconv.FormatFloat(v, 'f', -1, 64) + "\n"
}
