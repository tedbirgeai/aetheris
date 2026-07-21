// Package middleware, HTTP katmaninin guvenlik ve gozlemlenebilirlik
// halkalarini icerir: kimlik dogrulama, hiz sinirlama, loglama, kurtarma.
package middleware

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type ctxKey string

const (
	clientIDKey     ctxKey = "aetheris.client_id"
	clientHolderKey ctxKey = "aetheris.client_holder"
)

// clientHolder, Logging'in Auth sonucunu gorebilmesini saglar.
//
// NEDEN GEREKLI: Auth, r.WithContext() ile YENI bir request nesnesi
// uretir. Zincirde disarida kalan Logging'in elindeki eski r, o
// context'i hicbir zaman gormez. Sonuc: dogru calisan bir kimlik
// dogrulamaya ragmen loglarda client_id bos kalir ve denetim izi
// (audit trail) ise yaramaz.
//
// Holder, request'e ISARETCI olarak konur; Auth ayni isaretciyi
// doldurur, Logging ayni isaretciyi okur. Boylece Logging zincirde
// Auth'un DISINDA kalabilir ve basarisiz kimlik denemeleri (401) de
// loglanir - anahtar deneme saldirilarini gormek icin sart.
type clientHolder struct {
	mu sync.Mutex
	id string
}

func (h *clientHolder) set(id string) {
	h.mu.Lock()
	h.id = id
	h.mu.Unlock()
}

func (h *clientHolder) get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
}

// ClientIDFrom, dogrulanmis istemci kimligini context'ten okur.
func ClientIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(clientIDKey).(string)
	return id, ok
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": msg})
}

// Auth, Authorization: Bearer <key> basligini dogrular.
//
// GUVENLIK: Karsilastirma sabit zamanlidir. Dogrudan map lookup,
// anahtar icerigi hakkinda zamanlama sizintisi yaratabilir; bu yuzden
// tum kayitli anahtarlar taranir ve erken cikis yapilmaz.
func Auth(apiKeys map[string]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(header, prefix) {
				writeJSONError(w, http.StatusUnauthorized, "eksik veya hatali Authorization basligi")
				return
			}
			presented := strings.TrimSpace(strings.TrimPrefix(header, prefix))

			var matchedClient string
			for key, clientID := range apiKeys {
				if subtle.ConstantTimeCompare([]byte(key), []byte(presented)) == 1 {
					matchedClient = clientID
				}
			}
			if matchedClient == "" {
				writeJSONError(w, http.StatusUnauthorized, "gecersiz API anahtari")
				return
			}

			if hd, ok := r.Context().Value(clientHolderKey).(*clientHolder); ok {
				hd.set(matchedClient)
			}

			ctx := context.WithValue(r.Context(), clientIDKey, matchedClient)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// --- Hiz Sinirlayici (Token Bucket) ---

type bucket struct {
	tokens   float64
	lastFill time.Time
}

// RateLimiter, istemci basina token bucket uygular.
// Tek dugumludur; cok dugumlu dagitimda Redis tabanli ortak sayaca gecilmelidir.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     float64 // saniyedeki token
	capacity float64
	stop     chan struct{}
	stopOnce sync.Once
}

func NewRateLimiter(perMinute, burst int) *RateLimiter {
	rl := &RateLimiter{
		buckets:  make(map[string]*bucket),
		rate:     float64(perMinute) / 60.0,
		capacity: float64(burst),
		stop:     make(chan struct{}),
	}
	go rl.janitor()
	return rl
}

// Stop, temizlik goroutine'ini sonlandirir. Testlerde goroutine
// sizintisini onlemek icin cagrilir.
func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() { close(rl.stop) })
}

// janitor, 10 dakikadir dokunulmamis kovalari temizler.
// Olmazsa her yeni istemci kimligi kalici bellek tuketir.
func (rl *RateLimiter) janitor() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-10 * time.Minute)
			rl.mu.Lock()
			for k, b := range rl.buckets {
				if b.lastFill.Before(cutoff) {
					delete(rl.buckets, k)
				}
			}
			rl.mu.Unlock()
		}
	}
}

func (rl *RateLimiter) allow(clientID string) bool {
	now := time.Now()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[clientID]
	if !ok {
		rl.buckets[clientID] = &bucket{tokens: rl.capacity - 1, lastFill: now}
		return true
	}

	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.capacity {
		b.tokens = rl.capacity
	}
	b.lastFill = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Middleware, Auth'tan SONRA zincirlenmelidir (istemci kimligine ihtiyac duyar).
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := ClientIDFrom(r.Context())
		if !ok {
			clientID = r.RemoteAddr
		}
		if !rl.allow(clientID) {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "hiz siniri asildi")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Loglama ve Kurtarma ---

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Logging, her istegi yapilandirilmis bicimde loglar.
//
// GIZLILIK: Istek/yanit GOVDESI ASLA loglanmaz - yalnizca metadata.
// Sifir bilgi sozlesmesi log katmaninda da gecerlidir.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			hd := &clientHolder{}
			r = r.WithContext(context.WithValue(r.Context(), clientHolderKey, hd))

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"resp_bytes", rec.bytes,
				"client_id", hd.get(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// Recover, handler panic'lerini yakalar ve sureci ayakta tutar.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered", "panic", rec, "path", r.URL.Path)
					writeJSONError(w, http.StatusInternalServerError, "ic sunucu hatasi")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Chain, middleware'leri soldan saga sarar.
// Chain(h, A, B, C) => A(B(C(h)))
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
