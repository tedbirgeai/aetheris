package mesh

import (
	"errors"
	"sync"
)

// Bu dosya, hesaplanan yollara gore paketleri GERCEKTEN hop-by-hop aktaran
// dugum agini uygular. Her dugum bir Router'dir; komsularina Link'lerle
// baglidir. Paket TTL ile sinirlanir ve ziyaret edilen dugumler Path'te
// tutularak dongu (loop) engellenir.

const DefaultTTL = 16

var (
	ErrTTLExceeded = errors.New("mesh: TTL tukendi, paket dusuruldu")
	ErrLoop        = errors.New("mesh: dongu tespit edildi, paket dusuruldu")
	ErrNoNextHop   = errors.New("mesh: sonraki hop icin baglanti yok")
)

// Packet, mesh uzerinde tasinan bir veri birimidir.
type Packet struct {
	ID      string   // benzersiz paket kimligi (dedup/izleme icin)
	Src     string   // kaynak dugum
	Dst     string   // hedef dugum
	TTL     int      // kalan sicrama hakki
	Path    []string // simdiye kadar gecilen dugumler (dongu engelleme)
	Payload []byte   // zero-knowledge: motor icerigi yorumlamaz
}

// Link, iki komsu dugum arasinda paket tasir. Surec-ici test/mesh icin
// chanLink kullanilir; gercek uygulama LoRa/UDP/serial uzerinden olabilir.
type Link interface {
	Deliver(p Packet) error
}

// DeliverFunc, teslim edilen (hedefe ulasan) paketler icin geri cagridir.
type DeliverFunc func(p Packet)

// Router, tek bir mesh dugumudur. Kendi kimligini, komsu Link'lerini ve
// topolojiye gore hesaplanmis yonlendirme tablosunu tutar.
type Router struct {
	ID string

	mu       sync.RWMutex
	graph    *Graph
	links    map[string]Link // komsuID -> Link
	onDelivr DeliverFunc

	// gozlem
	forwarded uint64
	delivered uint64
	dropped   uint64
}

// NewRouter, verilen kimlikle bir dugum olusturur.
func NewRouter(id string) *Router {
	return &Router{
		ID:    id,
		graph: NewGraph(),
		links: make(map[string]Link),
	}
}

// SetGraph, dugumun yonlendirme icin kullanacagi topolojiyi belirler. Gercek
// sistemde bu, gossip ile yayilan link-state bilgisinden turetilir.
func (r *Router) SetGraph(g *Graph) {
	r.mu.Lock()
	r.graph = g
	r.mu.Unlock()
}

// Connect, bu dugumu bir komsuya baglar (komsuID -> Link).
func (r *Router) Connect(neighborID string, link Link) {
	r.mu.Lock()
	r.links[neighborID] = link
	r.mu.Unlock()
}

// OnDeliver, paket bu dugume (hedef) ulastiginda cagrilacak fonksiyonu ayarlar.
func (r *Router) OnDeliver(f DeliverFunc) {
	r.mu.Lock()
	r.onDelivr = f
	r.mu.Unlock()
}

// Send, bu dugumden yeni bir paket baslatir.
func (r *Router) Send(dst string, id string, payload []byte) error {
	p := Packet{
		ID:      id,
		Src:     r.ID,
		Dst:     dst,
		TTL:     DefaultTTL,
		Payload: payload,
	}
	return r.Deliver(p)
}

// Deliver, Link arayuzunu karsilar: bu dugume gelen (veya baslatilan) paketi
// isler. Hedef buysa teslim eder; degilse yonlendirir.
func (r *Router) Deliver(p Packet) error {
	// Hedefe ulasti mi?
	if p.Dst == r.ID {
		r.mu.Lock()
		r.delivered++
		f := r.onDelivr
		r.mu.Unlock()
		if f != nil {
			f(p)
		}
		return nil
	}

	// Dongu engelleme: bu dugum zaten yolda varsa dusur.
	for _, hop := range p.Path {
		if hop == r.ID {
			r.mu.Lock()
			r.dropped++
			r.mu.Unlock()
			return ErrLoop
		}
	}

	// TTL kontrolu.
	if p.TTL <= 0 {
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
		return ErrTTLExceeded
	}

	// Sonraki hop'u topolojiden hesapla.
	r.mu.RLock()
	g := r.graph
	r.mu.RUnlock()
	next, err := g.NextHop(r.ID, p.Dst)
	if err != nil {
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
		return err
	}

	r.mu.RLock()
	link, ok := r.links[next]
	r.mu.RUnlock()
	if !ok {
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
		return ErrNoNextHop
	}

	// Paketi guncelle: TTL azalt, yola bu dugumu ekle.
	fwd := p
	fwd.TTL = p.TTL - 1
	fwd.Path = append(append([]string(nil), p.Path...), r.ID)

	r.mu.Lock()
	r.forwarded++
	r.mu.Unlock()

	return link.Deliver(fwd)
}

// Stats, gozlem sayaclarini dondurur.
type Stats struct {
	Forwarded uint64
	Delivered uint64
	Dropped   uint64
}

func (r *Router) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Stats{Forwarded: r.forwarded, Delivered: r.delivered, Dropped: r.dropped}
}

// --- Surec-ici baglanti (test/tek-surec mesh) ---

// chanLink, hedef Router'a dogrudan (senkron) teslim eden bir Link'tir.
// Gercek aglarda yerini UDP/LoRa tasiyicisi alir.
type directLink struct{ target *Router }

func (l directLink) Deliver(p Packet) error { return l.target.Deliver(p) }

// Wire, iki router'i CIFT YONLU baglar (birbirlerine directLink verir).
func Wire(a, b *Router) {
	a.Connect(b.ID, directLink{target: b})
	b.Connect(a.ID, directLink{target: a})
}
