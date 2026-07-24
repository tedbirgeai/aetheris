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

	"github.com/tedbirgeai/aetheris/internal/billing"
	"github.com/tedbirgeai/aetheris/internal/config"
	"github.com/tedbirgeai/aetheris/internal/meter"
	"github.com/tedbirgeai/aetheris/internal/middleware"
	"github.com/tedbirgeai/aetheris/internal/router"
	"github.com/tedbirgeai/aetheris/internal/security"
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

	// --- v0.3b: Faturalama koprusu ---
	bridge, err := buildBillingBridge(cfg, logger)
	if err != nil {
		return err
	}
	if bridge != nil {
		defer func() {
			if cerr := bridge.Close(); cerr != nil {
				logger.Error("faturalama koprusu kapatilamadi", "err", cerr)
			}
		}()
	}

	var credits *billing.CreditEngine
	if cfg.CreditPerByte > 0 {
		credits = billing.NewCreditEngine(cfg.CreditPerByte, cfg.CreditMaxPerPeriod, bridge)
		logger.Info("role kredi motoru aktif",
			"credit_per_byte", cfg.CreditPerByte,
			"max_per_period", cfg.CreditMaxPerPeriod)
	}

	var thresholds *billing.ThresholdWatcher
	if len(cfg.UsageThresholds) > 0 && bridge != nil {
		thresholds = billing.NewThresholdWatcher(cfg.UsageThresholds, bridge)
		logger.Info("kullanim esigi izleyici aktif", "tiers", len(cfg.UsageThresholds))
	}

	// --- v0.3b: TLS / mTLS ---
	var tlsMgr *security.Manager
	if cfg.TLSCertFile != "" {
		tlsMgr, err = security.NewManager(security.TLSConfig{
			CertFile:       cfg.TLSCertFile,
			KeyFile:        cfg.TLSKeyFile,
			ClientCAFile:   cfg.TLSClientCAFile,
			ClientAuthMode: cfg.TLSClientAuth,
			ReloadInterval: cfg.TLSReloadInterval,
			Logger:         logger,
		})
		if err != nil {
			return err
		}
		defer tlsMgr.Close()
		st := tlsMgr.Status()
		logger.Info("TLS sonlandirma aktif",
			"client_auth", st.ClientAuthMode,
			"subject", st.Subject,
			"not_after", st.NotAfter,
			"kalan_gun", st.DaysRemaining)
	} else {
		logger.Warn("TLS KAPALI - gecit duz HTTP dinliyor. " +
			"Internete acmadan once AETHERIS_TLS_CERT/KEY tanimlayin " +
			"veya onune TLS sonlandiran bir reverse proxy koyun.")
	}

	h := &tunnel.Handler{
		Meter:           m,
		Router:          rtr,
		Prober:          prober,
		Logger:          logger,
		MaxPayloadBytes: cfg.MaxPayloadBytes,
		ReceiptSecret:   receiptSecret,
		Billing:         bridge,
		Credits:         credits,
		Thresholds:      thresholds,
	}
	if tlsMgr != nil {
		h.TLSStatus = func() any { return tlsMgr.Status() }
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
	mux.Handle("/api/v1/tunnel/chunked", protected(h.TunnelChunked))
	mux.Handle("/api/v1/meter/me", protected(h.MyUsage))

	// Metrik ucu: token ZORUNLU. Token yoksa uc nokta hic acilmaz —
	// metrikler musteri kimliklerini ve hacimlerini icerir.
	if cfg.MetricsEnabled {
		if cfg.MetricsToken == "" {
			return errors.New("AETHERIS_METRICS=true icin AETHERIS_METRICS_TOKEN zorunludur " +
				"(metrikler ticari olarak hassas veri icerir)")
		}
		mux.Handle("/metrics", middleware.Chain(
			&tunnel.MetricsHandler{Handler: h, Token: cfg.MetricsToken},
			middleware.Recover(logger),
		))
		logger.Info("Prometheus metrik ucu aktif", "path", "/metrics")
	}
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
		var lerr error
		if tlsMgr != nil {
			srv.TLSConfig = tlsMgr.ServerTLSConfig()
			// Sertifika ve anahtar TLSConfig.GetCertificate uzerinden
			// gelir; ListenAndServeTLS'e bos string vermek dogru kullanim.
			lerr = srv.ListenAndServeTLS("", "")
		} else {
			lerr = srv.ListenAndServe()
		}
		if lerr != nil && !errors.Is(lerr, http.ErrServerClosed) {
			errCh <- lerr
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

// buildBillingBridge, konfigurasyondaki hedeflere gore fatura koprusunu
// kurar. Hicbir hedef tanimli degilse nil doner (kopru devre disi).
func buildBillingBridge(cfg *config.Config, logger *slog.Logger) (*billing.Bridge, error) {
	var emitters []billing.Emitter

	if cfg.BillingWebhookURL != "" {
		em, err := billing.NewWebhookEmitter(
			cfg.BillingWebhookURL, []byte(cfg.BillingWebhookSecret), 10*time.Second)
		if err != nil {
			return nil, err
		}
		emitters = append(emitters, em)
		logger.Info("faturalama webhook hedefi kayitli", "emitter", em.Name())
	}

	if cfg.StripeAPIKey != "" {
		emitters = append(emitters,
			billing.NewStripeEmitter(cfg.StripeAPIKey, cfg.StripeMeterName, 15*time.Second))
		logger.Info("Stripe hedefi kayitli", "meter", cfg.StripeMeterName)
		logger.Warn("Stripe entegrasyonu CANLI API'ye karsi dogrulanmamistir. " +
			"Uretime almadan once test anahtariyla bir olay gonderip " +
			"meter'in dogru artigini teyit edin.")
	}

	if cfg.EInvoiceURL != "" {
		em, err := billing.NewEInvoiceEmitter(cfg.EInvoiceURL, cfg.EInvoiceAPIKey, 20*time.Second)
		if err != nil {
			return nil, err
		}
		emitters = append(emitters, em)
		logger.Info("e-Fatura hedefi kayitli")
		logger.Warn("e-Fatura govde bicimi SECILEN ENTEGRATORE gore uyarlanmalidir; " +
			"Turkiye'de tek bir standart e-Fatura API'si yoktur.")
	}

	if len(emitters) == 0 {
		return nil, nil
	}

	bridge := billing.New(emitters, billing.Config{Logger: logger})
	logger.Info("faturalama koprusu aktif", "emitters", len(emitters))
	return bridge, nil
}
