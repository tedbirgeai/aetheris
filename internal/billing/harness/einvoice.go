package harness

import (
	"encoding/xml"
	"fmt"
	"time"
)

// Kurus, para birimini TAM SAYI olarak (kurus cinsinden) tutar. Faturalama
// hesaplarinda float kullanmak yuvarlanma hatasi (ve gelir sizmasi) yaratir;
// bu yuzden tum para hesaplari tam sayidir.
type Kurus int64

// TL, kurus degerini "123.45" bicimli TL string'ine cevirir (XML tutar alani).
func (k Kurus) TL() string {
	neg := ""
	v := int64(k)
	if v < 0 {
		neg = "-"
		v = -v
	}
	return fmt.Sprintf("%s%d.%02d", neg, v/100, v%100)
}

// InvoiceInput, e-Fatura uretimi icin girdilerdir.
type InvoiceInput struct {
	InvoiceID    string
	IssueDate    time.Time
	SupplierName string
	SupplierVKN  string // vergi kimlik no
	CustomerName string
	CustomerVKN  string
	// LineNet, KDV haric satir tutari (kredi dususu SONRASI net).
	LineNet Kurus
	// VATRate, KDV orani (ornegin 20 => %20).
	VATRate  int
	Currency string
}

// e-Fatura (UBL-TR) icin sadelestirilmis XML modeli. Gercek GIB semasinin
// tamami cok genistir; bu, entegratore gonderilecek cekirdek alanlari
// yapisal olarak dogru bicimde uretir (well-formed, dogrulanabilir).
type ublInvoice struct {
	XMLName       xml.Name `xml:"Invoice"`
	Xmlns         string   `xml:"xmlns,attr"`
	XmlnsCAC      string   `xml:"xmlns:cac,attr"`
	XmlnsCBC      string   `xml:"xmlns:cbc,attr"`
	UBLVersion    string   `xml:"cbc:UBLVersionID"`
	Customization string   `xml:"cbc:CustomizationID"`
	ProfileID     string   `xml:"cbc:ProfileID"`
	ID            string   `xml:"cbc:ID"`
	IssueDate     string   `xml:"cbc:IssueDate"`
	InvoiceType   string   `xml:"cbc:InvoiceTypeCode"`
	DocCurrency   string   `xml:"cbc:DocumentCurrencyCode"`
	Supplier      ublParty `xml:"cac:AccountingSupplierParty>cac:Party"`
	Customer      ublParty `xml:"cac:AccountingCustomerParty>cac:Party"`
	TaxTotal      ublTaxTotal
	LegalMonetary ublMonetaryTotal `xml:"cac:LegalMonetaryTotal"`
	InvoiceLine   ublInvoiceLine   `xml:"cac:InvoiceLine"`
}

type ublParty struct {
	Name string `xml:"cac:PartyName>cbc:Name"`
	VKN  string `xml:"cac:PartyTaxScheme>cac:TaxScheme>cbc:Name"`
}

type ublTaxTotal struct {
	XMLName    xml.Name  `xml:"cac:TaxTotal"`
	TaxAmount  ublAmount `xml:"cbc:TaxAmount"`
	Percent    string    `xml:"cac:TaxSubtotal>cbc:Percent"`
	TaxableAmt ublAmount `xml:"cac:TaxSubtotal>cbc:TaxableAmount"`
	SubTaxAmt  ublAmount `xml:"cac:TaxSubtotal>cbc:TaxAmount"`
}

type ublMonetaryTotal struct {
	LineExtension ublAmount `xml:"cbc:LineExtensionAmount"`
	TaxExclusive  ublAmount `xml:"cbc:TaxExclusiveAmount"`
	TaxInclusive  ublAmount `xml:"cbc:TaxInclusiveAmount"`
	Payable       ublAmount `xml:"cbc:PayableAmount"`
}

type ublInvoiceLine struct {
	ID            string    `xml:"cbc:ID"`
	Quantity      string    `xml:"cbc:InvoicedQuantity"`
	LineExtension ublAmount `xml:"cbc:LineExtensionAmount"`
	ItemName      string    `xml:"cac:Item>cbc:Name"`
}

type ublAmount struct {
	Currency string `xml:"currencyID,attr"`
	Value    string `xml:",chardata"`
}

// BuildEInvoiceXML, girdilerden KDV'li e-Fatura XML'i uretir. Donen XML
// iyi-bicimlidir (xml.Unmarshal ile geri okunabilir). Tutarlar tam sayi
// kurus uzerinden hesaplanir; yuvarlanma faturalama lehine belirlenir.
func BuildEInvoiceXML(in InvoiceInput) ([]byte, error) {
	if in.InvoiceID == "" {
		return nil, fmt.Errorf("harness: fatura ID bos olamaz")
	}
	if in.VATRate < 0 {
		return nil, fmt.Errorf("harness: KDV orani negatif olamaz")
	}
	cur := in.Currency
	if cur == "" {
		cur = "TRY"
	}

	// KDV = net * oran / 100 (tam sayi, yukari yuvarlama vergi lehine).
	vat := Kurus((int64(in.LineNet)*int64(in.VATRate) + 99) / 100)
	if in.VATRate == 0 {
		vat = 0
	}
	gross := in.LineNet + vat

	amt := func(k Kurus) ublAmount { return ublAmount{Currency: cur, Value: k.TL()} }

	inv := ublInvoice{
		Xmlns:         "urn:oasis:names:specification:ubl:schema:xsd:Invoice-2",
		XmlnsCAC:      "urn:oasis:names:specification:ubl:schema:xsd:CommonAggregateComponents-2",
		XmlnsCBC:      "urn:oasis:names:specification:ubl:schema:xsd:CommonBasicComponents-2",
		UBLVersion:    "2.1",
		Customization: "TR1.2",
		ProfileID:     "TEMELFATURA",
		ID:            in.InvoiceID,
		IssueDate:     in.IssueDate.Format("2006-01-02"),
		InvoiceType:   "SATIS",
		DocCurrency:   cur,
		Supplier:      ublParty{Name: in.SupplierName, VKN: in.SupplierVKN},
		Customer:      ublParty{Name: in.CustomerName, VKN: in.CustomerVKN},
		TaxTotal: ublTaxTotal{
			TaxAmount:  amt(vat),
			Percent:    fmt.Sprintf("%d", in.VATRate),
			TaxableAmt: amt(in.LineNet),
			SubTaxAmt:  amt(vat),
		},
		LegalMonetary: ublMonetaryTotal{
			LineExtension: amt(in.LineNet),
			TaxExclusive:  amt(in.LineNet),
			TaxInclusive:  amt(gross),
			Payable:       amt(gross),
		},
		InvoiceLine: ublInvoiceLine{
			ID:            "1",
			Quantity:      "1",
			LineExtension: amt(in.LineNet),
			ItemName:      "Aetheris ag tasima hizmeti",
		},
	}

	body, err := xml.MarshalIndent(inv, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("harness: e-fatura XML uretilemedi: %w", err)
	}
	return append([]byte(xml.Header), body...), nil
}
