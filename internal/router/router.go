// Package router, opak tunel paketlerini hedef upstream'e ileten
// yonlendirme motorudur.
//
// EGRESS MALIYETI HAKKINDA DURUST NOT:
// Bir proxy, bulut saglayicisinin egress maliyetini DUSURMEZ. Bir bayt
// once gecide gelir (ingress), sonra hedefe gider (egress). Gecit bulut
// icinde calisiyorsa toplam egress AZALMAZ, aksine bir kat daha eklenir.
//
// Gercek tasarrufun tek yolu topolojiktir:
//   - "edge"    : trafigi bulut disindaki kenar dugumune yonlendirir;
//     tekrar eden icerik orada onbelleklenirse kaynak
//     sunucudan cikan bayt azalir.
//   - "peering" : dogrudan eslesme baglantisi uzerinden gider; bulut
//     saglayicisinin olculu egress'i hic devreye girmez.
//   - "direct"  : optimizasyon yok, duz iletim. Olcum ve test icin.
//
// Yani tasarrufu bu paket degil, rotalarin NEREYE isaret ettigi belirler.
// Kod yalnizca o topolojiyi uygulanabilir kilar.
package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tedbirgeai/aetheris/internal/carrier"
)

var (
	// ErrDisabled, hic rota tanimlanmadigini belirtir.
	ErrDisabled = errors.New("router: yonlendirme devre disi (rota tanimli degil)")
	// ErrNoRoute, istenen hedefin tanimli olmadigini belirtir.
	ErrNoRoute = errors.New("router: hedef rota bulunamadi")
	// ErrCarrierNotAllowed, tasiyicinin izin listesinde olmadigini belirtir.
	ErrCarrierNotAllowed = errors.New("router: tasiyici yonlendirmeye izinli degil")
)

// maxUpstreamResponse, upstream yanitindan okunacak azami bayt.
// Sinirsiz okuma, kotu niyetli veya hatali bir upstream'in gecidin
// bellegini tuketmesine izin verirdi.
const maxUpstreamResponse = 8 << 20 // 8 MiB

// Route, tek bir yonlendirme hedefidir.
type Route struct {
	Name     string
	Kind     string // "direct" | "edge" | "peering"
	Upstream *url.URL
	// Backup, bu rota sagliksizken denenecek yedek rotanin adi (opsiyonel).
	// Bos ise ve rota sagliksizsa "direct" hatta dusulur (varsa).
	Backup string
	// HealthPath, saglik kontrolu icin GET atilacak yol. Bos ise "/healthz".
	HealthPath string
}

// Result, basarili bir yonlendirmenin sonucudur.
type Result struct {
	RouteName      string        `json:"route_name"`
	RouteKind      string        `json:"route_kind"`
	UpstreamStatus int           `json:"upstream_status"`
	BytesSent      uint64        `json:"bytes_sent"`
	BytesReceived  uint64        `json:"bytes_received"`
	Duration       time.Duration `json:"-"`
	DurationMS     int64         `json:"duration_ms"`
}

// Router, rota tablosunu ve upstream HTTP istemcisini tutar.
type Router struct {
	routes map[string]Route
	client *http.Client
}

// New, rota tablosunu kurar. routes bos ise yonlendirme devre disidir.
func New(routes []Route, timeout time.Duration) *Router {
	table := make(map[string]Route, len(routes))
	for _, r := range routes {
		table[r.Name] = r
	}
	return &Router{
		routes: table,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Enabled, en az bir rota tanimli olup olmadigini bildirir.
func (r *Router) Enabled() bool { return len(r.routes) > 0 }

// Routes, tanimli rota adlarini dondurur (health ucu icin).
func (r *Router) Routes() map[string]string {
	out := make(map[string]string, len(r.routes))
	for name, rt := range r.routes {
		out[name] = rt.Kind
	}
	return out
}

// Forward, opak yuku hedef rotaya iletir.
//
// SIFIR BILGI: payload burada da opaktir. Router icerigi cozmez,
// ayristirmaz, degistirmez - oldugu gibi govdeye yazar.
func (r *Router) Forward(
	ctx context.Context,
	destination string,
	clientID string,
	carrierType carrier.Type,
	payload []byte,
) (*Result, error) {
	if !r.Enabled() {
		return nil, ErrDisabled
	}
	if !carrier.IsAllowed(carrierType) {
		return nil, ErrCarrierNotAllowed
	}

	route, ok := r.routes[destination]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoRoute, destination)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		route.Upstream.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("router: istek olusturulamadi: %w", err)
	}

	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Aetheris-Carrier", string(carrierType))
	req.Header.Set("X-Aetheris-Route-Kind", route.Kind)
	// Istemci kimligi upstream'e iletilir ki kenar dugumu de
	// kendi tarafinda iliskilendirme yapabilsin. Anahtar ASLA iletilmez.
	req.Header.Set("X-Aetheris-Client", clientID)
	req.ContentLength = int64(len(payload))

	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("router: upstream'e ulasilamadi (%s): %w", route.Name, err)
	}
	defer resp.Body.Close()

	// Yaniti sinirli okuyoruz. Govdeyi tamamen tuketmek, baglantinin
	// havuza geri donebilmesi (keep-alive) icin de gereklidir.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamResponse))
	if err != nil {
		return nil, fmt.Errorf("router: upstream yaniti okunamadi (%s): %w", route.Name, err)
	}

	elapsed := time.Since(start)
	res := &Result{
		RouteName:      route.Name,
		RouteKind:      route.Kind,
		UpstreamStatus: resp.StatusCode,
		BytesSent:      uint64(len(payload)),
		BytesReceived:  uint64(len(body)),
		Duration:       elapsed,
		DurationMS:     elapsed.Milliseconds(),
	}

	// 5xx, upstream arizasidir; cagiran taraf bunu ayirt edebilmelidir.
	if resp.StatusCode >= 500 {
		return res, fmt.Errorf("router: upstream %s hata dondu: %d", route.Name, resp.StatusCode)
	}
	return res, nil
}
