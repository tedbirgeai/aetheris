package router

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/tedbirge-labs/aetheris-gateway/internal/carrier"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestForwardDeliversOpaquePayload, yukun upstream'e BOZULMADAN
// ulastigini dogrular. Router icerigi cozmemeli, degistirmemelidir.
func TestForwardDeliversOpaquePayload(t *testing.T) {
	payload := []byte{0x00, 0xFF, 0x10, 0x42, 0x00, 0x99}

	var (
		mu       sync.Mutex
		gotBody  []byte
		gotHead  http.Header
		gotPath  string
		reqCount int
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		gotHead = r.Header.Clone()
		gotPath = r.URL.Path
		reqCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ack"))
	}))
	defer upstream.Close()

	r := New([]Route{{Name: "edge-1", Kind: "edge", Upstream: mustURL(t, upstream.URL+"/ingest")}},
		5*time.Second)

	res, err := r.Forward(context.Background(), "edge-1", "acme", carrier.OpticalLiFi, payload)
	if err != nil {
		t.Fatalf("Forward hata dondu: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if reqCount != 1 {
		t.Fatalf("upstream %d kez cagrildi, beklenen 1", reqCount)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("yuk bozuldu: gonderilen %v, alinan %v", payload, gotBody)
	}
	if gotPath != "/ingest" {
		t.Fatalf("upstream yolu = %q", gotPath)
	}
	if got := gotHead.Get("X-Aetheris-Carrier"); got != string(carrier.OpticalLiFi) {
		t.Fatalf("tasiyici basligi = %q", got)
	}
	if got := gotHead.Get("X-Aetheris-Client"); got != "acme" {
		t.Fatalf("istemci basligi = %q", got)
	}
	if got := gotHead.Get("X-Aetheris-Route-Kind"); got != "edge" {
		t.Fatalf("rota turu basligi = %q", got)
	}
	// API anahtari upstream'e ASLA sizmamalidir.
	if gotHead.Get("Authorization") != "" {
		t.Fatal("SIZINTI: Authorization basligi upstream'e iletildi")
	}

	if res.BytesSent != uint64(len(payload)) {
		t.Fatalf("BytesSent = %d, beklenen %d", res.BytesSent, len(payload))
	}
	if res.BytesReceived != 3 {
		t.Fatalf("BytesReceived = %d, beklenen 3", res.BytesReceived)
	}
	if res.RouteKind != "edge" || res.RouteName != "edge-1" {
		t.Fatalf("rota bilgisi hatali: %+v", res)
	}
	if res.UpstreamStatus != http.StatusOK {
		t.Fatalf("UpstreamStatus = %d", res.UpstreamStatus)
	}
}

// TestForwardDisabledWhenNoRoutes, rota tanimli degilken ErrDisabled
// dondugunu dogrular.
func TestForwardDisabledWhenNoRoutes(t *testing.T) {
	r := New(nil, time.Second)
	if r.Enabled() {
		t.Fatal("rota yokken Enabled() true dondu")
	}
	_, err := r.Forward(context.Background(), "herhangi", "acme", carrier.StandardInternet, []byte("x"))
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("ErrDisabled bekleniyordu, alinan: %v", err)
	}
}

// TestForwardUnknownDestination, tanimsiz hedef icin ErrNoRoute
// dondugunu dogrular.
func TestForwardUnknownDestination(t *testing.T) {
	r := New([]Route{{Name: "edge-1", Kind: "edge", Upstream: mustURL(t, "https://example.com")}},
		time.Second)
	_, err := r.Forward(context.Background(), "olmayan", "acme", carrier.StandardInternet, []byte("x"))
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("ErrNoRoute bekleniyordu, alinan: %v", err)
	}
}

// TestForwardRejectsIllegalCarrier, yasal olmayan tasiyicinin router
// katmaninda da reddedildigini dogrular (savunmada derinlik).
func TestForwardRejectsIllegalCarrier(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("yasadisi tasiyici icin upstream cagrilmamaliydi")
	}))
	defer upstream.Close()

	r := New([]Route{{Name: "e", Kind: "direct", Upstream: mustURL(t, upstream.URL)}}, time.Second)

	for _, bad := range []carrier.Type{"radio_rf", "satellite_dish", "izinsiz_dinleme"} {
		_, err := r.Forward(context.Background(), "e", "acme", bad, []byte("x"))
		if !errors.Is(err, ErrCarrierNotAllowed) {
			t.Fatalf("tasiyici %q icin ErrCarrierNotAllowed bekleniyordu, alinan: %v", bad, err)
		}
	}
}

// TestForwardUpstreamServerError, upstream 5xx dondugunde hata
// bildirildigini ama sonucun yine de dondugunu dogrular.
func TestForwardUpstreamServerError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	r := New([]Route{{Name: "e", Kind: "direct", Upstream: mustURL(t, upstream.URL)}}, time.Second)

	res, err := r.Forward(context.Background(), "e", "acme", carrier.StandardInternet, []byte("payload"))
	if err == nil {
		t.Fatal("upstream 5xx icin hata bekleniyordu")
	}
	if res == nil || res.UpstreamStatus != http.StatusBadGateway {
		t.Fatalf("sonuc upstream durumunu tasimali: %+v", res)
	}
}

// TestForwardTimeout, yavas upstream'in zaman asimina ugradigini dogrular.
func TestForwardTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	r := New([]Route{{Name: "e", Kind: "direct", Upstream: mustURL(t, upstream.URL)}},
		100*time.Millisecond)

	if _, err := r.Forward(context.Background(), "e", "acme", carrier.StandardInternet, []byte("x")); err == nil {
		t.Fatal("zaman asimi hatasi bekleniyordu")
	}
}

// TestForwardContextCancellation, cagiran taraf istegi iptal ettiginde
// yonlendirmenin de durdugunu dogrular.
func TestForwardContextCancellation(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	r := New([]Route{{Name: "e", Kind: "direct", Upstream: mustURL(t, upstream.URL)}}, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if _, err := r.Forward(ctx, "e", "acme", carrier.StandardInternet, []byte("x")); err == nil {
		t.Fatal("iptal hatasi bekleniyordu")
	}
}

// TestForwardConcurrent, es zamanli yonlendirmelerin birbirini
// bozmadigini dogrular. -race ile calistirilmalidir.
func TestForwardConcurrent(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]int)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen[string(b)]++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	r := New([]Route{{Name: "e", Kind: "peering", Upstream: mustURL(t, upstream.URL)}}, 5*time.Second)

	const n = 60
	var wg sync.WaitGroup
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte{byte(i)}
			if _, err := r.Forward(context.Background(), "e", "acme", carrier.MeshWiFi, body); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("es zamanli yonlendirme hatasi: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("upstream %d farkli govde gordu, beklenen %d", len(seen), n)
	}
}

// TestRoutesListing, health ucunun rota tablosunu dogru raporladigini
// dogrular.
func TestRoutesListing(t *testing.T) {
	r := New([]Route{
		{Name: "a", Kind: "edge", Upstream: mustURL(t, "https://a.example")},
		{Name: "b", Kind: "peering", Upstream: mustURL(t, "https://b.example")},
	}, time.Second)

	got := r.Routes()
	if got["a"] != "edge" || got["b"] != "peering" {
		t.Fatalf("rota tablosu hatali: %v", got)
	}
}
