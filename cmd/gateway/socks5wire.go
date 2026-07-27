// SOCKS5 canli baglama — additive. Mevcut hicbir mantigi degistirmez.
//
// internal/tunnel/socks5.go'daki gercek RFC1928 SOCKS5Server'i baslatir ve
// sayaclarini panele (dashboard.Telemetry.SOCKS5) + bant genisligine baglar.
// Boylece `curl -x socks5h://127.0.0.1:1081 https://ifconfig.me` gibi GERCEK
// trafik panelde "SOCKS5 Aktif", "Toplam Islenen" ve "Bant Genisligi"
// kartlarini canli doldurur. Sahte veri yok — tum sayimlar gercek baglantidan.
//
// Kurulum: cmd/gateway/socks5wire.go olarak koyun. main.go'ya iki satir eklenir
// (asagidaki komutlar). Port varsayilani 127.0.0.1:1081 (ForwardAddr 1080 ile
// cakismaz). Ozel port: AETHERIS_SOCKS_ADDR.

package main

import (
	"context"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/tedbirgeai/aetheris/internal/dashboard"
	"github.com/tedbirgeai/aetheris/internal/tunnel"
)

var (
	socksServer   *tunnel.SOCKS5Server
	socksListen   string
	socksBytes    atomic.Uint64 // kumulatif islenen bayt
	socksLastByte atomic.Uint64 // bir onceki saniyedeki toplam
	socksBps      atomic.Uint64 // anlik bayt/sn
)

// startSOCKS5, gercek bir SOCKS5 proxy baslatir (bayt sayan dialer ile).
// addr bos ise 127.0.0.1:1081. Port dinlenemezse sessizce atlar (cakisma).
func startSOCKS5(ctx context.Context, addr string, logger *slog.Logger) {
	if addr == "" {
		addr = "127.0.0.1:1081"
	}
	socksListen = addr
	// Bayt sayan dialer: her hedef baglantisini countConn ile sarar.
	dialer := func(dctx context.Context, network, target string) (net.Conn, error) {
		d := net.Dialer{Timeout: 10 * time.Second}
		c, err := d.DialContext(dctx, network, target)
		if err != nil {
			return nil, err
		}
		return &countConn{Conn: c}, nil
	}
	s := tunnel.NewSOCKS5Server(addr, dialer, "", "", logger)
	if err := s.Listen(); err != nil {
		logger.Warn("SOCKS5 dinleyemedi (port dolu olabilir) — atlaniyor", "addr", addr, "err", err)
		return
	}
	socksServer = s
	go func() { _ = s.Serve(ctx) }()
	// Anlik bant genisligi (bayt/sn) hesaplayici.
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				cur := socksBytes.Load()
				prev := socksLastByte.Swap(cur)
				if cur >= prev {
					socksBps.Store(cur - prev)
				}
			}
		}
	}()
	logger.Info("SOCKS5 proxy aktif — canli telemetri baglandi", "addr", addr)
}

// countConn, gecen baytlari sayan net.Conn sarmalayicisidir.
type countConn struct{ net.Conn }

func (c *countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		socksBytes.Add(uint64(n))
	}
	return n, err
}

func (c *countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		socksBytes.Add(uint64(n))
	}
	return n, err
}

// socks5Telemetry, panel icin canli SOCKS5 sayaclarini dondurur (nil = kapali).
func socks5Telemetry() *dashboard.SOCKS5Stat {
	if socksServer == nil {
		return nil
	}
	st := socksServer.Stats()
	return &dashboard.SOCKS5Stat{Active: st.Active, Handled: st.Handled, Listen: socksListen}
}

// socksThroughput, anlik SOCKS5 bant genisligini (bayt/sn) dondurur.
func socksThroughput() uint64 { return socksBps.Load() }
