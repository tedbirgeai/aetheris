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

	"github.com/tedbirge-labs/aetheris-gateway/internal/config"
	"github.com/tedbirge-labs/aetheris-gateway/internal/meter"
	"github.com/tedbirge-labs/aetheris-gateway/internal/middleware"
	"github.com/tedbirge-labs/aetheris-gateway/internal/tunnel"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("konfigurasyon gecersiz, sunucu baslatilmadi", "err", err)
		os.Exit(1)
	}

	receiptSecret, err := loadReceiptSecret(logger)
	if err != nil {
		logger.Error("makbuz imza anahtari hazirlanamadi", "err", err)
		os.Exit(1)
	}

	m := meter.New()

	h := &tunnel.Handler{
		Meter:           m,
		Logger:          logger,
		MaxPayloadBytes: cfg.MaxPayloadBytes,
		ReceiptSecret:   receiptSecret,
	}

	limiter := middleware.NewRateLimiter(cfg.RateLimitPerMin, cfg.RateLimitBurst)

	protected := func(next http.HandlerFunc) http.Handler {
		return middleware.Chain(next,
			middleware.Recover(logger),
			middleware.Auth(cfg.APIKeys),
			middleware.Logging(logger),
			limiter.Middleware,
		)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/tunnel", protected(h.Tunnel))
	mux.Handle("/api/v1/meter/me", protected(h.MyUsage))
	mux.Handle("/healthz", middleware.Chain(http.HandlerFunc(h.Health), middleware.Recover(logger)))

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
		logger.Error("sunucu kritik hatayla durdu", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("kapatma sinyali alindi, acik istekler bekleniyor", "grace", cfg.ShutdownGrace.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("zarif kapatma basarisiz", "err", err)
		os.Exit(1)
	}

	snap := m.Snapshot()
	logger.Info("kapatildi",
		"total_bytes_in", snap.TotalBytesIn,
		"total_bytes_out", snap.TotalBytesOut,
		"total_requests", snap.TotalRequests,
	)
}

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
	logger.Warn("AETHERIS_RECEIPT_SECRET tanimsiz - gecici anahtar uretiliyor. URETIMDE KULLANMAYIN.")
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return secret, nil
}
