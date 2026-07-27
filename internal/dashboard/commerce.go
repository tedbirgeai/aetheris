// Package dashboard — TİCARİLEŞTİRME + GÜVENLİK + KvKK + PWA (v2, kendi kendine yeten).
//
// SIFIR SÜRTÜNME: Bu dosya TEK BAŞINA çalışır. main.go / config.go / dashboard.go
// içinde EK ALAN doldurmanıza GEREK YOKTUR — tüm ayarları ortam değişkenlerinden
// (env) okur ve ilk istekte tembel (lazy) başlatır. Sadece bu dosyayı
// internal/dashboard/commerce.go olarak koyup derleyin.
//
// SIFIR DIŞ BAĞIMLILIK: yalnızca Go standart kütüphanesi + repo içi pkg/billing
// ve pkg/voucher.
//
// Kapsananlar (madde bazında):
//   1  tenant auth: AETHERIS_API_KEYS'e karşı sabit-zaman doğrulama
//   2  filterCredits: TAM clientID eşleşmesi (çapraz-kiracı sızıntı yok)
//   4  Stripe Checkout Session (pkg/billing.CreateCheckoutSession)
//   5  Stripe Webhook (HMAC-SHA256; checkout.session.completed → denetim günlüğü)
//   6  Voucher issue/redeem (pkg/voucher, Ed25519, double-spend, WAL kalıcı ledger)
//   7  e-Fatura taslağı (pkg/billing.Invoice, JSON + UBL-TR XML)
//   8  KvKK aydınlatma ucu
//   9  (panel tarafında çerez bildirimi)
//   10 gerçek TCKN algoritma doğrulaması
//   12 rate limiting (public commerce uçları için token-bucket)
//   13 PWA manifest + service worker
//
// Yeni ortam değişkenleri (hepsi opsiyonel; tanımsızsa güvenli varsayılan):
//   AETHERIS_VOUCHER_SEED          64 hex (kalıcı Ed25519 issuer). Yoksa geçici.
//   AETHERIS_VOUCHER_LEDGER        voucher WAL yolu (varsayılan ./wal/vouchers.jsonl)
//   AETHERIS_STRIPE_SECRET         Stripe gizli anahtarı (checkout için)
//   AETHERIS_STRIPE_WEBHOOK_SECRET Stripe webhook imza sırrı
//   AETHERIS_PRICE_STARTER/GROWTH/SCALE   plan→price_... eşlemesi
//   AETHERIS_BILLING_LOG           tamamlanan ödeme denetim günlüğü (varsayılan ./wal/billing.jsonl)
//   AETHERIS_API_KEYS              mevcut: "clientID:secret,clientID2:secret2"

package dashboard

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tedbirgeai/aetheris/pkg/billing"
	"github.com/tedbirgeai/aetheris/pkg/voucher"
)

// ============================================================
//  Tembel (lazy) ticari durum — env'den okunur, tek sefer kurulur.
// ============================================================

var commerce struct {
	once         sync.Once
	issuer       *voucher.Issuer
	ledger       *voucher.Ledger
	apiKeys      map[string]string // clientID -> secret
	stripeSecret string
	stripeHook   string
	priceIDs     map[string]string
	ledgerPath   string
	billingLog   string
	rl           *rateLimiter
	mu           sync.Mutex
}

func commerceInit() {
	commerce.once.Do(func() {
		// API anahtarları: "id:secret,id2:secret2"
		commerce.apiKeys = map[string]string{}
		for _, pair := range strings.Split(os.Getenv("AETHERIS_API_KEYS"), ",") {
			pair = strings.TrimSpace(pair)
			if id, secret, ok := strings.Cut(pair, ":"); ok && id != "" && secret != "" {
				commerce.apiKeys[id] = secret
			}
		}
		// Voucher issuer: kalıcı seed varsa deterministik, yoksa geçici.
		if seed := os.Getenv("AETHERIS_VOUCHER_SEED"); len(seed) >= 64 {
			if raw, err := hex.DecodeString(seed[:64]); err == nil {
				commerce.issuer, _ = voucher.IssuerFromSeed(raw)
			}
		}
		if commerce.issuer == nil {
			// P0: env seed yoksa diskten kalıcı seed türet/oluştur — restart'ta
			// operatör kimliği korunur (aksi halde eski voucher'lar doğrulanamaz).
			commerce.issuer = loadOrCreateIssuer(filepath.Join("wal", "voucher_seed.hex"))
		}
		// Voucher WAL ledger: redemption'ları JSONL olarak diske ekler (kalıcı).
		commerce.ledgerPath = envOr("AETHERIS_VOUCHER_LEDGER", filepath.Join("wal", "vouchers.jsonl"))
		commerce.ledger = voucher.NewLedger(func(e voucher.LedgerEntry) {
			appendJSONL(commerce.ledgerPath, e)
		})
		// P0: double-spend restart koruması — diskteki ledger'dan harcanmış
		// serial SHA'larını geri yükle (in-memory ledger.seen restart'ta boşalır).
		loadSpentSerials(commerce.ledgerPath)
		commerce.stripeSecret = os.Getenv("AETHERIS_STRIPE_SECRET")
		commerce.stripeHook = os.Getenv("AETHERIS_STRIPE_WEBHOOK_SECRET")
		commerce.priceIDs = map[string]string{
			"price_starter": os.Getenv("AETHERIS_PRICE_STARTER"),
			"price_growth":  os.Getenv("AETHERIS_PRICE_GROWTH"),
			"price_scale":   os.Getenv("AETHERIS_PRICE_SCALE"),
		}
		commerce.billingLog = envOr("AETHERIS_BILLING_LOG", filepath.Join("wal", "billing.jsonl"))
		commerce.rl = newRateLimiter(30, time.Minute) // IP başına dk'da 30 istek
	})
}

// RegisterCommerceRoutes, Pay uçlarını + PWA + KvKK'yı bağlar (dashboard.go'nun
// RegisterRoutes fonksiyonu bunu çağırır).
func (s *Server) RegisterCommerceRoutes(mux *http.ServeMux) {
	commerceInit()
	mux.HandleFunc("/api/v1/billing/checkout", s.rlWrap(s.handleCheckout))
	mux.HandleFunc("/api/v1/billing/webhook", s.rlWrap(s.handleStripeHook))
	mux.HandleFunc("/api/v1/voucher/issue", s.rlWrap(s.handleVoucherIssue))
	mux.HandleFunc("/api/v1/voucher/redeem", s.rlWrap(s.handleVoucherRedeem))
	mux.HandleFunc("/admin/invoice", s.rlWrap(s.handleInvoice))
	mux.HandleFunc("/admin/manifest.webmanifest", s.handleManifest)
	mux.HandleFunc("/admin/sw.js", s.handleSW)
	mux.HandleFunc("/kvkk", s.handleKVKK)
}

// ============================================================
//  MADDE 12 — rate limiting (stdlib token-bucket, IP başına)
// ============================================================

type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: map[string][]time.Time{}, limit: limit, window: window}
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cut := now.Add(-rl.window)
	fresh := rl.hits[ip][:0]
	for _, t := range rl.hits[ip] {
		if t.After(cut) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= rl.limit {
		rl.hits[ip] = fresh
		return false
	}
	rl.hits[ip] = append(fresh, now)
	return true
}

func (s *Server) rlWrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if commerce.rl != nil && !commerce.rl.allow(clientIP(r)) {
			http.Error(w, "çok fazla istek", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		if i := strings.IndexByte(h, ','); i > 0 {
			return strings.TrimSpace(h[:i])
		}
		return strings.TrimSpace(h)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ============================================================
//  MADDE 1 & 2 — GÜVENLİK: tenant auth + kredi izolasyonu
//  (dashboard.go bu iki fonksiyonu ARTIK tanımlamaz — buradadır)
// ============================================================

func (s *Server) validTenantKey(key string) bool {
	if key == "" {
		return false
	}
	if constTimeEq(key, s.cfg.AdminToken) {
		return true
	}
	commerceInit()
	id, secret, ok := strings.Cut(key, ":")
	if !ok || id == "" || secret == "" {
		return false
	}
	want, exists := commerce.apiKeys[id]
	if !exists {
		return false
	}
	// P0: API anahtarı düz metin YERİNE hash olarak saklanabilir. Kayıtlı değer
	// 64-hex ise sha256(secret) ile sabit-zamanda karşılaştırılır (sızıntı direnci).
	if len(want) == 64 && isHex(want) {
		return constTimeEq(sha256Hex(secret), want)
	}
	return constTimeEq(secret, want)
}

func tenantClientID(key string) string {
	if id, _, ok := strings.Cut(key, ":"); ok {
		return id
	}
	return key
}

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
	if !s.tokenOK(r) {
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
	if p, ok := commerce.priceIDs[req.Plan]; ok && p != "" {
		priceID = p
	}
	if commerce.stripeSecret == "" {
		writeJSON(w, map[string]any{"url": "", "error": "AETHERIS_STRIPE_SECRET tanımsız — canlı oturum üretilemez"})
		return
	}
	base := "https://" + r.Host
	url, err := billing.CreateCheckoutSession(
		priceID, base+"/billing/success?session_id={CHECKOUT_SESSION_ID}",
		base+"/billing/cancel", req.ClientID)
	writeJSON(w, map[string]any{"url": url, "error": errStr(err)})
}

// ============================================================
//  MADDE 5 — Stripe Webhook (gelen; HMAC-SHA256 + denetim günlüğü)
// ============================================================

func (s *Server) handleStripeHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST gerekli", http.StatusMethodNotAllowed)
		return
	}
	body := readLimited(r, 1<<20)
	if commerce.stripeHook == "" {
		http.Error(w, "webhook sırrı yapılandırılmamış", http.StatusServiceUnavailable)
		return
	}
	if err := billing.VerifyStripeSignature(body, r.Header.Get("Stripe-Signature"), commerce.stripeHook); err != nil {
		http.Error(w, "imza geçersiz", http.StatusBadRequest)
		return
	}
	ev, err := billing.ParseStripeEvent(body)
	if err != nil {
		http.Error(w, "gövde ayrıştırılamadı", http.StatusBadRequest)
		return
	}
	if sess, err := billing.HandleCheckoutCompleted(ev); err == nil {
		// Kalıcı denetim: hangi client_id ne kadar ödedi (kota yükseltme kaydı).
		appendJSONL(commerce.billingLog, map[string]any{
			"ts": time.Now().Unix(), "event": ev.Type, "session": sess.ID,
			"client_id": sess.Metadata["client_id"], "amount_total": sess.AmountTotal,
			"currency": sess.Currency, "email": sess.CustomerEmail,
		})
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"received":true}`))
}

// ============================================================
//  MADDE 6 — Voucher issue / redeem (Ed25519 + kalıcı WAL ledger)
// ============================================================

func (s *Server) handleVoucherIssue(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
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
	v, err := commerce.issuer.NewVoucher(req.Bearer, req.GB*1073741824, 90*24*time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raw, _ := v.Marshal()
	writeJSON(w, map[string]any{
		"serial": v.SerialNo, "issuer": v.IssuerID, "bearer": v.BearerID,
		"credits": v.Credits, "sig": hex.EncodeToString(v.Sig),
		"payload_sha": v.PayloadSHA256(), "expires_at": v.ExpiresAt,
		"raw": string(raw),
	})
}

func (s *Server) handleVoucherRedeem(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	body := readLimited(r, 1<<16)
	v, err := voucher.Unmarshal(body)
	if err != nil {
		http.Error(w, "voucher ayrıştırılamadı", http.StatusBadRequest)
		return
	}
	// P0 double-spend: kalıcı "harcanmış serial" kümesini restart'a dayanıklı
	// şekilde önce kontrol et (in-memory ledger.seen restart'ta boşalır).
	serialSHA := sha256Hex(v.SerialNo)
	if serialSpent(serialSHA) {
		writeJSON(w, map[string]any{"error": true, "message": voucher.ErrAlreadyRedeemed.Error()})
		return
	}
	// Zero-Knowledge: imza doğrulanır, double-spend engellenir, yalnızca
	// SHA-256 + bayt WAL'a yazılır (onWrite → vouchers.jsonl).
	commerce.mu.Lock()
	err = commerce.ledger.Redeem(v, commerce.issuer.PublicKey())
	bal := commerce.ledger.Balance(v.BearerID)
	commerce.mu.Unlock()
	if err != nil {
		writeJSON(w, map[string]any{"error": true, "message": err.Error()})
		return
	}
	markSpent(serialSHA)
	writeJSON(w, map[string]any{
		"error": false, "message": "İmza doğrulandı, kredi WAL ledger'a işlendi.",
		"payload_sha": v.PayloadSHA256(), "balance": bal,
	})
}

// ============================================================
//  MADDE 7 & 10 — e-Fatura taslağı + gerçek TCKN doğrulama
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

	vkn, tckn := req.VKN, ""
	if len(req.VKN) == 11 {
		if !validTCKN(req.VKN) { // MADDE 10: gerçek algoritma
			http.Error(w, "geçersiz TCKN (algoritma doğrulaması başarısız)", http.StatusBadRequest)
			return
		}
		vkn, tckn = "", req.VKN
	} else if len(req.VKN) == 10 && !validVKN(req.VKN) {
		http.Error(w, "geçersiz VKN (algoritma doğrulaması başarısız)", http.StatusBadRequest)
		return
	}

	idBuf := make([]byte, 16)
	_, _ = cryptorand.Read(idBuf)
	now := time.Now()
	inv := &billing.Invoice{
		UUID: hex.EncodeToString(idBuf), IssueDate: now,
		IssueDateStr: now.Format("2006-01-02"), IssueTimeStr: now.Format("15:04:05"),
		BuyerName: req.BuyerName, VKN: vkn, TCKN: tckn, Email: req.Email,
		GrossAmount: req.Gross, KDVRate: billing.KDVRate, Currency: "TRY",
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
	var out any = inv
	if req.Fmt == "xml" {
		b, _ := inv.ToXML()
		out = string(b)
	}
	writeJSON(w, map[string]any{"draft": out})
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// validTCKN, T.C. Kimlik No algoritmasını uygular (11 hane, 0 ile başlamaz,
// 10. ve 11. hane checksum kuralları).
func validTCKN(s string) bool {
	if len(s) != 11 || s[0] == '0' {
		return false
	}
	var d [11]int
	for i := 0; i < 11; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		d[i] = int(s[i] - '0')
	}
	odd := d[0] + d[2] + d[4] + d[6] + d[8]
	even := d[1] + d[3] + d[5] + d[7]
	if ((odd*7-even)%10+10)%10 != d[9] {
		return false
	}
	sum := 0
	for i := 0; i < 10; i++ {
		sum += d[i]
	}
	return sum%10 == d[10]
}

// validVKN, 10 haneli Vergi Kimlik No checksum algoritmasını uygular.
func validVKN(s string) bool {
	if len(s) != 10 {
		return false
	}
	var v [10]int
	for i := 0; i < 10; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		v[i] = int(s[i] - '0')
	}
	sum := 0
	for i := 0; i < 9; i++ {
		tmp := (v[i] + (9 - i)) % 10
		p := (tmp * pow2mod(9-i)) % 9
		if tmp != 0 && p == 0 {
			p = 9
		}
		sum += p
	}
	return (10-(sum%10))%10 == v[9]
}

func pow2mod(exp int) int {
	r := 1
	for i := 0; i < exp; i++ {
		r = (r * 2) % 9
	}
	return r
}

// ============================================================
//  MADDE 13 — PWA: manifest + service worker
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
	_, _ = w.Write([]byte(`const C="tedbirge-gw-v1";
self.addEventListener("install",e=>{self.skipWaiting();e.waitUntil(caches.open(C).then(c=>c.addAll(["/admin"])));});
self.addEventListener("activate",e=>{e.waitUntil(caches.keys().then(k=>Promise.all(k.filter(x=>x!==C).map(x=>caches.delete(x)))));self.clients.claim();});
self.addEventListener("fetch",e=>{const u=new URL(e.request.url);
if(e.request.method!=="GET"||u.pathname.indexOf("/api/")===0||u.pathname.indexOf("/ws")!==-1){return;}
e.respondWith(fetch(e.request).then(r=>{const cp=r.clone();caches.open(C).then(c=>c.put(e.request,cp));return r;}).catch(()=>caches.match(e.request)));});`))
}

// ============================================================
//  MADDE 8 — KvKK aydınlatma ucu
// ============================================================

func (s *Server) handleKVKK(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"controller": "Tedbirge Teknoloji A.Ş.", "contact": "kvkk@tedbirge.ai",
		"law":  "6698 KVKK",
		"data": []string{"unvan", "vkn_tckn", "email", "api_key", "usage_metadata_sha256"},
		"purposes":       []string{"sozlesme_ifasi", "e_fatura_gib", "ucretlendirme", "guvenlik"},
		"retention":      "mali kayitlar 10 yil (VUK/TTK); digerleri amac sona erince silinir",
		"transfers":      []string{"stripe", "gib_entegrator"},
		"rights":         "erisim, duzeltme, silme, itiraz (m.11)",
		"zero_knowledge": true,
	})
}

// ---- yardımcılar ----

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

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

func readLimited(r *http.Request, max int) []byte {
	out := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil || len(out) >= max {
			return out
		}
	}
}

func appendJSONL(path string, v any) {
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(v)
	_, _ = f.Write(append(b, '\n'))
}

// ============================================================
//  P0 yardımcıları — double-spend kalıcılığı, kalıcı seed, API key hash
// ============================================================

var voucherSpent = struct {
	mu  sync.Mutex
	set map[string]struct{}
}{set: make(map[string]struct{})}

func serialSpent(sha string) bool {
	voucherSpent.mu.Lock()
	defer voucherSpent.mu.Unlock()
	_, ok := voucherSpent.set[sha]
	return ok
}

func markSpent(sha string) {
	voucherSpent.mu.Lock()
	voucherSpent.set[sha] = struct{}{}
	voucherSpent.mu.Unlock()
}

// loadSpentSerials, diskteki voucher ledger'ından (JSONL) harcanmış serial
// SHA'larını belleğe yükler — restart sonrası double-spend'i engeller (P0).
func loadSpentSerials(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	voucherSpent.mu.Lock()
	defer voucherSpent.mu.Unlock()
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e voucher.LedgerEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e.SerialSHA != "" {
			voucherSpent.set[e.SerialSHA] = struct{}{}
		}
	}
}

// loadOrCreateIssuer, kalıcı Ed25519 issuer kimliği sağlar (P0): seed dosyası
// varsa ondan türetir, yoksa güvenli üretip diske yazar (0600). Böylece
// restart'ta operatör kimliği ve eski voucher doğrulaması korunur.
func loadOrCreateIssuer(seedPath string) *voucher.Issuer {
	if raw, err := os.ReadFile(seedPath); err == nil {
		if seed, derr := hex.DecodeString(strings.TrimSpace(string(raw))); derr == nil {
			if iss, ierr := voucher.IssuerFromSeed(seed); ierr == nil {
				return iss
			}
		}
	}
	seed := make([]byte, 32)
	if _, err := cryptorand.Read(seed); err != nil {
		iss, _ := voucher.NewIssuer()
		return iss
	}
	_ = os.MkdirAll(filepath.Dir(seedPath), 0o755)
	_ = os.WriteFile(seedPath, []byte(hex.EncodeToString(seed)), 0o600)
	if iss, err := voucher.IssuerFromSeed(seed); err == nil {
		return iss
	}
	iss, _ := voucher.NewIssuer()
	return iss
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
