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
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

// dtnCarrier, GERCEK bir store-carry-forward tasiyicisidir: bundle payload'ini
// yapilandirilmis merkez ucuna (AETHERIS_DTN_UPSTREAM) HTTP POST ile aktarir.
// Upstream tanimsizsa Available()=false → bundle'lar PENDING kalir (dogru off-grid
// davranisi; sahte teslim YOK). Gercek LoRa/BLE/arac-DTN tasiyicisi ayni
// arayuzu (dtn.Carrier) genisletir.
type dtnCarrier struct {
	upstream string
	client   *http.Client
}

func (c dtnCarrier) Available() bool {
	if c.upstream == "" {
		return false
	}
	req, err := http.NewRequest(http.MethodHead, c.upstream, nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

func (c dtnCarrier) Send(ctx context.Context, b *dtn.Bundle) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.upstream, bytes.NewReader(b.Payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-DTN-Bundle", b.ID)
	req.Header.Set("X-DTN-Dst", b.Dst)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return errDTNUpstream
	}
	return nil
}

var errDTNUpstream = &dtnErr{"dtn: upstream 2xx dönmedi"}

type dtnErr struct{ s string }

func (e *dtnErr) Error() string { return e.s }

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
	carrier := dtnCarrier{upstream: os.Getenv("AETHERIS_DTN_UPSTREAM"), client: &http.Client{Timeout: 8 * time.Second}}
	fwd := dtn.NewForwarder(st, []dtn.Carrier{carrier}, logger)
	fwd.RetryAfter = 2 * time.Second
	fwd.OnDelivered = func(_ *dtn.Bundle) { dtnDelivered.Add(1) }
	go fwd.Run(ctx, 2*time.Second)

	mux.HandleFunc("/admin/dtn/test", func(w http.ResponseWriter, r *http.Request) {
		if adminToken == "" || r.URL.Query().Get("token") != adminToken {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		// P1-11: disk backpressure — kuyruk sınırını aşınca yeni bundle reddedilir.
		if max := dtnMax(); st.Size() >= max {
			http.Error(w, "DTN kuyruğu dolu (backpressure)", http.StatusTooManyRequests)
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

// dtnMax (P1-11), DTN kuyruğunun izin verilen azami bundle sayısıdır.
// AETHERIS_DTN_MAX ile ayarlanır; varsayılan 10000. Disk dolmasını önler.
func dtnMax() int {
	if v := os.Getenv("AETHERIS_DTN_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 10000
}

func randHexDTN(n int) string {
	b := make([]byte, n)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}
