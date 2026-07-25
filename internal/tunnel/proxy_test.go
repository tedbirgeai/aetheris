package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// echoPipe, yazilani ayni sekilde geri okutan bir loopback ReadWriteCloser'dir
// (surec-ici echo hedefi). CloseWrite ile okuma tarafina EOF verir.
type echoPipe struct {
	pr *io.PipeReader
	pw *io.PipeWriter
}

func newEchoPipe() *echoPipe {
	pr, pw := io.Pipe()
	return &echoPipe{pr: pr, pw: pw}
}

func (e *echoPipe) Read(b []byte) (int, error)  { return e.pr.Read(b) }
func (e *echoPipe) Write(b []byte) (int, error) { return e.pw.Write(b) }
func (e *echoPipe) CloseWrite() error           { return e.pw.Close() }
func (e *echoPipe) Close() error {
	_ = e.pw.Close()
	return e.pr.Close()
}

// memConn, istemci baglantisini modelleyen bir ReadWriteCloser'dir:
// sabit girdi baytlarini Read ile verir (bitince EOF), yanit baytlarini
// Write ile bir tampona toplar.
type memConn struct {
	in     *bytes.Reader
	mu     sync.Mutex
	out    bytes.Buffer
	closed bool
}

func newMemConn(input []byte) *memConn {
	return &memConn{in: bytes.NewReader(input)}
}

func (m *memConn) Read(b []byte) (int, error) { return m.in.Read(b) }
func (m *memConn) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out.Write(b)
}
func (m *memConn) Close() error { m.closed = true; return nil }
func (m *memConn) received() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.out.Bytes()...)
}

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, AES256KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestProxyEndToEndEcho, uctan uca bayt aktarim DOGRULUGUNU dogrular:
// istemciden gonderilen baytlar, mesh + AES-256-GCM uzerinden echo hedefe
// gidip ayni sekilde geri gelmelidir. Ayrica zero-knowledge PayloadSHA'nin
// gonderilen duz-metnin SHA-256'siyla eslestigini kontrol eder.
func TestProxyEndToEndEcho(t *testing.T) {
	key := testKey(t)
	ingress, err := NewProxy(key, 1024)
	if err != nil {
		t.Fatal(err)
	}
	egress, err := NewProxy(key, 1024)
	if err != nil {
		t.Fatal(err)
	}

	// Rastgele, chunk sinirini asan bir yuk.
	payload := make([]byte, 50_000)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	wantSHA := hex.EncodeToString(func() []byte { s := sha256.Sum256(payload); return s[:] }())

	ingressLink, egressLink := NewPipeLink()
	local := newMemConn(payload)
	target := newEchoPipe()

	ctx := context.Background()
	var (
		wg               sync.WaitGroup
		inStats, egStats Stats
		inErr, egErr     error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		egStats, egErr = egress.ServeEgress(ctx, egressLink, func(context.Context) (io.ReadWriteCloser, error) {
			return target, nil
		})
	}()
	go func() {
		defer wg.Done()
		inStats, inErr = ingress.ServeIngress(ctx, local, ingressLink)
	}()

	waitTimeout(t, &wg, 5*time.Second)

	if inErr != nil {
		t.Fatalf("ingress hata: %v", inErr)
	}
	if egErr != nil {
		t.Fatalf("egress hata: %v", egErr)
	}

	got := local.received()
	if !bytes.Equal(got, payload) {
		t.Fatalf("bayt aktarim bozuk: gonderilen %d bayt, geri gelen %d bayt, esit degil",
			len(payload), len(got))
	}

	// Zero-knowledge: PayloadSHA gonderilen duz-metnin ozeti olmali.
	if inStats.PayloadSHA != wantSHA {
		t.Fatalf("PayloadSHA uyusmuyor:\n istenen %s\n gelen   %s", wantSHA, inStats.PayloadSHA)
	}
	if inStats.BytesIn != uint64(len(payload)) {
		t.Fatalf("BytesIn %d olmali, %d", len(payload), inStats.BytesIn)
	}
	if inStats.BytesOut != uint64(len(payload)) {
		t.Fatalf("BytesOut %d olmali (echo), %d", len(payload), inStats.BytesOut)
	}
	_ = egStats
}

// TestProxyOverRealTCP, gercek bir TCP echo sunucusuna karsi egress'i
// dogrular (net.Dial ile). Uctan uca baytlar korunmali.
func TestProxyOverRealTCP(t *testing.T) {
	// Gercek TCP echo sunucusu.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn) // echo
			}(c)
		}
	}()

	key := testKey(t)
	ingress, _ := NewProxy(key, 4096)
	egress, _ := NewProxy(key, 4096)

	payload := []byte("AETHERIS canli tunel — gercek TCP echo testi, tekrarli veri bloklari...")
	payload = bytes.Repeat(payload, 200)

	ingressLink, egressLink := NewPipeLink()
	local := newMemConn(payload)

	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = egress.ServeEgress(ctx, egressLink, func(ctx context.Context) (io.ReadWriteCloser, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "tcp", ln.Addr().String())
		})
	}()
	var inStats Stats
	go func() {
		defer wg.Done()
		inStats, _ = ingress.ServeIngress(ctx, local, ingressLink)
	}()
	waitTimeout(t, &wg, 5*time.Second)

	if !bytes.Equal(local.received(), payload) {
		t.Fatalf("gercek TCP echo bayt aktarimi bozuk (%d vs %d bayt)",
			len(local.received()), len(payload))
	}
	if inStats.BytesIn != uint64(len(payload)) {
		t.Fatalf("BytesIn beklenen %d, gelen %d", len(payload), inStats.BytesIn)
	}
}

// TestProxyNoGoroutineLeak, akis bittiginde arka plan goroutine'lerinin
// tamamen sonlandigini (bellek/goroutine sizintisi yok) dogrular.
func TestProxyNoGoroutineLeak(t *testing.T) {
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	key := testKey(t)
	for i := 0; i < 25; i++ {
		ingress, _ := NewProxy(key, 512)
		egress, _ := NewProxy(key, 512)
		ingressLink, egressLink := NewPipeLink()
		local := newMemConn(bytes.Repeat([]byte("x"), 5000))
		target := newEchoPipe()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = egress.ServeEgress(context.Background(), egressLink, func(context.Context) (io.ReadWriteCloser, error) {
				return target, nil
			})
		}()
		go func() {
			defer wg.Done()
			_, _ = ingress.ServeIngress(context.Background(), local, ingressLink)
		}()
		waitTimeout(t, &wg, 5*time.Second)
	}

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()
	// Kucuk bir tolerans (test kosucusu goroutine'leri). Sizinti olsaydi
	// 25 iterasyon * 3+ goroutine birikirdi.
	if after > before+5 {
		t.Fatalf("goroutine sizintisi supheli: once %d, sonra %d", before, after)
	}
}

// TestCipherRoundTrip, AES-256-GCM Seal/Open dogrulugunu ve nonce
// tazeligini kontrol eder.
func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("gizli yuk")
	a, _ := c.Seal(msg)
	b, _ := c.Seal(msg)
	if bytes.Equal(a, b) {
		t.Fatal("ayni mesaj icin nonce tekrar kullanilmis (Seal ciktilari ozdes)")
	}
	got, err := c.Open(a)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("Open sonucu ozgun mesajla eslesmiyor")
	}
	// Bozuk sifreli metin reddedilmeli (GCM butunluk).
	a[len(a)-1] ^= 0xFF
	if _, err := c.Open(a); err == nil {
		t.Fatal("bozulmus sifreli metin kabul edilmemeliydi")
	}
}

func TestCipherKeySize(t *testing.T) {
	if _, err := NewCipher(make([]byte, 16)); err != ErrKeySize {
		t.Fatalf("16 baytlik anahtar reddedilmeliydi, %v", err)
	}
	if _, err := NewProxy(make([]byte, 31), 0); err != ErrKeySize {
		t.Fatalf("31 baytlik anahtar reddedilmeliydi, %v", err)
	}
}

// TestPacketMode, UDP datagram modunda tek-parca sifrele/coz dogrulugu.
func TestPacketMode(t *testing.T) {
	key := testKey(t)
	client, _ := NewProxy(key, 0)
	server, _ := NewProxy(key, 0)

	datagram := []byte("tek UDP datagrami — sinir korunmali")
	sealed, err := client.SealPacket(datagram)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := server.OpenPacket(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, datagram) {
		t.Fatal("UDP datagram round-trip bozuk")
	}
}

func waitTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("zaman asimi: akis sonlanmadi (olasi kilitlenme/sizinti)")
	}
}
