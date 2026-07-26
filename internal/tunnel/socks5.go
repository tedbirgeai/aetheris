// Package tunnel — socks5.go
//
// RFC 1928 (SOCKS5) uyumlu yerel proxy sunucusu. İstemci (tarayıcı / uygulama)
// `CONNECT host:port` isteği gönderir; SOCKS5Server bunu AES-256-GCM şifreli
// relay üzerinden exit node'a (WAN çıkışı olan Aetheris düğümü) iletir.
//
// Akış:
//
//	İstemci → SOCKS5Server (localhost:1080)
//	           ↓  RFC 1928 el sıkışma
//	           → relay.ClientForwarder → exit node → WAN hedef
//
// Bu bileşen sayesinde uygulama katmanı hiçbir şey bilmeksizin WAN trafiği
// off-grid relay üzerinden çıkar.
package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
)

// SOCKS5 sabit baytları (RFC 1928 §3).
const (
	socks5Ver          byte = 0x05
	socks5AuthNone     byte = 0x00
	socks5AuthPass     byte = 0x02
	socks5AuthNoAccept byte = 0xFF
	socks5CmdConnect   byte = 0x01
	socks5AddrIPv4     byte = 0x01
	socks5AddrFQDN     byte = 0x03
	socks5AddrIPv6     byte = 0x04
	socks5RepOK        byte = 0x00
	socks5RepFail      byte = 0x01
	socks5RepNoConn    byte = 0x05
	socks5RepCmdNoSup  byte = 0x07
	socks5RepAddrNoSup byte = 0x08
)

// SOCKS5Dialer, hedef adrese TCP bağlantısı kuran işlevdir. Standart TCP
// yerine relay.ClientForwarder'ın Accept ettiği loopback'e bağlanmak için
// enjekte edilebilir.
type SOCKS5Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// SOCKS5Server, RFC 1928 SOCKS5 proxy sunucusudur.
type SOCKS5Server struct {
	listen string       // dinleme adresi (ör. "127.0.0.1:1080")
	dialer SOCKS5Dialer // hedef bağlantı kurucusu (nil = doğrudan TCP)
	auth   *socks5Auth  // nil = NoAuth
	logger *slog.Logger

	ln      net.Listener
	active  atomic.Int64
	handled atomic.Uint64
	closed  atomic.Bool
	once    sync.Once
}

type socks5Auth struct {
	user, pass string
}

// NewSOCKS5Server, bir SOCKS5 sunucusu oluşturur.
// dialer nil ise doğrudan TCP bağlantısı kullanılır.
// user/pass boşsa NoAuth modu (kimlik doğrulamasız).
func NewSOCKS5Server(listenAddr string, dialer SOCKS5Dialer, user, pass string, logger *slog.Logger) *SOCKS5Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &SOCKS5Server{listen: listenAddr, dialer: dialer, logger: logger}
	if user != "" {
		s.auth = &socks5Auth{user: user, pass: pass}
	}
	if dialer == nil {
		d := &net.Dialer{}
		s.dialer = d.DialContext
	}
	return s
}

// Listen, sunucuyu dinlemeye başlatır.
func (s *SOCKS5Server) Listen() error {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Addr, gerçek dinleme adresini döndürür.
func (s *SOCKS5Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Serve, gelen SOCKS5 bağlantılarını işler. ctx iptaline kadar bloklar.
func (s *SOCKS5Server) Serve(ctx context.Context) error {
	go func() { <-ctx.Done(); s.Close() }()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			return err
		}
		s.active.Add(1)
		go func(c net.Conn) {
			defer s.active.Add(-1)
			defer c.Close()
			if err := s.handle(ctx, c); err != nil {
				s.logger.Debug("socks5: bağlantı hatası", "err", err)
			}
			s.handled.Add(1)
		}(conn)
	}
}

// Close, sunucuyu kapatır.
func (s *SOCKS5Server) Close() {
	s.once.Do(func() {
		s.closed.Store(true)
		if s.ln != nil {
			_ = s.ln.Close()
		}
	})
}

// Stats, sunucu sayaçlarını döndürür.
type SOCKS5Stats struct {
	Active  int64
	Handled uint64
}

func (s *SOCKS5Server) Stats() SOCKS5Stats {
	return SOCKS5Stats{Active: s.active.Load(), Handled: s.handled.Load()}
}

// handle, tek bir SOCKS5 oturumunu işler.
func (s *SOCKS5Server) handle(ctx context.Context, c net.Conn) error {
	// Adım 1: Sürüm + yöntem müzakeresi.
	if err := s.negotiate(c); err != nil {
		return fmt.Errorf("socks5 müzakere: %w", err)
	}
	// Adım 2: İstek (hedef adres + komut).
	target, err := s.readRequest(c)
	if err != nil {
		return fmt.Errorf("socks5 istek: %w", err)
	}
	// Adım 3: Hedefe bağlan.
	dst, err := s.dialer(ctx, "tcp", target)
	if err != nil {
		_ = writeSOCKS5Reply(c, socks5RepNoConn, nil)
		return fmt.Errorf("hedef bağlantı (%s): %w", target, err)
	}
	defer dst.Close()
	// Adım 4: Başarı yanıtı yaz.
	if err := writeSOCKS5Reply(c, socks5RepOK, dst.LocalAddr()); err != nil {
		return err
	}
	// Adım 5: Çift yönlü kopyalama.
	relay(ctx, c, dst)
	return nil
}

// negotiate, RFC 1928 §3 yöntem müzakeresini yapar.
func (s *SOCKS5Server) negotiate(c net.Conn) error {
	// [VER=5][NMETHODS][METHODS...]
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != socks5Ver {
		return errors.New("geçersiz SOCKS sürümü")
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	// Yöntem seçimi.
	if s.auth == nil {
		// NoAuth: istemcide 0x00 varsa kabul.
		for _, m := range methods {
			if m == socks5AuthNone {
				_, err := c.Write([]byte{socks5Ver, socks5AuthNone})
				return err
			}
		}
		_, _ = c.Write([]byte{socks5Ver, socks5AuthNoAccept})
		return errors.New("istemci NoAuth desteklemiyor")
	}
	// UserPass (RFC 1929).
	for _, m := range methods {
		if m == socks5AuthPass {
			if _, err := c.Write([]byte{socks5Ver, socks5AuthPass}); err != nil {
				return err
			}
			return s.verifyUserPass(c)
		}
	}
	_, _ = c.Write([]byte{socks5Ver, socks5AuthNoAccept})
	return errors.New("ortak kimlik doğrulama yöntemi yok")
}

// verifyUserPass, RFC 1929 kullanıcı adı/parola doğrulaması yapar.
func (s *SOCKS5Server) verifyUserPass(c net.Conn) error {
	// [VER=1][ULEN][USER][PLEN][PASS]
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != 0x01 {
		return errors.New("geçersiz sub-negotiation sürümü")
	}
	user := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, user); err != nil {
		return err
	}
	plenBuf := make([]byte, 1)
	if _, err := io.ReadFull(c, plenBuf); err != nil {
		return err
	}
	pass := make([]byte, plenBuf[0])
	if _, err := io.ReadFull(c, pass); err != nil {
		return err
	}
	if string(user) != s.auth.user || string(pass) != s.auth.pass {
		_, _ = c.Write([]byte{0x01, 0x01}) // hata
		return errors.New("kimlik doğrulama başarısız")
	}
	_, err := c.Write([]byte{0x01, 0x00}) // başarı
	return err
}

// readRequest, RFC 1928 §4 isteğini okur ve hedef "host:port" döndürür.
func (s *SOCKS5Server) readRequest(c net.Conn) (string, error) {
	// [VER=5][CMD][RSV=0][ATYP][DST.ADDR][DST.PORT]
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return "", err
	}
	if hdr[0] != socks5Ver {
		return "", errors.New("geçersiz SOCKS sürümü")
	}
	if hdr[1] != socks5CmdConnect {
		_ = writeSOCKS5Reply(c, socks5RepCmdNoSup, nil)
		return "", fmt.Errorf("desteklenmeyen komut: 0x%02x", hdr[1])
	}
	// Adres.
	var host string
	switch hdr[3] {
	case socks5AddrIPv4:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case socks5AddrFQDN:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(c, lenBuf); err != nil {
			return "", err
		}
		name := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	case socks5AddrIPv6:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = "[" + net.IP(ip).String() + "]"
	default:
		_ = writeSOCKS5Reply(c, socks5RepAddrNoSup, nil)
		return "", fmt.Errorf("desteklenmeyen adres tipi: 0x%02x", hdr[3])
	}
	// Port (big-endian 2 bayt).
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(c, portBuf); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portBuf)
	return fmt.Sprintf("%s:%d", host, port), nil
}

// writeSOCKS5Reply, RFC 1928 §6 yanıtını yazar.
func writeSOCKS5Reply(c net.Conn, rep byte, boundAddr net.Addr) error {
	// [VER=5][REP][RSV=0][ATYP=1][0,0,0,0][PORT=0,0]
	resp := []byte{socks5Ver, rep, 0x00, socks5AddrIPv4, 0, 0, 0, 0, 0, 0}
	if boundAddr != nil {
		if tcp, ok := boundAddr.(*net.TCPAddr); ok && tcp != nil {
			if ip4 := tcp.IP.To4(); ip4 != nil {
				copy(resp[4:8], ip4)
				binary.BigEndian.PutUint16(resp[8:], uint16(tcp.Port))
			}
		}
	}
	_, err := c.Write(resp)
	return err
}

// relay, iki bağlantı arasında ctx iptaline kadar çift yönlü kopyalama yapar.
func relay(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		// Yazma yönünü kapat: karşı taraf EOF alır.
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	// İki kopya da bitene kadar veya ctx iptal olana kadar bekle.
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			_ = a.Close()
			_ = b.Close()
		case <-done:
		}
	}
}
