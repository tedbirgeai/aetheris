// Package dashboard, cekirdek binary icine GOMULU (go:embed) offline-first
// bir yonetim panelidir. Node.js veya dis CDN gerektirmez; tum HTML/CSS/JS
// tek binary icinde derlenir. /admin rotasinda sunulur ve WebSocket uzerinden
// canli telemetri yayinlar.
//
// ZERO-KNOWLEDGE: Panelin gosterdigi telemetri yalnizca toplam/istatistiksel
// verilerdir (bayt sayimi, kredi birimi, RTT). Hicbir tunel yukunun ICERIGI
// panele akmaz.
package dashboard

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/tedbirgeai/aetheris/pkg/voucher"
)

//go:embed static/*
var staticFS embed.FS

// NodeInfo, gossip mesh topolojisindeki bir dugumu tanimlar.
type NodeInfo struct {
	ID      string  `json:"id"`
	Addr    string  `json:"addr,omitempty"`
	RTTms   float64 `json:"rtt_ms"`
	Carrier string  `json:"carrier"`
	Alive   bool    `json:"alive"`
}

// CreditRow, faturalama/kredi dokumunun bir satiridir.
type CreditRow struct {
	ClientID string `json:"client_id"`
	Units    uint64 `json:"units"`
	Bytes    uint64 `json:"bytes"`
}

// Telemetry, panele gonderilen tek bir anlik durum kaydidir.
type Telemetry struct {
	TS            int64       `json:"ts"`
	Nodes         []NodeInfo  `json:"nodes"`
	WALDepth      int         `json:"wal_depth"`
	ActiveTunnels int         `json:"active_tunnels"`
	DiskBytes     uint64      `json:"disk_bytes"`
	ThroughputBps uint64      `json:"throughput_bps"`
	Credits       []CreditRow `json:"credits"`
	WANStatus     string      `json:"wan_status"`
	WANLabel      string      `json:"wan_label"`
	ExitPeer      string      `json:"exit_peer,omitempty"`
	ActiveCarrier string      `json:"active_carrier,omitempty"`
	// SOCKS5, SOCKS5 proxy sunucusunun anlik durumudur.
	SOCKS5 *SOCKS5Stat `json:"socks5,omitempty"`
	// DTN, gecikme-toleranli ag kuyrugunun durumudur.
	DTN *DTNStat `json:"dtn,omitempty"`
}

// SOCKS5Stat, SOCKS5 proxy sayaçlarıdır.
type SOCKS5Stat struct {
	Active  int64  `json:"active"`  // anlık açık bağlantı sayısı
	Handled uint64 `json:"handled"` // toplam işlenen bağlantı
	Listen  string `json:"listen"`  // dinleme adresi
}

// DTNStat, DTN store-and-forward kuyruğunun durumudur.
type DTNStat struct {
	Pending   int    `json:"pending"`   // bekleyen bundle sayısı
	Delivered uint64 `json:"delivered"` // toplam iletilen bundle
	Dir       string `json:"dir"`       // kalıcı depo dizini
}

// Provider, canli telemetriyi saglayan kaynaktir. Gateway, gossip/WAL/tunel/
// faturalama alt sistemlerini okuyan bir uygulama saglar. Snapshot ESZAMANLI
// olarak (WebSocket dongusunden) cagrilabilir olmalidir.
type Provider interface {
	Snapshot() Telemetry
}

// ProviderFunc, bir fonksiyonu Provider'a cevirir.
type ProviderFunc func() Telemetry

func (f ProviderFunc) Snapshot() Telemetry { return f() }

// Config, dashboard sunucusu ayarlaridir.
// AssetVersion, statik panel varliklarinin surumudur; cache-busting ve
// panel render garantisi icin ETag'e islenir.
const AssetVersion = "v1.0.0-enterprise"

type Config struct {
	// AdminToken, panele ve telemetri WebSocket'ine erisim icin zorunlu
	// oturum jetonudur. BOS BIRAKILAMAZ (fail-closed): token yoksa panel
	// hicbir sey sunmaz — telemetri ticari olarak hassastir.
	AdminToken string
	// Provider, canli telemetri kaynagi.
	Provider Provider
	// Interval, telemetri yayin araligi (varsayilan 1sn).
	Interval time.Duration

	// --- Ticarilestirme (commerce.go tarafindan kullanilir) ---
	// APIKeys, gecerli kiraci anahtarlari: clientID -> secret. validTenantKey
	// bu listeye karsi sabit-zamanda dogrulama yapar (madde 1).
	APIKeys map[string]string
	// Issuer, off-grid Ed25519 voucher imzalayicisi (madde 6). nil ise
	// voucher uclari 503 doner.
	Issuer *voucher.Issuer
	// Ledger, voucher redemption defteri (double-spend + WAL). nil ise
	// redeem ucu 503 doner.
	Ledger *voucher.Ledger
	// StripeWebhookSecret, gelen Stripe webhook'unun HMAC dogrulama sirri.
	StripeWebhookSecret string
	// StripePriceIDs, panel plan adini Stripe price_... kimligine esler.
	StripePriceIDs map[string]string
}

// Server, gomulu paneli ve telemetri WebSocket'ini sunar.
type Server struct {
	cfg      Config
	staticFS fs.FS
}

// New, verilen ayarlarla bir dashboard sunucusu olusturur.
func New(cfg Config) (*Server, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, staticFS: sub}, nil
}

// adminCookie, tarayici oturumu icin kullanilan cerez adidir.
const adminCookie = "aetheris_admin"

// tokenOK, istekteki admin jetonunu dogrular. Jeton su kaynaklardan gelebilir:
//   - Authorization: Bearer <token> basligi
//   - ?token= sorgu parametresi (ilk giris / WebSocket)
//   - aetheris_admin cerezi (tarayici, ilk girisin ardindan otomatik gonderir)
//
// AdminToken bos ise (yapilandirma hatasi) HER ISTEK reddedilir (fail-closed).
func (s *Server) tokenOK(r *http.Request) bool {
	if s.cfg.AdminToken == "" {
		return false // fail-closed
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if constTimeEq(strings.TrimPrefix(h, "Bearer "), s.cfg.AdminToken) {
			return true
		}
	}
	if q := r.URL.Query().Get("token"); q != "" && constTimeEq(q, s.cfg.AdminToken) {
		return true
	}
	if c, err := r.Cookie(adminCookie); err == nil && c.Value != "" {
		return constTimeEq(c.Value, s.cfg.AdminToken)
	}
	return false
}

// RegisterRoutes, panel ve telemetri rotalarini bir mux'a baglar:
//
//	GET /admin                     -> panel (index.html)
//	GET /admin/*                   -> gomulu statik varliklar (style.css, app.js)
//	GET /api/v1/ws/telemetry       -> canli telemetri WebSocket'i
//
// Tum rotalar admin jetonu korumasi altindadir. Ticarilestirme uclari
// commerce.go'daki RegisterCommerceRoutes ile eklenir.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin", s.handleAdmin)
	mux.HandleFunc("/admin/deploy", s.handleDeploy)
	mux.HandleFunc("/admin/", s.handleStatic)
	mux.HandleFunc("/api/v1/ws/telemetry", s.handleTelemetryWS)
	// Tenant (B2B SaaS) panel ve WS endpoint'i.
	mux.HandleFunc("/tenant", s.handleTenant)
	mux.HandleFunc("/tenant/", s.handleTenantStatic)
	mux.HandleFunc("/api/v1/ws/tenant", s.handleTenantWS)
	// Ticarilestirme + PWA + KvKK (commerce.go).
	s.RegisterCommerceRoutes(mux)
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	// Ilk giris jetonla yapildiysa (sorgu/baslik), tarayici oturumu icin bir
	// cerez birak; boylece sonraki statik varlik ve WebSocket istekleri jetonu
	// otomatik tasir (URL'de jeton gorunmez). HttpOnly: JS okuyamaz; SameSite
	// Strict: CSRF yuzeyi daraltilir.
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    s.cfg.AdminToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	s.serveFile(w, r, "index.html")
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" || name == "/" {
		name = "index.html"
	}
	// Yol asimini engelle.
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, name)
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	data, err := fs.ReadFile(s.staticFS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	// Cache-busting: tarayici ESKI paneli onbellekten gostermemeli.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("ETag", `"aetheris-`+AssetVersion+`"`)
	w.Header().Set("X-Aetheris-Panel", "tedbirge-"+AssetVersion)
	_, _ = w.Write(data)
}

func (s *Server) handleTelemetryWS(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	conn, err := wsUpgrade(w, r)
	if err != nil {
		http.Error(w, "WebSocket yukseltmesi basarisiz", http.StatusBadRequest)
		return
	}
	go conn.readLoop() // close/ping isle

	// Ilk kareyi hemen gonder, sonra periyodik yayin.
	if s.pushOnce(conn) != nil {
		_ = conn.Close()
		return
	}
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-conn.Done():
			return
		case <-r.Context().Done():
			_ = conn.Close()
			return
		case <-ticker.C:
			if s.pushOnce(conn) != nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func (s *Server) pushOnce(conn *wsConn) error {
	var t Telemetry
	if s.cfg.Provider != nil {
		t = s.cfg.Provider.Snapshot()
	}
	if t.TS == 0 {
		t.TS = time.Now().Unix()
	}
	if t.WANStatus == "" {
		t.WANStatus = "unknown"
	}
	if t.WANLabel == "" {
		t.WANLabel = wanLabel(t.WANStatus)
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return conn.WriteText(data)
}

func (s *Server) deny(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="aetheris-admin"`)
	http.Error(w, "yetkisiz: gecerli admin jetonu gerekli", http.StatusUnauthorized)
}

// handleDeploy, /admin/deploy uzerinden tek-tikla dugum konfigurasyon paketi
// uretir. JSON yaniti: NodeID, RelaySecret ve baslangic ortam degiskenleri.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	if !s.tokenOK(r) {
		s.deny(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST gerekli", http.StatusMethodNotAllowed)
		return
	}
	idBuf := make([]byte, 16)
	keyBuf := make([]byte, 32)
	if _, err := rand.Read(idBuf); err != nil {
		http.Error(w, "entropi hatasi", http.StatusInternalServerError)
		return
	}
	if _, err := rand.Read(keyBuf); err != nil {
		http.Error(w, "entropi hatasi", http.StatusInternalServerError)
		return
	}
	nodeID := "edge-" + hex.EncodeToString(idBuf[:8])
	relaySecret := hex.EncodeToString(keyBuf)

	pkg := map[string]interface{}{
		"node_id":      nodeID,
		"relay_secret": relaySecret,
		"version":      AssetVersion,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"env": map[string]string{
			"AETHERIS_MESH_NODE_ID":   nodeID,
			"AETHERIS_RELAY_SECRET":   relaySecret,
			"AETHERIS_DISCOVERY":      "true",
			"AETHERIS_WAN_CHECK":      "true",
			"AETHERIS_MESH":           "true",
			"AETHERIS_MESH_ADDR":      ":7946",
			"AETHERIS_DISCOVERY_PORT": "7947",
		},
		"note": "Bu paketi aetheris-gateway binary'si ile birlikte edge cihaza yukleyin.",
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+nodeID+`-config.json"`)
	_ = json.NewEncoder(w).Encode(pkg)
}

// wanLabel, WAN durum kodunu okunabilir panel etiketine cevirir (Turkce).
func wanLabel(status string) string {
	switch status {
	case "direct":
		return "Direkt WAN"
	case "relayed":
		return "Röle ile (Peer)"
	case "off_grid":
		return "İzole Mesh"
	default:
		return "Bilinmiyor"
	}
}

// --- Tenant (B2B SaaS) Panel Handlers ---

// handleTenant, tenant panelini sunar (API key cookie ile oturum acar).
func (s *Server) handleTenant(w http.ResponseWriter, r *http.Request) {
	apiKey := r.URL.Query().Get("key")
	if apiKey == "" {
		if c, err := r.Cookie("aetheris_tenant_key"); err == nil {
			apiKey = c.Value
		}
	}
	if apiKey == "" || !s.validTenantKey(apiKey) {
		http.Error(w, "Gecersiz API anahtari. /tenant?key=API_KEY", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "aetheris_tenant_key", Value: apiKey,
		Path: "/tenant", HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	s.serveFile(w, r, "tenant.html")
}

// handleTenantStatic, tenant statik varliklarini sunar. Madde 3: fail-closed —
// gecerli tenant anahtari olmadan varlik sunulmaz.
func (s *Server) handleTenantStatic(w http.ResponseWriter, r *http.Request) {
	apiKey := r.URL.Query().Get("key")
	if apiKey == "" {
		if c, err := r.Cookie("aetheris_tenant_key"); err == nil {
			apiKey = c.Value
		}
	}
	if apiKey == "" || !s.validTenantKey(apiKey) {
		http.Redirect(w, r, "/tenant", http.StatusFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/tenant/")
	if name == "" || strings.Contains(name, "..") {
		name = "tenant.html"
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("ETag", `"`+AssetVersion+`"`)
	data, err := fs.ReadFile(s.staticFS, name)
	if err != nil {
		http.Redirect(w, r, "/tenant", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	_, _ = w.Write(data)
}

// handleTenantWS, tenant-spesifik WebSocket telemetri akisini saglar.
// Her API key kendi izole oturumunu alir; sadece o musteriye ait veri akar.
func (s *Server) handleTenantWS(w http.ResponseWriter, r *http.Request) {
	apiKey := r.URL.Query().Get("key")
	if apiKey == "" {
		if c, err := r.Cookie("aetheris_tenant_key"); err == nil {
			apiKey = c.Value
		}
	}
	if apiKey == "" || !s.validTenantKey(apiKey) {
		http.Error(w, "yetkisiz", http.StatusUnauthorized)
		return
	}
	wc, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	defer wc.Close()
	interval := s.cfg.Interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-wc.Done():
			return
		case <-ticker.C:
			t := s.cfg.Provider.Snapshot()
			// Tenant izolasyonu: yalnizca bu API key'e ait veriler.
			t.Credits = filterCredits(t.Credits, apiKey)
			t.Nodes = nil // admin-only
			if t.TS == 0 {
				t.TS = time.Now().Unix()
			}
			data, err := json.Marshal(t)
			if err != nil {
				return
			}
			if err := wc.WriteText(data); err != nil {
				return
			}
		}
	}
}

// NOT: validTenantKey ve filterCredits'in GUVENLI surumleri commerce.go
// icinde tanimlidir (madde 1 & 2). Bu dosyada TEKRAR tanimlanmazlar.

// constTimeEq, iki jetonu sabit zamanda karsilastirir.
func constTimeEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".webmanifest"):
		return "application/manifest+json"
	default:
		return "application/octet-stream"
	}
}
