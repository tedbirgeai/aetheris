// QoS canlı bağlama — additive. Mevcut hiçbir mantığı değiştirmez.
//
// cmd/gateway/qoswire.go olarak koyun. HealthProber'ın topladığı GERÇEK QoS
// metriklerini (rota başına RTT/jitter/sağlık/probe oranı) hem JSON ucundan
// hem Prometheus /metrics formatından yayınlar. Prober yoksa (rota tanımsız
// veya AETHERIS_HEALTHPROBE=false) uçlar boş döner — yalan üretmez.
//
// KAYIT: main.go'da prober oluşturulduktan SONRA (registerEnrichment yanına):
//   registerQoS(mux, prober, cfg.AdminToken)
//
// AKTİVASYON (.env) — QoS'un veri toplaması için en az bunlar gerekir:
//   AETHERIS_HEALTHPROBE=true
//   AETHERIS_HEALTHPROBE_INTERVAL_SEC=5
//   AETHERIS_ROUTES=birincil=edge@https://1.1.1.1,yedek=edge@https://8.8.8.8
//   # (biçim: ad=tür@url,ad2=tür2@url2 — parseRoutes)
//   # Gerçek ICMP paket kaybı için (Linux): sudo setcap cap_net_raw+ep ./gw

package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/tedbirgeai/aetheris/internal/router"
)

// registerQoS, QoS uçlarını bağlar. prober nil olabilir (rota yoksa) — o
// durumda uçlar boş/anlamlı yanıt verir, panik etmez.
func registerQoS(mux *http.ServeMux, prober *router.HealthProber, adminToken string) {
	auth := func(r *http.Request) bool {
		return adminToken != "" && r.URL.Query().Get("token") == adminToken
	}

	// JSON: operatör paneli rota başına canlı QoS kartlarını buradan çeker.
	mux.HandleFunc("/admin/qos", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if prober == nil {
			// Dürüst durum: prober aktif değil (rota tanımsız veya kapalı).
			_, _ = w.Write([]byte(`{"active":false,"reason":"rota tanımsız veya AETHERIS_HEALTHPROBE=false","routes":[]}`))
			return
		}
		writeQoSJSON(w, prober.QoS())
	})

	// Prometheus text: /metrics'i tamamlar; rota başına etiketli metrik.
	mux.HandleFunc("/admin/qos/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if prober == nil {
			_, _ = w.Write([]byte("# QoS prober aktif değil (rota tanımsız)\n"))
			return
		}
		_, _ = w.Write([]byte(qosMetricsText(prober.QoS())))
	})
}

func writeQoSJSON(w http.ResponseWriter, m []router.QoSMetrics) {
	_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "routes": m})
}

// qosMetricsText, QoS metriklerini Prometheus text exposition formatına çevirir.
func qosMetricsText(m []router.QoSMetrics) string {
	b := &qosBuf{}
	b.line(`# HELP aetheris_route_rtt_ms Rota RTT (uygulama katmanı, ms)`)
	b.line(`# TYPE aetheris_route_rtt_ms gauge`)
	for _, q := range m {
		b.metric("aetheris_route_rtt_ms", q.RouteName, q.RTTAvgMS)
	}
	b.line(`# HELP aetheris_route_jitter_ms Rota jitter (ms)`)
	b.line(`# TYPE aetheris_route_jitter_ms gauge`)
	for _, q := range m {
		b.metric("aetheris_route_jitter_ms", q.RouteName, q.JitterMS)
	}
	b.line(`# HELP aetheris_route_rtt_p95_ms Rota RTT p95 (ms)`)
	b.line(`# TYPE aetheris_route_rtt_p95_ms gauge`)
	for _, q := range m {
		b.metric("aetheris_route_rtt_p95_ms", q.RouteName, q.RTTP95MS)
	}
	b.line(`# HELP aetheris_route_probe_failure_ratio Yoklama başarısızlık oranı [0,1]`)
	b.line(`# TYPE aetheris_route_probe_failure_ratio gauge`)
	for _, q := range m {
		b.metric("aetheris_route_probe_failure_ratio", q.RouteName, q.ProbeFailureRatio)
	}
	b.line(`# HELP aetheris_route_healthy Rota sağlıklı mı (1/0)`)
	b.line(`# TYPE aetheris_route_healthy gauge`)
	for _, q := range m {
		v := 0.0
		if q.Healthy {
			v = 1
		}
		b.metric("aetheris_route_healthy", q.RouteName, v)
	}
	return b.s
}

type qosBuf struct{ s string }

func (b *qosBuf) line(s string) { b.s += s + "\n" }
func (b *qosBuf) metric(name, route string, v float64) {
	b.s += name + `{route="` + escapeLabel(route) + `"} ` + strconv.FormatFloat(v, 'f', -1, 64) + "\n"
}

func escapeLabel(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			out = append(out, '\\', s[i])
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
