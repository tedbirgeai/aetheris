// Package config, tum calisma zamani ayarlarini ortam degiskenlerinden
// okur ve BASLANGICTA dogrular. Yanlis konfigurasyonla sunucu ayaga
// kalkmaz; hatanin uretimde degil, acilista goruldugu tasarim tercihidir.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen          string
	APIKeys         map[string]string // apiKey -> clientID
	MaxPayloadBytes int64
	RateLimitPerMin int
	RateLimitBurst  int

	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
	ShutdownGrace time.Duration

	// StoreKind: "memory" | "postgres"
	StoreKind string
	// DatabaseDSN, StoreKind=postgres oldugunda zorunludur.
	DatabaseDSN string

	// Routes, yonlendirme hedefleridir. Bos ise yonlendirme devre disidir;
	// gecit yalnizca olcum ve makbuz uretir.
	Routes []RouteConfig
	// ForwardTimeout, tek bir yonlendirme isteginin azami suresi.
	ForwardTimeout time.Duration
}

// RouteConfig, tek bir yonlendirme hedefidir.
type RouteConfig struct {
	Name     string
	Kind     string // "direct" | "edge" | "peering"
	Upstream *url.URL
}

// validRouteKinds, kabul edilen yonlendirme turleridir.
var validRouteKinds = map[string]struct{}{
	"direct":  {},
	"edge":    {},
	"peering": {},
}

func Load() (*Config, error) {
	cfg := &Config{
		Listen:          getEnv("AETHERIS_LISTEN", ":8080"),
		MaxPayloadBytes: int64(getEnvInt("AETHERIS_MAX_PAYLOAD_BYTES", 1<<20)),
		RateLimitPerMin: getEnvInt("AETHERIS_RATE_LIMIT_PER_MIN", 600),
		RateLimitBurst:  getEnvInt("AETHERIS_RATE_LIMIT_BURST", 60),
		ReadTimeout:     secs("AETHERIS_READ_TIMEOUT_SEC", 10),
		WriteTimeout:    secs("AETHERIS_WRITE_TIMEOUT_SEC", 15),
		IdleTimeout:     secs("AETHERIS_IDLE_TIMEOUT_SEC", 60),
		ShutdownGrace:   secs("AETHERIS_SHUTDOWN_GRACE_SEC", 15),
		StoreKind:       strings.ToLower(getEnv("AETHERIS_STORE", "memory")),
		DatabaseDSN:     os.Getenv("AETHERIS_DATABASE_DSN"),
		ForwardTimeout:  secs("AETHERIS_FORWARD_TIMEOUT_SEC", 20),
	}

	keys, err := parseAPIKeys(os.Getenv("AETHERIS_API_KEYS"))
	if err != nil {
		return nil, fmt.Errorf("AETHERIS_API_KEYS: %w", err)
	}
	cfg.APIKeys = keys

	routes, err := parseRoutes(os.Getenv("AETHERIS_ROUTES"))
	if err != nil {
		return nil, fmt.Errorf("AETHERIS_ROUTES: %w", err)
	}
	cfg.Routes = routes

	switch cfg.StoreKind {
	case "memory":
		// kabul
	case "postgres":
		if strings.TrimSpace(cfg.DatabaseDSN) == "" {
			return nil, errors.New("AETHERIS_STORE=postgres icin AETHERIS_DATABASE_DSN zorunludur")
		}
	default:
		return nil, fmt.Errorf("AETHERIS_STORE gecersiz: %q (memory veya postgres)", cfg.StoreKind)
	}

	if cfg.MaxPayloadBytes <= 0 {
		return nil, errors.New("AETHERIS_MAX_PAYLOAD_BYTES pozitif olmali")
	}
	if cfg.RateLimitPerMin <= 0 || cfg.RateLimitBurst <= 0 {
		return nil, errors.New("rate limit degerleri pozitif olmali")
	}
	if cfg.ForwardTimeout <= 0 {
		return nil, errors.New("AETHERIS_FORWARD_TIMEOUT_SEC pozitif olmali")
	}
	return cfg, nil
}

// parseAPIKeys, "clientID:apiKey,clientID2:apiKey2" bicimini ayristirir.
func parseAPIKeys(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("tanimsiz - en az bir istemci anahtari gerekli")
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("gecersiz cift %q, beklenen bicim clientID:apiKey", pair)
		}
		clientID := strings.TrimSpace(parts[0])
		apiKey := strings.TrimSpace(parts[1])
		if clientID == "" {
			return nil, errors.New("bos clientID")
		}
		if len(apiKey) < 32 {
			return nil, fmt.Errorf("istemci %q icin anahtar 32 karakterden kisa", clientID)
		}
		if _, dup := out[apiKey]; dup {
			return nil, fmt.Errorf("istemci %q icin yinelenen anahtar", clientID)
		}
		out[apiKey] = clientID
	}
	if len(out) == 0 {
		return nil, errors.New("hicbir gecerli anahtar ayristirilamadi")
	}
	return out, nil
}

// parseRoutes, "ad=tur@url,ad2=tur2@url2" bicimini ayristirir.
// Ornek: "eu-edge=edge@https://edge.example.com,peer1=peering@https://peer.example.net"
func parseRoutes(raw string) ([]RouteConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil // yonlendirme devre disi
	}

	var out []RouteConfig
	seen := make(map[string]struct{})

	for _, spec := range strings.Split(raw, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		nameAndRest := strings.SplitN(spec, "=", 2)
		if len(nameAndRest) != 2 {
			return nil, fmt.Errorf("gecersiz rota %q, beklenen bicim ad=tur@url", spec)
		}
		name := strings.TrimSpace(nameAndRest[0])
		if name == "" {
			return nil, errors.New("bos rota adi")
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("yinelenen rota adi: %q", name)
		}
		seen[name] = struct{}{}

		kindAndURL := strings.SplitN(nameAndRest[1], "@", 2)
		if len(kindAndURL) != 2 {
			return nil, fmt.Errorf("rota %q icin tur@url ayrimi bulunamadi", name)
		}
		kind := strings.ToLower(strings.TrimSpace(kindAndURL[0]))
		if _, ok := validRouteKinds[kind]; !ok {
			return nil, fmt.Errorf("rota %q icin gecersiz tur %q (direct|edge|peering)", name, kind)
		}

		u, err := url.Parse(strings.TrimSpace(kindAndURL[1]))
		if err != nil {
			return nil, fmt.Errorf("rota %q icin gecersiz url: %w", name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("rota %q icin url http veya https olmali, %q verildi", name, u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("rota %q icin url host bilgisi eksik", name)
		}

		out = append(out, RouteConfig{Name: name, Kind: kind, Upstream: u})
	}

	if len(out) == 0 {
		return nil, errors.New("hicbir gecerli rota ayristirilamadi")
	}
	return out, nil
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func secs(key string, fallback int) time.Duration {
	return time.Duration(getEnvInt(key, fallback)) * time.Second
}
