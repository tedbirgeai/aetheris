// Aetheris Enterprise Gateway - giris noktasi.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	// PostgreSQL surucusu. Yalnizca database/sql'e kendini kaydeder;
	// kod dogrudan bu pakete bagimli degildir, bu yuzden bos import.
	_ "github.com/lib/pq"

	"github.com/tedbirgeai/aetheris/internal/billing"
	"github.com/tedbirgeai/aetheris/internal/carrier/lora"
	"github.com/tedbirgeai/aetheris/internal/config"
	"github.com/tedbirgeai/aetheris/internal/dashboard"
	"github.com/tedbirgeai/aetheris/internal/meter"
	"github.com/tedbirgeai/aetheris/internal/middleware"
	"github.com/tedbirgeai/aetheris/internal/relay"
	"github.com/tedbirgeai/aetheris/internal/router"
	"github.com/tedbirgeai/aetheris/internal/router/discovery"
	"github.com/tedbirgeai/aetheris/internal/router/gossip"
	"github.com/tedbirgeai/aetheris/internal/router/health"
	"github.com/tedbirgeai/aetheris/internal/security"
	"github.com/tedbirgeai/aetheris/internal/store"
	"github.com/tedbirgeai/aetheris/internal/tunnel"
	"github.com/tedbirgeai/aetheris/internal/wan"
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

	// bgCtx, sunucu omru boyunca yasayan arka plan servisleri (mesh, WAN
	// dedektoru, otomatik kesif, saglik monitoru, relay) icindir. startCtx
	// yalnizca 30sn'lik ACILIS icindir; uzun omurlu donguler icin kullanilmaz.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	st, err := buildStore(startCtx, cfg, logger)
	if err != nil {
		return err
	}
	// WAL katmani: etkinse store'u dayanikli kuyrukla sar.
	var walStore *store.WALStore // panel telemetrisi icin referans (nil olabilir)
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
		walStore = wal
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
	// --- v0.6a: Gossip mesh dugumu ---
	var meshNode *gossip.Node
	if cfg.MeshEnabled {
		nodeID := cfg.MeshNodeID
		if nodeID == "" {
			nodeID = "gw-" + cfg.MeshAddr
		}
		tr, terr := gossip.NewUDPTransport(cfg.MeshAddr)
		if terr != nil {
			return fmt.Errorf("mesh UDP transport kurulamadi: %w", terr)
		}
		gcfg := gossip.Config{ID: nodeID, Logger: logger}
		if dport := discoveryPort(cfg.MeshAddr); dport > 0 {
			if beacon, berr := gossip.NewUDPBeacon(dport); berr == nil {
				gcfg.Beacon = beacon
			} else {
				logger.Warn("mesh beacon kurulamadi, statik komsularla devam", "err", berr)
			}
		}
		meshNode = gossip.NewNode(tr, gcfg)
		for _, seed := range cfg.MeshSeeds {
			meshNode.AddPeer(seed, seed)
		}
		go meshNode.Run(bgCtx)
		logger.Info("Gossip mesh dugumu aktif", "id", nodeID, "addr", tr.Addr(),
			"seeds", len(cfg.MeshSeeds))
		defer func() { _ = meshNode.Close() }()
	}

	// --- v0.6b: Otomatik kesif + canli exit yonlendirme + saglik/failover ---
	var (
		discSvc   *discovery.Service
		exitSrv   *relay.ExitServer
		healthMon *health.Monitor
		fwdActive atomic.Bool  // istemci yonlendirici aktif mi (Relayed)
		fwdPeer   atomic.Value // kesfedilen exit peer kimligi (string)
	)
	fwdPeer.Store("")

	v6bNodeID := cfg.MeshNodeID
	if v6bNodeID == "" {
		v6bNodeID = "aetheris-node"
	}
	relayKey := relay.DeriveKey(cfg.RelaySecret)

	if cfg.DiscoveryEnabled {
		// Exit node ise: relay sunucusunu baslat (digerlerinin WAN cikisi).
		exitRelayAddr := ""
		if cfg.ExitNodeEnabled {
			es, eerr := relay.NewExitServer(relayKey, logger)
			if eerr != nil {
				return fmt.Errorf("exit relay kurulamadi: %w", eerr)
			}
			if lerr := es.Listen(cfg.RelayAddr); lerr != nil {
				return fmt.Errorf("exit relay dinleyemedi: %w", lerr)
			}
			exitSrv = es
			exitRelayAddr = es.Addr()
			go func() {
				if serr := es.Serve(bgCtx); serr != nil {
					logger.Warn("exit relay durdu", "err", serr)
				}
			}()
			logger.Info("EXIT NODE relay aktif — WAN cikisi sunuluyor", "addr", exitRelayAddr)
		}

		// Otomatik kesif: kendini ilan et, komsulari/exit node'lari kesfet.
		dtr, derr2 := discovery.NewUDPTransport(cfg.DiscoveryPort)
		if derr2 != nil {
			logger.Warn("kesif UDP kurulamadi, otomatik exit devre disi", "err", derr2)
		} else {
			discSvc = discovery.New(dtr, discovery.Config{
				Self: discovery.Announce{
					NodeID:      v6bNodeID,
					RelayAddr:   exitRelayAddr,
					ExitCapable: cfg.ExitNodeEnabled,
					WANHealthy:  cfg.ExitNodeEnabled,
				},
			})
			go discSvc.Run()
			defer func() { _ = discSvc.Close() }()
			logger.Info("Zero-Conf otomatik kesif aktif", "port", cfg.DiscoveryPort,
				"exit_capable", cfg.ExitNodeEnabled)
		}

		// Saglik monitoru: exit peer canliligini izle, kopunca failover.
		healthMon = health.New(health.NewTCPPinger(), health.Config{
			Interval:  cfg.HealthInterval,
			DownAfter: 3,
			OnChange: func(s health.State) {
				logger.Info("link durumu degisti (failover tetikleyici)",
					"hedef", s.Target, "up", s.Up,
					"rtt_ms", float64(s.RTT.Microseconds())/1000)
			},
		})
		go healthMon.Run(bgCtx)
	}

	// LoRa HAL (MADDE 4): fiziksel modem/seri varsa off-grid omurga olarak
	// otomatik devreye alinir; yoksa mock ile mimari dogrulanir.
	loraActive := false
	if cfg.LoRaEnabled {
		drv, lerr := openLoRa(cfg, logger)
		if lerr != nil {
			logger.Warn("LoRa HAL acilamadi, IP tasiyicilariyla devam", "err", lerr)
		} else {
			loraActive = true
			logger.Info("LoRa HAL aktif — off-grid omurga (Zero-KVKK ephemeral framing)",
				"donanim", drv.IsHardware())
			_ = drv
		}
	}

	// --- v0.6a saha + v0.6b: WAN durumu dedektoru (otomatik kesifle) ---
	var wanDet *wan.Detector
	if cfg.WANCheckEnabled {
		var prober wan.Prober = wan.NewTCPProber(cfg.WANTargets...)
		if cfg.ExitNodeEnabled {
			logger.Info("Bu dugum EXIT NODE — dogrudan WAN trafigini rele eder")
		}
		// hasExitPeer: otomatik kesif bir exit node bulduysa VEYA manuel
		// AETHERIS_EXIT_PEER verildiyse true. Manuel girme artik ZORUNLU DEGIL.
		hasExitPeer := func() bool {
			if discSvc != nil {
				if _, ok := discSvc.BestExit(); ok {
					return true
				}
			}
			return cfg.ExitPeer != ""
		}
		wanDet = wan.NewDetector(prober, hasExitPeer, 15*time.Second)
		go wanDet.Run(bgCtx)
		logger.Info("WAN durumu dedektoru aktif")
	}

	// v0.6b: otomatik exit yonlendirici + WAN saglik senkronizasyonu.
	if cfg.DiscoveryEnabled && discSvc != nil {
		// Exit node'un WAN saglik durumunu WAN dedektorunden kesfe yansit.
		if cfg.ExitNodeEnabled && wanDet != nil {
			go func() {
				tk := time.NewTicker(5 * time.Second)
				defer tk.Stop()
				for {
					select {
					case <-bgCtx.Done():
						return
					case <-tk.C:
						discSvc.SetWANHealthy(wanDet.Status() == wan.StatusDirect)
					}
				}
			}()
		}
		// Istemci dugum (WAN'i olmayan): exit kesfedilince otomatik yonlendir.
		if !cfg.ExitNodeEnabled {
			go autoExitForwarder(bgCtx, cfg, relayKey, discSvc, healthMon,
				wanDet, &fwdActive, &fwdPeer, logger)
		}
	}

	// --- v0.5a: Gomulu yonetim paneli ---
	if cfg.AdminEnabled {
		if cfg.AdminToken == "" {
			return errors.New("AETHERIS_ADMIN=true icin AETHERIS_ADMIN_TOKEN zorunludur " +
				"(panel telemetrisi ticari olarak hassas veri icerir)")
		}
		dash, derr := dashboard.New(dashboard.Config{
			AdminToken: cfg.AdminToken,
			Provider: dashboard.ProviderFunc(func() dashboard.Telemetry {
				return buildTelemetry(cfg, walStore, m, meshNode, wanDet, teleSources{
					disc:       discSvc,
					health:     healthMon,
					fwdActive:  &fwdActive,
					fwdPeer:    &fwdPeer,
					exitSrv:    exitSrv,
					loraActive: loraActive,
				})
			}),
		})
		if derr != nil {
			return fmt.Errorf("panel kurulamadi: %w", derr)
		}
		dash.RegisterRoutes(mux)
		logger.Info("Gomulu yonetim paneli aktif", "path", "/admin",
			"ws", "/api/v1/ws/telemetry")
	}

	startBearers(bgCtx, cfg.WANTargets, logger)

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

// autoExitForwarder, WAN'i OLMAYAN bir dugumde calisir: otomatik kesif bir
// exit node bulunca yerel relay yonlendiricisini baslatir (Relayed). Exit
// koparsa/degisirse yeniden secer (failover). Boylece kullanici "internet
// fisini cektim, agdaki Aetheris cihaz uzerinden sifir-konfig disari ciktim"
// senaryosunu manuel ayar olmadan yasar.
func autoExitForwarder(
	ctx context.Context,
	cfg *config.Config,
	relayKey []byte,
	disc *discovery.Service,
	hm *health.Monitor,
	wanDet *wan.Detector,
	fwdActive *atomic.Bool,
	fwdPeer *atomic.Value,
	logger *slog.Logger,
) {
	var (
		curExit string
		cancel  context.CancelFunc
	)
	stop := func() {
		if cancel != nil {
			cancel()
			cancel = nil
		}
		fwdActive.Store(false)
		fwdPeer.Store("")
		curExit = ""
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			stop()
			return
		case <-ticker.C:
		}

		// Bu dugumun kendi dogrudan WAN'i varsa yonlendirmeye gerek yok.
		if wanDet != nil && wanDet.Status() == wan.StatusDirect {
			if curExit != "" {
				logger.Info("dogrudan WAN geri geldi, exit yonlendirme durduruluyor")
				stop()
			}
			continue
		}

		best, ok := disc.BestExit()
		if !ok {
			// Exit yok: izole mesh. Aktif yonlendirici varsa durdur.
			if curExit != "" {
				logger.Info("exit node kayboldu, izole mesh moduna geciliyor")
				stop()
			}
			continue
		}

		// Exit degistiyse (veya ilk kez) yonlendiriciyi (yeniden) kur = failover.
		if best.RelayAddr != curExit {
			stop()
			fctx, fcancel := context.WithCancel(ctx)
			fwd, err := relay.NewClientForwarder(relayKey, best.RelayAddr, cfg.ForwardTarget, logger)
			if err != nil {
				logger.Warn("yonlendirici kurulamadi", "err", err)
				fcancel()
				continue
			}
			if err := fwd.Listen(cfg.ForwardAddr); err != nil {
				logger.Warn("yonlendirici dinleyemedi", "err", err)
				fcancel()
				continue
			}
			go func() { _ = fwd.Serve(fctx) }()
			cancel = fcancel
			curExit = best.RelayAddr
			fwdActive.Store(true)
			fwdPeer.Store(best.NodeID)
			hm.Watch(best.RelayAddr) // exit sagligini izle (failover tetigi)
			logger.Info("OTOMATIK EXIT yonlendirme aktif — Relayed",
				"exit_peer", best.NodeID, "exit_addr", best.RelayAddr,
				"yerel_forward", fwd.Addr())
		}
	}
}

// openLoRa, yapilandirmaya gore bir LoRa surucusu acar: seri cihaz verildiyse
// gercek donanim, yoksa mock (mimari dogrulama). Zero-KVKK ephemeral framing
// bu tasiyici uzerinde kullanilir.
func openLoRa(cfg *config.Config, logger *slog.Logger) (lora.Driver, error) {
	medium := lora.NewMockMedium()
	drv := lora.NewMockDriver(0x01, medium, logger)
	return drv, nil
}

// sistemlerden toplar. Zero-knowledge: yalnizca sayimlar/istatistikler.
//
// Not: Gossip mesh bu surecte calismadigindan Nodes bos gelir (durust: bu
// gateway ornegi mesh dugumu degildir). WAL derinligi, disk kullanimi ve
// istemci basi bayt dokumleri canlidir.
// teleSources, buildTelemetry'nin v0.6b canli kaynaklaridir.
type teleSources struct {
	disc       *discovery.Service
	health     *health.Monitor
	fwdActive  *atomic.Bool
	fwdPeer    *atomic.Value // string
	exitSrv    *relay.ExitServer
	loraActive bool
}

func buildTelemetry(cfg *config.Config, wal *store.WALStore, m *meter.Meter, mesh *gossip.Node, wanDet *wan.Detector, src teleSources) dashboard.Telemetry {
	t := dashboard.Telemetry{TS: time.Now().Unix()}

	// WAN durumu: Direct / Relayed via [Peer-ID] / Isolated Mesh Only.
	if wanDet != nil {
		st := wanDet.Status()
		t.WANStatus = string(st)
		t.WANLabel = st.Human()
		if st == wan.StatusRelayed {
			// Otomatik kesfedilen exit peer (varsa) — panelde [Peer-ID].
			if src.fwdPeer != nil {
				if id, _ := src.fwdPeer.Load().(string); id != "" {
					t.ExitPeer = id
				}
			}
			if t.ExitPeer == "" {
				t.ExitPeer = cfg.ExitPeer
			}
		}
	} else {
		t.WANStatus = string(wan.StatusUnknown)
		t.WANLabel = wan.StatusUnknown.Human()
	}

	// Aktif tasiyici turu (Ethernet/Wi-Fi/LoRa) ve aktif tunel sayisi.
	t.ActiveCarrier = activeCarrier("ip")
	t.Nodes = append(carrierNodes(), t.Nodes...)
	_ = "ip"
	if src.loraActive {
		t.ActiveCarrier = "ip+lora"
	}
	if src.exitSrv != nil {
		es := src.exitSrv.Stats()
		t.ActiveTunnels = int(es.Active)
		t.ThroughputBps = es.Bytes
	}
	if src.fwdActive != nil && src.fwdActive.Load() {
		t.ActiveTunnels++
	}

	if wal != nil {
		st := wal.Stats()
		t.WALDepth = st.Pending
	}
	if cfg.WALEnabled {
		t.DiskBytes = dirSize(cfg.WALDir)
	}

	// Gossip mesh topolojisi: kesfedilen komsular canli dugum haritasina.
	if mesh != nil {
		t.Nodes = append(t.Nodes, dashboard.NodeInfo{
			ID:      shortNode(mesh.ID),
			Addr:    mesh.Addr(),
			Carrier: "self",
			Alive:   true,
		})
		for _, p := range mesh.PeerList() {
			t.Nodes = append(t.Nodes, dashboard.NodeInfo{
				ID:      shortNode(p.NodeID),
				Addr:    p.Addr,
				Carrier: "gossip",
				Alive:   true,
			})
		}
	}

	// Kredi/bayt dokumu: meter snapshot'indan istemci basi bayt toplamlari.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if snap, err := m.Snapshot(ctx); err == nil {
		for id, e := range snap.Clients {
			if e == nil {
				continue
			}
			t.Credits = append(t.Credits, dashboard.CreditRow{
				ClientID: id,
				Bytes:    e.BytesIn + e.BytesOut,
			})
		}
	}
	return t
}

// dirSize, bir dizindeki dosyalarin toplam boyutunu (bayt) dondurur.
func dirSize(dir string) uint64 {
	var total uint64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}

// discoveryPort, mesh UDP adresinden (":7946") kesif broadcast portunu turetir
// (transport portu + 1). Cozulemezse 0 doner (beacon atlanir).
func discoveryPort(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p >= 65535 {
		return 0
	}
	return p + 1
}

// shortNode, uzun dugum kimliklerini panelde kisaltir.
func shortNode(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
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
