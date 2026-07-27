package dashboard

import (
	"testing"
	"time"
)

// MADDE 10 — TCKN algoritma doğrulaması.
func TestValidTCKN(t *testing.T) {
	valid := []string{"10000000146", "19191919190"}
	invalid := []string{"", "1234567890", "01234567890", "12345678901", "abcdefghijk", "11111111111"}
	for _, v := range valid {
		if !validTCKN(v) {
			t.Errorf("geçerli TCKN reddedildi: %s", v)
		}
	}
	for _, v := range invalid {
		if validTCKN(v) {
			t.Errorf("geçersiz TCKN kabul edildi: %s", v)
		}
	}
}

// MADDE 2 — kredi izolasyonu: yalnızca TAM clientID eşleşmesi.
func TestFilterCreditsExactMatch(t *testing.T) {
	rows := []CreditRow{
		{ClientID: "acme", Bytes: 100},
		{ClientID: "acme-2", Bytes: 200}, // ön-ek çakışması TUZAĞI
		{ClientID: "meridian", Bytes: 300},
	}
	got := filterCredits(rows, "acme:secret1234567890abcd")
	if len(got) != 1 || got[0].ClientID != "acme" {
		t.Fatalf("izolasyon hatası: %+v (yalnızca acme dönmeliydi)", got)
	}
}

// MADDE 12 — rate limiter penceresi.
func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("ilk %d istek geçmeliydi", i+1)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("4. istek reddedilmeliydi")
	}
	if !rl.allow("5.6.7.8") {
		t.Fatal("farklı IP bağımsız olmalı")
	}
}
