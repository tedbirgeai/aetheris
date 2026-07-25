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
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"
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
	// WANStatus, dugumun dis dunya erisim durumudur:
	//   "direct"   — dogrudan internet
	//   "relayed"  — komsu exit node uzerinden internet
	//   "off_grid" — yalnizca yerel mesh (dis internet yok)
	//   "unknown"  — henuz olculmedi
	WANStatus string `json:"wan_status"`
	// WANLabel, WANStatus'un okunabilir panel etiketi.
	WANLabel string `json:"wan_label"`
	// ExitPeer, Relayed durumda internete cikilan komsu dugum (varsa).
	ExitPeer string `json:"exit_peer,omitempty"`
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
type Config struct {
	// AdminToken, panele ve telemetri WebSocket'ine erisim icin zorunlu
	// oturum jetonudur. BOS BIRAKILAMAZ (fail-closed): token yoksa panel
	// hicbir sey sunmaz — telemetri ticari olarak hassastir.
	AdminToken string
	// Provider, canli telemetri kaynagi.
	Provider Provider
	// Interval, telemetri yayin araligi (varsayilan 1sn).
	Interval time.Duration
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
// Tum rotalar admin jetonu korumasi altindadir.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin", s.handleAdmin)
	mux.HandleFunc("/admin/", s.handleStatic)
	mux.HandleFunc("/api/v1/ws/telemetry", s.handleTelemetryWS)
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
	// Panel offline; onbelleklemeye izin ver ama dogrulama iste.
	w.Header().Set("Cache-Control", "no-cache")
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

// wanLabel, WAN durum kodunu okunabilir panel etiketine cevirir.
func wanLabel(status string) string {
	switch status {
	case "direct":
		return "Direct Internet"
	case "relayed":
		return "Relayed via Peer"
	case "off_grid":
		return "Off-Grid Mesh Only"
	default:
		return "Unknown"
	}
}

// constTimeEq, iki jetonu sabit zamanda karsilastirir (zamanlama saldirisi
// yuzeyi olmadan).
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
	default:
		return "application/octet-stream"
	}
}
