package billing

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CreditEngine, baskalarinin sifreli paketlerini kendi dugumunden
// aktaran (relay eden) musterilere FATURA INDIRIM KREDISI hesaplar.
//
// # KRIPTO YOK, TOKEN YOK
//
// Kazanilan sey bir jeton veya kripto para DEGILDIR. Musterinin bir
// sonraki faturasindan dusulecek TL/USD cinsinden indirim hakkidir.
// Bu, hukuki olarak sade bir ticari iskonto mekanizmasidir; menkul
// kiymet, odeme araci veya kripto varlik ihraci degildir.
//
// # NASIL HESAPLANIR
//
//	kredi_birimi = role_edilen_bayt * CreditPerByte
//
// CreditPerByte, isletmecinin belirledigi orandir (ornegin 1 GB role
// icin X birim). Birim -> para donusumu FATURALAMA tarafinda yapilir;
// bu motor yalnizca birim sayar.
//
// # KOTUYE KULLANIMA KARSI
//
// Kredi yalnizca GERCEKTEN ILETILMIS trafik icin verilir: router'in
// upstream'den 2xx aldigi ve baytlarin telde aktigi durumlar. Kendi
// kendine trafik uretip kredi toplamayi zorlastirmak icin:
//   - MaxCreditsPerPeriod ile donem basina tavan uygulanir
//   - Kendi trafigini role etmek kredi kazandirmaz (self-relay engeli)
type CreditEngine struct {
	mu sync.RWMutex

	// CreditPerByte, bayt basina kazanilan kredi birimi.
	creditPerByte float64
	// maxPerPeriod, bir musterinin donem basina kazanabilecegi tavan.
	// 0 = sinirsiz.
	maxPerPeriod uint64

	ledger map[string]*CreditRecord
	bridge *Bridge
}

// CreditRecord, tek bir musterinin kredi durumudur.
type CreditRecord struct {
	ClientID     string    `json:"client_id"`
	RelayedBytes uint64    `json:"relayed_bytes"`
	CreditUnits  uint64    `json:"credit_units"`
	CappedUnits  uint64    `json:"capped_units"`
	RelayCount   uint64    `json:"relay_count"`
	FirstRelayAt time.Time `json:"first_relay_at"`
	LastRelayAt  time.Time `json:"last_relay_at"`
}

// NewCreditEngine, kredi motorunu kurar.
// bridge nil olabilir (olay yayini istenmiyorsa).
func NewCreditEngine(creditPerByte float64, maxPerPeriod uint64, bridge *Bridge) *CreditEngine {
	if creditPerByte < 0 {
		creditPerByte = 0
	}
	return &CreditEngine{
		creditPerByte: creditPerByte,
		maxPerPeriod:  maxPerPeriod,
		ledger:        make(map[string]*CreditRecord),
		bridge:        bridge,
	}
}

// RecordRelay, basarili bir role islemini kaydeder ve kredi hesaplar.
//
// relayerID : trafigi kendi dugumunden gecirenin kimligi
// originID  : trafigin gercek sahibi (self-relay engeli icin)
// bytes     : GERCEKTEN iletilmis bayt (iddia degil, olcum)
//
// Kazanilan kredi birimini dondurur.
func (c *CreditEngine) RecordRelay(relayerID, originID string, bytes uint64) uint64 {
	// Kendi trafigini role etmek kredi kazandirmaz.
	if relayerID == "" || relayerID == originID || bytes == 0 {
		return 0
	}

	units := uint64(float64(bytes) * c.creditPerByte)

	c.mu.Lock()
	rec, ok := c.ledger[relayerID]
	if !ok {
		rec = &CreditRecord{ClientID: relayerID, FirstRelayAt: time.Now().UTC()}
		c.ledger[relayerID] = rec
	}
	rec.RelayedBytes += bytes
	rec.RelayCount++
	rec.LastRelayAt = time.Now().UTC()

	granted := units
	if c.maxPerPeriod > 0 && rec.CreditUnits+units > c.maxPerPeriod {
		// Tavan asildi: yalnizca tavana kadar ver, gerisini kaydet.
		if rec.CreditUnits >= c.maxPerPeriod {
			granted = 0
		} else {
			granted = c.maxPerPeriod - rec.CreditUnits
		}
		rec.CappedUnits += units - granted
	}
	rec.CreditUnits += granted
	snapshot := *rec
	c.mu.Unlock()

	if granted > 0 && c.bridge != nil {
		c.bridge.Publish(Event{
			ID:          fmt.Sprintf("credit-%s-%d", relayerID, snapshot.RelayCount),
			Type:        CreditEarned,
			ClientID:    relayerID,
			BytesOut:    bytes,
			CreditUnits: granted,
			OccurredAt:  time.Now().UTC(),
		})
	}
	return granted
}

// Balance, bir musterinin kredi durumunu dondurur.
func (c *CreditEngine) Balance(clientID string) (CreditRecord, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rec, ok := c.ledger[clientID]
	if !ok {
		return CreditRecord{}, false
	}
	return *rec, true
}

// All, tum kredi kayitlarini dondurur (fatura donemi kapanisinda).
func (c *CreditEngine) All() []CreditRecord {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CreditRecord, 0, len(c.ledger))
	for _, r := range c.ledger {
		out = append(out, *r)
	}
	return out
}

// ResetPeriod, donem sonunda kredileri sifirlar.
// Faturaya islenmis krediler burada temizlenir.
func (c *CreditEngine) ResetPeriod() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ledger = make(map[string]*CreditRecord)
}

// --- Kullanim esigi izleyici ---

// ThresholdWatcher, istemci kullanimi belirlenen esigi astiginda
// UsageThresholdExceeded olayi yayinlar.
//
// Her esik icin YALNIZCA BIR KEZ olay uretilir; aksi halde esigin
// ustundeki her istek yeni bir olay dogurur ve alici sistem boguluur.
type ThresholdWatcher struct {
	mu       sync.Mutex
	bridge   *Bridge
	tiers    []uint64          // artan sirali esik degerleri
	notified map[string]uint64 // clientID -> bildirilmis en yuksek esik
}

func NewThresholdWatcher(tiers []uint64, bridge *Bridge) *ThresholdWatcher {
	sorted := make([]uint64, len(tiers))
	copy(sorted, tiers)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return &ThresholdWatcher{
		bridge:   bridge,
		tiers:    sorted,
		notified: make(map[string]uint64),
	}
}

// Check, toplam kullanimi kontrol eder ve gerekirse olay yayinlar.
// Yayinlanan esigi dondurur (0 = yayin yok).
func (t *ThresholdWatcher) Check(clientID string, totalBytes uint64) uint64 {
	if len(t.tiers) == 0 || t.bridge == nil {
		return 0
	}

	t.mu.Lock()
	already := t.notified[clientID]
	var crossed uint64
	for _, tier := range t.tiers {
		if totalBytes >= tier && tier > already {
			crossed = tier
		}
	}
	if crossed > 0 {
		t.notified[clientID] = crossed
	}
	t.mu.Unlock()

	if crossed == 0 {
		return 0
	}

	t.bridge.Publish(Event{
		ID:             fmt.Sprintf("threshold-%s-%d", clientID, crossed),
		Type:           UsageThresholdExceeded,
		ClientID:       clientID,
		BytesIn:        totalBytes,
		ThresholdBytes: crossed,
		OccurredAt:     time.Now().UTC(),
	})
	return crossed
}

// Reset, donem sonunda bildirim gecmisini temizler.
func (t *ThresholdWatcher) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notified = make(map[string]uint64)
}

// Yardimci: context kullanimi icin (emitter arayuzu ile tutarlilik).
var _ = context.Background
