package tunnel_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/tedbirge-labs/aetheris-gateway/internal/meter"
	"github.com/tedbirge-labs/aetheris-gateway/internal/middleware"
	"github.com/tedbirge-labs/aetheris-gateway/internal/router"
	"github.com/tedbirge-labs/aetheris-gateway/internal/store"
	"github.com/tedbirge-labs/aetheris-gateway/internal/tunnel"
)

const (
	testKey    = "test-api-key-0123456789abcdefghijklmno"
	testSecret = "0123456789abcdef0123456789abcdef"
)

// failingStore, defter yazmalarinin basarisiz oldugu senaryoyu taklit eder.
type failingStore struct {
	store.Store
	failRecord bool
	failRead   bool
}

func (f *failingStore) Record(ctx context.Context, u store.Usage) error {
	if f.failRecord {
		return errors.New("veritabani ulasilamaz")
	}
	return f.Store.Record(ctx, u)
}

func (f *failingStore) ClientUsage(ctx context.Context, id string) (*store.Entry, error) {
	if f.failRead {
		return nil, errors.New("veritabani ulasilamaz")
	}
	return f.Store.ClientUsage(ctx, id)
}

type testEnv struct {
	handler http.Handler
	store   store.Store
	meter   *meter.Meter
}

func newEnv(t *testing.T, st store.Store, rtr *router.Router) *testEnv {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := meter.New(st)

	h := &tunnel.Handler{
		Meter:           m,
		Router:          rtr,
		Logger:          logger,
		MaxPayloadBytes: 4096,
		ReceiptSecret:   []byte(testSecret),
	}

	keys := map[string]string{testKey: "acme"}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/tunnel", middleware.Chain(http.HandlerFunc(h.Tunnel), middleware.Auth(keys)))
	mux.Handle("/api/v1/meter/me", middleware.Chain(http.HandlerFunc(h.MyUsage), middleware.Auth(keys)))
	mux.Handle("/healthz", http.HandlerFunc(h.Health))

	return &testEnv{handler: mux, store: st, meter: m}
}

// clientEncrypt, ISTEMCI tarafini taklit eder. Sifreleme sunucuda degil,
// burada yapilir - sifir bilgi sozlesmesinin somut karsiligi budur.
func clientEncrypt(t *testing.T, plaintext string) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil))
}

func post(t *testing.T, env *testEnv, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tunnel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTunnelHappyPath(t *testing.T) {
	env := newEnv(t, store.NewMemory(), router.New(nil, 0))
	ct := clientEncrypt(t, "gizli kurumsal veri")

	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
		CarrierType: "mesh_wifi",
		Ciphertext:  ct,
	}), true)

	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d, govde = %s", rec.Code, rec.Body.String())
	}

	var receipt tunnel.Receipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ClientID != "acme" {
		t.Fatalf("ClientID = %q", receipt.ClientID)
	}
	if receipt.CarrierUsed != "mesh_wifi" {
		t.Fatalf("CarrierUsed = %q", receipt.CarrierUsed)
	}
	if receipt.Status != "metered" {
		t.Fatalf("Status = %q, yonlendirme yokken metered bekleniyordu", receipt.Status)
	}
	if receipt.Route != nil {
		t.Fatal("yonlendirme yapilmadigi halde Route dolu")
	}

	raw, _ := base64.StdEncoding.DecodeString(ct)
	if receipt.MeteredBytes != uint64(len(raw)) {
		t.Fatalf("MeteredBytes = %d, beklenen %d", receipt.MeteredBytes, len(raw))
	}

	sum := sha256.Sum256(raw)
	if receipt.PayloadSHA != hex.EncodeToString(sum[:]) {
		t.Fatal("PayloadSHA yukun ozeti ile uyusmuyor")
	}

	e, err := env.store.ClientUsage(context.Background(), "acme")
	if err != nil || e.Requests != 1 {
		t.Fatalf("defter guncellenmedi: %v %+v", err, e)
	}
}

// TestReceiptSignatureIsVerifiable, istemcinin makbuzun degistirilmedigini
// dogrulayabildigini gosterir. Faturalama itirazlarinin dayanagi budur.
func TestReceiptSignatureIsVerifiable(t *testing.T) {
	env := newEnv(t, store.NewMemory(), router.New(nil, 0))
	ct := clientEncrypt(t, "veri")

	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{Ciphertext: ct}), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d", rec.Code)
	}

	var receipt tunnel.Receipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}

	presented := receipt.Signature
	receipt.Signature = ""
	payload, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(presented), []byte(want)) {
		t.Fatal("makbuz imzasi bagimsiz olarak dogrulanamadi")
	}

	// Bayt sayisi degistirilirse imza tutmamali.
	receipt.MeteredBytes = 1
	tampered, _ := json.Marshal(receipt)
	mac2 := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac2.Write(tampered)
	if hmac.Equal([]byte(presented), mac2.Sum(nil)) {
		t.Fatal("degistirilmis makbuz ayni imzayi uretti")
	}
}

func TestTunnelRejectsMissingAuth(t *testing.T) {
	env := newEnv(t, store.NewMemory(), router.New(nil, 0))
	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{Ciphertext: clientEncrypt(t, "x")}), false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("durum = %d, beklenen 401", rec.Code)
	}
}

// TestTunnelRejectsIllegalCarrier, hukuki kapsam korumasinin HTTP
// katmaninda da uygulandigini dogrular.
func TestTunnelRejectsIllegalCarrier(t *testing.T) {
	env := newEnv(t, store.NewMemory(), router.New(nil, 0))
	for _, bad := range []string{"radio_rf", "satellite_dish", "uydu_sizintisi"} {
		rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
			CarrierType: bad,
			Ciphertext:  clientEncrypt(t, "x"),
		}), true)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("tasiyici %q icin durum = %d, beklenen 422", bad, rec.Code)
		}
	}
}

func TestTunnelRejectsOversizedPayload(t *testing.T) {
	env := newEnv(t, store.NewMemory(), router.New(nil, 0))
	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
		Ciphertext: strings.Repeat("A", 8192),
	}), true)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("durum = %d, beklenen 413", rec.Code)
	}
}

func TestTunnelRejectsMalformedInput(t *testing.T) {
	env := newEnv(t, store.NewMemory(), router.New(nil, 0))
	cases := map[string]struct {
		body string
		want int
	}{
		"bos ciphertext":  {`{"ciphertext":""}`, http.StatusBadRequest},
		"gecersiz base64": {`{"ciphertext":"!!!not-base64!!!"}`, http.StatusBadRequest},
		"kisa ciphertext": {`{"ciphertext":"YWFhYWFhYWFhYQ=="}`, http.StatusBadRequest},
		"bilinmeyen alan": {`{"ciphertext":"AAAA","sizma_alani":"kotu"}`, http.StatusBadRequest},
		"bozuk json":      {`{"ciphertext":`, http.StatusBadRequest},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := post(t, env, c.body, true); rec.Code != c.want {
				t.Fatalf("durum = %d, beklenen %d", rec.Code, c.want)
			}
		})
	}
}

// TestTunnelFailsClosedWhenLedgerUnavailable, defter yazilamadiginda
// istegin REDDEDILDIGINI dogrular.
//
// Bu davranis bilincli bir tercihtir: olculemeyen trafik faturalanamaz.
// Sessizce hizmet vermek dogrudan gelir kaybidir. Testin amaci, birinin
// ileride "kullanilabilirlik icin" bu kontrolu kaldirmasini engellemektir.
func TestTunnelFailsClosedWhenLedgerUnavailable(t *testing.T) {
	st := &failingStore{Store: store.NewMemory(), failRecord: true}
	env := newEnv(t, st, router.New(nil, 0))

	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
		Ciphertext: clientEncrypt(t, "veri"),
	}), true)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("durum = %d, beklenen 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "veritabani ulasilamaz") {
		t.Fatal("SIZINTI: ic hata detayi istemciye dondu")
	}
}

func TestMyUsageFailsWhenLedgerUnavailable(t *testing.T) {
	st := &failingStore{Store: store.NewMemory(), failRead: true}
	env := newEnv(t, st, router.New(nil, 0))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/meter/me", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("durum = %d, beklenen 503", rec.Code)
	}
}

// TestMyUsageOnlyShowsOwnData, bir istemcinin baskasinin tuketimini
// goremedigini dogrular.
func TestMyUsageOnlyShowsOwnData(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	_ = st.Record(ctx, store.Usage{ClientID: "acme", CarrierType: "standard_internet", BytesIn: 100, BytesOut: 10})
	_ = st.Record(ctx, store.Usage{ClientID: "rakip-firma", CarrierType: "standard_internet", BytesIn: 999999, BytesOut: 10})

	env := newEnv(t, st, router.New(nil, 0))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/meter/me", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "rakip-firma") || strings.Contains(body, "999999") {
		t.Fatal("SIZINTI: baska istemcinin verisi yanitta gorundu")
	}
}

// --- Yonlendirme entegrasyonu ---

func TestTunnelForwardsToUpstream(t *testing.T) {
	var (
		mu      sync.Mutex
		gotBody []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("okey"))
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	rtr := router.New([]router.Route{{Name: "edge-1", Kind: "edge", Upstream: u}}, 0)

	st := store.NewMemory()
	env := newEnv(t, st, rtr)

	ct := clientEncrypt(t, "iletilecek veri")
	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
		CarrierType: "optical_li_fi",
		Ciphertext:  ct,
		Destination: "edge-1",
	}), true)

	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d, govde = %s", rec.Code, rec.Body.String())
	}

	var receipt tunnel.Receipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "routed" {
		t.Fatalf("Status = %q, beklenen routed", receipt.Status)
	}
	if receipt.Route == nil || receipt.Route.RouteName != "edge-1" || receipt.Route.RouteKind != "edge" {
		t.Fatalf("makbuzda rota bilgisi eksik: %+v", receipt.Route)
	}

	raw, _ := base64.StdEncoding.DecodeString(ct)
	mu.Lock()
	defer mu.Unlock()
	if string(gotBody) != string(raw) {
		t.Fatal("upstream'e ulasan yuk, istemcinin gonderdiginden farkli")
	}

	// Giden bayt hem makbuzu hem upstream'e gonderileni icermeli.
	e, err := st.ClientUsage(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if e.BytesOut <= uint64(len(raw)) {
		t.Fatalf("BytesOut = %d, yonlendirilen %d bayti icermiyor", e.BytesOut, len(raw))
	}
}

func TestTunnelRejectsDestinationWhenRoutingDisabled(t *testing.T) {
	env := newEnv(t, store.NewMemory(), router.New(nil, 0))
	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
		Ciphertext:  clientEncrypt(t, "x"),
		Destination: "edge-1",
	}), true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("durum = %d, beklenen 503", rec.Code)
	}
}

func TestTunnelRejectsUnknownDestination(t *testing.T) {
	u, _ := url.Parse("https://example.com")
	rtr := router.New([]router.Route{{Name: "edge-1", Kind: "edge", Upstream: u}}, 0)
	env := newEnv(t, store.NewMemory(), rtr)

	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
		Ciphertext:  clientEncrypt(t, "x"),
		Destination: "olmayan-rota",
	}), true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("durum = %d, beklenen 422", rec.Code)
	}
}

// TestTunnelDoesNotMeterFailedForward, upstream basarisiz oldugunda
// deftere kayit dusulmedigini dogrular. Iletilemeyen trafik faturalanmaz.
func TestTunnelDoesNotMeterFailedForward(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	rtr := router.New([]router.Route{{Name: "edge-1", Kind: "direct", Upstream: u}}, 0)

	st := store.NewMemory()
	env := newEnv(t, st, rtr)

	rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
		Ciphertext:  clientEncrypt(t, "x"),
		Destination: "edge-1",
	}), true)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("durum = %d, beklenen 502", rec.Code)
	}
	if _, err := st.ClientUsage(context.Background(), "acme"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("basarisiz iletim faturalandi")
	}
}

// TestHealthReportsState, health ucunun aktif store ve rotalari
// bildirdigini dogrular.
func TestHealthReportsState(t *testing.T) {
	u, _ := url.Parse("https://edge.example")
	rtr := router.New([]router.Route{{Name: "edge-1", Kind: "edge", Upstream: u}}, 0)
	env := newEnv(t, store.NewMemory(), rtr)

	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["store"] != "memory" {
		t.Fatalf("store = %v", got["store"])
	}
	routes, ok := got["routes"].(map[string]any)
	if !ok || routes["edge-1"] != "edge" {
		t.Fatalf("rotalar raporlanmadi: %v", got["routes"])
	}
}

// TestTunnelConcurrentRequests, es zamanli isteklerde sayacin
// bozulmadigini dogrular. -race ile calistirilmalidir.
func TestTunnelConcurrentRequests(t *testing.T) {
	st := store.NewMemory()
	env := newEnv(t, st, router.New(nil, 0))

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := post(t, env, mustJSON(t, tunnel.TunnelRequest{
				Ciphertext: clientEncrypt(t, "es zamanli"),
			}), true)
			if rec.Code != http.StatusOK {
				t.Errorf("durum = %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	e, err := st.ClientUsage(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if e.Requests != n {
		t.Fatalf("Requests = %d, beklenen %d", e.Requests, n)
	}
	if active := env.meter.ActiveTunnels(); active != 0 {
		t.Fatalf("ActiveTunnels = %d, beklenen 0", active)
	}
}
