package driver

import (
	"context"
	"sync"
)

// StubDriver, gercek cip kutuphanesi olmadan HAL sozlesmesini test etmek ve
// mimari zeminin dogrulugunu kanitlamak icin kullanilan kontrollu bir
// surucudur. Gelecekte gercek BLE/SoftAP suruculeri ayni sozlesmeyi karsilar.
type StubDriver struct {
	caps      Capabilities
	available bool
	inbox     chan Frame
	mu        sync.Mutex
	sent      [][]byte
	opened    bool
	closed    bool
}

// NewStub, verilen yetenek ve mevcudiyetle bir stub surucu olusturur.
func NewStub(caps Capabilities, available bool) *StubDriver {
	return &StubDriver{
		caps:      caps,
		available: available,
		inbox:     make(chan Frame, 16),
	}
}

func (d *StubDriver) Capabilities() Capabilities { return d.caps }

func (d *StubDriver) Open(context.Context) error {
	if !d.available {
		return ErrNotAvailable
	}
	d.mu.Lock()
	d.opened = true
	d.mu.Unlock()
	return nil
}

func (d *StubDriver) Send(_ context.Context, _ string, frame []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.opened || d.closed {
		return ErrNotAvailable
	}
	cp := make([]byte, len(frame))
	copy(cp, frame)
	d.sent = append(d.sent, cp)
	return nil
}

func (d *StubDriver) Receive() <-chan Frame { return d.inbox }

func (d *StubDriver) Available() bool { return d.available }

func (d *StubDriver) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return nil
}

// Inject, test icin gelen bir cerceve enjekte eder.
func (d *StubDriver) Inject(src string, data []byte) {
	d.inbox <- Frame{Src: src, Data: data}
}

// SentCount, gonderilen cerceve sayisini dondurur.
func (d *StubDriver) SentCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}
