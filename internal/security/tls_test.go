package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// certAuthority, test icin bir CA ve ondan imzali sertifikalar uretir.
type certAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T) *certAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Aetheris Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &certAuthority{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue, CA'dan imzali bir sertifika/anahtar cifti uretip dosyaya yazar.
func (ca *certAuthority) issue(t *testing.T, dir, name, cn string,
	notAfter time.Time, isClient bool) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	usage := x509.ExtKeyUsageServerAuth
	if isClient {
		usage = x509.ExtKeyUsageClientAuth
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}

	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func (ca *certAuthority) writePEM(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, ca.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTLSTerminationWorks, gecidin HTTPS dinleyebildigini uctan uca
// dogrular.
func TestTLSTerminationWorks(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "server", "localhost",
		time.Now().Add(time.Hour), false)

	mgr, err := NewManager(TLSConfig{
		CertFile: certPath, KeyFile: keyPath, Logger: silent(),
	})
	if err != nil {
		t.Fatalf("Manager kurulamadi: %v", err)
	}
	defer mgr.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("guvenli"))
		}),
		TLSConfig: mgr.ServerTLSConfig(),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.pem)
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("HTTPS istegi basarisiz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "guvenli" {
		t.Fatalf("yanit = %q", string(body))
	}
}

// TestMTLSRequiresClientCert, mTLS "require" modunda SERTIFIKASIZ
// istemcinin REDDEDILDIGINI dogrular.
func TestMTLSRequiresClientCert(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "server", "localhost",
		time.Now().Add(time.Hour), false)
	caPath := ca.writePEM(t, dir, "ca.pem")

	mgr, err := NewManager(TLSConfig{
		CertFile: certPath, KeyFile: keyPath,
		ClientCAFile: caPath, ClientAuthMode: "require",
		Logger: silent(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	srv := &http.Server{
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		TLSConfig: mgr.ServerTLSConfig(),
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.pem)

	// 1) SERTIFIKASIZ istemci: REDDEDILMELI
	noCertClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		Timeout:   5 * time.Second,
	}
	if _, err := noCertClient.Get("https://" + ln.Addr().String() + "/"); err == nil {
		t.Fatal("sertifikasiz istemci KABUL EDILDI - mTLS calismiyor")
	}

	// 2) GECERLI sertifikali istemci: KABUL EDILMELI
	clientCert, clientKey := ca.issue(t, dir, "client", "aetheris-node-1",
		time.Now().Add(time.Hour), true)
	pair, err := tls.LoadX509KeyPair(clientCert, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	okClient := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{pair},
		}},
		Timeout: 5 * time.Second,
	}
	resp, err := okClient.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("gecerli sertifikali istemci reddedildi: %v", err)
	}
	_ = resp.Body.Close()
}

// TestMTLSRejectsForeignCA, BASKA bir CA'dan imzali istemci
// sertifikasinin reddedildigini dogrular.
func TestMTLSRejectsForeignCA(t *testing.T) {
	dir := t.TempDir()
	ourCA := newCA(t)
	foreignCA := newCA(t) // saldirganin kendi CA'si

	certPath, keyPath := ourCA.issue(t, dir, "server", "localhost",
		time.Now().Add(time.Hour), false)
	caPath := ourCA.writePEM(t, dir, "ca.pem")

	mgr, err := NewManager(TLSConfig{
		CertFile: certPath, KeyFile: keyPath,
		ClientCAFile: caPath, ClientAuthMode: "require",
		Logger: silent(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	srv := &http.Server{
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		TLSConfig: mgr.ServerTLSConfig(),
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	defer srv.Close()

	// Saldirgan kendi CA'sindan istemci sertifikasi uretiyor.
	fCert, fKey := foreignCA.issue(t, dir, "sahte-client", "sahte-node",
		time.Now().Add(time.Hour), true)
	pair, err := tls.LoadX509KeyPair(fCert, fKey)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ourCA.pem)

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{pair},
		}},
		Timeout: 5 * time.Second,
	}
	if _, err := client.Get("https://" + ln.Addr().String() + "/"); err == nil {
		t.Fatal("YABANCI CA'dan imzali sertifika KABUL EDILDI")
	}
}

// TestCertificateRotation, sertifika dosyasi degistiginde SUREC
// YENIDEN BASLATILMADAN yeni sertifikanin devreye girdigini dogrular.
func TestCertificateRotation(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "server", "eski-isim",
		time.Now().Add(time.Hour), false)

	mgr, err := NewManager(TLSConfig{
		CertFile: certPath, KeyFile: keyPath,
		ReloadInterval: 50 * time.Millisecond,
		Logger:         silent(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	if got := mgr.Status().Subject; got != "eski-isim" {
		t.Fatalf("baslangic subject = %q", got)
	}

	// Sertifikayi YENI bir isimle degistir (rotasyon simulasyonu).
	time.Sleep(20 * time.Millisecond)
	ca.issue(t, dir, "server", "yeni-isim", time.Now().Add(2*time.Hour), false)

	// Izleyicinin fark etmesini bekle.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mgr.Status().Subject == "yeni-isim" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	st := mgr.Status()
	if st.Subject != "yeni-isim" {
		t.Fatalf("rotasyon calismadi: subject = %q", st.Subject)
	}
	if st.Reloads == 0 {
		t.Fatal("Reloads sayaci artmadi")
	}
}

// TestExpiredCertificateRejected, suresi dolmus sertifikayla acilisin
// ENGELLENDIGINI dogrular.
//
// Aksi halde gecit "calisiyor" gorunur ama her TLS el sikismasi
// basarisiz olur — teshisi zor bir ariza.
func TestExpiredCertificateRejected(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "expired", "localhost",
		time.Now().Add(-time.Hour), false) // DUN sona ermis

	_, err := NewManager(TLSConfig{
		CertFile: certPath, KeyFile: keyPath, Logger: silent(),
	})
	if err == nil {
		t.Fatal("suresi dolmus sertifika KABUL EDILDI")
	}
}

// TestBadRotationKeepsOldCertificate, rotasyon sirasinda BOZUK bir
// dosya yazilirsa ESKI sertifikanin korundugunu dogrular.
//
// cert-manager veya bir betik dosyayi yazarken yarim kalirsa, gecidin
// calismaya devam etmesi gerekir.
func TestBadRotationKeepsOldCertificate(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "server", "saglam-isim",
		time.Now().Add(time.Hour), false)

	mgr, err := NewManager(TLSConfig{
		CertFile: certPath, KeyFile: keyPath,
		ReloadInterval: 50 * time.Millisecond,
		Logger:         silent(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// Sertifika dosyasini BOZ.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(certPath, []byte("BOZUK VERI"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Eski sertifika hala aktif olmali.
	if got := mgr.Status().Subject; got != "saglam-isim" {
		t.Fatalf("bozuk dosya eski sertifikayi bozdu: %q", got)
	}
	// TLS config hala sertifika dondurebilmeli.
	cfg := mgr.ServerTLSConfig()
	if _, err := cfg.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Fatalf("sertifika alinamadi: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "s", "localhost", time.Now().Add(time.Hour), false)

	cases := map[string]TLSConfig{
		"cert yok":           {KeyFile: keyPath},
		"key yok":            {CertFile: certPath},
		"gecersiz auth modu": {CertFile: certPath, KeyFile: keyPath, ClientAuthMode: "sihirli", ClientCAFile: "x"},
		"mTLS icin CA yok":   {CertFile: certPath, KeyFile: keyPath, ClientAuthMode: "require"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			cfg.Logger = silent()
			if _, err := NewManager(cfg); err == nil {
				t.Fatalf("%s: hata bekleniyordu", name)
			}
		})
	}
}

// TestMinTLSVersion, TLS 1.0/1.1'in reddedildigini dogrular.
func TestMinTLSVersion(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath, keyPath := ca.issue(t, dir, "s", "localhost", time.Now().Add(time.Hour), false)

	mgr, err := NewManager(TLSConfig{CertFile: certPath, KeyFile: keyPath, Logger: silent()})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	if v := mgr.ServerTLSConfig().MinVersion; v != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, beklenen TLS 1.2 (%x)", v, tls.VersionTLS12)
	}
}
