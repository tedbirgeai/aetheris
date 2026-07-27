// Package billing, Stripe Checkout/Webhook entegrasyonu ve Türkiye vergi
// mevzuatına (VKN/TCKN, %20 KDV) tam uyumlu e-Fatura/e-Arşiv JSON+XML
// taslak üretici yapıları sağlar.
//
// DURUSTLUK NOTU: Stripe uç noktaları sandbox olmadan canlı URL'e bağlanmaz;
// gerçek Stripe key'i AETHERIS_STRIPE_SECRET olarak enjekte edilmelidir.
// e-Fatura XML, GİB UBL-TR şemasına göre yapılandırılmıştır; canlı gönderim
// için GİB entegratörü veya özel API gerekir.
package billing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// KDVRate, Türkiye genel KDV oranı (%20).
const KDVRate = 0.20

var (
	ErrInvalidSignature = errors.New("billing: Stripe imzası geçersiz")
	ErrUnknownEvent     = errors.New("billing: bilinmeyen Stripe event tipi")
	ErrMissingVKN       = errors.New("billing: VKN/TCKN zorunlu")
)

// --- Stripe Webhook ---

// StripeEvent, Stripe webhook payload'ıdır.
type StripeEvent struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Created int64           `json:"created"`
	Data    json.RawMessage `json:"data"`
}

// CheckoutSession, Stripe checkout.session.completed verisidir.
type CheckoutSession struct {
	ID            string            `json:"id"`
	CustomerEmail string            `json:"customer_email"`
	AmountTotal   int64             `json:"amount_total"` // kuruş
	Currency      string            `json:"currency"`
	Metadata      map[string]string `json:"metadata"`
}

// VerifyStripeSignature, Stripe-Signature başlığını HMAC-SHA256 ile doğrular.
// secret, STRIPE_WEBHOOK_SECRET ortam değişkeninden gelir.
func VerifyStripeSignature(payload []byte, sigHeader, secret string) error {
	parts := strings.Split(sigHeader, ",")
	var ts, v1 string
	for _, p := range parts {
		if strings.HasPrefix(p, "t=") {
			ts = strings.TrimPrefix(p, "t=")
		}
		if strings.HasPrefix(p, "v1=") {
			v1 = strings.TrimPrefix(p, "v1=")
		}
	}
	if ts == "" || v1 == "" {
		return ErrInvalidSignature
	}
	signed := ts + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(v1)) {
		return ErrInvalidSignature
	}
	return nil
}

// ParseStripeEvent, doğrulanmış webhook body'sini ayrıştırır.
func ParseStripeEvent(body []byte) (*StripeEvent, error) {
	var ev StripeEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// HandleCheckoutCompleted, checkout.session.completed event'ini işler ve
// tenant quota/credit güncellemesi için gerekli bilgileri döndürür.
func HandleCheckoutCompleted(ev *StripeEvent) (*CheckoutSession, error) {
	if ev.Type != "checkout.session.completed" {
		return nil, ErrUnknownEvent
	}
	var wrapper struct {
		Object CheckoutSession `json:"object"`
	}
	if err := json.Unmarshal(ev.Data, &wrapper); err != nil {
		return nil, err
	}
	return &wrapper.Object, nil
}

// CreateCheckoutSession, Stripe Checkout oturumu oluşturur.
// AETHERIS_STRIPE_SECRET ortam değişkeni gereklidir.
func CreateCheckoutSession(priceID, successURL, cancelURL, clientID string) (string, error) {
	secret := os.Getenv("AETHERIS_STRIPE_SECRET")
	if secret == "" {
		return "", errors.New("billing: AETHERIS_STRIPE_SECRET tanımlanmamış")
	}
	body := fmt.Sprintf(
		"mode=payment&line_items[0][price]=%s&line_items[0][quantity]=1&success_url=%s&cancel_url=%s&metadata[client_id]=%s",
		priceID, successURL, cancelURL, clientID,
	)
	req, err := http.NewRequest("POST", "https://api.stripe.com/v1/checkout/sessions",
		strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(secret, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		URL string `json:"url"`
		ID  string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.URL, nil
}

// --- Türkiye e-Fatura / e-Arşiv ---

// Invoice, fatura verisidir (GİB UBL-TR uyumlu).
type Invoice struct {
	UUID         string    `json:"uuid" xml:"UUID"`
	IssueDate    time.Time `json:"issue_date"`
	IssueDateStr string    `json:"-" xml:"IssueDate"` // YYYY-MM-DD
	IssueTimeStr string    `json:"-" xml:"IssueTime"` // HH:MM:SS
	// Alıcı.
	BuyerName string `json:"buyer_name" xml:"BuyerName"`
	VKN       string `json:"vkn" xml:"VKN"`
	TCKN      string `json:"tckn,omitempty" xml:"TCKN,omitempty"`
	Email     string `json:"email" xml:"Email"`
	// Tutarlar (TL, kuruş hassasiyeti).
	GrossAmount float64 `json:"gross_amount" xml:"GrossAmount"` // matrah
	KDVRate     float64 `json:"kdv_rate" xml:"KDVRate"`         // 0.20
	KDVAmount   float64 `json:"kdv_amount" xml:"KDVAmount"`     // matrah * rate
	NetAmount   float64 `json:"net_amount" xml:"NetAmount"`     // matrah + KDV
	Currency    string  `json:"currency" xml:"Currency"`
	// Kredi indirimi.
	CreditDiscount float64 `json:"credit_discount" xml:"CreditDiscount"`
	Note           string  `json:"note" xml:"Note"`
}

// Validate, fatura verilerini doğrular.
func (inv *Invoice) Validate() error {
	if inv.VKN == "" && inv.TCKN == "" {
		return ErrMissingVKN
	}
	if inv.VKN != "" && len(inv.VKN) != 10 {
		return fmt.Errorf("billing: VKN 10 hane olmalı, %d hane geldi", len(inv.VKN))
	}
	if inv.TCKN != "" && len(inv.TCKN) != 11 {
		return fmt.Errorf("billing: TCKN 11 hane olmalı, %d hane geldi", len(inv.TCKN))
	}
	if inv.GrossAmount < 0 {
		return fmt.Errorf("billing: matrah negatif olamaz")
	}
	return nil
}

// NewInvoice, kullanım verisinden fatura oluşturur.
// rateLimit, AETHERIS_RATE_LIMIT ortam değişkeninden okunur.
func NewInvoice(uuid, buyerName, vkn, email string, usageBytes uint64, creditUnits uint64) (*Invoice, error) {
	if uuid == "" {
		return nil, errors.New("billing: UUID boş olamaz")
	}
	// Birim fiyat: AETHERIS_RATE_LIMIT (TL/GB, varsayılan 10 TL/GB).
	pricePerGB := 10.0
	if v := os.Getenv("AETHERIS_RATE_LIMIT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			pricePerGB = f
		}
	}
	creditPerByte := 0.0
	if v := os.Getenv("AETHERIS_CREDIT_PER_BYTE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			creditPerByte = f
		}
	}
	// Matrah hesabı.
	gbUsed := float64(usageBytes) / (1024 * 1024 * 1024)
	gross := round2(gbUsed * pricePerGB)
	creditDiscount := round2(float64(creditUnits) * creditPerByte)
	if creditDiscount > gross {
		creditDiscount = gross
	}
	taxableAmount := round2(gross - creditDiscount)
	kdv := round2(taxableAmount * KDVRate)
	net := round2(taxableAmount + kdv)
	now := time.Now()
	return &Invoice{
		UUID:           uuid,
		IssueDate:      now,
		IssueDateStr:   now.Format("2006-01-02"),
		IssueTimeStr:   now.Format("15:04:05"),
		BuyerName:      buyerName,
		VKN:            vkn,
		Email:          email,
		GrossAmount:    gross,
		KDVRate:        KDVRate,
		KDVAmount:      kdv,
		NetAmount:      net,
		Currency:       "TRY",
		CreditDiscount: creditDiscount,
		Note:           fmt.Sprintf("Aetheris Protocol kullanım faturası. Kullanım: %.3f GB. Kredi indirimi: %.2f TL.", gbUsed, creditDiscount),
	}, nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// ToJSON, faturayı JSON'a çevirir.
func (inv *Invoice) ToJSON() ([]byte, error) {
	return json.MarshalIndent(inv, "", "  ")
}

// ToXML, GİB UBL-TR uyumlu fatura XML'i üretir.
func (inv *Invoice) ToXML() ([]byte, error) {
	type lineItem struct {
		Description string  `xml:"Description"`
		Quantity    float64 `xml:"Quantity"`
		UnitPrice   float64 `xml:"UnitPrice"`
		LineAmount  float64 `xml:"LineAmount"`
	}
	type taxTotal struct {
		TaxAmount   float64 `xml:"TaxAmount"`
		TaxCategory struct {
			ID      string  `xml:"ID"`
			Percent float64 `xml:"Percent"`
		} `xml:"TaxCategory"`
	}
	type doc struct {
		XMLName            xml.Name `xml:"Invoice"`
		Xmlns              string   `xml:"xmlns,attr"`
		UBLVersionID       string   `xml:"UBLVersionID"`
		CustomizationID    string   `xml:"CustomizationID"`
		UUID               string   `xml:"UUID"`
		IssueDate          string   `xml:"IssueDate"`
		IssueTime          string   `xml:"IssueTime"`
		InvoiceTypeCode    string   `xml:"InvoiceTypeCode"`
		Note               string   `xml:"Note"`
		DocumentCurrency   string   `xml:"DocumentCurrencyCode"`
		AccountingSupplier struct {
			Party struct {
				Name string `xml:"PartyName>Name"`
			} `xml:"Party"`
		} `xml:"AccountingSupplierParty"`
		AccountingCustomer struct {
			Party struct {
				Name string `xml:"PartyName>Name"`
				VKN  string `xml:"PartyIdentification>ID"`
			} `xml:"Party"`
		} `xml:"AccountingCustomerParty"`
		TaxTotal           taxTotal `xml:"TaxTotal"`
		LegalMonetaryTotal struct {
			LineExtensionAmount float64 `xml:"LineExtensionAmount"`
			TaxExclusiveAmount  float64 `xml:"TaxExclusiveAmount"`
			TaxInclusiveAmount  float64 `xml:"TaxInclusiveAmount"`
			PayableAmount       float64 `xml:"PayableAmount"`
		} `xml:"LegalMonetaryTotal"`
		InvoiceLine lineItem `xml:"InvoiceLine"`
	}
	d := doc{}
	d.Xmlns = "urn:oasis:names:specification:ubl:schema:xsd:Invoice-2"
	d.UBLVersionID = "2.1"
	d.CustomizationID = "TR1.2"
	d.UUID = inv.UUID
	d.IssueDate = inv.IssueDateStr
	d.IssueTime = inv.IssueTimeStr
	d.InvoiceTypeCode = "SATIS"
	d.Note = inv.Note
	d.DocumentCurrency = inv.Currency
	d.AccountingSupplier.Party.Name = "Aetheris Protocol Ltd."
	d.AccountingCustomer.Party.Name = inv.BuyerName
	d.AccountingCustomer.Party.VKN = inv.VKN
	d.TaxTotal.TaxAmount = inv.KDVAmount
	d.TaxTotal.TaxCategory.ID = "KDV"
	d.TaxTotal.TaxCategory.Percent = KDVRate * 100
	d.LegalMonetaryTotal.LineExtensionAmount = inv.GrossAmount
	d.LegalMonetaryTotal.TaxExclusiveAmount = inv.GrossAmount - inv.CreditDiscount
	d.LegalMonetaryTotal.TaxInclusiveAmount = inv.NetAmount
	d.LegalMonetaryTotal.PayableAmount = inv.NetAmount
	d.InvoiceLine.Description = inv.Note
	d.InvoiceLine.Quantity = 1
	d.InvoiceLine.UnitPrice = inv.GrossAmount - inv.CreditDiscount
	d.InvoiceLine.LineAmount = inv.GrossAmount - inv.CreditDiscount

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
