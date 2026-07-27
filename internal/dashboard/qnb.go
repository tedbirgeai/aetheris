// QNB sanal POS + QNB eFinans'ı panele bağlayan HTTP route'ları.
//
// Bu dosyayı internal/dashboard/qnb.go olarak koyun. Yalnızca stdlib + pkg/billing.
// Mevcut commerce.go'ya DOKUNULMAZ; tek gereken: RegisterCommerceRoutes'un
// SONUNA şu satırı ekleyin:
//
//     s.registerQNBRoutes(mux)
//
// Akış (uçtan uca):
//   1. Panel "Öde" → GET/POST /api/v1/billing/qnb/start?plan=price_growth&client_id=acme
//      → sunucu imzalı 3D formu üretir ve otomatik-submit HTML döndürür.
//   2. Tarayıcı QNB'ye POST eder → kullanıcı 3D doğrular.
//   3. QNB → POST /billing/qnb/ok  (VerifyCallback → başarılıysa e-Fatura kesilir)
//              POST /billing/qnb/fail (hata sayfası)
//
// CANLIYA ALMA: yalnızca .env'e QNB anahtarları girilir (qnbpos.go/qnbefinans.go
// başlıklarındaki AETHERIS_QNBPOS_* ve AETHERIS_EFINANS_* değişkenleri).

package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tedbirgeai/aetheris/pkg/billing"
)

// registerQNBRoutes, QNB ödeme + e-Fatura uçlarını bağlar.
func (s *Server) registerQNBRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/billing/qnb/start", s.rlWrap(s.handleQNBStart))
	mux.HandleFunc("/billing/qnb/ok", s.rlWrap(s.handleQNBOk))
	mux.HandleFunc("/billing/qnb/fail", s.rlWrap(s.handleQNBFail))
	mux.HandleFunc("/admin/invoice/send", s.rlWrap(s.handleInvoiceSend))
	mux.HandleFunc("/admin/invoice/record", s.rlWrap(s.handleInvoiceRecord))
	mux.HandleFunc("/admin/invoice/list", s.rlWrap(s.handleInvoiceList))
}

// invoiceDraft, panelin ürettiği e-Fatura taslağının sunucu tarafı karşılığı.
type invoiceDraft struct {
	FaturaNo  string  `json:"fatura_no"`
	ETTN      string  `json:"ettn"`
	BelgeTipi string  `json:"belge_tipi"`
	BuyerName string  `json:"buyer_name"`
	VKN       string  `json:"vkn"`
	TCKN      string  `json:"tckn"`
	Email     string  `json:"email"`
	Matrah    float64 `json:"matrah"`
	Indirim   float64 `json:"kredi_indirimi"`
	Vergiye   float64 `json:"vergiye_tabi"`
	KDVOrani  int     `json:"kdv_orani"`
	KDVTutari float64 `json:"kdv_tutari"`
	Odenecek  float64 `json:"odenecek"`
	Status    string  `json:"status"`
}

// handleInvoiceRecord, kesilen fatura taslağını billing.jsonl'e kalıcı yazar.
func (s *Server) handleInvoiceRecord(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	var d invoiceDraft
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "gövde ayrıştırılamadı", http.StatusBadRequest)
		return
	}
	if d.Status == "" {
		d.Status = "taslak"
	}
	appendJSONL(commerce.billingLog, map[string]any{
		"ts": time.Now().Unix(), "event": "invoice", "fatura_no": d.FaturaNo, "ettn": d.ETTN,
		"buyer_name": d.BuyerName, "vkn": d.VKN, "tckn": d.TCKN, "odenecek": d.Odenecek, "status": d.Status,
	})
	writeJSON(w, map[string]any{"ok": true})
}

// handleInvoiceList, billing.jsonl'deki fatura kayıtlarını (en yeni önce) döndürür.
func (s *Server) handleInvoiceList(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	data, err := os.ReadFile(commerce.billingLog)
	if err != nil {
		writeJSON(w, []any{})
		return
	}
	out := make([]map[string]any, 0, 32)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if ev, _ := m["event"].(string); ev == "invoice" || ev == "einvoice_sent" {
			if _, ok := m["fatura_no"]; ok {
				out = append(out, m)
			}
		}
	}
	// en yeni önce
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	writeJSON(w, out)
}

// handleInvoiceSend, e-Fatura taslağını QNB eFinans'a gönderir (manuel gönderim).
func (s *Server) handleInvoiceSend(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	var d invoiceDraft
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		http.Error(w, "gövde ayrıştırılamadı", http.StatusBadRequest)
		return
	}
	ef := billing.EFinansFromEnv()
	if !ef.Ready() {
		writeJSON(w, map[string]any{"error": true, "message": "QNB eFinans yapılandırılmamış (AETHERIS_EFINANS_*)"})
		return
	}
	inv := &billing.Invoice{
		UUID: d.ETTN, IssueDate: time.Now(),
		IssueDateStr: time.Now().Format("2006-01-02"), IssueTimeStr: time.Now().Format("15:04:05"),
		BuyerName: d.BuyerName, VKN: d.VKN, TCKN: d.TCKN, Email: d.Email,
		GrossAmount: d.Matrah, KDVRate: float64(d.KDVOrani) / 100, KDVAmount: d.KDVTutari,
		NetAmount: d.Odenecek, CreditDiscount: d.Indirim, Currency: "TRY",
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := ef.SendEArsiv(ctx, inv)
	if err != nil {
		appendJSONL(commerce.billingLog, map[string]any{"ts": time.Now().Unix(), "event": "einvoice_error", "fatura_no": d.FaturaNo, "error": err.Error()})
		writeJSON(w, map[string]any{"error": true, "message": err.Error()})
		return
	}
	appendJSONL(commerce.billingLog, map[string]any{
		"ts": time.Now().Unix(), "event": "einvoice_sent", "fatura_no": d.FaturaNo,
		"buyer_name": d.BuyerName, "odenecek": d.Odenecek, "ettn": res.ETTN, "status": "gönderildi",
	})
	writeJSON(w, map[string]any{"error": false, "ettn": res.ETTN, "status": res.Status})
}

// planKurus, panel planını kuruş (₺*100) tutarına çevirir.
func planKurus(plan string) int64 {
	switch plan {
	case "price_starter":
		return 10000 // ₺100,00
	case "price_growth":
		return 75000 // ₺750,00
	case "price_scale":
		return 450000 // ₺4.500,00
	default:
		return 10000
	}
}

// handleQNBStart, imzalı 3D ödeme formunu üretir ve otomatik-submit HTML döndürür.
func (s *Server) handleQNBStart(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	cfg := billing.NestPayFromEnv()
	if !cfg.Ready() {
		http.Error(w, "QNB sanal POS yapılandırılmamış (AETHERIS_QNBPOS_CLIENTID/STOREKEY/URL)", http.StatusServiceUnavailable)
		return
	}
	plan := r.URL.Query().Get("plan")
	clientID := r.URL.Query().Get("client_id")
	email := r.URL.Query().Get("email")
	orderID := "AGW-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if clientID != "" {
		orderID = clientID + "-" + orderID
	}
	fields := cfg.Build3DFields(orderID, planKurus(plan), email)

	// Otomatik-submit form: tarayıcı anında QNB 3D geçidine POST eder.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><title>QNB güvenli ödeme…</title></head>`+
		`<body style="font-family:monospace;background:#f6f7fb;color:#1f2251;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">`+
		`<div style="text-align:center"><div style="font-size:15px;margin-bottom:8px">QNB güvenli ödeme sayfasına yönlendiriliyorsunuz…</div>`+
		`<div style="font-size:12px;color:#6b7280">3D Secure</div></div>`+
		`<form id="f" method="post" action="`+html.EscapeString(cfg.GatewayURL)+`">`)
	for k, v := range fields {
		fmt.Fprintf(w, `<input type="hidden" name="%s" value="%s">`, html.EscapeString(k), html.EscapeString(v))
	}
	fmt.Fprint(w, `</form><script>document.getElementById("f").submit();</script></body></html>`)
}

// handleQNBOk, banka başarı dönüşünü doğrular ve e-Faturayı otomatik keser.
func (s *Server) handleQNBOk(w http.ResponseWriter, r *http.Request) {
	cfg := billing.NestPayFromEnv()
	params := formToMap(r)
	ok, reason := cfg.VerifyCallback(params)

	// Denetim günlüğü (her sonuç kaydedilir).
	appendJSONL(commerce.billingLog, map[string]any{
		"ts": time.Now().Unix(), "gateway": "qnb_nestpay", "oid": params["oid"],
		"amount": params["amount"], "mdStatus": params["mdStatus"],
		"procReturnCode": params["ProcReturnCode"], "verified": ok, "reason": reason,
	})

	if !ok {
		s.qnbResultPage(w, false, "Ödeme doğrulanamadı: "+reason, "")
		return
	}

	// Ödeme onaylı → e-Fatura taslağını kesip QNB eFinans'a gönder (yapılandırıldıysa).
	ettn := ""
	ef := billing.EFinansFromEnv()
	if ef.Ready() {
		amount := parseTLAmount(params["amount"])
		taxable := amount / 1.20
		inv := &billing.Invoice{
			IssueDate:    time.Now(),
			IssueDateStr: time.Now().Format("2006-01-02"),
			IssueTimeStr: time.Now().Format("15:04:05"),
			BuyerName:    params["email"], Email: params["email"],
			GrossAmount: round2(taxable), KDVRate: billing.KDVRate, Currency: "TRY",
		}
		inv.KDVAmount = round2(taxable * billing.KDVRate)
		inv.NetAmount = round2(amount)
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if res, err := ef.SendEArsiv(ctx, inv); err == nil {
			ettn = res.ETTN
			appendJSONL(commerce.billingLog, map[string]any{
				"ts": time.Now().Unix(), "event": "einvoice_sent", "oid": params["oid"],
				"ettn": ettn, "status": res.Status,
			})
		} else {
			appendJSONL(commerce.billingLog, map[string]any{
				"ts": time.Now().Unix(), "event": "einvoice_error", "oid": params["oid"],
				"error": err.Error(),
			})
		}
	}
	s.qnbResultPage(w, true, "Ödemeniz onaylandı.", ettn)
}

// handleQNBFail, banka hata dönüşünü işler.
func (s *Server) handleQNBFail(w http.ResponseWriter, r *http.Request) {
	params := formToMap(r)
	appendJSONL(commerce.billingLog, map[string]any{
		"ts": time.Now().Unix(), "gateway": "qnb_nestpay", "event": "fail",
		"oid": params["oid"], "errMsg": params["ErrMsg"], "mdStatus": params["mdStatus"],
	})
	s.qnbResultPage(w, false, "Ödeme tamamlanamadı: "+firstNonEmpty(params["ErrMsg"], params["mdErrorMsg"], "banka reddetti"), "")
}

// qnbResultPage, kullanıcıya sonuç sayfası döndürür (panele geri dön linki).
func (s *Server) qnbResultPage(w http.ResponseWriter, ok bool, msg, ettn string) {
	color := "#0a9e2f"
	icon := "✓"
	if !ok {
		color = "#d64550"
		icon = "✕"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	extra := ""
	if ettn != "" {
		extra = `<div style="font-size:12px;color:#6b7280;margin-top:8px">e-Fatura ETTN: ` + html.EscapeString(ettn) + `</div>`
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>QNB ödeme sonucu</title></head>`+
		`<body style="font-family:monospace;background:#f6f7fb;color:#1f2251;display:flex;align-items:center;justify-content:center;height:100vh;margin:0">`+
		`<div style="text-align:center;background:#fff;border:1px solid #e7e7ef;border-radius:16px;padding:40px 48px;box-shadow:0 10px 30px rgba(30,30,142,.08)">`+
		`<div style="font-size:40px;color:%s;margin-bottom:14px">%s</div>`+
		`<div style="font-size:16px;font-weight:700;margin-bottom:6px">%s</div>%s`+
		`<a href="/admin" style="display:inline-block;margin-top:20px;color:#fff;background:#1E1E8E;border-radius:8px;padding:10px 22px;text-decoration:none;font-size:13px">Panele dön</a>`+
		`</div></body></html>`, color, icon, html.EscapeString(msg), extra)
}

// ---- yardımcılar ----

func formToMap(r *http.Request) map[string]string {
	_ = r.ParseForm()
	m := make(map[string]string, len(r.Form))
	for k, v := range r.Form {
		if len(v) > 0 {
			m[k] = v[0]
		}
	}
	return m
}

// parseTLAmount, "123.45" (TL, KDV dahil) → float TL.
func parseTLAmount(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
