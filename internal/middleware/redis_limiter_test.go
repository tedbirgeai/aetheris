package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Bu testler CANLI Redis gerektirir. AETHERIS_TEST_REDIS tanimli degilse
// atlanir. CI'da gercek Redis 7 konteynerine karsi calisir.
//
//   AETHERIS_TEST_REDIS=127.0.0.1:6399 go test -race ./internal/middleware/

func redisAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("AETHERIS_TEST_REDIS")
	if addr == "" {
		t.Skip("AETHERIS_TEST_REDIS tanimsiz - Redis testi atlandi")
	}
	return addr
}

func nopLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newRedisLim(t *testing.T, perMin, burst int) *RedisLimiter {
	t.Helper()
	rl, err := NewRedisLimiter(context.Background(), RedisLimiterConfig{
		Addr:      redisAddr(t),
		PerMinute: perMin,
		Burst:     burst,
		Logger:    nopLog(),
	})
	if err != nil {
		t.Fatalf("Redis limiter kurulamadi: %v", err)
	}
	// Her test icin temiz anahtar uzayi.
	rl.client.FlushDB(context.Background())
	t.Cleanup(func() { rl.Stop() })
	return rl
}

// TestRedisLimiterBurstThenBlock, burst sonrasi engellemeyi gercek
// Redis'te dogrular.
func TestRedisLimiterBurstThenBlock(t *testing.T) {
	rl := newRedisLim(t, 60, 3)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if !rl.allow(ctx, "acme") {
			t.Fatalf("%d. istek burst icinde reddedildi", i+1)
		}
	}
	if rl.allow(ctx, "acme") {
		t.Fatal("burst asildiktan sonra istek kabul edildi")
	}
}

// TestRedisLimiterIsolatesClients, istemcilerin ayri kovalari oldugunu
// dogrular.
func TestRedisLimiterIsolatesClients(t *testing.T) {
	rl := newRedisLim(t, 60, 2)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_ = rl.allow(ctx, "acme")
	}
	if rl.allow(ctx, "acme") {
		t.Fatal("acme limiti calismadi")
	}
	if !rl.allow(ctx, "globex") {
		t.Fatal("globex, acme'nin limitinden etkilendi")
	}
}

// TestRedisLimiterAtomicUnderConcurrency, es zamanli isteklerde Lua
// betiginin atomikligini dogrular. Kapasiteden fazla istek GECMEMELI.
// Bu testin degeri: naif "oku-hesapla-yaz" burada patlardi.
func TestRedisLimiterAtomicUnderConcurrency(t *testing.T) {
	rl := newRedisLim(t, 60, 20)
	ctx := context.Background()

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rl.allow(ctx, "acme") {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	got := allowed.Load()
	// Kapasite 20; kisa surede yenilenme ihmal edilebilir. Atomik degilse
	// bu sayi 20'yi asardi.
	if got < 18 || got > 25 {
		t.Fatalf("izin verilen = %d, ~20 bekleniyordu (atomiklik ihlali?)", got)
	}
}

// TestRedisLimiterSharedAcrossInstances, IKI AYRI limiter ornegi (iki
// dugum gibi) ayni Redis'i paylastiginda limitin ORTAK oldugunu dogrular.
// Dagitik rate limiting'in tum amaci budur.
func TestRedisLimiterSharedAcrossInstances(t *testing.T) {
	addr := redisAddr(t)
	ctx := context.Background()

	mk := func() *RedisLimiter {
		rl, err := NewRedisLimiter(ctx, RedisLimiterConfig{
			Addr: addr, PerMinute: 60, Burst: 4, Logger: nopLog(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return rl
	}

	node1 := mk()
	defer node1.Stop()
	node1.client.FlushDB(ctx)

	node2 := mk()
	defer node2.Stop()

	// Kapasite 4. Iki dugume dagitilmis 4 istek gecmeli, 5. reddedilmeli.
	allowed := 0
	limiters := []*RedisLimiter{node1, node2, node1, node2, node1}
	for _, n := range limiters {
		if n.allow(ctx, "shared-client") {
			allowed++
		}
	}
	if allowed != 4 {
		t.Fatalf("iki dugum toplam %d istek gecirdi, beklenen 4 (ortak limit calismiyor)", allowed)
	}
}

// TestRedisLimiterFallsBackToMemory, Redis erisilemez oldugunda bellek
// fallback'inin devreye girdigini dogrular. forceUnhealthy ile Redis
// yolunu atlatiyoruz.
func TestRedisLimiterFallsBackToMemory(t *testing.T) {
	rl := newRedisLim(t, 60, 3)
	ctx := context.Background()

	rl.forceUnhealthy() // Redis "dusuk" simule et

	// Artik fallback (bellek) devrede. Yine de calismali.
	allowed := 0
	for i := 0; i < 5; i++ {
		if rl.allow(ctx, "acme") {
			allowed++
		}
	}
	// Bellek fallback kapasitesi de 3, yani 3 gecmeli.
	if allowed != 3 {
		t.Fatalf("fallback %d istek gecirdi, beklenen 3", allowed)
	}
	if rl.Healthy() {
		t.Fatal("forceUnhealthy sonrasi hala saglikli gorunuyor")
	}
}

// TestRedisLimiterMiddleware429, HTTP katmaninda 429 dondugunu dogrular.
func TestRedisLimiterMiddleware429(t *testing.T) {
	rl := newRedisLim(t, 60, 1)

	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), Auth(map[string]string{validKey: "acme"}), rl.Middleware)

	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Authorization", "Bearer "+validKey)
		return r
	}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newReq())
	if rec1.Code != http.StatusOK {
		t.Fatalf("ilk istek = %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newReq())
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("ikinci istek = %d, beklenen 429", rec2.Code)
	}
}

// TestRedisLimiterRefills, token yenilenmesini gercek Redis'te dogrular.
func TestRedisLimiterRefills(t *testing.T) {
	rl := newRedisLim(t, 6000, 1) // saniyede 100 token
	ctx := context.Background()

	if !rl.allow(ctx, "acme") {
		t.Fatal("ilk istek reddedildi")
	}
	if rl.allow(ctx, "acme") {
		t.Fatal("kapasite 1 iken ikinci istek gecti")
	}
	time.Sleep(30 * time.Millisecond) // ~3 token birikir
	if !rl.allow(ctx, "acme") {
		t.Fatal("token yenilenmedi")
	}
}
