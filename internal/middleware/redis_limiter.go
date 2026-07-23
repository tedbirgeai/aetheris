package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter, hem tek-dugumlu (RateLimiter) hem cok-dugumlu (RedisLimiter)
// sinirlayicilarin ortak sozlesmesidir. main.go bu arayuze bagimli olur;
// hangi implementasyonun aktif oldugunu bilmez.
type Limiter interface {
	Middleware(next http.Handler) http.Handler
	Stop()
}

// Derleme zamaninda her iki implementasyonun da arayuzu karsiladigini garanti et.
var (
	_ Limiter = (*RateLimiter)(nil)
	_ Limiter = (*RedisLimiter)(nil)
)

// tokenBucketLua, token bucket'i Redis'te ATOMIK olarak uygular.
//
// NEDEN LUA: Dagitik ortamda "oku-hesapla-yaz" uc ayri komut olursa, iki
// dugum ayni anda okuyup ayni token'i harcayabilir (yaris kosulu). Lua
// betigi Redis'te tek islem olarak calisir; tum dugumler icin atomiktir.
//
// KEYS[1]           : istemci anahtari
// ARGV[1] rate      : saniyedeki token
// ARGV[2] capacity  : kova kapasitesi (burst)
// ARGV[3] now       : simdiki zaman (saniye, ondalikli)
// ARGV[4] ttl       : anahtar yasam suresi (saniye)
// Donus: 1 = izin verildi, 0 = reddedildi
const tokenBucketLua = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local data = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])

if tokens == nil then
  tokens = capacity
  ts = now
end

local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = math.min(capacity, tokens + elapsed * rate)

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call('HSET', key, 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', key, ttl)
return allowed
`

// RedisLimiter, Redis tabanli dagitik token bucket'tir.
//
// FALLBACK: Redis erisilemezse istekler SESSIZCE REDDEDILMEZ. Bunun yerine
// yerel bellek sinirlayicisina (RateLimiter) dusulur. Boylece Redis kesintisi
// hizmeti durdurmaz, yalnizca sinirlamayi gecici olarak dugum-yerel yapar.
// Bu, kullanilabilirlik lehine bilincli bir takastir: kisa bir Redis
// kesintisinde bir istemci teorik olarak dugum sayisi kadar fazla istek
// atabilir, ama hizmet ayakta kalir.
type RedisLimiter struct {
	client   *redis.Client
	script   *redis.Script
	fallback *RateLimiter
	rate     float64
	capacity float64
	ttl      time.Duration
	logger   *slog.Logger

	// redisHealthy, saglik durumunu izler. Art arda hata gorulunce
	// false'a duser; basarili islemde true'ya doner. Amac: her istekte
	// Redis'i yoklayip gecikmeyi artirmak yerine, kesinti suresince
	// dogrudan fallback'e gitmek.
	redisHealthy atomic.Bool

	failCount atomic.Int32
}

// RedisLimiterConfig, RedisLimiter kurulum parametreleridir.
type RedisLimiterConfig struct {
	Addr      string
	Password  string
	DB        int
	PerMinute int
	Burst     int
	Logger    *slog.Logger
}

// NewRedisLimiter, Redis baglantisini kurar ve baglanabilirligi dogrular.
// Baglanti kurulamazsa hata doner; cagiran taraf bellek sinirlayiciya
// gecmeye karar verebilir.
func NewRedisLimiter(ctx context.Context, cfg RedisLimiterConfig) (*RedisLimiter, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		PoolSize:     20,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}

	rl := &RedisLimiter{
		client:   client,
		script:   redis.NewScript(tokenBucketLua),
		fallback: NewRateLimiter(cfg.PerMinute, cfg.Burst),
		rate:     float64(cfg.PerMinute) / 60.0,
		capacity: float64(cfg.Burst),
		ttl:      2 * time.Minute,
		logger:   cfg.Logger,
	}
	rl.redisHealthy.Store(true)
	return rl, nil
}

// allow, Redis'e sorar; hata olursa fallback'e duser.
func (rl *RedisLimiter) allow(ctx context.Context, clientID string) bool {
	// Redis saglikliysa once onu dene.
	if rl.redisHealthy.Load() {
		now := float64(time.Now().UnixNano()) / 1e9
		res, err := rl.script.Run(ctx, rl.client,
			[]string{"aetheris:rl:" + clientID},
			rl.rate, rl.capacity, now, rl.ttl.Seconds(),
		).Int()

		if err == nil {
			rl.failCount.Store(0)
			return res == 1
		}

		// Baglam iptali (istemci baglantiyi kesti) bir Redis arizasi
		// degildir; fallback'e dusmeye gerek yok.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return rl.fallback.allow(clientID)
		}

		// Art arda 3 hatada Redis'i saglıksiz isaretle.
		if rl.failCount.Add(1) >= 3 {
			if rl.redisHealthy.CompareAndSwap(true, false) {
				rl.logger.Warn("Redis rate limiter erisilemez, bellek fallback'e geciliyor",
					"err", err)
				go rl.probeRedis()
			}
		}
	}

	// Redis saglıksiz veya hata: bellek fallback.
	return rl.fallback.allow(clientID)
}

// probeRedis, arka planda Redis'in geri gelip gelmedigini yoklar.
// Geri gelince redisHealthy'yi true yapar.
func (rl *RedisLimiter) probeRedis() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-rl.fallback.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := rl.client.Ping(ctx).Err()
			cancel()
			if err == nil {
				rl.failCount.Store(0)
				rl.redisHealthy.Store(true)
				rl.logger.Info("Redis rate limiter yeniden erisilebilir, dagitik moda donuluyor")
				return
			}
		}
	}
}

// Middleware, Auth'tan SONRA zincirlenmelidir.
func (rl *RedisLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := ClientIDFrom(r.Context())
		if !ok {
			clientID = r.RemoteAddr
		}
		if !rl.allow(r.Context(), clientID) {
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "hiz siniri asildi")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Stop, Redis baglantisini ve fallback goroutine'ini kapatir.
func (rl *RedisLimiter) Stop() {
	rl.fallback.Stop()
	if rl.client != nil {
		_ = rl.client.Close()
	}
}

// Healthy, testler ve health ucu icin mevcut Redis durumunu bildirir.
func (rl *RedisLimiter) Healthy() bool {
	return rl.redisHealthy.Load()
}

// forceUnhealthy, YALNIZCA testler icindir; fallback yolunu tetikler.
func (rl *RedisLimiter) forceUnhealthy() {
	rl.redisHealthy.Store(false)
}
