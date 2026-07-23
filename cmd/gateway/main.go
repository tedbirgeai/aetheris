// Aetheris Enterprise Gateway - giris noktasi.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// PostgreSQL surucusu. Yalnizca database/sql'e kendini kaydeder;
	// kod dogrudan bu pakete bagimli degildir, bu yuzden bos import.
	_ "github.com/lib/pq"

	"github.com/tedbirgeai/aetheris/internal/config"
	"github.com/tedbirgeai/aetheris/internal/meter"
	"github.com/tedbirgeai/aetheris/internal/middleware"
	"github.com/tedbirgeai/aetheris/internal/router"
	"github.com/tedbirgeai/aetheris/internal/store"
	"github.com/tedbirgeai/aetheris/internal/tunnel"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(logger); err != nil {
		logger.Error("gecit baslatilamadi", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	receiptSecret, err := loadReceiptSecret(logger)
	if err != nil {
		return err
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStart()

	st, err := buildStore(startCtx, cfg, logger)
	if err != nil {
		return err
	}
	// WAL katmani: etkinse store'u dayanikli kuyrukla sar.
	if cfg.WALEnabled {
		wal, werr := store.NewWAL(startCtx, st, store.WALConfig{
			Dir:    cfg.WALDir,
			Logger: logger,
		})
		if werr != nil {
			return werr
		}
		logger.Info("WAL dayanikli kuyruk aktif", "dir", cfg.WALDir)
		st = wal
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Error("store kapatilamadi", "err", cerr)
		}
	}()

	m := meter.New(st)

	routes := make([]router.Route, 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		routes = append(routes, router.Route{
			Name:       rc.Name,
			Kind:       rc.Kind,
			Upstream:   rc.Upstream,
			Backup:     rc.Backup,
			HealthPath: rc.HealthPath,
		})
	}
	rtr := router.New(routes, cfg.ForwardTimeout)

	// Failover prober: etkinse ve rota varsa, arka planda saglik yoklar.
	var prober *router.HealthProber
	if cfg.HealthProbeEnabled && rtr.Enabled() {
		prober = router.NewHealthProber(rtr, router.ProberConfig{
			Interval: cfg.HealthProbeInterval,
			Logger:   logger,
		})
		prober.Start()
		defer prober.Stop()
		logger.Info("rota saglik yoklamasi aktif", "interval", cfg.HealthProbeInterval.String())
	}

	h := &tunnel.Handler{
		Meter:           m,
		Router:          rtr,
		Prober:          prober,
		Logger:          logger,
		MaxPayloadBytes: cfg.MaxPayloadBytes,
		ReceiptSecret:   receiptSecret,
	}

	// Rate limiter: Redis adresi tanimliysa dagitik, degilse bellek-ici.
	// Redis'e baglanılamazsa bellek-iciye duser (fail-open kurulum).
	var limiter middleware.Limiter
	if cfg.RedisAddr != "" {
		rl, rerr := middleware.NewRedisLimiter(startCtx, middleware.RedisLimiterConfig{
			Addr:      cfg.RedisAddr,
			Password:  cfg.RedisPassword,
			DB:        cfg.RedisDB,
			PerMinute: cfg.RateLimitPerMin,
			Burst:     cfg.RateLimitBurst,
			Logger:    logger,
		})
		if rerr != nil {
			logger.Warn("Redis rate limiter kurulamadi, bellek-ici limiter kullanilacak",
				"addr", cfg.RedisAddr, "err", rerr)
			limiter = middleware.NewRateLimiter(cfg.RateLimitPerMin, cfg.RateLimitBurst)
		} else {
			logger.Info("dagitik Redis rate limiter aktif", "addr", cfg.RedisAddr)
			limiter = rl
		}
	} else {
		limiter = middleware.NewRateLimiter(cfg.RateLimitPerMin, cfg.RateLimitBurst)
	}
	defer limiter.Stop()

	// Zincir sirasi: Recover -> Logging -> Auth -> RateLimit
	// Logging, Auth'un DISINDA; boylece basarisiz kimlik denemeleri (401)
	// de loglanir. Istemci kimligi holder deseniyle Logging'e ulasir.
	protected := func(next http.HandlerFunc) http.Handler {
		return middleware.Chain(next,
			middleware.Recover(logger),
			middleware.Logging(logger),
			middleware.Auth(cfg.APIKeys),
			limiter.Middleware,
		)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/tunnel", protected(h.Tunnel))
	mux.Handle("/api/v1/meter/me", protected(h.MyUsage))
	mux.Handle("/healthz", middleware.Chain(http.HandlerFunc(h.Health),
		middleware.Recover(logger)))

	srv := &http.Server{
		Addr:         cfg.Listen,
		Handler:      mux,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("Aetheris Gateway aktif",
			"listen", cfg.Listen,
			"clients", len(cfg.APIKeys),
			"store", st.Kind(),
			"routes", len(routes),
			"max_payload_bytes", cfg.MaxPayloadBytes,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("kapatma sinyali alindi, acik istekler bekleniyor",
			"grace", cfg.ShutdownGrace.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}

	snapCtx, snapCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer snapCancel()
	if snap, serr := m.Snapshot(snapCtx); serr == nil {
		logger.Info("kapatildi",
			"total_bytes_in", snap.TotalBytesIn,
			"total_bytes_out", snap.TotalBytesOut,
			"total_requests", snap.TotalRequests,
		)
	} else {
		logger.Warn("kapanis ozeti alinamadi", "err", serr)
	}
	return nil
}

// buildStore, konfigurasyona gore kalicilik katmanini secer.
func buildStore(ctx context.Context, cfg *config.Config, logger *slog.Logger) (store.Store, error) {
	switch cfg.StoreKind {
	case "postgres":
		st, err := store.NewPostgres(ctx, "postgres", cfg.DatabaseDSN)
		if err != nil {
			return nil, err
		}
		logger.Info("kalici defter aktif", "store", "postgres")
		return st, nil
	default:
		logger.Warn("BELLEK ICI DEFTER AKTIF - sunucu yeniden baslarsa " +
			"tum tuketim kaydi silinir. Faturalama icin AETHERIS_STORE=postgres kullanin.")
		return store.NewMemory(), nil
	}
}

// loadReceiptSecret, imza anahtarini ortamdan okur.
// Tanimli degilse gecici anahtar uretir ve UYARI verir - uretimde bu
// anahtar kalici olmalidir, aksi halde yeniden baslatmada eski
// makbuzlar dogrulanamaz.
func loadReceiptSecret(logger *slog.Logger) ([]byte, error) {
	raw := os.Getenv("AETHERIS_RECEIPT_SECRET")
	if raw != "" {
		secret, err := hex.DecodeString(raw)
		if err != nil {
			return nil, errors.New("AETHERIS_RECEIPT_SECRET gecerli hex degil")
		}
		if len(secret) < 32 {
			return nil, errors.New("AETHERIS_RECEIPT_SECRET en az 32 bayt (64 hex) olmali")
		}
		return secret, nil
	}

	logger.Warn("AETHERIS_RECEIPT_SECRET tanimsiz - gecici anahtar uretiliyor. " +
		"URETIMDE KULLANMAYIN: yeniden baslatmada eski makbuzlar dogrulanamaz.")
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return secret, nil
}
