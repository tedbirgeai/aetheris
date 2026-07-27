// Package dashboard — TİCARİLEŞTİRME + GÜVENLİK + KvKK + PWA yaması.
//
// Bu dosyayı `internal/dashboard/commerce.go` olarak projeye ekleyin.
// Panel UI'ının (index.html) beklediği HTTP uçlarını gerçek pkg/billing ve
// pkg/voucher koduna bağlar; ayrıca tespit edilen güvenlik açıklarını kapatır.
//
// KURULUM (git bash):
//   cp ~/Downloads/commerce.go internal/dashboard/commerce.go
//   # 1) internal/dashboard/dashboard.go içinde:
//   #    - AssetVersion sabitini "v1.0.0-enterprise" yapın (madde 11).
//   #    - Config struct'ına şu alanları ekleyin (madde 1/6/7):
//   #        APIKeys            map[string]string // clientID -> secret
//   #        Issuer             *voucher.Issuer
//   #        Ledger             *voucher.Ledger
//   #        StripeWebhookSecret string
//   #        StripePriceIDs     map[string]string // plan -> Stripe price_...
//   #    - mevcut validTenantKey ve filterCredits fonksiyonlarını SİLİN
//   #      (bu dosyadaki düzeltilmiş sürümler onların yerine geçer).
//   #    - RegisterRoutes'un SONUNA: s.RegisterCommerceRoutes(mux)
//   # 2) cmd/gateway/main.go içinde dashboard.Config doldururken yeni alanları verin
//   #    (Issuer için voucher.IssuerFromSeed(seed) — seed'i AETHERIS_VOUCHER_SEED'ten).
//   gofmt -w ./... && go build ./... && go test ./...

package dashboard

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tedbirgeai/aetheris/pkg/billing"
	"github.com/tedbirgeai/aetheris/pkg/voucher"
)

// RegisterCommerceRoutes, Pay sekmesinin uçlarını + PWA + KvKK'yı bağlar.
// dashboard.go içindeki RegisterRoutes'un sonundan çağrılmalıdır.
func (s *Server) RegisterCommerceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/billing/checkout", s.handleCheckout)   // madde 4
	mux.HandleFunc("/api/v1/billing/webhook", s.handleStripeHook)  // madde 5
	mux.HandleFunc("/api/v1/voucher/issue", s.handleVoucherIssue)  // madde 6
	mux.HandleFunc("/api/v1/voucher/redeem", s.handleVoucherRedeem) // madde 6
	mux.HandleFunc("/admin/invoice", s.handleInvoice)              // madde 7
	mux.HandleFunc("/admin/manifest.webmanifest", s.handleManifest) // madde 13 (PWA)
	mux.HandleFunc("/admin/sw.js", s.handleSW)                     // madde 13 (PWA)
	mux.HandleFunc("/kvkk", s.handleKVKK)                          // madde 8
}

// ============================================================
//  MADDE 1 & 6 & 7 — GÜVENLİK: tenant auth + kredi izolasyonu
//  (dashboard.go'daki eski sürümlerin YERİNE geçer)
// ============================================================

// validTenantKey, API anahtarını yapılandırılmış GERÇEK anahtar listesine karşı
// doğrular. Eski sürüm sadece uzunluğa bakıyordu (herhangi 16+ karakter kabul).
func (s *Server) validTenantKey(key string) bool {
	if key == "" {
		return false
	}
	if constTimeEq(key, s.cfg.AdminToken) {
		return true
	}
	// Biçim: "clientID:secret" — secret sabit-zamanda karşılaştırılır.
	id, secret, ok := strings.Cut(key, ":")
	if !ok || id == "" || secret == "" {
		return false
	}
	want, exists := s.cfg.APIKeys[id]
	if !exists {
		return false
	}
	return constTimeEq(secret, want)
}

// tenantClientID, API anahtarından clientID kısmını çıkarır.
func tenantClientID(key string) string {
	if id, _, ok := strings.Cut(key, ":"); ok {
		return id
	}
	return key
}

// filterCredits, YALNIZCA tam eşleşen clientID satırlarını döndürür.
// Eski sürüm 8-karakter ÖN-EK eşleşmesi yapıyordu → çapraz-kiracı sızıntı riski.
func filterCredits(rows []CreditRow, apiKey string) []CreditRow {
	id := tenantClientID(apiKey)
	out := make([]CreditRow, 0, len(rows))
	for _, r := range rows {
		if r.ClientID == id {
			out = append(out, r)
		}
	}
	return out
}

// ============================================================
//  MADDE 4 — Stripe Checkout Session
// ============================================================

func (s *Server) handleCheckout(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) { // admin veya geçerli oturum
		s.deny(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST gerekli", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Plan     string `json:"plan"`
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	priceID := req.Plan
	if s.cfg.StripePriceIDs != nil {
		if p, ok := s.cfg.StripePriceIDs[req.Plan]; ok {
			priceID = p
		}
	}
	base := "https://" + r.Host
	url, err := billing.CreateCheckoutSession(
		priceID, base+"/billing/success?session_id={CHECKOUT_SESSION_ID}",
		base+"/billing/cancel", req.ClientID)
	writeJSON(w, map[string]any{"url": url, "error": errStr(err)})
}

// ============================================================
//  MADDE 5 — Stripe Webhook (gelen; HMAC-SHA256 Stripe-Signature)
// ============================================================

func (s *Server) handleStripeHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST gerekli", http.StatusMethodNotAllowed)
		return
	}
	body := make([]byte, 0, 1<<16)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
		if len(body) > 1<<20 {
			http.Error(w, "gövde çok büyük", http.StatusRequestEntityTooLarge)
			return
		}
	}
	sig := r.Header.Get("Stripe-Signature")
	if err := billing.VerifyStripeSignature(body, sig, s.cfg.StripeWebhookSecret); err != nil {
		http.Error(w, "imza geçersiz", http.StatusBadRequest)
		return
	}
	ev, err := billing.ParseStripeEvent(body)
	if err != nil {
		http.Error(w, "gövde ayrıştırılamadı", http.StatusBadRequest)
		return
	}
	if sess, err := billing.HandleCheckoutCompleted(ev); err == nil {
		// TODO: sess.Metadata["client_id"] için kota/kredi yükselt.
		_ = sess
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"received":true}`))
}

// ============================================================
//  MADDE 6 — Voucher issue / redeem (pkg/voucher, Ed25519, WAL ledger)
// ============================================================

func (s *Server) handleVoucherIssue(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	if s.cfg.Issuer == nil {
		http.Error(w, "issuer yapılandırılmamış (AETHERIS_VOUCHER_SEED)", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Bearer string `json:"bearer"`
		GB     uint64 `json:"gb"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.GB == 0 {
		req.GB = 1
	}
	v, err := s.cfg.Issuer.NewVoucher(req.Bearer, req.GB*1073741824, 90*24*time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"serial": v.SerialNo, "issuer": v.IssuerID, "bearer": v.BearerID,
		"credits": v.Credits, "sig": hex.EncodeToString(v.Sig),
		"payload_sha": v.PayloadSHA256(), "expires_at": v.ExpiresAt,
	})
}

func (s *Server) handleVoucherRedeem(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	if s.cfg.Issuer == nil || s.cfg.Ledger == nil {
		http.Error(w, "voucher altyapısı yapılandırılmamış", http.StatusServiceUnavailable)
		return
	}
	body, _ := readAll(r)
	v, err := voucher.Unmarshal(body)
	if err != nil {
		http.Error(w, "voucher ayrıştırılamadı", http.StatusBadRequest)
		return
	}
	// Zero-Knowledge: Ledger.Redeem imzayı doğrular, double-spend'i engeller,
	// yalnızca SHA-256 + bayt miktarını WAL'a yazar.
	if err := s.cfg.Ledger.Redeem(v, s.cfg.Issuer.PublicKey()); err != nil {
		writeJSON(w, map[string]any{"error": true, "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{
		"error": false, "message": "İmza doğrulandı, kredi WAL ledger'a işlendi.",
		"payload_sha": v.PayloadSHA256(), "balance": s.cfg.Ledger.Balance(v.BearerID),
	})
}

// ============================================================
//  MADDE 7 — e-Fatura taslağı (pkg/billing.Invoice, JSON/UBL-TR XML)
// ============================================================

func (s *Server) handleInvoice(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	var req struct {
		BuyerName string  `json:"buyer_name"`
		VKN       string  `json:"vkn"`
		Email     string  `json:"email"`
		Gross     float64 `json:"gross"`
		Discount  float64 `json:"discount"`
		Fmt       string  `json:"fmt"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	idBuf := make([]byte, 16)
	_, _ = cryptorand.Read(idBuf)
	vkn, tckn := req.VKN, ""
	if len(req.VKN) == 11 { // madde 10: TCKN 11 hane
		vkn, tckn = "", req.VKN
	}
	inv := &billing.Invoice{
		UUID:           hex.EncodeToString(idBuf),
		IssueDate:      time.Now(),
		IssueDateStr:   time.Now().Format("2006-01-02"),
		IssueTimeStr:   time.Now().Format("15:04:05"),
		BuyerName:      req.BuyerName,
		VKN:            vkn,
		TCKN:           tckn,
		Email:          req.Email,
		GrossAmount:    req.Gross,
		KDVRate:        billing.KDVRate,
		Currency:       "TRY",
		CreditDiscount: req.Discount,
	}
	taxable := req.Gross - req.Discount
	if taxable < 0 {
		taxable = 0
	}
	inv.KDVAmount = round2(taxable * billing.KDVRate)
	inv.NetAmount = round2(taxable + inv.KDVAmount)
	if err := inv.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var out any
	if req.Fmt == "xml" {
		b, _ := inv.ToXML()
		out = string(b)
	} else {
		out = inv
	}
	writeJSON(w, map[string]any{"draft": out})
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ============================================================
//  MADDE 13 — PWA: manifest + service worker (offline kabuk)
// ============================================================

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	_, _ = w.Write([]byte(`{
  "name": "tedbirge Gateway",
  "short_name": "tedbirge GW",
  "description": "tedbirge Gateway Enterprise Console",
  "start_url": "/admin",
  "scope": "/admin",
  "display": "standalone",
  "background_color": "#f6f7fb",
  "theme_color": "#1E1E8E",
  "icons": [
    { "src": "/admin/logo.png", "sizes": "192x192", "type": "image/png", "purpose": "any maskable" },
    { "src": "/admin/logo.png", "sizes": "512x512", "type": "image/png", "purpose": "any maskable" }
  ]
}`))
}

func (s *Server) handleSW(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Service-Worker-Allowed", "/admin")
	// Kabuk-önbellekli minimal SW: panel açılışını hızlandırır, kısa süreli
	// çevrimdışı erişim sağlar. Telemetri WS'i her zaman canlıdır (cache'lenmez).
	_, _ = w.Write([]byte(`const C="tedbirge-gw-v1";
self.addEventListener("install",e=>{self.skipWaiting();e.waitUntil(caches.open(C).then(c=>c.addAll(["/admin"])));});
self.addEventListener("activate",e=>{e.waitUntil(caches.keys().then(k=>Promise.all(k.filter(x=>x!==C).map(x=>caches.delete(x)))));self.clients.claim();});
self.addEventListener("fetch",e=>{
  const u=new URL(e.request.url);
  if(e.request.method!=="GET"||u.pathname.indexOf("/api/")===0||u.pathname.indexOf("/ws")!==-1){return;}
  e.respondWith(fetch(e.request).then(r=>{const cp=r.clone();caches.open(C).then(c=>c.put(e.request,cp));return r;}).catch(()=>caches.match(e.request)));
});`))
}

// ============================================================
//  MADDE 8 — KvKK aydınlatma metni (makine-okunur; UI ayrıca gömülü gösterir)
// ============================================================

func (s *Server) handleKVKK(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"controller":     "Tedbirge Teknoloji A.Ş.",
		"contact":        "kvkk@tedbirge.ai",
		"law":            "6698 KVKK",
		"data":           []string{"unvan", "vkn_tckn", "email", "api_key", "usage_metadata_sha256"},
		"purposes":       []string{"sozlesme_ifasi", "e_fatura_gib", "ucretlendirme", "guvenlik"},
		"retention":      "mali kayitlar 10 yil (VUK/TTK); digerleri amac sona erince silinir",
		"transfers":      []string{"stripe", "gib_entegrator"},
		"rights":         "erisim, duzeltme, silme, itiraz (m.11)",
		"zero_knowledge": true,
	})
}

// ---- küçük yardımcılar (bu dosyaya özel) ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func readAll(r *http.Request) ([]byte, error) {
	out := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil || len(out) > 1<<20 {
			return out, nil
		}
	}
}
