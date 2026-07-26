// Package dtn, Gecikme-Toleranslı Ağ (Delay-Tolerant Networking) motoru
// sağlar. Off-grid sahadaki düğümler doğrudan bağlantı olmadan veri paketlerini
// kalıcı kuyrukta saklar; taşıyıcı (LoRa, BLE, araç DTN) mevcut olduğunda
// toplu gönderir.
//
// RFC 4838 (DTN Mimarisi) ruhuna uygun hafif uygulama:
//
//   - Bundle: self-contained, öncelikli, süresi dolan veri birimi
//   - Store: disk-destekli kalıcı kuyruk (WAL dosyası)
//   - Forwarder: taşıyıcı mevcut olduğunda gönderir, hata olunca yeniden dener
//   - Custodian: teslimat ACK alınınca bundle'ı siler
package dtn

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Priority, bundle önceliği.
type Priority int

const (
	PriorityLow    Priority = 0
	PriorityNormal Priority = 1
	PriorityHigh   Priority = 2 // örn. acil durum mesajları
)

// Bundle, DTN veri birimidir.
type Bundle struct {
	ID          string    `json:"id"`
	Src         string    `json:"src"`
	Dst         string    `json:"dst"`
	Priority    Priority  `json:"priority"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Payload     []byte    `json:"payload"`
	Attempts    int       `json:"attempts"`
	LastAttempt time.Time `json:"last_attempt"`
}

// Expired, bundle'ın süresinin dolup dolmadığını kontrol eder.
func (b *Bundle) Expired() bool {
	return !b.ExpiresAt.IsZero() && time.Now().After(b.ExpiresAt)
}

// Carrier, bir veri taşıyıcısının gönderim arayüzüdür.
// Gerçek LoRa, BLE veya araç DTN burada implement edilir.
type Carrier interface {
	// Available, taşıyıcının o an kullanılabilir olduğunu bildirir.
	Available() bool
	// Send, tek bir bundle'ı iletir.
	Send(ctx context.Context, b *Bundle) error
}

// Store, kalıcı DTN bundle deposudur. Disk tabanlı JSON WAL kullanır.
type Store struct {
	mu      sync.Mutex
	bundles map[string]*Bundle
	dir     string
}

var ErrNotFound = errors.New("dtn: bundle bulunamadı")

// NewStore, verilen dizinde kalıcı bir DTN deposu oluşturur.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, bundles: make(map[string]*Bundle)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Put, bir bundle'ı depoya ekler (veya günceller) ve diske yazar.
func (s *Store) Put(b *Bundle) error {
	s.mu.Lock()
	s.bundles[b.ID] = b
	s.mu.Unlock()
	return s.persist(b)
}

// Get, ID ile bir bundle getirir.
func (s *Store) Get(id string) (*Bundle, error) {
	s.mu.Lock()
	b, ok := s.bundles[id]
	s.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

// Delete, bir bundle'ı depodan ve diskten siler (ACK sonrası).
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	delete(s.bundles, id)
	s.mu.Unlock()
	path := filepath.Join(s.dir, id+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Pending, süresi dolmamış ve önceliğe göre sıralı bekleyen bundle'ları
// döndürür.
func (s *Store) Pending() []*Bundle {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Bundle
	for _, b := range s.bundles {
		if !b.Expired() {
			out = append(out, b)
		}
	}
	// Öncelik (yüksek önce), sonra oluşturma zamanı (eskisi önce).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// Size, depodaki bundle sayısını döndürür.
func (s *Store) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bundles)
}

func (s *Store) persist(b *Bundle) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, b.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomik
}

func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var b Bundle
		if err := json.Unmarshal(data, &b); err != nil {
			continue
		}
		if !b.Expired() {
			s.bundles[b.ID] = &b
		} else {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
		}
	}
	return nil
}

// Forwarder, taşıyıcı mevcut olduğunda depolanan bundle'ları iletir.
type Forwarder struct {
	store    *Store
	carriers []Carrier
	logger   *slog.Logger
	// MaxAttempts, bir bundle'ın kaç kez deneneceği. 0 = sınırsız.
	MaxAttempts int
	// RetryAfter, başarısız denemeler arasındaki bekleme süresi.
	RetryAfter time.Duration
	// OnDelivered, başarılı teslimat sonrası çağrılır.
	OnDelivered func(*Bundle)

	stop chan struct{}
	once sync.Once
}

// NewForwarder, bir DTN forwarder oluşturur.
func NewForwarder(store *Store, carriers []Carrier, logger *slog.Logger) *Forwarder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Forwarder{
		store:       store,
		carriers:    carriers,
		logger:      logger,
		MaxAttempts: 10,
		RetryAfter:  30 * time.Second,
		stop:        make(chan struct{}),
	}
}

// Run, taşıyıcı mevcut olduğunda gönderim döngüsünü başlatır.
// ctx iptaline veya Close()'a kadar bloklar.
func (f *Forwarder) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	f.logger.Info("DTN forwarder aktif",
		"taşıyıcı_sayısı", len(f.carriers),
		"interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.stop:
			return
		case <-ticker.C:
			f.tryForward(ctx)
		}
	}
}

// Close, forwarder'ı durdurur.
func (f *Forwarder) Close() {
	f.once.Do(func() { close(f.stop) })
}

func (f *Forwarder) tryForward(ctx context.Context) {
	car := f.availableCarrier()
	if car == nil {
		f.logger.Debug("DTN: taşıyıcı mevcut değil, bekleniyor")
		return
	}
	pending := f.store.Pending()
	if len(pending) == 0 {
		return
	}
	f.logger.Info("DTN gönderim turu", "bekleyen", len(pending))
	for _, b := range pending {
		if f.MaxAttempts > 0 && b.Attempts >= f.MaxAttempts {
			f.logger.Warn("DTN: max deneme aşıldı, bundle siliniyor", "id", b.ID)
			_ = f.store.Delete(b.ID)
			continue
		}
		if !b.LastAttempt.IsZero() && time.Since(b.LastAttempt) < f.RetryAfter {
			continue // henüz yeniden deneme zamanı değil
		}
		if err := car.Send(ctx, b); err != nil {
			b.Attempts++
			b.LastAttempt = time.Now()
			_ = f.store.Put(b) // güncelle
			f.logger.Warn("DTN: gönderim başarısız", "id", b.ID, "deneme", b.Attempts, "err", err)
			continue
		}
		f.logger.Info("DTN: bundle iletildi", "id", b.ID, "dst", b.Dst)
		_ = f.store.Delete(b.ID)
		if f.OnDelivered != nil {
			f.OnDelivered(b)
		}
	}
}

func (f *Forwarder) availableCarrier() Carrier {
	for _, c := range f.carriers {
		if c.Available() {
			return c
		}
	}
	return nil
}
