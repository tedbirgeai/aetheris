// Sistem zenginleştirme — Kişisel Gelen Kutusu + Veri Katırı (Data Mule).
// Additive; mevcut DTN motorunun (internal/dtn) üstüne oturur, hiçbir şeyi bozmaz.
//
// cmd/gateway/enrich.go olarak koyun. main.go'da startDTN'den SONRA:
//   registerEnrichment(mux, dtnStore, cfg.AdminToken)
//
// UÇLAR (hepsi token korumalı):
//   POST /admin/inbox/put?token=..&to=<bearer>     gövde = mesaj  → gezgine kuyruğa al
//   GET  /admin/inbox/get?token=..&to=<bearer>     → bekleyen mesajları getir + teslim işaretle
//   GET  /admin/inbox/peek?token=..&to=<bearer>    → sadece sayım (silmez)
//   POST /admin/mule/sync?token=..                 gövde = JSON bundle[] → veri katırından topla
//   GET  /admin/mule/drain?token=..&carrier=<id>   → katıra yüklenecek bekleyen bundle'lar
//
// Kişisel Gelen Kutusu: kullanıcı menzil dışındayken mesaj/güncelleme birikir,
// dönünce /inbox/get ile iner (DTN-destekli, sıfır kayıp).
// Veri Katırı: araç/dolmuşa takılı mobil düğüm köye uğrayınca /mule/sync ile
// taşıdığı bundle'ları boşaltır, /mule/drain ile yenilerini yükler.

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tedbirgeai/aetheris/internal/dtn"
)

// InboxMessage, bir bearer (kullanıcı/düğüm) için bekleyen mesajdır.
type InboxMessage struct {
	ID        string    `json:"id"`
	To        string    `json:"to"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

var inbox = struct {
	mu sync.Mutex
	m  map[string][]InboxMessage // bearer -> mesajlar
}{m: make(map[string][]InboxMessage)}

// registerEnrichment, gelen kutusu + veri katırı uçlarını bağlar.
func registerEnrichment(mux *http.ServeMux, store *dtn.Store, adminToken string) {
	auth := func(r *http.Request) bool {
		return adminToken != "" && r.URL.Query().Get("token") == adminToken
	}

	mux.HandleFunc("/admin/inbox/put", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		to := strings.TrimSpace(r.URL.Query().Get("to"))
		if to == "" {
			http.Error(w, "to gerekli", http.StatusBadRequest)
			return
		}
		body := string(readAllEnrich(r, 1<<20))
		msg := InboxMessage{ID: randHexDTN(8), To: to, Body: body, CreatedAt: time.Now()}
		inbox.mu.Lock()
		inbox.m[to] = append(inbox.m[to], msg)
		n := len(inbox.m[to])
		inbox.mu.Unlock()
		// DTN'e de yaz: gezgin başka bir federasyon düğümüne uğrarsa oradan da alınır.
		if store != nil {
			_ = store.Put(&dtn.Bundle{
				ID: "inbox-" + msg.ID, Src: "gw", Dst: to, Priority: dtn.PriorityNormal,
				CreatedAt: msg.CreatedAt, ExpiresAt: msg.CreatedAt.Add(72 * time.Hour),
				Payload: []byte(body),
			})
		}
		writeJSONEnrich(w, map[string]any{"queued": msg.ID, "pending": n})
	})

	mux.HandleFunc("/admin/inbox/peek", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		to := r.URL.Query().Get("to")
		inbox.mu.Lock()
		n := len(inbox.m[to])
		inbox.mu.Unlock()
		writeJSONEnrich(w, map[string]any{"to": to, "pending": n})
	})

	mux.HandleFunc("/admin/inbox/get", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		to := r.URL.Query().Get("to")
		inbox.mu.Lock()
		msgs := inbox.m[to]
		delete(inbox.m, to) // teslim edildi → kuyruktan düş
		inbox.mu.Unlock()
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].CreatedAt.Before(msgs[j].CreatedAt) })
		writeJSONEnrich(w, map[string]any{"to": to, "count": len(msgs), "messages": msgs})
	})

	mux.HandleFunc("/admin/mule/drain", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		if store == nil {
			writeJSONEnrich(w, map[string]any{"bundles": []any{}})
			return
		}
		// Katıra yüklenecek bekleyen bundle'lar (mobil röle bunları uzağa taşır).
		pend := store.Pending()
		out := make([]map[string]any, 0, len(pend))
		for _, b := range pend {
			out = append(out, map[string]any{"id": b.ID, "dst": b.Dst, "payload": string(b.Payload), "created_at": b.CreatedAt})
		}
		writeJSONEnrich(w, map[string]any{"count": len(out), "bundles": out})
	})

	mux.HandleFunc("/admin/mule/sync", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		var in struct {
			Bundles []struct {
				ID      string `json:"id"`
				Dst     string `json:"dst"`
				Payload string `json:"payload"`
			} `json:"bundles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "gövde ayrıştırılamadı", http.StatusBadRequest)
			return
		}
		accepted := 0
		for _, b := range in.Bundles {
			if store != nil {
				_ = store.Put(&dtn.Bundle{
					ID: b.ID, Src: "mule", Dst: b.Dst, Priority: dtn.PriorityNormal,
					CreatedAt: time.Now(), ExpiresAt: time.Now().Add(72 * time.Hour),
					Payload: []byte(b.Payload),
				})
			}
			// Hedef bir kullanıcıysa gelen kutusuna da düş.
			if b.Dst != "" {
				inbox.mu.Lock()
				inbox.m[b.Dst] = append(inbox.m[b.Dst], InboxMessage{ID: b.ID, To: b.Dst, Body: b.Payload, CreatedAt: time.Now()})
				inbox.mu.Unlock()
			}
			accepted++
		}
		writeJSONEnrich(w, map[string]any{"accepted": accepted})
	})
}

func writeJSONEnrich(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func readAllEnrich(r *http.Request, max int) []byte {
	out := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil || len(out) >= max {
			return out
		}
	}
}
