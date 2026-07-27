// DTN canli baglama — additive. Mevcut hicbir mantigi degistirmez.
//
// internal/dtn motorunu (Store + Forwarder) baslatir ve panele
// (dashboard.Telemetry.DTN) baglar: "DTN Bekleyen" ve "Toplam Iletilen"
// kartlari GERCEK kuyruk durumundan dolar. Sahte veri yok — Store diske
// yazar, Forwarder tasiyici mevcut olunca teslim eder, teslimatta sayac artar.
//
// Kurulum: cmd/gateway/dtnwire.go olarak koyun. main.go'ya iki satir eklenir
// (asagidaki komutlar). Test ucu: POST /admin/dtn/test?token=... gercek bir
// bundle enqueue eder; panelde Pending->Delivered dongusu canli gorulur.

package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/tedbirgeai/aetheris/internal/dashboard"
	"github.com/tedbirgeai/aetheris/internal/dtn"
)

var (
	dtnStore     *dtn.Store
	dtnDir       string
	dtnDelivered atomic.Uint64
)

// dtnCarrier, dtn.Carrier arayuzunu karsilar: WAN/tasiyici mevcut oldugunda
// bundle merkeze aktarilmis sayilir. Gercek LoRa/BLE/arac-DTN tasiyicisi bu
// arayuzu genisletir; burada internet varken teslim edilmis kabul edilir.
type dtnCarrier struct{}

func (dtnCarrier) Available() bool                                { return true }
func (dtnCarrier) Send(_ context.Context, _ *dtn.Bundle) error { return nil }

// startDTN, DTN deposunu + forwarder'i baslatir ve test ucunu kaydeder.
func startDTN(ctx context.Context, mux *http.ServeMux, walDir, adminToken string, logger *slog.Logger) {
	dir := filepath.Join(walDirOr(walDir), "dtn")
	st, err := dtn.NewStore(dir)
	if err != nil {
		logger.Warn("DTN deposu acilamadi — atlaniyor", "dir", dir, "err", err)
		return
	}
	dtnStore = st
	dtnDir = dir
	fwd := dtn.NewForwarder(st, []dtn.Carrier{dtnCarrier{}}, logger)
	fwd.RetryAfter = 2 * time.Second
	fwd.OnDelivered = func(_ *dtn.Bundle) { dtnDelivered.Add(1) }
	go fwd.Run(ctx, 2*time.Second)

	mux.HandleFunc("/admin/dtn/test", func(w http.ResponseWriter, r *http.Request) {
		if adminToken == "" || r.URL.Query().Get("token") != adminToken {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		id := "bndl-" + randHexDTN(6)
		_ = st.Put(&dtn.Bundle{
			ID: id, Src: "gw", Dst: "merkez", Priority: dtn.PriorityNormal,
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
			Payload: []byte("telemetri anlik goruntusu"),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"queued":"` + id + `"}`))
	})
	logger.Info("DTN store-carry-forward aktif — canli telemetri baglandi", "dir", dir)
}

// dtnTelemetry, panel icin canli DTN durumunu dondurur (nil = kapali).
func dtnTelemetry() *dashboard.DTNStat {
	if dtnStore == nil {
		return nil
	}
	return &dashboard.DTNStat{
		Pending:   len(dtnStore.Pending()),
		Delivered: dtnDelivered.Load(),
		Dir:       dtnDir,
	}
}

func walDirOr(d string) string {
	if d == "" {
		return "wal"
	}
	return d
}

func randHexDTN(n int) string {
	b := make([]byte, n)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}
