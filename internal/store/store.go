// Package store, tüketim defterinin kalıcılık katmanıdır.
//
// Store arayüzü, bellek içi ve PostgreSQL implementasyonları arasında
// şeffaf geçiş sağlar. Uygulamanın geri kalanı hangi implementasyonun
// kullanıldığını bilmez.
//
// FATURALAMA GÜVENCESİ: Record çağrısı hata dönerse, çağıran taraf isteği
// REDDETMELİDİR (fail-closed). Ölçülemeyen trafik faturalanamaz; sessizce
// hizmet vermek doğrudan gelir kaybıdır. Bu tercih, kullanılabilirliği
// veritabanına bağlar — alternatifi (dayanıklı kuyruk + asenkron yazma)
// README'de tartışılmıştır.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound, istemcinin defterde kaydı olmadığını belirtir.
var ErrNotFound = errors.New("store: istemci defteri bulunamadi")

// Usage, tek bir tünel geçişinin ölçüm kaydıdır.
type Usage struct {
	ClientID    string
	CarrierType string
	BytesIn     uint64
	BytesOut    uint64
	// PayloadSHA, opak yükün SHA-256 özetidir. Faturalama itirazlarında
	// hangi baytların ölçüldüğünü kanıtlar. Yükün kendisi saklanmaz.
	PayloadSHA string
	// Signature, makbuzun HMAC imzasıdır.
	Signature string
	// Destination, yönlendirme hedefi etiketi (boş olabilir).
	Destination string
	OccurredAt  time.Time
}

// Entry, tek bir istemcinin birikimli tüketimidir.
type Entry struct {
	BytesIn   uint64            `json:"bytes_in"`
	BytesOut  uint64            `json:"bytes_out"`
	Requests  uint64            `json:"requests"`
	ByCarrier map[string]uint64 `json:"by_carrier"`
	FirstSeen time.Time         `json:"first_seen"`
	LastSeen  time.Time         `json:"last_seen"`
}

// Clone, Entry'nin derin kopyasını döndürür. Faturalama verisinin
// çağıran tarafından değiştirilememesi için zorunludur.
func (e *Entry) Clone() *Entry {
	if e == nil {
		return nil
	}
	cp := &Entry{
		BytesIn:   e.BytesIn,
		BytesOut:  e.BytesOut,
		Requests:  e.Requests,
		ByCarrier: make(map[string]uint64, len(e.ByCarrier)),
		FirstSeen: e.FirstSeen,
		LastSeen:  e.LastSeen,
	}
	for k, v := range e.ByCarrier {
		cp.ByCarrier[k] = v
	}
	return cp
}

// Snapshot, tüm defterin tutarlı anlık görüntüsüdür.
type Snapshot struct {
	TotalBytesIn  uint64            `json:"total_bytes_in"`
	TotalBytesOut uint64            `json:"total_bytes_out"`
	TotalRequests uint64            `json:"total_requests"`
	Clients       map[string]*Entry `json:"clients"`
	GeneratedAt   time.Time         `json:"generated_at"`
}

// Store, tüketim defterinin kalıcılık sözleşmesidir.
type Store interface {
	// Record, tek bir tünel geçişini atomik olarak deftere işler.
	Record(ctx context.Context, u Usage) error

	// ClientUsage, tek bir istemcinin birikimli tüketimini döndürür.
	// Kayıt yoksa ErrNotFound döner.
	ClientUsage(ctx context.Context, clientID string) (*Entry, error)

	// Snapshot, tüm defterin anlık görüntüsünü döndürür.
	Snapshot(ctx context.Context) (Snapshot, error)

	// Kind, implementasyon adını döndürür ("memory" | "postgres").
	// Log ve health uçlarında hangi katmanın aktif olduğunu göstermek için.
	Kind() string

	// Close, altta yatan kaynakları serbest bırakır.
	Close() error
}
