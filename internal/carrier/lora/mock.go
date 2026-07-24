package lora

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// MockMedium, paylasilan RF ortamini (havayi) simule eder: bir dugumun
// yaydigi cerceveyi ortamdaki DIGER tum dugumler duyar (yayin). Gercek
// LoRa'da oldugu gibi kaynak, kendi yaydigini duymaz.
//
// Bu, fiziksel donanim olmadan cok-dugumlu mesh senaryolarini (MADDE 2/4)
// deterministik ve -race guvenli test etmeyi saglar.
type MockMedium struct {
	mu      sync.RWMutex
	drivers map[*MockLoRaDriver]struct{}

	// dropRate ve partition, ag bozulmasini simule etmek icindir.
	// partitioned[a][b] = true ise a'nin yaydigini b duymaz.
	partitioned map[byte]map[byte]bool
}

// NewMockMedium, bos bir paylasilan ortam olusturur.
func NewMockMedium() *MockMedium {
	return &MockMedium{
		drivers:     make(map[*MockLoRaDriver]struct{}),
		partitioned: make(map[byte]map[byte]bool),
	}
}

func (m *MockMedium) attach(d *MockLoRaDriver) {
	m.mu.Lock()
	m.drivers[d] = struct{}{}
	m.mu.Unlock()
}

func (m *MockMedium) detach(d *MockLoRaDriver) {
	m.mu.Lock()
	delete(m.drivers, d)
	m.mu.Unlock()
}

// Partition, src adresinden dst adresine iletimi keser (tek yonlu).
// Cift yonlu kesinti icin iki kez cagirin.
func (m *MockMedium) Partition(src, dst byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.partitioned[src] == nil {
		m.partitioned[src] = make(map[byte]bool)
	}
	m.partitioned[src][dst] = true
}

// Heal, src->dst kesintisini kaldirir.
func (m *MockMedium) Heal(src, dst byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.partitioned[src] != nil {
		delete(m.partitioned[src], dst)
	}
}

func (m *MockMedium) blocked(src, dst byte) bool {
	if m.partitioned[src] == nil {
		return false
	}
	return m.partitioned[src][dst]
}

// broadcast, src surucusunun yaydigi cerceveyi diger tum suruculere iletir.
func (m *MockMedium) broadcast(src *MockLoRaDriver, frame []byte) {
	m.mu.RLock()
	targets := make([]*MockLoRaDriver, 0, len(m.drivers))
	for d := range m.drivers {
		if d == src {
			continue // kaynak kendi yaydigini duymaz
		}
		if m.blocked(src.addr, d.addr) {
			continue // ag bolunmesi: bu dugum duymaz
		}
		targets = append(targets, d)
	}
	m.mu.RUnlock()

	for _, d := range targets {
		// Cerceve kopyasi gonder; alici, gonderenin tamponunu degistiremez.
		cp := make([]byte, len(frame))
		copy(cp, frame)
		d.deliver(cp)
	}
}

// MockLoRaDriver, fiziksel aygit yokken devreye giren simulasyon surucusudur.
// Driver arayuzunu uygular ve tum cerceveleri DEBUG seviyesinde loglar.
type MockLoRaDriver struct {
	addr   byte
	medium *MockMedium
	logger *slog.Logger

	inbox  chan []byte
	closed atomic.Bool
	done   chan struct{}

	sent atomic.Uint64
	rcvd atomic.Uint64
}

// NewMockDriver, verilen adres ve ortamla bir mock surucu olusturur.
// medium nil ise surucu LOOPBACK modunda calisir: gonderilen cerceve
// dogrudan kendi gelen kutusuna dusrer (tek dugumlu cerceveleme testleri icin).
func NewMockDriver(addr byte, medium *MockMedium, logger *slog.Logger) *MockLoRaDriver {
	if logger == nil {
		logger = slog.Default()
	}
	d := &MockLoRaDriver{
		addr:   addr,
		medium: medium,
		logger: logger,
		inbox:  make(chan []byte, 256),
		done:   make(chan struct{}),
	}
	if medium != nil {
		medium.attach(d)
	}
	return d
}

func (d *MockLoRaDriver) Name() string     { return "mock" }
func (d *MockLoRaDriver) IsHardware() bool { return false }
func (d *MockLoRaDriver) Addr() byte       { return d.addr }
func (d *MockLoRaDriver) Sent() uint64     { return d.sent.Load() }
func (d *MockLoRaDriver) Received() uint64 { return d.rcvd.Load() }

// deliver, ortamdan gelen bir cerceveyi gelen kutusuna koyar.
func (d *MockLoRaDriver) deliver(frame []byte) {
	if d.closed.Load() {
		return
	}
	select {
	case d.inbox <- frame:
		d.rcvd.Add(1)
	default:
		// Gelen kutusu dolu: gercek LoRa'da oldugu gibi cerceve DUSER.
		// Ust katman (gossip anti-entropy) kayip cerceveyi tolere eder.
		d.logger.Debug("mock lora: gelen kutusu dolu, cerceve dusuruldu", "addr", d.addr)
	}
}

func (d *MockLoRaDriver) Send(ctx context.Context, frame []byte) error {
	if d.closed.Load() {
		return ErrClosed
	}
	if len(frame) > MTU {
		return ErrFrameTooLarge
	}
	d.sent.Add(1)
	d.logger.Debug("mock lora TX", "addr", d.addr, "bytes", len(frame))

	if d.medium != nil {
		d.medium.broadcast(d, frame)
		return nil
	}
	// Loopback: kendi gelen kutusuna koy.
	cp := make([]byte, len(frame))
	copy(cp, frame)
	d.deliver(cp)
	return nil
}

func (d *MockLoRaDriver) Receive(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.done:
		return nil, ErrClosed
	case f := <-d.inbox:
		d.logger.Debug("mock lora RX", "addr", d.addr, "bytes", len(f))
		return f, nil
	}
}

func (d *MockLoRaDriver) Close() error {
	if d.closed.Swap(true) {
		return nil
	}
	close(d.done)
	if d.medium != nil {
		d.medium.detach(d)
	}
	return nil
}
