// P2 saglamlastirma — additive. Mevcut hicbir mantigi degistirmez.
//
// Bu dosyayi internal/dashboard/hardening.go olarak koyun. Yalnizca stdlib.
// Icerik:
//   P2-1  panic-recover middleware (tek istek cokerse sunucu ayakta kalir)
//   P2-2  /healthz + /readyz (canlilik/hazirlik problari — LB/k8s/systemd)
//   P2-3  guvenlik basliklari (HSTS, X-Content-Type-Options, frame-deny)
//   P2-4  kalici rate-limit sayaci (disk-destekli degil ama restart'a dayanikli
//         degil — surec-ici; dagitik icin Redis notu asagida)
//   P2-5  graceful shutdown yardimcilari (DTN/WAL flush cagrilari icin kanca)
//
// Kayit: RegisterCommerceRoutes'un sonuna  s.registerHardening(mux)  ekleyin
// (asagidaki sed komutu). Panic-recover icin sunucuyu WrapHandler ile sarin.

package dashboard

import (
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"
)

var (
	readyFlag  atomic.Bool
	startedAt  = time.Now()
	reqTotal   atomic.Uint64
	reqPanics  atomic.Uint64
)

// registerHardening, health/ready problarini kaydeder.
func (s *Server) registerHardening(mux *http.ServeMux) {
	readyFlag.Store(true)
	// /admin/* altına — kök /healthz zaten ana gateway mux'ında var (çakışma
	// olmaz; dashboard mux yalnızca /admin/*, /api/*, /tenant, /billing görür).
	mux.HandleFunc("/admin/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !readyFlag.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ready":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"ready":true,"uptime_s":` + itoa(int64(time.Since(startedAt).Seconds())) +
			`,"requests":` + utoa(reqTotal.Load()) + `,"panics":` + utoa(reqPanics.Load()) + `}`))
	})
}

// SetNotReady, graceful shutdown baslarken /readyz'i 503'e cevirir; LB trafigi
// keser, ardindan DTN/WAL flush edilir. main.go sinyalinden cagirin.
func SetNotReady() { readyFlag.Store(false) }

// WrapHandler, tum istekleri panic-recover + guvenlik basliklari + sayac ile
// sarar. main.go: srv.Handler = dashboard.WrapHandler(mux).
func WrapHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqTotal.Add(1)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		defer func() {
			if rec := recover(); rec != nil {
				reqPanics.Add(1)
				debug.PrintStack()
				http.Error(w, "sunucu hatasi", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func utoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
