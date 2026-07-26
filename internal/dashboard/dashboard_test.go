package dashboard

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, prov Provider) *Server {
	t.Helper()
	s, err := New(Config{
		AdminToken: "gizli-admin-jetonu",
		Provider:   prov,
		Interval:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestServesEmbeddedAssets, gomulu HTML/CSS/JS'in tek binary'den sunuldugunu
// ve DIS CDN referansi icermedigini dogrular (offline-first).
func TestServesEmbeddedAssets(t *testing.T) {
	s := testServer(t, nil)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	for _, tc := range []struct{ path, wantType, mustContain string }{
		{"/admin?token=gizli-admin-jetonu", "text/html", "AETHERIS"},
		{"/admin/style.css?token=gizli-admin-jetonu", "text/css", "--bg"},
		{"/admin/app.js?token=gizli-admin-jetonu", "application/javascript", "WebSocket"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: kod %d", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), tc.wantType) {
			t.Fatalf("%s: content-type %q", tc.path, rec.Header().Get("Content-Type"))
		}
		body := rec.Body.String()
		if !strings.Contains(body, tc.mustContain) {
			t.Fatalf("%s: beklenen icerik %q yok", tc.path, tc.mustContain)
		}
	}

	// ZERO EXTERNAL CDN: hicbir varlik http(s):// ile dis kaynaga baglanmamali.
	for _, asset := range []string{"index.html", "style.css", "app.js"} {
		data, err := readAsset(s, asset)
		if err != nil {
			t.Fatal(err)
		}
		low := strings.ToLower(string(data))
		for _, bad := range []string{"http://", "https://", "cdn.", "googleapis", "unpkg", "jsdelivr"} {
			if strings.Contains(low, bad) {
				t.Fatalf("%s dis kaynak referansi iceriyor: %q", asset, bad)
			}
		}
	}
}

func readAsset(s *Server, name string) ([]byte, error) {
	f, err := s.staticFS.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// TestTokenProtection, admin jetonu olmadan hicbir rotanin acilmadigini
// dogrular.
func TestTokenProtection(t *testing.T) {
	s := testServer(t, nil)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	for _, path := range []string{"/admin", "/admin/app.js", "/api/v1/ws/telemetry"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s jetonsuz 401 vermeliydi, %d", path, rec.Code)
		}
	}

	// Yanlis jeton da reddedilmeli.
	req := httptest.NewRequest(http.MethodGet, "/admin?token=yanlis", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("yanlis jeton 401 vermeliydi, %d", rec.Code)
	}

	// Authorization: Bearer ile de gecebilmeli.
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer gizli-admin-jetonu")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gecerli Bearer jetonu 200 vermeliydi, %d", rec.Code)
	}
}

// TestFailClosedWithoutToken, AdminToken bos ise panelin hicbir sey
// sunmadigini (fail-closed) dogrular.
func TestFailClosedWithoutToken(t *testing.T) {
	s, err := New(Config{AdminToken: ""})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/admin?token=", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token yapilandirilmamisken 401 beklenir, %d", rec.Code)
	}
}

// TestWebSocketLiveTelemetry, GERCEK bir HTTP sunucusuna WebSocket el
// sıkışması yapar ve canli telemetri karesinin JSON olarak geldigini
// dogrular (kabul kriteri 3).
func TestWebSocketLiveTelemetry(t *testing.T) {
	prov := ProviderFunc(func() Telemetry {
		return Telemetry{
			Nodes: []NodeInfo{
				{ID: "node-0", Carrier: "lora_ism", RTTms: 12.5, Alive: true},
				{ID: "node-1", Carrier: "udp", RTTms: 3.1, Alive: true},
			},
			WALDepth:      7,
			ActiveTunnels: 3,
			DiskBytes:     1024 * 1024,
			ThroughputBps: 2048,
			Credits:       []CreditRow{{ClientID: "acme", Units: 500, Bytes: 100000}},
			WANStatus:     "off_grid",
		}
	})
	s := testServer(t, prov)
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// El sıkışma icin ham TCP baglantisi.
	u := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	keyRaw := make([]byte, 16)
	_, _ = rand.Read(keyRaw)
	key := base64.StdEncoding.EncodeToString(keyRaw)

	req := "GET /api/v1/ws/telemetry?token=gizli-admin-jetonu HTTP/1.1\r\n" +
		"Host: " + u + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)
	// 101 yanit satiri + basliklar.
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("101 Switching Protocols beklenir, gelen: %q", status)
	}
	var accept string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-accept:") {
			accept = strings.TrimSpace(line[len("sec-websocket-accept:"):])
		}
	}
	// Accept anahtari dogrulanmali.
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	want := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if accept != want {
		t.Fatalf("Sec-WebSocket-Accept yanlis:\n gelen %q\n beklenen %q", accept, want)
	}

	// Ilk telemetri karesini oku (sunucu->istemci, maskesiz metin cerceve).
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	payload, err := readServerTextFrame(br)
	if err != nil {
		t.Fatalf("telemetri karesi okunamadi: %v", err)
	}
	var tel Telemetry
	if err := json.Unmarshal(payload, &tel); err != nil {
		t.Fatalf("telemetri JSON degil: %v\nham: %s", err, payload)
	}
	if tel.WALDepth != 7 || tel.ActiveTunnels != 3 || len(tel.Nodes) != 2 {
		t.Fatalf("telemetri icerigi beklenenden farkli: %+v", tel)
	}
	// WAN durumu akmali ve etiketi otomatik dolmali.
	if tel.WANStatus != "off_grid" {
		t.Fatalf("WAN durumu off_grid olmali, %q", tel.WANStatus)
	}
	if tel.WANLabel != "Isolated Mesh Only" {
		t.Fatalf("WAN etiketi otomatik dolmali, %q", tel.WANLabel)
	}
	if tel.TS == 0 {
		t.Fatal("telemetri zaman damgasi bos")
	}

	// Ikinci kare de gelmeli (periyodik yayin calisiyor).
	payload2, err := readServerTextFrame(br)
	if err != nil {
		t.Fatalf("ikinci kare gelmedi (periyodik yayin yok): %v", err)
	}
	if len(payload2) == 0 {
		t.Fatal("ikinci kare bos")
	}
}

// readServerTextFrame, sunucudan gelen maskesiz metin cercevesinin yukunu
// okur (yalnizca test icin; kucuk cerceveler beklenir).
// TestCookieFlow, tarayici akisini dogrular: /admin'e jetonla girince cerez
// birakilir; sonraki varlik ve WebSocket istekleri (jeton URL'de OLMADAN)
// yalnizca cerezle yetkilenir. Ayrica index.html'in MUTLAK yol (/admin/...)
// kullandigini kontrol eder.
func TestCookieFlow(t *testing.T) {
	s := testServer(t, ProviderFunc(func() Telemetry { return Telemetry{WALDepth: 1} }))
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// 1) /admin?token=... -> 200 ve Set-Cookie.
	resp, err := client.Get(srv.URL + "/admin?token=gizli-admin-jetonu")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin jetonla 200 vermeliydi, %d", resp.StatusCode)
	}
	// index.html MUTLAK yol kullanmali (tarayici goreli-yol 404'unu onler).
	html := string(body)
	if !strings.Contains(html, `href="/admin/style.css"`) || !strings.Contains(html, `src="/admin/app.js"`) {
		t.Fatal("index.html mutlak varlik yollari (/admin/...) icermiyor")
	}
	if len(resp.Cookies()) == 0 {
		t.Fatal("giriste oturum cerezi birakilmadi")
	}

	// 2) Varliklar JETON OLMADAN, yalnizca cerezle gelmeli (tarayici boyle yapar).
	for _, p := range []string{"/admin/style.css", "/admin/app.js"} {
		r2, err := client.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("%s cerezle 200 vermeliydi, %d", p, r2.StatusCode)
		}
	}

	// 3) Cerezsiz istemci ayni varliga erisememeli (koruma hala aktif).
	bare := &http.Client{}
	r3, _ := bare.Get(srv.URL + "/admin/app.js")
	r3.Body.Close()
	if r3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cerezsiz/jetonsuz app.js 401 vermeliydi, %d", r3.StatusCode)
	}
}

func readServerTextFrame(br *bufio.Reader) ([]byte, error) {
	h := make([]byte, 2)
	if _, err := io.ReadFull(br, h); err != nil {
		return nil, err
	}
	length := uint64(h[1] & 0x7F)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := io.ReadFull(br, ext); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(br, ext); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
