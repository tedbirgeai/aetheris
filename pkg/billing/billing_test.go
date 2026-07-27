package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func makeStripeSignature(secret, ts string, payload []byte) string {
	signed := ts + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestStripeSignatureValid(t *testing.T) {
	secret := "whsec_test123"
	payload := []byte(`{"id":"evt_1","type":"checkout.session.completed"}`)
	sig := makeStripeSignature(secret, "1234567890", payload)
	if err := VerifyStripeSignature(payload, sig, secret); err != nil {
		t.Fatalf("geçerli imza kabul edilmeli: %v", err)
	}
}

func TestStripeSignatureInvalid(t *testing.T) {
	if err := VerifyStripeSignature([]byte(`{}`), "t=1234,v1=deadbeef", "secret"); err != ErrInvalidSignature {
		t.Fatalf("geçersiz imza reddedilmeli: %v", err)
	}
}

func TestNewInvoiceKDV(t *testing.T) {
	inv, err := NewInvoice("uuid-001", "Test Şirketi", "1234567890", "test@test.com",
		1*1024*1024*1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	if inv.GrossAmount < 9.99 || inv.GrossAmount > 10.01 {
		t.Fatalf("matrah ~10 TL olmalı, %.2f", inv.GrossAmount)
	}
	expectedKDV := round2(inv.GrossAmount * KDVRate)
	if inv.KDVAmount != expectedKDV {
		t.Fatalf("KDV %.2f olmalı, %.2f", expectedKDV, inv.KDVAmount)
	}
	if inv.NetAmount != round2(inv.GrossAmount+inv.KDVAmount) {
		t.Fatalf("net tutar hatalı: %.2f", inv.NetAmount)
	}
}

func TestInvoiceValidation(t *testing.T) {
	inv := &Invoice{GrossAmount: 100}
	if err := inv.Validate(); err != ErrMissingVKN {
		t.Fatalf("VKN/TCKN yoksa hata beklenir: %v", err)
	}
	inv.VKN = "123"
	if err := inv.Validate(); err == nil {
		t.Fatal("yanlış VKN uzunluğu hata vermeli")
	}
	inv.VKN = "1234567890"
	if err := inv.Validate(); err != nil {
		t.Fatalf("geçerli VKN: %v", err)
	}
}

func TestInvoiceToJSON(t *testing.T) {
	inv, _ := NewInvoice("uuid-002", "Test", "1234567890", "t@t.com", 500*1024*1024, 0)
	data, err := inv.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("JSON ayrıştırılamadı: %v", err)
	}
	if _, ok := parsed["uuid"]; !ok {
		t.Fatal("JSON'da uuid alanı olmalı")
	}
}

func TestInvoiceToXML(t *testing.T) {
	inv, _ := NewInvoice("uuid-003", "Test A.Ş.", "1234567890", "a@b.com", 2*1024*1024*1024, 10)
	xmlBytes, err := inv.ToXML()
	if err != nil {
		t.Fatal(err)
	}
	xmlStr := string(xmlBytes)
	for _, f := range []string{"UUID", "IssueDate", "KDV", "TaxAmount", "PayableAmount"} {
		if !strings.Contains(xmlStr, f) {
			t.Fatalf("XML'de %q eksik", f)
		}
	}
}

func TestParseStripeEvent(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed","created":1700000000,"data":{"object":{"id":"cs_1","customer_email":"x@y.com","amount_total":10000,"currency":"try","metadata":{}}}}`)
	ev, err := ParseStripeEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "checkout.session.completed" {
		t.Fatalf("event tipi yanlış: %s", ev.Type)
	}
	sess, err := HandleCheckoutCompleted(ev)
	if err != nil {
		t.Fatal(err)
	}
	if sess.AmountTotal != 10000 {
		t.Fatalf("tutar yanlış: %d", sess.AmountTotal)
	}
}
