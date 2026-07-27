package provisioning

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tedbirgeai/aetheris/internal/security"
)

func TestIdentityGeneration(t *testing.T) {
	p := New(FlashConfig{}, nil)
	id, err := p.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(id.NodeID()) != 64 {
		t.Fatalf("NodeID 64 hex karakter olmalı: %s", id.NodeID())
	}
}

func TestIdentityFromKeyFile(t *testing.T) {
	dir := t.TempDir()
	// Geçerli seed dosyası yaz.
	seed := make([]byte, 32)
	seed[0] = 0xAB
	keyPath := filepath.Join(dir, "node.key")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(FlashConfig{KeyFile: keyPath}, nil)
	id, err := p.Identity()
	if err != nil {
		t.Fatal(err)
	}
	// Aynı seed → aynı NodeID.
	p2 := New(FlashConfig{KeyFile: keyPath}, nil)
	id2, _ := p2.Identity()
	if id.NodeID() != id2.NodeID() {
		t.Fatal("aynı key dosyası aynı kimliği vermeli")
	}
}

func TestIdentitySavedToConfigDir(t *testing.T) {
	dir := t.TempDir()
	p := New(FlashConfig{ConfigDir: dir}, nil)
	id1, err := p.Identity()
	if err != nil {
		t.Fatal(err)
	}
	// Kaydedilen key dosyasından yeniden yükle.
	keyPath := filepath.Join(dir, "node.key")
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		t.Fatal("kimlik kaydedilmeli")
	}
	p2 := New(FlashConfig{KeyFile: keyPath}, nil)
	id2, _ := p2.Identity()
	if id1.NodeID() != id2.NodeID() {
		t.Fatal("kaydedilen ve yüklenen kimlik aynı olmalı")
	}
}

func TestSelfProvisionedConfig(t *testing.T) {
	p := New(FlashConfig{}, nil)
	id, _ := p.Identity()
	cfg, err := p.Provision(id)
	if err != nil {
		t.Fatal(err)
	}
	// Bootstrap yoksa self-provisioned — boş değerler döndürülmemeli.
	if cfg.MeshAddr == "" {
		t.Fatal("MeshAddr dolu olmalı")
	}
	if cfg.RelaySecret == "" {
		t.Fatal("RelaySecret dolu olmalı")
	}
}

func TestBootstrapServerProvides(t *testing.T) {
	issuer, _ := security.NewIdentity()
	srv := NewBootstrapServer(issuer, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	p := New(FlashConfig{BootstrapAddrs: []string{ts.Listener.Addr().String()}}, nil)
	id, _ := p.Identity()
	cfg, err := p.Provision(id)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MeshAddr == "" {
		t.Fatal("bootstrap yapılandırma MeshAddr içermeli")
	}
}

func TestLoadFlashConfigNoFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := LoadFlashConfig(dir)
	if cfg.KeyFile != "" {
		t.Fatal("dosya yoksa KeyFile boş olmalı")
	}
	if len(cfg.BootstrapAddrs) != 0 {
		t.Fatal("dosya yoksa BootstrapAddrs boş olmalı")
	}
}

func TestLoadFlashConfigWithBootstrap(t *testing.T) {
	dir := t.TempDir()
	bs := "10.0.0.1:7948\n10.0.0.2:7948\n# yorum\n"
	_ = os.WriteFile(filepath.Join(dir, "bootstrap"), []byte(bs), 0o644)
	cfg := LoadFlashConfig(dir)
	if len(cfg.BootstrapAddrs) != 2 {
		t.Fatalf("2 adres bekleniyor: %v", cfg.BootstrapAddrs)
	}
}

func TestApplyToEnv(t *testing.T) {
	// Mevcut env'i korumak için temizlik.
	for _, k := range []string{"AETHERIS_MESH_ADDR", "AETHERIS_RELAY_SECRET"} {
		old := os.Getenv(k)
		defer os.Setenv(k, old)
		os.Unsetenv(k)
	}
	cfg := &NodeConfig{MeshAddr: ":7999", RelaySecret: "test-secret"}
	ApplyToEnv(cfg)
	if os.Getenv("AETHERIS_MESH_ADDR") != ":7999" {
		t.Fatal("AETHERIS_MESH_ADDR set edilmeli")
	}
	// Zaten set edilmişse değiştirmemeli.
	ApplyToEnv(&NodeConfig{MeshAddr: ":8000", RelaySecret: "other"})
	if os.Getenv("AETHERIS_MESH_ADDR") != ":7999" {
		t.Fatal("mevcut env değiştirilmemeli")
	}
}

func TestBootstrapServerHTTP(t *testing.T) {
	issuer, _ := security.NewIdentity()
	srv := NewBootstrapServer(issuer, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/provision/abcdef123456", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("200 bekleniyor: %d", rr.Code)
	}
	if rr.Body.Len() == 0 {
		t.Fatal("yanıt boş olmamalı")
	}
}
