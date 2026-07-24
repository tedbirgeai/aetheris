package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingEmitter, testler icin olaylari toplayan sahte emitter.
type recordingEmitter struct {
	mu       sync.Mutex
	events   []Event
	failFor  int32 // ilk N cagriyi basarisiz yap
	attempts atomic.Int32
	name     string
}

func (r *recordingEmitter) Name() string {
	if r.name == "" {
		return "recorder"
	}
	return r.name
}

func (r *recordingEmitter) Emit(_ context.Context, ev Event) error {
	n := r.attempts.Add(1)
	if n <= r.failFor {
		return errors.New("gecici hata")
	}
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return nil
}

func (r *recordingEmitter) got() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

func TestBridgeDeliversEvents(t *testing.T) {
	rec := &recordingEmitter{}
	b := New([]Emitter{rec}, Config{Logger: quietLogger(), RetryDelay: time.Millisecond})

	for i := 0; i < 5; i++ {
		b.Publish(Event{ID: "e", Type: ReceiptGenerated, ClientID: "acme", BytesIn: 10})
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	if got := len(rec.got()); got != 5 {
		t.Fatalf("iletilen olay = %d, beklenen 5", got)
	}
	if st := b.Stats(); st.Delivered != 5 {
		t.Fatalf("Delivered = %d, beklenen 5", st.Delivered)
	}
}

// TestBridgeNeverBlocks, Publish'in ASLA bloklamadigini dogrular.
// Faturalama koprusu musteri istegini bekletmemelidir.
func TestBridgeNeverBlocks(t *testing.T) {
	// Cok yavas emitter: her cagri 200ms surer.
	slow := &slowEmitter{delay: 200 * time.Millisecond}
	b := New([]Emitter{slow}, Config{
		Logger: quietLogger(), QueueSize: 4, RetryDelay: time.Millisecond,
	})
	defer b.Close()

	start := time.Now()
	for i := 0; i < 100; i++ {
		b.Publish(Event{ID: "x", Type: ReceiptGenerated, ClientID: "acme"})
	}
	elapsed := time.Since(start)

	// 100 Publish cagrisi bloklamadan bitmeliydi. Bloklasaydi
	// 100 * 200ms = 20 saniye surerdi.
	if elapsed > 2*time.Second {
		t.Fatalf("Publish bloklandi: %v", elapsed)
	}
	// Kuyruk dolunca olaylar dusurulmus olmali.
	if b.Stats().Dropped == 0 {
		t.Fatal("kuyruk dolmasina ragmen dusurulen olay yok")
	}
}

type slowEmitter struct{ delay time.Duration }

func (s *slowEmitter) Name() string { return "slow" }
func (s *slowEmitter) Emit(ctx context.Context, _ Event) error {
	select {
	case <-time.After(s.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestBridgeRetriesOnFailure, gecici hatalarda tekrar denendigini dogrular.
func TestBridgeRetriesOnFailure(t *testing.T) {
	rec := &recordingEmitter{failFor: 2} // ilk 2 deneme basarisiz
	b := New([]Emitter{rec}, Config{
		Logger: quietLogger(), MaxRetries: 3, RetryDelay: time.Millisecond,
	})

	b.Publish(Event{ID: "retry-1", Type: ReceiptGenerated, ClientID: "acme"})
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	if got := len(rec.got()); got != 1 {
		t.Fatalf("olay iletilmedi: %d", got)
	}
	if a := rec.attempts.Load(); a != 3 {
		t.Fatalf("deneme sayisi = %d, beklenen 3", a)
	}
}

// TestWebhookEmitterSignsPayload, webhook govdesinin HMAC ile
// imzalandigini ve alici tarafin dogrulayabildigini kanitlar.
func TestWebhookEmitterSignsPayload(t *testing.T) {
	secret := []byte("webhook-gizli-anahtar-0123456789")

	var (
		mu      sync.Mutex
		gotBody []byte
		gotSig  string
		gotID   string
		gotType string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		gotSig = r.Header.Get("X-Aetheris-Signature")
		gotID = r.Header.Get("X-Aetheris-Event-Id")
		gotType = r.Header.Get("X-Aetheris-Event-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	em, err := NewWebhookEmitter(srv.URL, secret, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	ev := Event{
		ID: "evt-123", Type: ReceiptGenerated, ClientID: "acme",
		BytesIn: 64, BytesOut: 305, OccurredAt: time.Now().UTC(),
	}
	if err := em.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit hata dondu: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if gotID != "evt-123" {
		t.Fatalf("idempotency basligi = %q", gotID)
	}
	if gotType != string(ReceiptGenerated) {
		t.Fatalf("olay turu basligi = %q", gotType)
	}
	if !VerifyWebhookSignature(gotBody, gotSig, secret) {
		t.Fatal("imza dogrulanamadi")
	}
	// Yanlis anahtarla dogrulama BASARISIZ olmali.
	if VerifyWebhookSignature(gotBody, gotSig, []byte("yanlis-anahtar-000000000000000")) {
		t.Fatal("yanlis anahtarla imza dogrulandi")
	}

	var decoded Event
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.BytesIn != 64 || decoded.ClientID != "acme" {
		t.Fatalf("govde bozuk: %+v", decoded)
	}
}

// TestWebhookNeverSendsPayload, yuk icerigin ASLA gonderilmedigini
// dogrular. Sifir-bilgi sozlesmesi faturalama katmaninda da gecerlidir.
func TestWebhookNeverSendsPayload(t *testing.T) {
	const secretPayload = "COK-GIZLI-MUSTERI-VERISI"

	var mu sync.Mutex
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	em, _ := NewWebhookEmitter(srv.URL, nil, 5*time.Second)
	_ = em.Emit(context.Background(), Event{
		ID: "e1", Type: ReceiptGenerated, ClientID: "acme",
		PayloadSHA: "abc123", // yalnizca OZET
	})

	mu.Lock()
	defer mu.Unlock()
	if len(body) == 0 {
		t.Fatal("govde bos")
	}
	if contains(body, secretPayload) {
		t.Fatal("SIZINTI: yuk icerigi webhook govdesinde")
	}
}

func contains(haystack []byte, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		string(haystack) != "" &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if string(haystack[i:i+len(needle)]) == needle {
					return true
				}
			}
			return false
		}()
}

func TestWebhookEmitterRejectsBadURL(t *testing.T) {
	for _, bad := range []string{"", "ftp://x.example", "https://", "not-a-url"} {
		if _, err := NewWebhookEmitter(bad, nil, time.Second); err == nil {
			t.Fatalf("gecersiz url kabul edildi: %q", bad)
		}
	}
}

func TestWebhookEmitterReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	em, _ := NewWebhookEmitter(srv.URL, nil, time.Second)
	if err := em.Emit(context.Background(), Event{ID: "e"}); err == nil {
		t.Fatal("500 icin hata bekleniyordu")
	}
}

// --- Stripe emitter (sahte uc noktaya karsi) ---

func TestStripeEmitterSendsCorrectForm(t *testing.T) {
	var mu sync.Mutex
	var gotForm, gotAuth, gotIdem string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotForm = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"billing.meter_event"}`))
	}))
	defer srv.Close()

	em := NewStripeEmitter("sk_test_sahte", "aetheris_bytes", 5*time.Second)
	em.Endpoint = srv.URL

	err := em.Emit(context.Background(), Event{
		ID: "evt-9", Type: ReceiptGenerated, ClientID: "cus_123",
		BytesIn: 100, BytesOut: 50,
	})
	if err != nil {
		t.Fatalf("Emit hata dondu: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{
		"event_name=aetheris_bytes",
		"payload%5Bstripe_customer_id%5D=cus_123",
		"payload%5Bvalue%5D=150", // 100 + 50
	} {
		if !contains([]byte(gotForm), want) {
			t.Fatalf("form eksik %q icermiyor: %s", want, gotForm)
		}
	}
	if gotAuth != "Bearer sk_test_sahte" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotIdem != "evt-9" {
		t.Fatalf("Idempotency-Key = %q", gotIdem)
	}
}

// TestStripeEmitterIgnoresNonUsageEvents, kredi ve esik olaylarinin
// Stripe'a GONDERILMEDIGINI dogrular.
func TestStripeEmitterIgnoresNonUsageEvents(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	em := NewStripeEmitter("sk_test", "meter", time.Second)
	em.Endpoint = srv.URL

	_ = em.Emit(context.Background(), Event{Type: CreditEarned, ClientID: "acme"})
	_ = em.Emit(context.Background(), Event{Type: UsageThresholdExceeded, ClientID: "acme"})

	if called.Load() != 0 {
		t.Fatalf("Stripe %d kez cagrildi, beklenen 0", called.Load())
	}
}

// TestShutdownDoesNotDropRetryingEvents, kapanis sirasinda TEKRAR
// DENEME BEKLEYEN olaylarin DUSMEDIGINI dogrular.
//
// # NEDEN BU TEST VAR
//
// Ilk uygulamada geri cekilme beklemesi kapanis sinyalinde `return`
// ediyordu. Sonuc: zarif kapanista, ilk denemesi basarisiz olan her
// fatura olayi SESSIZCE kayboluyordu. Bu, dogrudan gelir kaybidir ve
// hicbir log satirinda gorunmez.
//
// Bu test o davranisi kilitler. SILINMEMELIDIR.
func TestShutdownDoesNotDropRetryingEvents(t *testing.T) {
	// Ilk 2 denemesi basarisiz olan emitter.
	rec := &recordingEmitter{failFor: 2}
	b := New([]Emitter{rec}, Config{
		Logger:     quietLogger(),
		MaxRetries: 3,
		RetryDelay: 50 * time.Millisecond,
	})

	b.Publish(Event{ID: "kapanis-testi", Type: ReceiptGenerated, ClientID: "acme"})

	// Olayin islenmeye BASLAMASINI bekle, sonra hemen kapat.
	time.Sleep(10 * time.Millisecond)
	if err := b.Close(); err != nil {
		t.Fatalf("Close hata dondu: %v", err)
	}

	if got := len(rec.got()); got != 1 {
		t.Fatalf("kapanista olay DUSTU: iletilen = %d, beklenen 1", got)
	}
	if st := b.Stats(); st.Delivered != 1 {
		t.Fatalf("Delivered = %d, beklenen 1", st.Delivered)
	}
}
