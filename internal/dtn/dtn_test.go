package dtn

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// memCarrier, test için bellek tabanlı taşıyıcı.
type memCarrier struct {
	avail   atomic.Bool
	sent    atomic.Int64
	failFor int // bu kadar çağrı hata verir
	calls   atomic.Int64
}

func (c *memCarrier) Available() bool { return c.avail.Load() }
func (c *memCarrier) Send(_ context.Context, _ *Bundle) error {
	c.calls.Add(1)
	if int(c.calls.Load()) <= c.failFor {
		return fmt.Errorf("taşıyıcı geçici hata")
	}
	c.sent.Add(1)
	return nil
}

// TestStorePutGetDelete, temel CRUD ve kalıcılık.
func TestStorePutGetDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b := &Bundle{
		ID: "b1", Src: "A", Dst: "B", Priority: PriorityHigh,
		CreatedAt: time.Now(), Payload: []byte("veri"),
	}
	if err := s.Put(b); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("b1")
	if err != nil || string(got.Payload) != "veri" {
		t.Fatalf("Get başarısız: %v %v", got, err)
	}
	// Yeniden yükleme (disk kalıcılığı).
	s2, _ := NewStore(dir)
	if s2.Size() != 1 {
		t.Fatalf("disk kalıcılığı başarısız: %d", s2.Size())
	}
	if err := s.Delete("b1"); err != nil {
		t.Fatal(err)
	}
	if s.Size() != 0 {
		t.Fatal("silme sonrası depo boş olmalı")
	}
}

// TestStorePriorityOrder, yüksek öncelikli bundle'ların önce sıralandığını.
func TestStorePriorityOrder(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	bundles := []*Bundle{
		{ID: "low", Priority: PriorityLow, CreatedAt: time.Now(), Payload: []byte("d")},
		{ID: "high", Priority: PriorityHigh, CreatedAt: time.Now(), Payload: []byte("d")},
		{ID: "normal", Priority: PriorityNormal, CreatedAt: time.Now(), Payload: []byte("d")},
	}
	for _, b := range bundles {
		_ = s.Put(b)
	}
	pending := s.Pending()
	if len(pending) != 3 {
		t.Fatalf("3 bundle beklenir: %d", len(pending))
	}
	if pending[0].ID != "high" || pending[1].ID != "normal" || pending[2].ID != "low" {
		t.Fatalf("sıralama yanlış: %v %v %v", pending[0].ID, pending[1].ID, pending[2].ID)
	}
}

// TestStoreExpiry, süresi dolmuş bundle'ların Pending'den çıkarıldığını.
func TestStoreExpiry(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	_ = s.Put(&Bundle{ID: "expired", ExpiresAt: time.Now().Add(-time.Second), Payload: []byte("x")})
	_ = s.Put(&Bundle{ID: "fresh", ExpiresAt: time.Now().Add(time.Hour), Payload: []byte("y")})
	pending := s.Pending()
	if len(pending) != 1 || pending[0].ID != "fresh" {
		t.Fatalf("süresi dolmuş bundle Pending'de görünmemeli: %v", pending)
	}
}

// TestForwarderDeliversWhenCarrierAvailable, taşıyıcı mevcut olunca iletilir.
func TestForwarderDeliversWhenCarrierAvailable(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	car := &memCarrier{}
	car.avail.Store(true)

	delivered := make(chan string, 1)
	f := NewForwarder(s, []Carrier{car}, nil)
	f.OnDelivered = func(b *Bundle) { delivered <- b.ID }

	_ = s.Put(&Bundle{ID: "m1", Priority: PriorityNormal, CreatedAt: time.Now(), Payload: []byte("msg")})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go f.Run(ctx, 50*time.Millisecond)

	select {
	case id := <-delivered:
		if id != "m1" {
			t.Fatalf("yanlış bundle iletildi: %s", id)
		}
		if s.Size() != 0 {
			t.Fatal("teslimattan sonra depo boş olmalı")
		}
	case <-ctx.Done():
		t.Fatal("iletim zaman aşımına uğradı")
	}
}

// TestForwarderWaitsWhenNoCarrier, taşıyıcı yoksa bekler.
func TestForwarderWaitsWhenNoCarrier(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	car := &memCarrier{}
	car.avail.Store(false) // taşıyıcı yok

	f := NewForwarder(s, []Carrier{car}, nil)
	_ = s.Put(&Bundle{ID: "m2", Payload: []byte("msg")})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	go f.Run(ctx, 50*time.Millisecond)
	<-ctx.Done()

	// Taşıyıcı hiç mevcut olmadığından bundle hâlâ depoda olmalı.
	if s.Size() != 1 {
		t.Fatal("taşıyıcı yokken bundle iletilmemeli")
	}
}

// TestForwarderRetryOnFail, başarısız gönderimde yeniden dener.
func TestForwarderRetryOnFail(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	car := &memCarrier{failFor: 2} // ilk 2 çağrı başarısız
	car.avail.Store(true)

	delivered := make(chan struct{}, 1)
	f := NewForwarder(s, []Carrier{car}, nil)
	f.RetryAfter = 0 // test için bekleme yok
	f.OnDelivered = func(_ *Bundle) { delivered <- struct{}{} }
	_ = s.Put(&Bundle{ID: "m3", Payload: []byte("retry")})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go f.Run(ctx, 30*time.Millisecond)

	select {
	case <-delivered:
		if car.sent.Load() < 1 {
			t.Fatal("en az 1 başarılı gönderim olmalı")
		}
	case <-ctx.Done():
		t.Fatal("yeniden deneme sonucu iletim zaman aşımına uğradı")
	}
}

// TestForwarderMaxAttempts, max deneme aşılınca bundle silinir.
func TestForwarderMaxAttempts(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	car := &memCarrier{failFor: 999} // hep başarısız
	car.avail.Store(true)

	f := NewForwarder(s, []Carrier{car}, nil)
	f.MaxAttempts = 3
	f.RetryAfter = 0

	_ = s.Put(&Bundle{ID: "fail", Payload: []byte("x")})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go f.Run(ctx, 30*time.Millisecond)

	// Max deneme aşılınca depodan silinmeli.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Size() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("max deneme sonrası bundle silinmeli, hâlâ %d var", s.Size())
}
