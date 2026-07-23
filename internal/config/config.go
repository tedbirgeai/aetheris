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

	// --- v0.3a: Redis dagitik rate limiter ---
	// RedisAddr bos ise bellek-ici limiter kullanilir.
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	// --- v0.3a: WAL dayanikli kuyruk ---
	// WALEnabled true ise store, WAL katmaniyla sarilir.
	WALEnabled bool
	WALDir     string

	// --- v0.3a: Rota failover / saglik kontrolu ---
	HealthProbeEnabled  bool
	HealthProbeInterval time.Duration
}

// RouteConfig, tek bir yonlendirme hedefidir.
type RouteConfig struct {
	Name     string
	Kind     string // "direct" | "edge" | "peering"
	Upstream *url.URL
	// Backup, bu rota sagliksizken denenecek yedek rotanin adi.
	// Bos ise ve rota sagliksizsa "direct" turunde bir rotaya dusulur.
	Backup string
	// HealthPath, saglik yoklamasinda kullanilacak yol. Bos ise "/healthz".
	HealthPath string
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

		RedisAddr:     strings.TrimSpace(os.Getenv("AETHERIS_REDIS_ADDR")),
		RedisPassword: os.Getenv("AETHERIS_REDIS_PASSWORD"),
		RedisDB:       getEnvInt("AETHERIS_REDIS_DB", 0),

		WALEnabled: strings.EqualFold(getEnv("AETHERIS_WAL_ENABLED", "false"), "true"),
		WALDir:     getEnv("AETHERIS_WAL_DIR", "./wal"),

		HealthProbeEnabled:  strings.EqualFold(getEnv("AETHERIS_HEALTHPROBE", "false"), "true"),
		HealthProbeInterval: secs("AETHERIS_HEALTHPROBE_INTERVAL_SEC", 10),
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

		// URL'den sonra opsiyonel secenekler ';' ile ayrilir:
		//   ad=tur@url;backup=YEDEK_ROTA_ADI;health=/saglik-yolu
		parts := strings.Split(kindAndURL[1], ";")
		rawURL := strings.TrimSpace(parts[0])

		var backup, healthPath string
		for _, opt := range parts[1:] {
			opt = strings.TrimSpace(opt)
			if opt == "" {
				continue
			}
			kv := strings.SplitN(opt, "=", 2)
			if len(kv) != 2 {
				return nil, fmt.Errorf("rota %q icin gecersiz secenek %q (beklenen anahtar=deger)", name, opt)
			}
			key := strings.ToLower(strings.TrimSpace(kv[0]))
			val := strings.TrimSpace(kv[1])
			switch key {
			case "backup":
				if val == "" {
					return nil, fmt.Errorf("rota %q icin bos backup degeri", name)
				}
				backup = val
			case "health":
				if !strings.HasPrefix(val, "/") {
					return nil, fmt.Errorf("rota %q icin health yolu '/' ile baslamali, %q verildi", name, val)
				}
				healthPath = val
			default:
				return nil, fmt.Errorf("rota %q icin bilinmeyen secenek %q (backup|health)", name, key)
			}
		}

		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("rota %q icin gecersiz url: %w", name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("rota %q icin url http veya https olmali, %q verildi", name, u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("rota %q icin url host bilgisi eksik", name)
		}

		out = append(out, RouteConfig{
			Name:       name,
			Kind:       kind,
			Upstream:   u,
			Backup:     backup,
			HealthPath: healthPath,
		})
	}

	if len(out) == 0 {
		return nil, errors.New("hicbir gecerli rota ayristirilamadi")
	}

	// Yedek rota adlari GERCEKTEN tanimli olmali. Aksi halde failover
	// sessizce calismaz — sagliksiz rota yedegine dusmek isterken hedefi
	// bulamaz ve istek basarisiz olur. Bunu ACILISTA yakalamak,
	// uretimde kesinti aninda kesfetmekten cok daha ucuzdur.
	for _, rc := range out {
		if rc.Backup == "" {
			continue
		}
		if rc.Backup == rc.Name {
			return nil, fmt.Errorf("rota %q kendini yedek gosteremez", rc.Name)
		}
		if _, ok := seen[rc.Backup]; !ok {
			return nil, fmt.Errorf("rota %q icin tanimsiz yedek rota: %q", rc.Name, rc.Backup)
		}
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
