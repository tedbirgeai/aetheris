// Package meter, ölçüm cephesidir (facade).
//
// Kalıcılık tamamen store.Store arayüzüne devredilmiştir; meter yalnızca
// süreç-yerel göstergeleri (açık tünel sayısı, çalışma süresi) tutar ve
// çağrıları store'a iletir. Bu ayrım sayesinde bellek ve PostgreSQL
// implementasyonları arasında geçiş, tek satırlık bir konfigürasyon
// değişikliğidir.
package meter

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/tedbirgeai/aetheris/internal/store"
)

type Meter struct {
	st            store.Store
	activeTunnels atomic.Int64
	startAt       time.Time
}

func New(st store.Store) *Meter {
	return &Meter{st: st, startAt: time.Now().UTC()}
}

// TunnelOpened / TunnelClosed, eşzamanlı açık tünel sayısını izler.
// Bu gösterge süreç-yereldir ve kalıcı değildir; anlık yük ölçüsüdür,
// faturalama verisi değildir.
func (m *Meter) TunnelOpened() { m.activeTunnels.Add(1) }
func (m *Meter) TunnelClosed() { m.activeTunnels.Add(-1) }

func (m *Meter) ActiveTunnels() int64 { return m.activeTunnels.Load() }

func (m *Meter) UptimeSeconds() int64 { return int64(time.Since(m.startAt).Seconds()) }

// Kind, aktif kalıcılık katmanının adını döndürür.
func (m *Meter) Kind() string { return m.st.Kind() }

// Record, geçişi deftere işler. Hata dönerse çağıran taraf isteği
// REDDETMELİDİR — ölçülemeyen trafik faturalanamaz.
func (m *Meter) Record(ctx context.Context, u store.Usage) error {
	return m.st.Record(ctx, u)
}

// ClientUsage, tek bir istemcinin defterini döndürür.
func (m *Meter) ClientUsage(ctx context.Context, clientID string) (*store.Entry, error) {
	return m.st.ClientUsage(ctx, clientID)
}

// Snapshot, tüm defterin anlık görüntüsünü döndürür.
func (m *Meter) Snapshot(ctx context.Context) (store.Snapshot, error) {
	return m.st.Snapshot(ctx)
}

func (m *Meter) Close() error { return m.st.Close() }
