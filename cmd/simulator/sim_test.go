package main

import (
	"testing"
	"time"
)

// TestFiveNodeZeroLoss, KABUL KRITERI #3'u dogrular:
// 5 dugumlu simulatorde ag kopuklugunda veri kaybi %0 olmali.
func TestFiveNodeZeroLoss(t *testing.T) {
	ok, report := RunSplitBrainScenario(ScenarioConfig{
		Nodes:          5,
		RecordsPerSide: 15,
		GossipMS:       20,
		PartitionFor:   400 * time.Millisecond,
	})
	t.Log("\n" + report)
	if !ok {
		t.Fatal("5 dugumlu split-brain senaryosu sifir kayipla yakinsamadi")
	}
}

// TestScenarioLargerMesh, daha buyuk bir mesh'te de sifir kayip.
func TestScenarioLargerMesh(t *testing.T) {
	if testing.Short() {
		t.Skip("kisa modda atlaniyor")
	}
	ok, report := RunSplitBrainScenario(ScenarioConfig{
		Nodes:          7,
		RecordsPerSide: 20,
		GossipMS:       20,
		PartitionFor:   400 * time.Millisecond,
	})
	if !ok {
		t.Fatalf("7 dugumlu senaryo basarisiz:\n%s", report)
	}
}

// TestSimDirectAPI, Sim API'sini dogrudan kullanarak WAL/gossip tutarliligini
// dogrular (senaryo sarmalayicisindan bagimsiz birim testi).
func TestSimDirectAPI(t *testing.T) {
	sim, err := NewSim(SimConfig{Nodes: 4, GossipMS: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	sim.Start()

	if !sim.WaitPeers(3, 5*time.Second) {
		t.Fatal("kesif basarisiz")
	}

	// Tek bir dugume 10 kayit yaz; hepsi tum dugumlere yayilmali.
	for i := 0; i < 10; i++ {
		sim.RecordUsage(0, mkUsage("solo", i))
	}
	if !sim.WaitConverged(10, 5*time.Second) {
		t.Fatalf("gossip yakinsamadi: %v", sim.GossipLens())
	}
	if !sim.WaitWALDrained(10, 5*time.Second) {
		t.Fatalf("WAL yakinsamadi: %v", sim.WALLens())
	}
}
