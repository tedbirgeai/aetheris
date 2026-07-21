package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen          string
	APIKeys         map[string]string
	MaxPayloadBytes int64
	RateLimitPerMin int
	RateLimitBurst  int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownGrace   time.Duration
}

func Load() (*Config, error) {
	cfg := &Config{
		Listen:          getEnv("AETHERIS_LISTEN", ":8080"),
		MaxPayloadBytes: int64(getEnvInt("AETHERIS_MAX_PAYLOAD_BYTES", 1<<20)),
		RateLimitPerMin: getEnvInt("AETHERIS_RATE_LIMIT_PER_MIN", 600),
		RateLimitBurst:  getEnvInt("AETHERIS_RATE_LIMIT_BURST", 60),
		ReadTimeout:     time.Duration(getEnvInt("AETHERIS_READ_TIMEOUT_SEC", 10)) * time.Second,
		WriteTimeout:    time.Duration(getEnvInt("AETHERIS_WRITE_TIMEOUT_SEC", 15)) * time.Second,
		IdleTimeout:     time.Duration(getEnvInt("AETHERIS_IDLE_TIMEOUT_SEC", 60)) * time.Second,
		ShutdownGrace:   time.Duration(getEnvInt("AETHERIS_SHUTDOWN_GRACE_SEC", 15)) * time.Second,
	}

	keys, err := parseAPIKeys(os.Getenv("AETHERIS_API_KEYS"))
	if err != nil {
		return nil, fmt.Errorf("AETHERIS_API_KEYS: %w", err)
	}
	cfg.APIKeys = keys

	if cfg.MaxPayloadBytes <= 0 {
		return nil, errors.New("AETHERIS_MAX_PAYLOAD_BYTES pozitif olmali")
	}
	if cfg.RateLimitPerMin <= 0 || cfg.RateLimitBurst <= 0 {
		return nil, errors.New("rate limit degerleri pozitif olmali")
	}
	return cfg, nil
}

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
