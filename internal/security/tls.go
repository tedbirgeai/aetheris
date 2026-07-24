// Package security, TLS sonlandirma ve karsilikli TLS (mTLS) katmanidir.
//
// # NE SAGLAR
//
//	TLS sonlandirma : Gecit artik duz HTTP yerine HTTPS dinleyebilir.
//	mTLS            : Istemci de sertifika sunar; gecit onu dogrular.
//	Sertifika rotasyonu : Sertifika dosyalari degistiginde SUREC YENIDEN
//	                      BASLATILMADAN yeni sertifika devreye girer.
//
// # ROTASYON NASIL CALISIR
//
// tls.Config.GetCertificate her el sikismada cagrilir. Biz burada
// sertifikayi bir atomic.Value'dan okuyoruz; arka planda calisan izleyici
// dosyalarin degisip degismedigini kontrol edip yeni sertifikayi yukluyor.
// Boylece Let's Encrypt / cert-manager yenilemeleri kesintisiz uygulanir.
//
// # mTLS KIMLERI ILGILENDIRIR
//
// Emirdeki kapsam: dugumler (node), edge ve peering hatlari arasindaki
// iletisim. Yani makineden makineye baglantilar. Son kullanici tarayici
// trafigi icin mTLS uygun degildir (sertifika dagitimi pratik degil);
// orada Bearer anahtar dogrulamasi devam eder.
//
// ClientAuthMode ile iki mod desteklenir:
//   - "require" : istemci sertifikasi ZORUNLU ve dogrulanir (dugumler arasi)
//   - "optional": sertifika varsa dogrulanir, yoksa istek yine kabul edilir
//     (gecis donemi icin; uretimde "require" hedeflenmelidir)
package security

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// TLSConfig, TLS/mTLS kurulum parametreleridir.
type TLSConfig struct {
	// CertFile / KeyFile, sunucu sertifikasi ve ozel anahtari.
	CertFile string
	KeyFile  string

	// ClientCAFile, istemci sertifikalarini dogrulamak icin kullanilan
	// CA demeti. mTLS icin ZORUNLUDUR.
	ClientCAFile string

	// ClientAuthMode: "" (mTLS kapali) | "optional" | "require"
	ClientAuthMode string

	// ReloadInterval, sertifika dosyalarinin degisim kontrolu araligi.
	// 0 ise rotasyon izleme kapali.
	ReloadInterval time.Duration

	Logger *slog.Logger
}

// Manager, sertifikalari tutar ve rotasyonu yonetir.
type Manager struct {
	cfg TLSConfig

	// cert, atomic olarak degistirilebilen aktif sertifika.
	cert atomic.Pointer[tls.Certificate]

	// clientCAs, istemci dogrulama havuzu.
	clientCAs atomic.Pointer[x509.CertPool]

	certModTime atomic.Int64
	keyModTime  atomic.Int64

	reloads atomic.Uint64

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	logger   *slog.Logger
}

var (
	ErrNoCertFiles   = errors.New("security: TLS icin CertFile ve KeyFile zorunlu")
	ErrNoClientCA    = errors.New("security: mTLS icin ClientCAFile zorunlu")
	ErrBadClientAuth = errors.New("security: ClientAuthMode gecersiz (optional|require)")
)

// NewManager, sertifikalari yukler ve rotasyon izleyicisini baslatir.
func NewManager(cfg TLSConfig) (*Manager, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, ErrNoCertFiles
	}
	switch cfg.ClientAuthMode {
	case "", "optional", "require":
	default:
		return nil, fmt.Errorf("%w: %q", ErrBadClientAuth, cfg.ClientAuthMode)
	}
	if cfg.ClientAuthMode != "" && cfg.ClientCAFile == "" {
		return nil, ErrNoClientCA
	}

	m := &Manager{
		cfg:     cfg,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		logger:  cfg.Logger,
	}

	if err := m.loadCertificate(); err != nil {
		return nil, err
	}
	if cfg.ClientAuthMode != "" {
		if err := m.loadClientCAs(); err != nil {
			return nil, err
		}
	}

	if cfg.ReloadInterval > 0 {
		go m.watch()
	} else {
		close(m.stopped)
	}
	return m, nil
}

// loadCertificate, sertifika ciftini diskten yukler.
func (m *Manager) loadCertificate() error {
	cert, err := tls.LoadX509KeyPair(m.cfg.CertFile, m.cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("security: sertifika yuklenemedi: %w", err)
	}

	// Sertifikanin gecerlilik suresini kontrol et — suresi dolmus bir
	// sertifikayla acilmak, sessizce baglanti reddine yol acar.
	if len(cert.Certificate) > 0 {
		leaf, perr := x509.ParseCertificate(cert.Certificate[0])
		if perr == nil {
			cert.Leaf = leaf
			now := time.Now()
			if now.After(leaf.NotAfter) {
				return fmt.Errorf("security: sertifikanin suresi dolmus (NotAfter=%s)",
					leaf.NotAfter.Format(time.RFC3339))
			}
			if now.Before(leaf.NotBefore) {
				return fmt.Errorf("security: sertifika henuz gecerli degil (NotBefore=%s)",
					leaf.NotBefore.Format(time.RFC3339))
			}
			// Yaklasan sona ermeyi uyar (30 gun).
			if time.Until(leaf.NotAfter) < 30*24*time.Hour {
				m.logger.Warn("TLS sertifikasinin suresi yaklasiyor",
					"not_after", leaf.NotAfter.Format(time.RFC3339),
					"kalan_gun", int(time.Until(leaf.NotAfter).Hours()/24))
			}
		}
	}

	m.cert.Store(&cert)
	m.certModTime.Store(modTime(m.cfg.CertFile))
	m.keyModTime.Store(modTime(m.cfg.KeyFile))
	return nil
}

func (m *Manager) loadClientCAs() error {
	pem, err := os.ReadFile(m.cfg.ClientCAFile)
	if err != nil {
		return fmt.Errorf("security: istemci CA dosyasi okunamadi: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return errors.New("security: istemci CA dosyasinda gecerli sertifika yok")
	}
	m.clientCAs.Store(pool)
	return nil
}

// watch, sertifika dosyalarinin degisimini izler ve yeniden yukler.
func (m *Manager) watch() {
	defer close(m.stopped)
	ticker := time.NewTicker(m.cfg.ReloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			cm, km := modTime(m.cfg.CertFile), modTime(m.cfg.KeyFile)
			if cm == m.certModTime.Load() && km == m.keyModTime.Load() {
				continue
			}
			// Dosya degismis: yeniden yukle. HATA OLURSA ESKI SERTIFIKA
			// KORUNUR — yarim yazilmis bir dosya yuzunden hizmet dusmez.
			if err := m.loadCertificate(); err != nil {
				m.logger.Error("sertifika yeniden yuklenemedi, eski sertifika korunuyor",
					"err", err)
				continue
			}
			if m.cfg.ClientAuthMode != "" {
				if err := m.loadClientCAs(); err != nil {
					m.logger.Error("istemci CA yeniden yuklenemedi", "err", err)
				}
			}
			m.reloads.Add(1)
			m.logger.Info("TLS sertifikasi yeniden yuklendi",
				"reload_count", m.reloads.Load())
		}
	}
}

func modTime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// ServerTLSConfig, http.Server'a verilecek TLS yapilandirmasini dondurur.
func (m *Manager) ServerTLSConfig() *tls.Config {
	cfg := &tls.Config{
		// TLS 1.2 alt sinir. 1.0/1.1 kirilmis kabul edilir ve PCI-DSS
		// gibi standartlarca yasaklanmistir.
		MinVersion: tls.VersionTLS12,

		// GetCertificate her el sikismada cagrilir -> rotasyon otomatik.
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := m.cert.Load()
			if c == nil {
				return nil, errors.New("security: yuklu sertifika yok")
			}
			return c, nil
		},

		// Modern, ileri gizlilik (forward secrecy) saglayan sifre takimlari.
		// TLS 1.3 takimlari Go tarafindan zaten otomatik secilir.
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
	}

	switch m.cfg.ClientAuthMode {
	case "require":
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = m.clientCAs.Load()
	case "optional":
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
		cfg.ClientCAs = m.clientCAs.Load()
	}
	return cfg
}

// ClientTLSConfig, gecidin UPSTREAM'e (edge/peering) baglanirken
// kullanacagi mTLS istemci yapilandirmasini dondurur.
func (m *Manager) ClientTLSConfig(rootCAs *x509.CertPool) *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    rootCAs,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c := m.cert.Load()
			if c == nil {
				return nil, errors.New("security: yuklu istemci sertifikasi yok")
			}
			return c, nil
		},
	}
	return cfg
}

// Status, health ucu icin sertifika durumunu dondurur.
type Status struct {
	Enabled        bool   `json:"enabled"`
	ClientAuthMode string `json:"client_auth_mode"`
	Subject        string `json:"subject,omitempty"`
	NotAfter       string `json:"not_after,omitempty"`
	DaysRemaining  int    `json:"days_remaining,omitempty"`
	Reloads        uint64 `json:"reloads"`
}

func (m *Manager) Status() Status {
	st := Status{
		Enabled:        true,
		ClientAuthMode: m.cfg.ClientAuthMode,
		Reloads:        m.reloads.Load(),
	}
	if c := m.cert.Load(); c != nil && c.Leaf != nil {
		st.Subject = c.Leaf.Subject.CommonName
		st.NotAfter = c.Leaf.NotAfter.Format(time.RFC3339)
		st.DaysRemaining = int(time.Until(c.Leaf.NotAfter).Hours() / 24)
	}
	return st
}

// Close, rotasyon izleyicisini durdurur.
func (m *Manager) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	<-m.stopped
	return nil
}

// LoadCAPool, bir PEM dosyasindan CA havuzu olusturur (upstream dogrulama).
func LoadCAPool(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil // sistem havuzunu kullan
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("security: CA dosyasi okunamadi: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("security: CA dosyasinda gecerli sertifika yok")
	}
	return pool, nil
}
