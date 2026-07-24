package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/tedbirgeai/aetheris/internal/billing"
)

// Pricing, kullanim -> para donusum oranlaridir (hepsi tam sayi kurus).
type Pricing struct {
	// PerMB, her 1 MB (1_000_000 bayt) icin ucret (kurus).
	PerMB Kurus
	// CreditUnitValue, bir kredi biriminin para karsiligi (kurus).
	// Kredi motoru birim sayar; para donusumu BURADA yapilir.
	CreditUnitValue Kurus
	// VATRate, KDV orani (ornegin 20).
	VATRate int
}

// CycleInput, tek bir fatura donemi hesabi icin girdilerdir.
type CycleInput struct {
	ClientID    string
	CustomerVKN string
	Bytes       uint64 // donemde olculen toplam bayt
	CreditUnits uint64 // kredi motorundan gelen kazanilmis birim
	InvoiceID   string
	IssueDate   time.Time
}

// CycleResult, bir fatura dongusunun sonucudur.
type CycleResult struct {
	GrossKurus  Kurus  // kredi oncesi brut (KDV haric)
	CreditKurus Kurus  // dusulen kredi tutari
	NetKurus    Kurus  // KDV haric net (brut - kredi, 0 tabanli)
	EInvoiceXML []byte // uretilen e-Fatura
	StripeSig   string // uretilen Stripe webhook imzasi
}

// RunBillingCycle, uctan uca dongunun TAMAMINI kimlik bilgisi olmadan
// yurutur:
//
//	bayt -> brut -> kredi dususu -> net -> KDV -> e-Fatura XML
//	     -> imzali Stripe meter olayi -> webhook alicisi dogrular
//
// Kredi hesabi GERCEK billing.CreditEngine mantigiyla tutarli olacak
// sekilde para birimine cevrilir. Donen sonucta net asla negatif olmaz
// (kredi brutten fazlaysa net 0'a sabitlenir; kalan kredi devreder).
func RunBillingCycle(in CycleInput, p Pricing, stripeSecret []byte, sup Supplier) (CycleResult, error) {
	if in.InvoiceID == "" {
		return CycleResult{}, fmt.Errorf("harness: fatura ID gerekli")
	}
	// Brut: bayt / 1MB * PerMB (tam sayi, asagi yuvarlama).
	gross := Kurus(int64(in.Bytes) * int64(p.PerMB) / 1_000_000)
	credit := Kurus(int64(in.CreditUnits) * int64(p.CreditUnitValue))

	net := gross - credit
	if net < 0 {
		net = 0
	}

	xmlBytes, err := BuildEInvoiceXML(InvoiceInput{
		InvoiceID:    in.InvoiceID,
		IssueDate:    in.IssueDate,
		SupplierName: sup.Name,
		SupplierVKN:  sup.VKN,
		CustomerName: in.ClientID,
		CustomerVKN:  in.CustomerVKN,
		LineNet:      net,
		VATRate:      p.VATRate,
		Currency:     "TRY",
	})
	if err != nil {
		return CycleResult{}, err
	}

	// Stripe meter olayini olustur ve imzala.
	ev := StripeMeterEvent{
		EventName:  "aetheris_bytes",
		Identifier: in.InvoiceID,
		CustomerID: in.ClientID,
		Value:      in.Bytes,
		Timestamp:  in.IssueDate.Unix(),
	}
	payload, err := MarshalMeterEvent(ev)
	if err != nil {
		return CycleResult{}, err
	}
	sig := SignStripePayload(stripeSecret, in.IssueDate, payload)

	return CycleResult{
		GrossKurus:  gross,
		CreditKurus: credit,
		NetKurus:    net,
		EInvoiceXML: xmlBytes,
		StripeSig:   sig,
	}, nil
}

// Supplier, faturayi kesen tarafin (isletmeci) bilgisidir.
type Supplier struct {
	Name string
	VKN  string
}

// --- Webhook alicisi (Stripe imzasini dogrulayan test sunucusu) ---

// WebhookReceiver, imzali Stripe olaylarini alan ve dogrulayan bir
// http.Handler'dir. Dogrulanan olaylar kaydedilir; testler bunlari
// inceleyebilir. Idempotency: ayni identifier iki kez islenmez.
type WebhookReceiver struct {
	secret    []byte
	tolerance time.Duration

	mu       sync.Mutex
	received map[string]StripeMeterEvent // identifier -> olay
	rejected int
}

// NewWebhookReceiver, verilen gizli anahtarla bir alici olusturur.
func NewWebhookReceiver(secret []byte, tolerance time.Duration) *WebhookReceiver {
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}
	return &WebhookReceiver{
		secret:    secret,
		tolerance: tolerance,
		received:  make(map[string]StripeMeterEvent),
	}
}

func (r *WebhookReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "govde okunamadi", http.StatusBadRequest)
		return
	}
	sig := req.Header.Get("Stripe-Signature")
	if err := VerifyStripeSignature(r.secret, body, sig, r.tolerance, time.Now()); err != nil {
		r.mu.Lock()
		r.rejected++
		r.mu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var ev StripeMeterEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "gecersiz olay govdesi", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	// Idempotency: ayni identifier'i iki kez faturalamayiz.
	_, dup := r.received[ev.Identifier]
	if !dup {
		r.received[ev.Identifier] = ev
	}
	r.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	if dup {
		_, _ = w.Write([]byte(`{"status":"duplicate_ignored"}`))
		return
	}
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ReceivedCount, dogrulanip kaydedilen benzersiz olay sayisi.
func (r *WebhookReceiver) ReceivedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.received)
}

// RejectedCount, imza dogrulamasi basarisiz olan istek sayisi.
func (r *WebhookReceiver) RejectedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rejected
}

// Get, kaydedilmis bir olayi identifier ile dondurur.
func (r *WebhookReceiver) Get(identifier string) (StripeMeterEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev, ok := r.received[identifier]
	return ev, ok
}

// CreditsToUnits, harness'in kredi motoruyla tutarli calistigini gostermek
// icin billing.CreditEngine'i sarmalar: gercek role trafiginden kredi birimi
// uretir. Boylece dongu, gercek kredi mantigina baglanabilir.
func CreditsToUnits(engine *billing.CreditEngine, relayerID, originID string, bytes uint64) uint64 {
	return engine.RecordRelay(relayerID, originID, bytes)
}
