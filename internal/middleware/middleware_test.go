package middleware

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const validKey = "gecerli-anahtar-0123456789abcdefghij"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captureLogger, log satirlarini yakalayan bir logger dondurur.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func okHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

func TestAuthAcceptsValidKey(t *testing.T) {
	var seenClient string
	h := Auth(map[string]string{validKey: "acme"})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenClient, _ = ClientIDFrom(r.Context())
			w.WriteHeader(http.StatusOK)
		}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+validKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d", rec.Code)
	}
	if seenClient != "acme" {
		t.Fatalf("context'teki istemci = %q, beklenen acme", seenClient)
	}
}

func TestAuthRejects(t *testing.T) {
	cases := map[string]string{
		"baslik yok":     "",
		"yanlis sema":    "Basic " + validKey,
		"gecersiz key":   "Bearer yanlis-anahtar-0123456789abcdefgh",
		"bos bearer":     "Bearer ",
		"kismi eslesme":  "Bearer " + validKey[:20],
		"fazladan sonek": "Bearer " + validKey + "x",
	}
	h := Auth(map[string]string{validKey: "acme"})(okHandler())

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: durum = %d, beklenen 401", name, rec.Code)
			}
			// Hata mesaji anahtar hakkinda bilgi sizdirmamali.
			if strings.Contains(rec.Body.String(), validKey) {
				t.Fatal("SIZINTI: hata yanitinda gecerli anahtar goruldu")
			}
		})
	}
}

// TestLoggingCapturesClientIDAndUnauthorized, Logging'in Auth'un
// DISINDA olmasina ragmen istemci kimligini gordugunu ve basarisiz
// kimlik denemelerinin de loglandigini dogrular.
//
// Bu, holder deseninin varlik sebebidir: Auth r.WithContext() ile yeni
// bir request uretir; naif bir zincirde Logging o context'i goremez ve
// denetim izi bos client_id ile ise yaramaz hale gelir.
func TestLoggingCapturesClientIDAndUnauthorized(t *testing.T) {
	cap := &logCapture{}
	logger := slog.New(slog.NewJSONHandler(cap, nil))

	h := Chain(okHandler(),
		Logging(logger),
		Auth(map[string]string{validKey: "acme"}),
	)

	// 1) Basarili istek -> client_id loglanmali
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tunnel", nil)
	req.Header.Set("Authorization", "Bearer "+validKey)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// 2) Basarisiz istek -> 401 loglanmali
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/tunnel", nil)
	req2.Header.Set("Authorization", "Bearer kotu-anahtar-0123456789abcdefg")
	h.ServeHTTP(httptest.NewRecorder(), req2)

	out := cap.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("2 log satiri bekleniyordu, %d alindi:\n%s", len(lines), out)
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("ilk log satiri cozulemedi: %v", err)
	}
	if first["client_id"] != "acme" {
		t.Fatalf("basarili istekte client_id = %v, beklenen acme", first["client_id"])
	}
	if first["status"].(float64) != 200 {
		t.Fatalf("basarili istekte status = %v", first["status"])
	}

	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("ikinci log satiri cozulemedi: %v", err)
	}
	if second["status"].(float64) != 401 {
		t.Fatalf("basarisiz istek loglanmadi, status = %v", second["status"])
	}
	if second["client_id"] != "" {
		t.Fatalf("dogrulanmamis istekte client_id dolu: %v", second["client_id"])
	}
}

// TestLoggingNeverLogsBody, gizlilik sozlesmesinin log katmaninda da
// gecerli oldugunu dogrular.
func TestLoggingNeverLogsBody(t *testing.T) {
	cap := &logCapture{}
	logger := slog.New(slog.NewJSONHandler(cap, nil))

	const secret = "COK-GIZLI-YUK-ICERIGI"
	h := Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(secret))
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(secret))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(cap.String(), secret) {
		t.Fatal("SIZINTI: govde icerigi loglandi")
	}
}

func TestRateLimiterAllowsBurstThenBlocks(t *testing.T) {
	rl := NewRateLimiter(60, 3)
	defer rl.Stop()

	for i := 0; i < 3; i++ {
		if !rl.allow("acme") {
			t.Fatalf("%d. istek burst icinde reddedildi", i+1)
		}
	}
	if rl.allow("acme") {
		t.Fatal("burst asildiktan sonra istek kabul edildi")
	}
}

// TestRateLimiterIsolatesClients, bir istemcinin limitinin digerini
// etkilemedigini dogrular.
func TestRateLimiterIsolatesClients(t *testing.T) {
	rl := NewRateLimiter(60, 2)
	defer rl.Stop()

	for i := 0; i < 2; i++ {
		_ = rl.allow("acme")
	}
	if rl.allow("acme") {
		t.Fatal("acme limiti calismadi")
	}
	if !rl.allow("globex") {
		t.Fatal("globex, acme'nin limitinden etkilendi")
	}
}

func TestRateLimiterRefills(t *testing.T) {
	// Dakikada 6000 -> saniyede 100 token. 20ms'de ~2 token birikir.
	rl := NewRateLimiter(6000, 1)
	defer rl.Stop()

	if !rl.allow("acme") {
		t.Fatal("ilk istek reddedildi")
	}
	if rl.allow("acme") {
		t.Fatal("kapasite 1 iken ikinci istek kabul edildi")
	}

	time.Sleep(30 * time.Millisecond)

	if !rl.allow("acme") {
		t.Fatal("token yenilenmedi")
	}
}

// TestRateLimiterConcurrent, es zamanli erisimde sayacin bozulmadigini
// dogrular. -race ile calistirilmalidir.
func TestRateLimiterConcurrent(t *testing.T) {
	rl := NewRateLimiter(60, 50)
	defer rl.Stop()

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rl.allow("acme") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Kapasite 50; yenilenme sirasinda birkac token daha eklenebilir,
	// ama 200'un tamami asla gecmemelidir.
	if allowed < 40 || allowed > 60 {
		t.Fatalf("izin verilen istek sayisi = %d, 40-60 araligi bekleniyordu", allowed)
	}
}

func TestRateLimiterMiddlewareReturns429(t *testing.T) {
	rl := NewRateLimiter(60, 1)
	defer rl.Stop()

	h := Chain(okHandler(), Auth(map[string]string{validKey: "acme"}), rl.Middleware)

	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+validKey)
		return r
	}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newReq())
	if rec1.Code != http.StatusOK {
		t.Fatalf("ilk istek durumu = %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newReq())
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("ikinci istek durumu = %d, beklenen 429", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After basligi eksik")
	}
}

// TestRecoverCatchesPanic, handler panik attiginda surecin ayakta
// kaldigini ve 500 dondugunu dogrular.
func TestRecoverCatchesPanic(t *testing.T) {
	h := Recover(quietLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("beklenmeyen durum")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("durum = %d, beklenen 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "beklenmeyen durum") {
		t.Fatal("SIZINTI: panik detayi istemciye dondu")
	}
}

// TestChainOrder, middleware'lerin soldan saga sarildigini dogrular.
func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	}), mk("A"), mk("B"), mk("C"))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"A", "B", "C", "handler"}
	if len(order) != len(want) {
		t.Fatalf("cagri sirasi = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("cagri sirasi = %v, beklenen %v", order, want)
		}
	}
}
