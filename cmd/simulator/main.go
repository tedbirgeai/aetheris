package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/tedbirgeai/aetheris/internal/store"
)

func main() {
	nodes := flag.Int("nodes", 5, "sanal dugum sayisi")
	perSide := flag.Int("records", 10, "bolunme sirasinda HER TARAFA yazilacak kayit sayisi")
	gossipMS := flag.Int("gossip-ms", 30, "gossip turu araligi (ms)")
	partitionSecs := flag.Int("partition-secs", 1, "bolunmenin surdurulecegi sure (saniye)")
	verbose := flag.Bool("v", false, "ayrintili log")
	flag.Parse()

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ok, report := RunSplitBrainScenario(ScenarioConfig{
		Nodes:          *nodes,
		RecordsPerSide: *perSide,
		GossipMS:       *gossipMS,
		PartitionFor:   time.Duration(*partitionSecs) * time.Second,
		Logger:         logger,
	})

	fmt.Println(report)
	if !ok {
		fmt.Println("SONUC: BASARISIZ — veri kaybi veya yakinsama saglanamadi")
		os.Exit(1)
	}
	fmt.Println("SONUC: BASARILI — bolunme sonrasi sifir kayipla senkronize olundu")
}

// ScenarioConfig, split-brain senaryosunun parametreleridir.
type ScenarioConfig struct {
	Nodes          int
	RecordsPerSide int
	GossipMS       int
	PartitionFor   time.Duration
	Logger         *slog.Logger
}

// RunSplitBrainScenario, tam senaryoyu yurutur ve (basari, rapor) dondurur.
// Bu fonksiyon hem CLI'dan hem testten cagrilir; kabul kriteri #3'un
// dogrulama noktasidir.
func RunSplitBrainScenario(cfg ScenarioConfig) (bool, string) {
	if cfg.Nodes < 4 {
		cfg.Nodes = 5 // bolunme icin en az 2+2 gerek
	}
	if cfg.RecordsPerSide <= 0 {
		cfg.RecordsPerSide = 10
	}
	if cfg.PartitionFor <= 0 {
		cfg.PartitionFor = time.Second
	}

	sim, err := NewSim(SimConfig{Nodes: cfg.Nodes, GossipMS: cfg.GossipMS, Logger: cfg.Logger})
	if err != nil {
		return false, "simulator kurulamadi: " + err.Error()
	}
	defer sim.Close()

	var b []string
	log := func(f string, a ...any) { b = append(b, fmt.Sprintf(f, a...)) }
	log("=== AETHERIS SPLIT-BRAIN WAL SENKRONIZASYON SIMULATORU ===")
	log("dugum sayisi        : %d", cfg.Nodes)
	log("her tarafa kayit     : %d", cfg.RecordsPerSide)

	sim.Start()

	// 1) Kesif: tum dugumler birbirini bulmali (merkezi sunucu YOK).
	if !sim.WaitPeers(cfg.Nodes-1, 5*time.Second) {
		return false, joinReport(b, "HATA: dugumler birbirini kesfedemedi")
	}
	log("[1] kesif tamam: her dugum %d komsu buldu", cfg.Nodes-1)

	// 2) BOL: {0,1} grubunu geri kalandan izole et.
	left := []int{0, 1}
	right := make([]int, 0, cfg.Nodes-2)
	for i := 2; i < cfg.Nodes; i++ {
		right = append(right, i)
	}
	sim.Partition(left, right)
	log("[2] AG BOLUNDU: %v  <-X->  %v", left, right)

	// 3) Bolunme sirasinda HER IKI tarafa da kayit yaz.
	total := 0
	for i := 0; i < cfg.RecordsPerSide; i++ {
		sim.RecordUsage(left[i%len(left)], mkUsage("left", i))
		sim.RecordUsage(right[i%len(right)], mkUsage("right", i))
		total += 2
	}
	log("[3] bolunme sirasinda %d kayit yazildi (WAL'a dayanikli)", total)

	// Bolunme sirasinda TAM yakinsama OLMAMALI (iki taraf ayri).
	time.Sleep(cfg.PartitionFor)
	lens := sim.GossipLens()
	log("    bolunme aninda gossip kume boyutlari: %v", lens)
	converged := true
	for _, n := range lens {
		if n != total {
			converged = false
		}
	}
	if converged {
		return false, joinReport(b, "HATA: bolunmeye ragmen erken yakinsama (izolasyon calismiyor)")
	}
	log("    dogru: taraflar bolunme sirasinda ayri (henuz yakinsamadi)")

	// 4) IYILESTIR.
	sim.Heal(left, right)
	log("[4] AG BIRLESTIRILDI (re-convergence baslatildi)")

	// 5) Tum dugumler TUM kayitlara yakinsamali — sifir kayip.
	if !sim.WaitConverged(total, 10*time.Second) {
		return false, joinReport(b, fmt.Sprintf(
			"HATA: yakinsama saglanamadi, son gossip boyutlari: %v (beklenen %d)",
			sim.GossipLens(), total))
	}
	log("[5] gossip yakinsamasi: tum dugumler %d kayda ulasti", total)

	// 6) WAL dayanikliligi: her dugumun WAL backend'i de tam olmali.
	if !sim.WaitWALDrained(total, 10*time.Second) {
		return false, joinReport(b, fmt.Sprintf(
			"HATA: WAL flush tamamlanmadi, boyutlar: %v (beklenen %d)",
			sim.WALLens(), total))
	}
	log("[6] WAL senkron: her dugumun defteri %d kayit (store-and-forward)", total)
	log("")
	log("VERI KAYBI: %%0  —  bolunme sirasinda yazilan %d kaydin tamami", total)
	log("            iyilesme sonrasi TUM dugumlerde mevcut.")

	return true, joinReport(b, "")
}

func mkUsage(side string, i int) store.Usage {
	return store.Usage{
		ClientID:    fmt.Sprintf("%s-client-%d", side, i),
		CarrierType: "lora_ism",
		BytesIn:     uint64(100 + i),
		BytesOut:    uint64(50 + i),
		PayloadSHA:  fmt.Sprintf("%s-%d-payload", side, i),
		OccurredAt:  time.Now().UTC(),
	}
}

func joinReport(lines []string, last string) string {
	if last != "" {
		lines = append(lines, last)
	}
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}
