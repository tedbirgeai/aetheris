package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// --- Webhook Emitter (genel amacli, TAM TEST EDILMIS) ---

// WebhookEmitter, olaylari JSON olarak bir HTTP uc noktasina POST eder.
// Kendi muhasebe sisteminizi baglamanin en basit yolu.
//
// GUVENLIK: Govde HMAC-SHA256 ile imzalanir ve X-Aetheris-Signature
// basliginda gonderilir. Alici taraf bu imzayi dogrulayarak olayin
// gercekten sizin gecidinizden geldigini teyit edebilir.
type WebhookEmitter struct {
	URL    string
	Secret []byte
	Client *http.Client
	label  string
}

func NewWebhookEmitter(rawURL string, secret []byte, timeout time.Duration) (*WebhookEmitter, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("billing: gecersiz webhook url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("billing: webhook url http/https olmali, %q verildi", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("billing: webhook url host eksik")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &WebhookEmitter{
		URL:    rawURL,
		Secret: secret,
		Client: &http.Client{Timeout: timeout},
		label:  "webhook:" + u.Host,
	}, nil
}

func (w *WebhookEmitter) Name() string { return w.label }

func (w *WebhookEmitter) Emit(ctx context.Context, ev Event) error {
	body, err := MarshalEvent(ev)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Aetheris-Event-Type", string(ev.Type))
	// Idempotency: alici ayni ID'li olayi iki kez faturalamamali.
	req.Header.Set("X-Aetheris-Event-Id", ev.ID)

	if len(w.Secret) > 0 {
		mac := hmac.New(sha256.New, w.Secret)
		_, _ = mac.Write(body)
		req.Header.Set("X-Aetheris-Signature", hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := w.Client.Do(req)
	if err != nil {
		return fmt.Errorf("billing: webhook istegi basarisiz: %w", err)
	}
	defer resp.Body.Close()
	// Govdeyi tuket ki baglanti havuza donebilsin.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode >= 300 {
		return fmt.Errorf("billing: webhook %d dondu", resp.StatusCode)
	}
	return nil
}

// VerifyWebhookSignature, ALICI tarafin kullanmasi icin yardimcidir.
// Sabit zamanli karsilastirma yapar.
func VerifyWebhookSignature(body []byte, signature string, secret []byte) bool {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(want))
}

// --- Stripe Emitter (DOGRULANMAMIS: canli API testi yapilmadi) ---

// StripeEmitter, kullanim olaylarini Stripe'in meter events ucuna gonderir.
//
// DURUSTLUK UYARISI: Bu emitter, Stripe'in bekledigi govde bicimini
// (application/x-www-form-urlencoded, event_name + payload alanlari)
// uretir ve HTTP hatalarini dogru isler. Ancak GERCEK Stripe hesabina
// karsi DOGRULANMAMISTIR — bunun icin canli API anahtari gerekir.
//
// Uretime almadan once yapilmasi gerekenler:
//  1. Stripe test anahtariyla bir olay gonderip 200 alindigini dogrulayin.
//  2. Stripe panelinde meter'in dogru artigini teyit edin.
//  3. Stripe'in guncel API surumunu ve alan adlarini kontrol edin
//     (Stripe API'si zaman icinde degisir; bu kod yazildigi andaki
//     "billing/meter_events" bicimini varsayar).
type StripeEmitter struct {
	// APIKey, Stripe gizli anahtari (sk_...). ASLA loglanmaz.
	APIKey string
	// EventName, Stripe panelinde tanimlanan meter adi.
	EventName string
	// Endpoint, varsayilan olarak Stripe uretim ucudur.
	// Testte httptest sunucusuna yonlendirilebilir.
	Endpoint string
	Client   *http.Client
}

func NewStripeEmitter(apiKey, eventName string, timeout time.Duration) *StripeEmitter {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &StripeEmitter{
		APIKey:    apiKey,
		EventName: eventName,
		Endpoint:  "https://api.stripe.com/v1/billing/meter_events",
		Client:    &http.Client{Timeout: timeout},
	}
}

func (s *StripeEmitter) Name() string { return "stripe" }

func (s *StripeEmitter) Emit(ctx context.Context, ev Event) error {
	// Yalnizca kullanim olaylari Stripe'a gider; kredi ve esik olaylari
	// dahili muhasebeyi ilgilendirir.
	if ev.Type != ReceiptGenerated {
		return nil
	}

	form := url.Values{}
	form.Set("event_name", s.EventName)
	form.Set("payload[stripe_customer_id]", ev.ClientID)
	// Stripe sayisal degerleri string olarak bekler.
	form.Set("payload[value]", strconv.FormatUint(ev.BytesIn+ev.BytesOut, 10))
	form.Set("identifier", ev.ID) // idempotency

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.Endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Idempotency-Key", ev.ID)

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("billing: stripe istegi basarisiz: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode >= 300 {
		// Stripe hata govdesi tani icin degerlidir, ama API anahtari
		// iceremez — govdeyi guvenle loglayabiliriz.
		return fmt.Errorf("billing: stripe %d dondu: %s", resp.StatusCode, string(body))
	}
	return nil
}

// --- e-Fatura Emitter (DOGRULANMAMIS: saglayici secimi gerekli) ---

// EInvoiceEmitter, kullanim kayitlarini bir e-Fatura entegratorune iletir.
//
// DURUSTLUK UYARISI: Turkiye'de e-Fatura, GIB onayli bir ozel entegrator
// uzerinden kesilir (Logo, Uyumsoft, Parasut vb.) ve HER ENTEGRATORUN
// API'si FARKLIDIR. Tek bir "e-Fatura API'si" yoktur.
//
// Bu emitter, entegratore gonderilecek kullanim ozetini standart bir
// JSON govdesiyle iletir. Entegrator secildikten sonra govde bicimi
// onun sozlesmesine uyarlanmalidir. Su haliyle, kendi ara servisinize
// (entegrator adaptoru) gonderim icin kullanilabilir.
//
// Ayrica: e-Fatura kesimi genellikle DONEM SONUNDA toplu yapilir,
// her istek basina degil. Bu emitter olay bazli calisir; donemsel
// toplama isini alici taraf yapmalidir.
type EInvoiceEmitter struct {
	Endpoint string
	APIKey   string
	Client   *http.Client
}

func NewEInvoiceEmitter(endpoint, apiKey string, timeout time.Duration) (*EInvoiceEmitter, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("billing: gecersiz e-fatura uc noktasi: %q", endpoint)
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &EInvoiceEmitter{
		Endpoint: endpoint,
		APIKey:   apiKey,
		Client:   &http.Client{Timeout: timeout},
	}, nil
}

func (e *EInvoiceEmitter) Name() string { return "e-invoice" }

func (e *EInvoiceEmitter) Emit(ctx context.Context, ev Event) error {
	if ev.Type != ReceiptGenerated && ev.Type != UsageThresholdExceeded {
		return nil
	}

	body, err := MarshalEvent(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.APIKey)
	req.Header.Set("X-Idempotency-Key", ev.ID)

	resp, err := e.Client.Do(req)
	if err != nil {
		return fmt.Errorf("billing: e-fatura istegi basarisiz: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode >= 300 {
		return fmt.Errorf("billing: e-fatura %d dondu", resp.StatusCode)
	}
	return nil
}

// Derleme zamaninda arayuz uyumunu garanti et.
var (
	_ Emitter = (*WebhookEmitter)(nil)
	_ Emitter = (*StripeEmitter)(nil)
	_ Emitter = (*EInvoiceEmitter)(nil)
)
