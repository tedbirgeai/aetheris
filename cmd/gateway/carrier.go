// Package main — tasiyici (bearer) Manager baglantisi.
//
// Bu dosyayi cmd/gateway/carrier.go olarak koyun (yeni dosya).
//
// AMAC: internal/router/bearer.Manager'i olusturup calistirir ve secilen
// aktif tasiyiciyi telemetriye baglar. bearers.go'daki lisanssiz mock
// adaptorleri (WiGig/FSO/HaLow) + Ethernet/WiFi TCP prob'lari Manager
// tarafindan yoklanir; en saglikli olan "aktif tasiyici" olur ve panelde
// TASIYICI BORU HATTI'nda yesil yanar.
//
// KURULUM (main.go'da 2 kucuk duzenleme — asagidaki yorumlarda birebir):
//
//  (A) run() icinde, "WAN durumu dedektoru aktif" blogundan HEMEN SONRA
//      (admin paneli blogundan once) su satiri ekleyin:
//
//          startBearers(bgCtx, cfg.WANTargets, logger)
//
//  (B) buildTelemetry() icinde su iki satiri:
//
//          t.ActiveCarrier = "ip"
//          if src.loraActive {
//              t.ActiveCarrier = "ip+lora"
//          }
//
//      sununla DEGISTIRIN:
//
//          t.ActiveCarrier = activeCarrier("ip")
//          if src.loraActive && (t.ActiveCarrier == "ip" || t.ActiveCarrier == "ethernet" || t.ActiveCarrier == "wifi_wan") {
//              t.ActiveCarrier = "ip+lora"
//          }
//          t.Nodes = append(carrierNodes(), t.Nodes...)

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/tedbirgeai/aetheris/internal/dashboard"
	"github.com/tedbirgeai/aetheris/internal/router/bearer"
)

// gBearer, calisan tasiyici Manager'i (nil = henuz baslatilmadi).
var gBearer *bearer.Manager

// startBearers, tasiyici Manager'i olusturur, varsayilan tasiyicilari
// (Ethernet/WiFi TCP + WiGig/FSO/HaLow mock) kaydeder ve saglik/failover
// dongusunu bgCtx omru boyunca calistirir.
func startBearers(ctx context.Context, wanTargets []string, logger *slog.Logger) {
	if len(wanTargets) == 0 {
		wanTargets = []string{"1.1.1.1:53", "8.8.8.8:53"}
	}
	gBearer = bearer.New(logger, func(ev bearer.ChangeEvent) {
		logger.Info("FAILOVER: aktif tasiyici degisti",
			"onceki", ev.From, "yeni", ev.To, "rtt_ms", ev.RTT)
	}, 3*time.Second)
	for _, b := range bearer.DefaultBearers(wanTargets) {
		gBearer.Register(b)
	}
	go gBearer.Run(ctx)
	logger.Info("Tasiyici Manager aktif — lisanssiz boru hatti canli",
		"tasiyici_sayisi", 10)
}

// activeCarrier, secili aktif tasiyicinin kind'ini dondurur; Manager yoksa
// veya hic saglikli tasiyici secilmediyse fallback doner.
func activeCarrier(fallback string) string {
	if gBearer != nil {
		if k := gBearer.Active(); k != "" {
			return string(k)
		}
	}
	return fallback
}

// carrierNodes, tum tasiyicilarin anlik durumunu panel dugum satirlarina
// cevirir (Available + saglik + RTT). Boylece boru hatti tablosunda her
// tasiyici gercek durumuyla gorunur.
func carrierNodes() []dashboard.NodeInfo {
	if gBearer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out := make([]dashboard.NodeInfo, 0, 10)
	for _, s := range gBearer.Snapshot(ctx) {
		out = append(out, dashboard.NodeInfo{
			ID:      string(s.Kind),
			Carrier: string(s.Kind),
			RTTms:   s.RTTms,
			Alive:   s.Available && s.Healthy,
		})
	}
	return out
}
