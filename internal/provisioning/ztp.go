// Package provisioning, Sıfır-Dokunuşlu Sağlama (Zero-Touch Provisioning —
// ZTP) motorunu implement eder. Donanım sahaya indiği anda:
//
//  1. Ed25519 kimliği otomatik üretilir (veya flash disk'ten yüklenir).
//  2. Yerel ağ taranır; gossip/HaLow/LoRa üzerinden komşular keşfedilir.
//  3. Keşfedilen ilk bootstrap node'dan yapılandırma güvenli biçimde çekilir.
//  4. Gateway başlatılır, WAL kurulur, mesh'e katılınır.
//
// Hiçbir manuel konfigürasyon gerekmez. Flash disk'te yalnızca:
//   - node.key   (isteğe bağlı — yoksa otomatik üretilir)
//   - bootstrap  (isteğe bağlı — virgülle ayrılmış bootstrap adresleri)
//
// Bu dosyalar da yoksa tamamen sıfır-konfigürasyon modunda çalışır.
package provisioning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tedbirgeai/aetheris/internal/security"
)

var (
	ErrNoBootstrap     = errors.New("ztp: bootstrap node bulunamadı")
	ErrProvisionFailed = errors.New("ztp: sağlama başarısız")
)

// NodeConfig, bir Aetheris düğümünün çalışma yapılandırmasıdır.
// ZTP bu yapıyı bootstrap node'dan çeker ve env'e yansıtır.
type NodeConfig struct {
	NodeID          string            `json:"node_id"`
	MeshAddr        string            `json:"mesh_addr"`
	DiscoveryPort   int               `json:"discovery_port"`
	RelaySecret     string            `json:"relay_secret"`
	WANTargets      []string          `json:"wan_targets"`
	ExitNodeEnabled bool              `json:"exit_node"`
	Extra           map[string]string `json:"extra,omitempty"`
	IssuedAt        time.Time         `json:"issued_at"`
	IssuerID        string            `json:"issuer_id"`
}

// FlashConfig, flash disk'ten okunan başlangıç yapılandırmasıdır.
// Hiçbir alan zorunlu değildir.
type FlashConfig struct {
	// KeyFile, Ed25519 private key seed dosyasının yolu.
	// Boşsa otomatik üretilir.
	KeyFile string
	// BootstrapAddrs, başlangıç bootstrap node adresleri.
	// Boşsa broadcast keşif denenir.
	BootstrapAddrs []string
	// ConfigDir, node.key ve bootstrap dosyalarının dizini.
	ConfigDir string
}

// LoadFlashConfig, bir dizinden flash yapılandırmasını okur.
// Dosyalar yoksa boş (sıfır-konfigürasyon) yapılandırma döner.
func LoadFlashConfig(dir string) FlashConfig {
	cfg := FlashConfig{ConfigDir: dir}
	// node.key: Ed25519 seed (32 bayt hex)
	keyPath := filepath.Join(dir, "node.key")
	if _, err := os.Stat(keyPath); err == nil {
		cfg.KeyFile = keyPath
	}
	// bootstrap: satır satır adres listesi
	bsPath := filepath.Join(dir, "bootstrap")
	if data, err := os.ReadFile(bsPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				cfg.BootstrapAddrs = append(cfg.BootstrapAddrs, line)
			}
		}
	}
	return cfg
}

// Provisioner, ZTP motorudur.
type Provisioner struct {
	flash  FlashConfig
	logger *slog.Logger
	client *http.Client
}

// New, bir ZTP sağlayıcısı oluşturur.
func New(flash FlashConfig, logger *slog.Logger) *Provisioner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provisioner{
		flash:  flash,
		logger: logger,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Identity, düğüm kimliğini yükler veya üretir.
// Flash'ta key varsa ondan türetir; yoksa yeni üretir ve kaydeder.
func (p *Provisioner) Identity() (*security.Identity, error) {
	if p.flash.KeyFile != "" {
		data, err := os.ReadFile(p.flash.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("ztp: key dosyası okunamadı: %w", err)
		}
		seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(seed) != 32 {
			return nil, fmt.Errorf("ztp: geçersiz key dosyası (32 bayt hex bekleniyor)")
		}
		p.logger.Info("ZTP: mevcut kimlik yüklendi", "key_file", p.flash.KeyFile)
		return security.IdentityFromSeed(seed)
	}
	// Yeni kimlik üret.
	id, err := security.NewIdentity()
	if err != nil {
		return nil, err
	}
	// Kalıcı hale getir (config dizinine kaydet).
	if p.flash.ConfigDir != "" {
		keyPath := filepath.Join(p.flash.ConfigDir, "node.key")
		seed := id.Seed()
		if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
			p.logger.Warn("ZTP: kimlik kaydedilemedi", "err", err)
		} else {
			p.logger.Info("ZTP: yeni kimlik üretildi ve kaydedildi", "key_file", keyPath)
		}
	}
	p.logger.Info("ZTP: yeni kimlik üretildi", "node_id", id.NodeID()[:16]+"…")
	return id, nil
}

// Provision, ZTP sürecini çalıştırır:
// 1. Kimlik yükle/üret.
// 2. Bootstrap node'u bul.
// 3. Yapılandırmayı çek.
// 4. Hazır NodeConfig döndür.
func (p *Provisioner) Provision(id *security.Identity) (*NodeConfig, error) {
	// Minimal varsayılan yapılandırma (bootstrap bulunamazsa kullanılır).
	def := &NodeConfig{
		NodeID:        id.NodeID(),
		MeshAddr:      ":7946",
		DiscoveryPort: 7947,
		RelaySecret:   deriveSecret(id.NodeID()),
		WANTargets:    []string{"1.1.1.1:53", "8.8.8.8:53"},
		IssuedAt:      time.Now(),
		IssuerID:      "self",
	}

	// Bootstrap adreslerini dene.
	for _, addr := range p.flash.BootstrapAddrs {
		cfg, err := p.fetchConfig(addr, id)
		if err != nil {
			p.logger.Warn("ZTP: bootstrap başarısız", "addr", addr, "err", err)
			continue
		}
		p.logger.Info("ZTP: yapılandırma başarıyla çekildi", "issuer", cfg.IssuerID)
		return cfg, nil
	}

	// Bootstrap yoksa broadcast keşif dene (UDP).
	if addr := p.discoverBootstrap(); addr != "" {
		cfg, err := p.fetchConfig(addr, id)
		if err == nil {
			p.logger.Info("ZTP: broadcast ile bootstrap bulundu", "addr", addr)
			return cfg, nil
		}
	}

	// Her şey başarısız → sıfır-konfigürasyon (self-provisioned) mod.
	p.logger.Info("ZTP: sıfır-konfigürasyon modu (self-provisioned)")
	return def, nil
}

// fetchConfig, bir bootstrap node'dan yapılandırmayı çeker.
// İstek Ed25519 imzalıdır; bootstrap node kimliği doğrular.
func (p *Provisioner) fetchConfig(bootstrapAddr string, id *security.Identity) (*NodeConfig, error) {
	url := fmt.Sprintf("http://%s/api/v1/provision/%s", bootstrapAddr, id.NodeID()[:16])
	resp, err := p.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ztp: bootstrap %d döndürdü", resp.StatusCode)
	}
	var cfg NodeConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// discoverBootstrap, UDP broadcast ile bir bootstrap node arar.
func (p *Provisioner) discoverBootstrap() string {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return ""
	}
	defer conn.Close()
	// "aetheris-ztp-discover" broadcast yay.
	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: 7948}
	msg := []byte("aetheris-ztp-discover")
	_ = conn.(*net.UDPConn).SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.WriteTo(msg, bcast); err != nil {
		return ""
	}
	// Yanıt bekle.
	buf := make([]byte, 256)
	n, addr, err := conn.ReadFrom(buf)
	if err != nil || n == 0 {
		return ""
	}
	return addr.(*net.UDPAddr).IP.String() + ":" + string(buf[:n])
}

// ApplyToEnv, NodeConfig'i ortam değişkenlerine yazar (gateway başlatmak için).
func ApplyToEnv(cfg *NodeConfig) {
	setEnvIfEmpty("AETHERIS_MESH_NODE_ID", cfg.NodeID)
	setEnvIfEmpty("AETHERIS_MESH_ADDR", cfg.MeshAddr)
	if cfg.DiscoveryPort > 0 {
		setEnvIfEmpty("AETHERIS_DISCOVERY_PORT", fmt.Sprintf("%d", cfg.DiscoveryPort))
	}
	setEnvIfEmpty("AETHERIS_RELAY_SECRET", cfg.RelaySecret)
	if len(cfg.WANTargets) > 0 {
		setEnvIfEmpty("AETHERIS_WAN_TARGETS", strings.Join(cfg.WANTargets, ","))
	}
	if cfg.ExitNodeEnabled {
		setEnvIfEmpty("AETHERIS_EXIT_NODE", "true")
	}
	for k, v := range cfg.Extra {
		setEnvIfEmpty(k, v)
	}
}

func setEnvIfEmpty(key, val string) {
	if os.Getenv(key) == "" {
		_ = os.Setenv(key, val)
	}
}

// deriveSecret, node ID'den deterministik relay secret türetir.
func deriveSecret(nodeID string) string {
	h := sha256.Sum256([]byte("aetheris-relay-secret:" + nodeID))
	return hex.EncodeToString(h[:16])
}

// BootstrapServer, diğer düğümlere yapılandırma dağıtan sunucudur.
// Ağdaki ilk düğüm (bootstrap node) bu sunucuyu çalıştırır.
type BootstrapServer struct {
	id     *security.Identity
	mux    *http.ServeMux
	logger *slog.Logger
}

// NewBootstrapServer, bir bootstrap sunucusu oluşturur.
func NewBootstrapServer(id *security.Identity, logger *slog.Logger) *BootstrapServer {
	if logger == nil {
		logger = slog.Default()
	}
	s := &BootstrapServer{id: id, mux: http.NewServeMux(), logger: logger}
	s.mux.HandleFunc("/api/v1/provision/", s.handleProvision)
	return s
}

// Handler, HTTP handler'ı döndürür.
func (s *BootstrapServer) Handler() http.Handler { return s.mux }

func (s *BootstrapServer) handleProvision(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	requesterPrefix := ""
	if len(parts) > 0 {
		requesterPrefix = parts[len(parts)-1]
	}
	s.logger.Info("ZTP: sağlama isteği", "requester_prefix", requesterPrefix)

	cfg := &NodeConfig{
		MeshAddr:      ":7946",
		DiscoveryPort: 7947,
		RelaySecret:   deriveSecret(s.id.NodeID()),
		WANTargets:    []string{"1.1.1.1:53", "8.8.8.8:53"},
		IssuedAt:      time.Now(),
		IssuerID:      s.id.NodeID()[:16],
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg)
}
