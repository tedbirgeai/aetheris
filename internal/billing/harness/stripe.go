// Package harness, faturalama entegrasyonlarini KIMLIK BILGISI OLMADAN
// uctan uca dogrulayan offline bir test cifti (test double) saglar.
//
// # NE SIMULE EDILIR
//
//  1. Stripe webhook olaylari: gercek Stripe'in kullandigi imza semasi
//     (Stripe-Signature: t=...,v1=HMAC_SHA256("t.payload", secret)) BIREBIR
//     uretilir ve dogrulanir. Bu, canli Stripe hesabina gitmeden webhook
//     alici tarafinin dogru calistigini kanitlar.
//
//  2. e-Fatura XML: UBL-TR bicimine yapisal olarak sadik bir fatura XML'i
//     uretilir ve iyi-bicimli (well-formed) oldugu dogrulanir.
//
//  3. Tam dongu: kullanim -> brut tutar -> kredi dususu -> net tutar ->
//     KDV -> e-Fatura XML. Kredi motoru (billing.CreditEngine) gercek
//     koddur; harness yalnizca dis sistemleri taklit eder.
//
// # DURUSTLUK NOTU
//
// Bu harness GERCEK Stripe/GIB uc noktalarina BAGLANMAZ. Amaci, kimlik
// bilgisi olmadan faturalandirma mantigini ve imza dogrulamasini CI'da
// deterministik test etmektir. Canli dogrulama, ayri bir test anahtariyla
// (billing paketi dururstluk notlarinda belirtildigi gibi) yapilmalidir.
package harness

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrSignatureFormat, Stripe-Signature basligi cozulemediginde.
	ErrSignatureFormat = errors.New("harness: gecersiz Stripe-Signature bicimi")
	// ErrSignatureMismatch, imza uyusmadiginda.
	ErrSignatureMismatch = errors.New("harness: Stripe imzasi uyusmuyor")
	// ErrSignatureExpired, zaman damgasi tolerans disinda oldugunda (replay).
	ErrSignatureExpired = errors.New("harness: Stripe imza zaman damgasi tolerans disi")
)

// SignStripePayload, verilen govdeyi Stripe semasiyla imzalar ve
// Stripe-Signature basligi degerini dondurur.
func SignStripePayload(secret []byte, ts time.Time, payload []byte) string {
	tsUnix := ts.Unix()
	signed := fmt.Sprintf("%d.%s", tsUnix, payload)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signed))
	sig := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("t=%d,v1=%s", tsUnix, sig)
}

// VerifyStripeSignature, Stripe imza dogrulamasini BIREBIR uygular:
// zaman damgasini ayristirir, tolerans kontrolu yapar (replay saldirisi
// onlemi), imzayi sabit-zamanli karsilastirir.
func VerifyStripeSignature(secret, payload []byte, header string, tolerance time.Duration, now time.Time) error {
	var tsStr string
	var v1 string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			tsStr = kv[1]
		case "v1":
			v1 = kv[1]
		}
	}
	if tsStr == "" || v1 == "" {
		return ErrSignatureFormat
	}

	tsUnix, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return ErrSignatureFormat
	}

	if tolerance > 0 {
		age := now.Sub(time.Unix(tsUnix, 0))
		if age < 0 {
			age = -age
		}
		if age > tolerance {
			return ErrSignatureExpired
		}
	}

	signed := fmt.Sprintf("%d.%s", tsUnix, payload)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signed))
	want := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(v1), []byte(want)) {
		return ErrSignatureMismatch
	}
	return nil
}

// StripeMeterEvent, Stripe "billing/meter_events" ucunun bekledigi govdeyi
// yansitan bir olaydir. Harness bunu imzalayip webhook alicisina gonderir.
type StripeMeterEvent struct {
	EventName  string `json:"event_name"`
	Identifier string `json:"identifier"` // idempotency anahtari
	CustomerID string `json:"stripe_customer_id"`
	Value      uint64 `json:"value"` // olculen deger (bayt)
	Timestamp  int64  `json:"timestamp"`
}

// MarshalMeterEvent, olayi kanonik JSON'a cevirir. Imzalanan govde ile
// gonderilen govdenin BAYT-BIREBIR ayni olmasi icin hem uretim hem gonderim
// tarafinda AYNI fonksiyon kullanilmalidir; aksi halde imza dogrulanmaz.
func MarshalMeterEvent(ev StripeMeterEvent) ([]byte, error) {
	return json.Marshal(ev)
}
