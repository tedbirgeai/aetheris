package harness

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tedbirgeai/aetheris/internal/billing"
)

func TestStripeSignRoundTrip(t *testing.T) {
	secret := []byte("whsec_test_gizli")
	payload := []byte(`{"event_name":"aetheris_bytes","value":1024}`)
	now := time.Now()

	header := SignStripePayload(secret, now, payload)
	if err := VerifyStripeSignature(secret, payload, header, 5*time.Minute, now); err != nil {
		t.Fatalf("gecerli imza dogrulanmaliydi: %v", err)
	}
}

func TestStripeSignatureRejectsTamper(t *testing.T) {
	secret := []byte("whsec_test")
	payload := []byte(`{"value":100}`)
	now := time.Now()
	header := SignStripePayload(secret, now, payload)

	// Govdeyi degistir: imza artik uyusmamali.
	tampered := []byte(`{"value":999999}`)
	if err := VerifyStripeSignature(secret, tampered, header, 5*time.Minute, now); err != ErrSignatureMismatch {
		t.Fatalf("degistirilen govde reddedilmeliydi: %v", err)
	}

	// Yanlis secret: reddedilmeli.
	if err := VerifyStripeSignature([]byte("yanlis"), payload, header, 5*time.Minute, now); err != ErrSignatureMismatch {
		t.Fatalf("yanlis secret reddedilmeliydi: %v", err)
	}
}

func TestStripeSignatureReplayExpired(t *testing.T) {
	secret := []byte("whsec_test")
	payload := []byte(`{"value":1}`)
	old := time.Now().Add(-10 * time.Minute)
	header := SignStripePayload(secret, old, payload)

	// 5 dk tolerans; 10 dk eski imza reddedilmeli (replay onlemi).
	if err := VerifyStripeSignature(secret, payload, header, 5*time.Minute, time.Now()); err != ErrSignatureExpired {
		t.Fatalf("eski imza replay olarak reddedilmeliydi: %v", err)
	}
}

func TestEInvoiceXMLWellFormed(t *testing.T) {
	xmlBytes, err := BuildEInvoiceXML(InvoiceInput{
		InvoiceID:    "AET2026000001",
		IssueDate:    time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC),
		SupplierName: "Aetheris Labs",
		SupplierVKN:  "1234567890",
		CustomerName: "acme",
		CustomerVKN:  "0987654321",
		LineNet:      Kurus(10000), // 100.00 TL
		VATRate:      20,
	})
	if err != nil {
		t.Fatalf("BuildEInvoiceXML: %v", err)
	}

	// 1) Iyi-bicimlilik: tum token'lar hatasiz okunabilmeli.
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("uretilen XML iyi-bicimli degil: %v", err)
		}
	}

	// 2) Icerik: alanlar YEREL AD ile eslesir (namespace onekinden bagimsiz).
	var back struct {
		ID        string `xml:"ID"`
		Payable   string `xml:"LegalMonetaryTotal>PayableAmount"`
		TaxAmount string `xml:"TaxTotal>TaxAmount"`
	}
	if err := xml.Unmarshal(xmlBytes, &back); err != nil {
		t.Fatalf("XML cozulemedi: %v", err)
	}
	if back.ID != "AET2026000001" {
		t.Fatalf("fatura ID kayboldu: %q", back.ID)
	}
	// 100.00 net + %20 KDV = 120.00 odenecek.
	if back.Payable != "120.00" {
		t.Fatalf("odenecek tutar 120.00 olmali, %q", back.Payable)
	}
	if back.TaxAmount != "20.00" {
		t.Fatalf("KDV 20.00 olmali, %q", back.TaxAmount)
	}
}

func TestKurusFormatting(t *testing.T) {
	cases := map[Kurus]string{
		0:     "0.00",
		5:     "0.05",
		99:    "0.99",
		100:   "1.00",
		12345: "123.45",
		-250:  "-2.50",
	}
	for k, want := range cases {
		if k.TL() != want {
			t.Fatalf("Kurus(%d).TL()=%q, beklenen %q", k, k.TL(), want)
		}
	}
}

func TestEndToEndBillingCycle(t *testing.T) {
	secret := []byte("whsec_e2e")
	sup := Supplier{Name: "Aetheris Labs", VKN: "1234567890"}
	pricing := Pricing{
		PerMB:           Kurus(50), // 0.50 TL / MB
		CreditUnitValue: Kurus(1),  // 1 birim = 0.01 TL
		VATRate:         20,
	}

	// 100 MB kullanim, 500 kredi birimi kazanilmis.
	in := CycleInput{
		ClientID:    "acme",
		CustomerVKN: "0987654321",
		Bytes:       100 * 1_000_000,
		CreditUnits: 500,
		InvoiceID:   "AET2026000042",
		IssueDate:   time.Now(),
	}

	res, err := RunBillingCycle(in, pricing, secret, sup)
	if err != nil {
		t.Fatalf("RunBillingCycle: %v", err)
	}

	// Brut: 100 MB * 0.50 = 50.00 TL = 5000 kurus.
	if res.GrossKurus != 5000 {
		t.Fatalf("brut 5000 kurus olmali, %d", res.GrossKurus)
	}
	// Kredi: 500 * 0.01 = 5.00 TL = 500 kurus.
	if res.CreditKurus != 500 {
		t.Fatalf("kredi 500 kurus olmali, %d", res.CreditKurus)
	}
	// Net: 5000 - 500 = 4500 kurus.
	if res.NetKurus != 4500 {
		t.Fatalf("net 4500 kurus olmali, %d", res.NetKurus)
	}

	// Uretilen webhook'u GERCEK bir alici sunucuya gonder, dogrulansin.
	recv := NewWebhookReceiver(secret, 5*time.Minute)
	srv := httptest.NewServer(recv)
	defer srv.Close()

	payload := mustMeterPayload(t, in)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(payload))
	req.Header.Set("Stripe-Signature", res.StripeSig)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook gonderimi: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook alicisi %d dondu, imza dogrulanmaliydi", resp.StatusCode)
	}
	if recv.ReceivedCount() != 1 {
		t.Fatalf("1 dogrulanmis olay beklenir, %d", recv.ReceivedCount())
	}

	// Ayni olayi tekrar gonder: idempotency, iki kez faturalanmamali.
	req2, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(payload))
	req2.Header.Set("Stripe-Signature", res.StripeSig)
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if recv.ReceivedCount() != 1 {
		t.Fatalf("tekrarli olay sonrasi hala 1 olmali, %d", recv.ReceivedCount())
	}
}

func TestCreditEngineIntegration(t *testing.T) {
	// Harness'in GERCEK kredi motoruyla calistigini goster.
	engine := billing.NewCreditEngine(0.001, 0, nil) // bayt basina 0.001 birim
	// acme, baskasinin (originB) 100000 baytini role etti.
	units := CreditsToUnits(engine, "acme", "originB", 100000)
	if units == 0 {
		t.Fatal("role edilen trafik icin kredi birimi uretilmeliydi")
	}
	bal, ok := engine.Balance("acme")
	if !ok || bal.CreditUnits != units {
		t.Fatalf("kredi bakiyesi tutarsiz: %+v", bal)
	}
	// Self-relay kredi kazandirmamali.
	if CreditsToUnits(engine, "acme", "acme", 100000) != 0 {
		t.Fatal("kendi trafigini role etmek kredi kazandirmamali")
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	recv := NewWebhookReceiver([]byte("dogru-secret"), 5*time.Minute)
	srv := httptest.NewServer(recv)
	defer srv.Close()

	body := []byte(`{"identifier":"x","value":1}`)
	badSig := SignStripePayload([]byte("yanlis-secret"), time.Now(), body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", badSig)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("yanlis imza 400 vermeliydi, %d", resp.StatusCode)
	}
	if recv.RejectedCount() != 1 {
		t.Fatalf("1 reddedilmis istek beklenir, %d", recv.RejectedCount())
	}
	if recv.ReceivedCount() != 0 {
		t.Fatal("yanlis imzali olay kaydedilmemeliydi")
	}
}

func mustMeterPayload(t *testing.T, in CycleInput) []byte {
	t.Helper()
	ev := StripeMeterEvent{
		EventName:  "aetheris_bytes",
		Identifier: in.InvoiceID,
		CustomerID: in.ClientID,
		Value:      in.Bytes,
		Timestamp:  in.IssueDate.Unix(),
	}
	b, err := MarshalMeterEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
