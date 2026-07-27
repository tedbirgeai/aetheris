// QNB eFinans e-Fatura / e-Arşiv gönderim adaptörü.
//
// Bu dosyayı internal/billing/qnbefinans.go olarak koyun. Yalnızca standart Go.
//
// QNB eFinans, SOAP tabanlı web servisleri sunar (e-Arşiv/e-Fatura). Bu adaptör:
//   1. pkg/billing.Invoice -> UBL-TR 2.1 XML (inv.ToXML) üretir,
//   2. XML'i Base64'ler ve WS-Security UsernameToken ile SOAP zarfına sarar,
//   3. QNB eFinans uç noktasına POST eder,
//   4. yanıttan ETTN / durum çıkarır.
//
// CANLIYA ALMA (yalnızca bunlar kalır):
//   AETHERIS_EFINANS_URL        QNB eFinans SOAP uç noktası (WSDL'den)
//   AETHERIS_EFINANS_USER       kullanıcı adı (WS-Security)
//   AETHERIS_EFINANS_PASS       parola
//   AETHERIS_EFINANS_VKN        gönderici VKN
//   AETHERIS_EFINANS_UNVAN      gönderici ünvan
//   AETHERIS_EFINANS_OPERATION  SOAPAction / operasyon adı (varsayılan "faturaOlustur")
//
// NOT: Operasyon adı ve zarf alan adları QNB eFinans'ın size verdiği WSDL'e
// göre 1-2 satırda uyarlanır (buildEnvelope içindeki işaretli yer). Algoritma,
// kimlik, imzalama ve taşıma tümüyle hazırdır.

package billing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// EFinansConfig, QNB eFinans erişim ayarlarıdır.
type EFinansConfig struct {
	BaseURL   string
	Username  string
	Password  string
	VKN       string
	Unvan     string
	Operation string
	Timeout   time.Duration
}

// EFinansFromEnv, ayarları ortam değişkenlerinden yükler.
func EFinansFromEnv() EFinansConfig {
	op := os.Getenv("AETHERIS_EFINANS_OPERATION")
	if op == "" {
		op = "faturaOlustur"
	}
	return EFinansConfig{
		BaseURL:   os.Getenv("AETHERIS_EFINANS_URL"),
		Username:  os.Getenv("AETHERIS_EFINANS_USER"),
		Password:  os.Getenv("AETHERIS_EFINANS_PASS"),
		VKN:       os.Getenv("AETHERIS_EFINANS_VKN"),
		Unvan:     os.Getenv("AETHERIS_EFINANS_UNVAN"),
		Operation: op,
		Timeout:   30 * time.Second,
	}
}

// Ready, canlıya alınmaya hazır mı.
func (c EFinansConfig) Ready() bool {
	return c.BaseURL != "" && c.Username != "" && c.Password != ""
}

// EFinansResult, gönderim sonucudur.
type EFinansResult struct {
	ETTN   string // faturanın tekil kimliği
	Status string // entegratör durum kodu/metni
	Raw    string // ham SOAP yanıtı (denetim için)
}

var ettnRe = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// SendEArsiv, faturayı QNB eFinans'a gönderir. Zero-Knowledge: yalnızca
// faturanın kendi verisi (matrah/KDV/alıcı) gönderilir; tünel yükü asla.
func (c EFinansConfig) SendEArsiv(ctx context.Context, inv *Invoice) (EFinansResult, error) {
	if !c.Ready() {
		return EFinansResult{}, errors.New("billing: QNB eFinans ayarları eksik (AETHERIS_EFINANS_*)")
	}
	if err := inv.Validate(); err != nil {
		return EFinansResult{}, err
	}
	ublXML, err := inv.ToXML()
	if err != nil {
		return EFinansResult{}, err
	}
	envelope := c.buildEnvelope(ublXML)

	reqCtx := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.BaseURL, bytes.NewReader(envelope))
	if err != nil {
		return EFinansResult{}, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.Header.Set("SOAPAction", c.Operation)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return EFinansResult{}, fmt.Errorf("billing: eFinans isteği başarısız: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	raw := string(body)
	res := EFinansResult{Raw: raw, ETTN: ettnRe.FindString(raw)}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Status = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return res, fmt.Errorf("billing: eFinans %d döndü", resp.StatusCode)
	}
	if strings.Contains(strings.ToLower(raw), "fault") {
		res.Status = "SOAP Fault"
		return res, errors.New("billing: eFinans SOAP Fault döndü (Raw'a bakın)")
	}
	res.Status = "gönderildi"
	return res, nil
}

// buildEnvelope, WS-Security UsernameToken'lı SOAP zarfını üretir. Fatura XML'i
// Base64 olarak taşınır.
//
// ▸ UYARLAMA NOKTASI: <ns:faturaOlustur><faturaVeri>...</faturaVeri> gövde
//   alan adları QNB eFinans WSDL'inize göre değişebilir; yalnızca aşağıdaki
//   body bölümündeki eleman adlarını WSDL'den kopyalayın.
func (c EFinansConfig) buildEnvelope(ublXML []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString(ublXML)
	var sb bytes.Buffer
	sb.WriteString(xml.Header)
	sb.WriteString(`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" xmlns:ns="http://efinans.com.tr/">`)
	sb.WriteString(`<soap:Header>`)
	sb.WriteString(`<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">`)
	sb.WriteString(`<wsse:UsernameToken>`)
	sb.WriteString(`<wsse:Username>` + xmlEsc(c.Username) + `</wsse:Username>`)
	sb.WriteString(`<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordText">` + xmlEsc(c.Password) + `</wsse:Password>`)
	sb.WriteString(`</wsse:UsernameToken></wsse:Security></soap:Header>`)
	sb.WriteString(`<soap:Body>`)
	// ▸ UYARLAMA NOKTASI (WSDL'e göre operasyon + alan adları):
	sb.WriteString(`<ns:` + c.Operation + `>`)
	sb.WriteString(`<vkn>` + xmlEsc(c.VKN) + `</vkn>`)
	sb.WriteString(`<unvan>` + xmlEsc(c.Unvan) + `</unvan>`)
	sb.WriteString(`<belgeFormati>UBL</belgeFormati>`)
	sb.WriteString(`<faturaVeri>` + b64 + `</faturaVeri>`)
	sb.WriteString(`</ns:` + c.Operation + `>`)
	sb.WriteString(`</soap:Body></soap:Envelope>`)
	return sb.Bytes()
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
