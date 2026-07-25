// Package mesh, cok-sicramali (multi-hop) dinamik yonlendirme motorudur.
// A dugumu C'ye dogrudan erisemedigçinde, ara dugumler (B) uzerinden EN DUSUK
// maliyetli (RTT + tasiyici agirligi) yol Dijkstra ile hesaplanir ve paket
// hop-by-hop aktarilir. TTL ve dongu engelleme (loop-prevention) icerir.
package mesh

import (
	"errors"
	"sort"
)

// Carrier, bir baglantinin fiziksel tasiyicisidir. Her tasiyicinin goreli bir
// maliyet carpani vardir (LoRa yavas/pahali, Ethernet hizli/ucuz).
type Carrier string

const (
	CarrierEthernet Carrier = "ethernet"
	CarrierWiFi     Carrier = "wifi"
	CarrierLoRa     Carrier = "lora_ism"
)

// carrierCost, tasiyici basi goreli agirlik carpani. Dusuk = tercih edilir.
func carrierCost(c Carrier) float64 {
	switch c {
	case CarrierEthernet:
		return 1.0
	case CarrierWiFi:
		return 1.5
	case CarrierLoRa:
		return 4.0
	default:
		return 2.0
	}
}

var (
	ErrNoRoute   = errors.New("mesh: hedefe yol yok")
	ErrSelfEdge  = errors.New("mesh: kenar kaynak ve hedef ayni olamaz")
	ErrEmptyNode = errors.New("mesh: dugum kimligi bos olamaz")
)

// Edge, iki dugum arasindaki yonlu bir baglantidir.
type Edge struct {
	To      string
	RTTms   float64
	Carrier Carrier
}

// weight, kenar maliyeti = RTT * tasiyici carpani. Boylece hem gecikme hem
// tasiyici uygunlugu birlikte degerlendirilir.
func (e Edge) weight() float64 {
	rtt := e.RTTms
	if rtt <= 0 {
		rtt = 0.1 // sifir RTT'yi engelle (ayni maliyetli kenarlar icin)
	}
	return rtt * carrierCost(e.Carrier)
}

// Graph, mesh topolojisidir. Yonsuz kullanim icin AddLink her iki yonu ekler.
type Graph struct {
	adj map[string][]Edge
}

// NewGraph, bos bir topoloji olusturur.
func NewGraph() *Graph {
	return &Graph{adj: make(map[string][]Edge)}
}

// AddLink, a<->b arasinda CIFT YONLU bir baglanti ekler (mesh baglantilari
// simetriktir). Ayni cift icin cagrilirsa en iyi (dusuk maliyetli) kenar
// gecerli olur cunku Dijkstra minimumu secer.
func (g *Graph) AddLink(a, b string, rttMs float64, c Carrier) error {
	if a == "" || b == "" {
		return ErrEmptyNode
	}
	if a == b {
		return ErrSelfEdge
	}
	g.adj[a] = append(g.adj[a], Edge{To: b, RTTms: rttMs, Carrier: c})
	g.adj[b] = append(g.adj[b], Edge{To: a, RTTms: rttMs, Carrier: c})
	return nil
}

// AddDirected, yalnizca a->b yonunde kenar ekler (asimetrik baglantilar icin).
func (g *Graph) AddDirected(a, b string, rttMs float64, c Carrier) error {
	if a == "" || b == "" {
		return ErrEmptyNode
	}
	if a == b {
		return ErrSelfEdge
	}
	g.adj[a] = append(g.adj[a], Edge{To: b, RTTms: rttMs, Carrier: c})
	return nil
}

// Nodes, topolojideki tum dugum kimliklerini (sirali) dondurur.
func (g *Graph) Nodes() []string {
	seen := make(map[string]struct{})
	for n, edges := range g.adj {
		seen[n] = struct{}{}
		for _, e := range edges {
			seen[e.To] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// PathResult, hesaplanan yolun sonucudur.
type PathResult struct {
	Hops     []string  // src ... dst (dahil)
	Cost     float64   // toplam yol maliyeti
	Carriers []Carrier // her hop'ta kullanilan tasiyici (len = len(Hops)-1)
}

// NextHop, src'nin dst'ye ulasmak icin gonderecegi BIR SONRAKI dugumu
// dondurur. Yol yoksa ErrNoRoute.
func (g *Graph) NextHop(src, dst string) (string, error) {
	res, err := g.ShortestPath(src, dst)
	if err != nil {
		return "", err
	}
	if len(res.Hops) < 2 {
		return "", ErrNoRoute
	}
	return res.Hops[1], nil
}

// ShortestPath, Dijkstra ile src'den dst'ye EN DUSUK maliyetli yolu hesaplar.
// Maliyet = sum(RTT * tasiyici_carpani). Esit maliyette dugum kimligi
// alfabetik siraya gore belirlenir (deterministik).
func (g *Graph) ShortestPath(src, dst string) (PathResult, error) {
	if src == "" || dst == "" {
		return PathResult{}, ErrEmptyNode
	}
	if src == dst {
		return PathResult{Hops: []string{src}, Cost: 0}, nil
	}

	const inf = 1e18
	dist := map[string]float64{src: 0}
	prev := map[string]string{}
	prevEdge := map[string]Carrier{}
	visited := map[string]struct{}{}

	// Basit O(V^2) Dijkstra: dugum sayisi mesh olceginde kucuktur.
	for {
		// Ziyaret edilmemis en dusuk mesafeli dugumu sec (deterministik).
		cur := ""
		best := inf
		candidates := make([]string, 0, len(dist))
		for n := range dist {
			if _, ok := visited[n]; !ok {
				candidates = append(candidates, n)
			}
		}
		sort.Strings(candidates)
		for _, n := range candidates {
			if dist[n] < best {
				best = dist[n]
				cur = n
			}
		}
		if cur == "" {
			break // ulasilabilir dugum kalmadi
		}
		if cur == dst {
			break // hedefe en kisa mesafe kesinlesti
		}
		visited[cur] = struct{}{}

		// Komsulari gevset. Kenarlari deterministik sirada isle.
		edges := append([]Edge(nil), g.adj[cur]...)
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].To != edges[j].To {
				return edges[i].To < edges[j].To
			}
			return edges[i].weight() < edges[j].weight()
		})
		for _, e := range edges {
			nd := dist[cur] + e.weight()
			if d, ok := dist[e.To]; !ok || nd < d {
				dist[e.To] = nd
				prev[e.To] = cur
				prevEdge[e.To] = e.Carrier
			}
		}
	}

	if _, ok := dist[dst]; !ok {
		return PathResult{}, ErrNoRoute
	}

	// Yolu geri izle.
	var revHops []string
	var revCarr []Carrier
	for at := dst; at != ""; {
		revHops = append(revHops, at)
		if at == src {
			break
		}
		revCarr = append(revCarr, prevEdge[at])
		p, ok := prev[at]
		if !ok {
			return PathResult{}, ErrNoRoute
		}
		at = p
	}
	// Ters cevir.
	hops := make([]string, len(revHops))
	for i := range revHops {
		hops[i] = revHops[len(revHops)-1-i]
	}
	carr := make([]Carrier, len(revCarr))
	for i := range revCarr {
		carr[i] = revCarr[len(revCarr)-1-i]
	}

	return PathResult{Hops: hops, Cost: dist[dst], Carriers: carr}, nil
}
