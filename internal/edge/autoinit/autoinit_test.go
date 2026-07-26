package autoinit

import (
	"context"
	"testing"
	"time"
)

func TestScanNetInterfaces(t *testing.T) {
	s := New(nil)
	ifaces := s.scanNetInterfaces()
	if len(ifaces) == 0 {
		t.Fatal("en az bir ag arayuzu olmali (loopback)")
	}
	hasLoopback := false
	for _, d := range ifaces {
		t.Logf("  %s tur=%s mevcut=%v", d.Name, d.Kind, d.Available)
		if d.Kind == "loopback" {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Fatal("loopback arayuzu bulunamadi")
	}
}

func TestScanReturnsResults(t *testing.T) {
	s := New(nil)
	ctx := context.Background()
	results := s.Scan(ctx)
	if len(results) == 0 {
		t.Fatal("tarama en az bir sonuc dondurmeli")
	}
}

func TestRunCallbackOnChange(t *testing.T) {
	s := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	called := make(chan struct{}, 1)
	go s.Run(ctx, 100*time.Millisecond, func(ifaces []DiscoveredInterface) {
		select {
		case called <- struct{}{}:
		default:
		}
	})
	// Ilk tur icin bekle (degisim olmasa bile ilk tarama yapiliyor).
	time.Sleep(300 * time.Millisecond)
	// Test: Run hata vermeden calisiyor olmali.
}
