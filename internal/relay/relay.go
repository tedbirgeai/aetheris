// Package relay, tunnel.Proxy motorunu GERCEK AG uzerine tasir: WAN erisimi
// olmayan bir istemci dugumun (A), WAN'i olan bir exit node (B) uzerinden
// dis dunyaya SIFIR-KONFIGURASYONLA cikmasini saglar.
//
// Mimarî:
//
//	A (client)                        B (exit node, WAN var)
//	┌───────────────────┐   TCP link   ┌───────────────────┐
//	│ ClientForwarder   │═════════════►│ ExitServer        │   WAN
//	│ yerel dinleyici   │  [hedef|veri]│ hedefe net.Dial   │ ─────► Internet
//	│ tunnel.ServeIngress│  AES-256-GCM │ tunnel.ServeEgress│ ◄─────
//	└───────────────────┘              └───────────────────┘
//
// Yerel baglantidan gelen ham baytlar A'da sifrelenir, TCP link uzerinden
// B'ye tasinir, B cozup gercek hedefe iletir. Zero-knowledge: B yalnizca
// hedef adresini bilir, yukun icerigini degil.
package relay

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tedbirgeai/aetheris/internal/tunnel"
)

var (
	ErrTargetTooLong = errors.New("relay: hedef adresi cok uzun")
	ErrClosed        = errors.New("relay: kapali")
)

// netLink, bir TCP baglantisini tunnel.Link'e uyarlar: her cerceve 4-bayt
// uzunluk oneki ile yazilir/okunur.
type netLink struct {
	conn   net.Conn
	br     *bufio.Reader
	wmu    sync.Mutex
	closed atomic.Bool
}

// NewNetLink, bir net.Conn'u tunnel.Link'e cevirir.
func NewNetLink(conn net.Conn) tunnel.Link {
	return &netLink{conn: conn, br: bufio.NewReader(conn)}
}

func (l *netLink) Send(frame []byte) error {
	if l.closed.Load() {
		return ErrClosed
	}
	l.wmu.Lock()
	defer l.wmu.Unlock()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	if _, err := l.conn.Write(hdr[:]); err != nil {
		return err
	}
	_, err := l.conn.Write(frame)
	return err
}

func (l *netLink) Recv() ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(l.br, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 1<<24 { // 16 MB guvenlik siniri
		return nil, errors.New("relay: cerceve cok buyuk")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(l.br, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (l *netLink) Close() error {
	l.closed.Store(true)
	return l.conn.Close()
}

// --- Exit tarafi (B): mesh'ten geleni WAN'a baglar ---

// ExitServer, exit node'un relay dinleyicisidir. Her baglantida once hedef
// adresi okunur, sonra tunnel.ServeEgress ile hedefe (gercek WAN) baglanilir.
type ExitServer struct {
	proxy  *tunnel.Proxy
	logger *slog.Logger
	ln     net.Listener

	served atomic.Uint64 // toplam servis edilen baglanti
	active atomic.Int64  // aktif tunel sayisi
	bytes  atomic.Uint64 // WAN'a iletilen bayt
}

// NewExitServer, verilen AES-256 anahtariyla bir exit sunucusu olusturur.
func NewExitServer(key []byte, logger *slog.Logger) (*ExitServer, error) {
	p, err := tunnel.NewProxy(key, 0)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ExitServer{proxy: p, logger: logger}, nil
}

// Listen, exit sunucusunu verilen adreste dinlemeye baslar (ornek ":9800").
func (s *ExitServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Addr, dinlenen gercek adresi dondurur.
func (s *ExitServer) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Serve, gelen relay baglantilarini isler. ctx iptaline kadar bloklar.
func (s *ExitServer) Serve(ctx context.Context) error {
	go func() { <-ctx.Done(); _ = s.ln.Close() }()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *ExitServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// Ilk cerceve: hedef adresi (duz, sifresiz — B'nin bilmesi gereken tek sey).
	target, err := readLenPrefixed(conn)
	if err != nil {
		s.logger.Warn("relay: hedef okunamadi", "err", err)
		return
	}
	s.served.Add(1)
	s.active.Add(1)
	defer s.active.Add(-1)

	link := NewNetLink(conn)
	st, err := s.proxy.ServeEgress(ctx, link, func(dctx context.Context) (io.ReadWriteCloser, error) {
		d := net.Dialer{Timeout: 10 * time.Second}
		return d.DialContext(dctx, "tcp", string(target))
	})
	if err != nil {
		s.logger.Debug("relay: egress bitti", "hedef", string(target), "err", err)
	}
	s.bytes.Add(st.BytesIn)
}

// Stats, exit sunucusunun gozlem sayaclari.
type ExitStats struct {
	Served uint64
	Active int64
	Bytes  uint64
}

func (s *ExitServer) Stats() ExitStats {
	return ExitStats{Served: s.served.Load(), Active: s.active.Load(), Bytes: s.bytes.Load()}
}

// --- Istemci tarafi (A): yerel baglantiyi exit uzerinden disari firlatir ---

// ClientForwarder, WAN'i olmayan dugumdeki yerel yonlendiricidir. Yerel bir
// TCP dinleyici acar; gelen her baglantiyi exit node'a tuneller ve hedefe
// (WAN) exit uzerinden ulastirir.
type ClientForwarder struct {
	proxy    *tunnel.Proxy
	exitAddr string // exit node'un relay adresi (otomatik kesiften gelir)
	target   string // yonlendirilecek WAN hedefi
	logger   *slog.Logger
	ln       net.Listener

	active atomic.Int64
	bytes  atomic.Uint64
}

// NewClientForwarder, exit adresine ve sabit bir WAN hedefine yonlendiren bir
// istemci olusturur. (Gercek SOCKS yerine sabit hedef; test/kanit icin yeterli
// ve deterministik.)
func NewClientForwarder(key []byte, exitAddr, target string, logger *slog.Logger) (*ClientForwarder, error) {
	p, err := tunnel.NewProxy(key, 0)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ClientForwarder{proxy: p, exitAddr: exitAddr, target: target, logger: logger}, nil
}

// Listen, yerel yonlendirici dinleyicisini acar (ornek "127.0.0.1:0").
func (c *ClientForwarder) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	c.ln = ln
	return nil
}

// Addr, yerel dinleme adresini dondurur.
func (c *ClientForwarder) Addr() string {
	if c.ln == nil {
		return ""
	}
	return c.ln.Addr().String()
}

// Serve, yerel baglantilari exit uzerinden yonlendirir.
func (c *ClientForwarder) Serve(ctx context.Context) error {
	go func() { <-ctx.Done(); _ = c.ln.Close() }()
	for {
		local, err := c.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go c.handle(ctx, local)
	}
}

func (c *ClientForwarder) handle(ctx context.Context, local net.Conn) {
	defer local.Close()
	// Exit node'a baglan.
	d := net.Dialer{Timeout: 10 * time.Second}
	exitConn, err := d.DialContext(ctx, "tcp", c.exitAddr)
	if err != nil {
		c.logger.Warn("relay: exit'e baglanilamadi", "exit", c.exitAddr, "err", err)
		return
	}
	defer exitConn.Close()

	// Ilk cerceve: hedef adresi (exit bunu okuyup WAN'a baglanacak).
	if err := writeLenPrefixed(exitConn, []byte(c.target)); err != nil {
		c.logger.Warn("relay: hedef gonderilemedi", "err", err)
		return
	}

	c.active.Add(1)
	defer c.active.Add(-1)

	link := NewNetLink(exitConn)
	st, err := c.proxy.ServeIngress(ctx, local, link)
	if err != nil {
		c.logger.Debug("relay: ingress bitti", "err", err)
	}
	c.bytes.Add(st.BytesIn)
}

// Stats, istemci yonlendiricisinin sayaclari.
type ClientStats struct {
	Active int64
	Bytes  uint64
}

func (c *ClientForwarder) Stats() ClientStats {
	return ClientStats{Active: c.active.Load(), Bytes: c.bytes.Load()}
}

// --- Yardimcilar: uzunluk-onekli cerceve (hedef adresi icin) ---

func writeLenPrefixed(w io.Writer, b []byte) error {
	if len(b) > 0xFFFF {
		return ErrTargetTooLong
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readLenPrefixed(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// DeriveKey, ortak bir sir'dan (ornegin yapilandirma) deterministik 32-bayt
// AES-256 anahtari turetir. Gercek dagitimda anahtar takasi ayri; burada
// paylasilan sir yeterlidir.
func DeriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte("aetheris-relay-v1:" + secret))
	return sum[:]
}
