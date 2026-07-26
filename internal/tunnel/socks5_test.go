package tunnel

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// startEchoSrv, TCP echo sunucusu başlatır (SOCKS5 hedef olarak kullanılır).
func startEchoSrv(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) { defer conn.Close(); _, _ = io.Copy(conn, conn) }(c)
		}
	}()
	return ln.Addr().String()
}

// dialSOCKS5NoAuth, NoAuth SOCKS5 el sıkışması yapıp CONNECT isteği gönderir.
func dialSOCKS5NoAuth(t *testing.T, socksAddr, target string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	// Müzakere: VER=5, NMETHODS=1, METHOD=NoAuth.
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("müzakere yanıtı beklenmiyor: %v", resp)
	}
	// CONNECT isteği (FQDN).
	host, port := splitHostPort(t, target)
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	// Yanıt (10 bayt).
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0x00 {
		t.Fatalf("CONNECT yanıtı hatalı: 0x%02x", reply[1])
	}
	return c
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return host, port
}

// TestSOCKS5NoAuthConnect, SOCKS5 CONNECT + echo aracılığıyla veri bütünlüğü.
func TestSOCKS5NoAuthConnect(t *testing.T) {
	echoAddr := startEchoSrv(t)
	srv := NewSOCKS5Server("127.0.0.1:0", nil, "", "", nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)

	// IPv4 adresi için doğrudan CONNECT (0x01 = IPv4).
	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Müzakere.
	_, _ = c.Write([]byte{0x05, 0x01, 0x00})
	resp := make([]byte, 2)
	_, _ = io.ReadFull(c, resp)
	if resp[1] != 0x00 {
		t.Fatalf("NoAuth kabul edilmeli: %v", resp)
	}
	// IPv4 CONNECT.
	_, portStr, _ := net.SplitHostPort(echoAddr)
	var port int
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	ip := net.ParseIP("127.0.0.1").To4()
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, ip...)
	req = append(req, byte(port>>8), byte(port))
	_, _ = c.Write(req)
	reply := make([]byte, 10)
	_, _ = io.ReadFull(c, reply)
	if reply[1] != 0x00 {
		t.Fatalf("CONNECT başarısız: 0x%02x", reply[1])
	}

	payload := bytes.Repeat([]byte("SOCKS5 RFC1928 testi "), 200)
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()

	got := make([]byte, 0, len(payload))
	buf := make([]byte, 4096)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for len(got) < len(payload) {
		n, err := c.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("veri bütünlüğü bozuldu: gönderilen %d, gelen %d", len(payload), len(got))
	}
}

// TestSOCKS5UserPassAuth, kullanıcı adı/parola kimlik doğrulamasını test eder.
func TestSOCKS5UserPassAuth(t *testing.T) {
	echoAddr := startEchoSrv(t)
	srv := NewSOCKS5Server("127.0.0.1:0", nil, "aetheris", "gizli", nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)

	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Müzakere: VER=5, NMETHODS=1, METHOD=UserPass (0x02).
	_, _ = c.Write([]byte{0x05, 0x01, 0x02})
	resp := make([]byte, 2)
	_, _ = io.ReadFull(c, resp)
	if resp[1] != 0x02 {
		t.Fatalf("UserPass metodu seçilmeli: %v", resp)
	}
	// Kimlik bilgileri.
	user := "aetheris"
	pass := "gizli"
	auth := []byte{0x01, byte(len(user))}
	auth = append(auth, []byte(user)...)
	auth = append(auth, byte(len(pass)))
	auth = append(auth, []byte(pass)...)
	_, _ = c.Write(auth)
	authResp := make([]byte, 2)
	_, _ = io.ReadFull(c, authResp)
	if authResp[1] != 0x00 {
		t.Fatalf("kimlik doğrulama başarısız olmalıydı: %v", authResp)
	}
	// CONNECT.
	host, port := splitHostPort(t, echoAddr)
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, []byte(host)...)
	req = append(req, byte(port>>8), byte(port))
	_, _ = c.Write(req)
	reply := make([]byte, 10)
	_, _ = io.ReadFull(c, reply)
	if reply[1] != 0x00 {
		t.Fatalf("CONNECT başarısız: 0x%02x", reply[1])
	}
	// Küçük veri turu.
	msg := []byte("hello socks5 userpass")
	_, _ = c.Write(msg)
	got := make([]byte, len(msg))
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.ReadFull(c, got)
	if !bytes.Equal(got, msg) {
		t.Fatal("UserPass sonrası veri bütünlüğü bozuldu")
	}
}

// TestSOCKS5WrongVersion, yanlış SOCKS sürümünün reddedildiğini doğrular.
func TestSOCKS5WrongVersion(t *testing.T) {
	srv := NewSOCKS5Server("127.0.0.1:0", nil, "", "", nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)

	c, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// SOCKS4 sürümü gönder.
	_, _ = c.Write([]byte{0x04, 0x01, 0x00})
	// Bağlantı kapatılmalı (sürüm hatası).
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 16)
	n, _ := c.Read(buf)
	// Sunucu ya hata yanıtı ya da bağlantı kesme yapmalı.
	if n > 0 && buf[0] == 0x04 {
		t.Fatal("SOCKS4 kabul edilmemeli")
	}
}

// TestSOCKS5Stats, sunucu sayaçlarının doğruluğunu test eder.
func TestSOCKS5Stats(t *testing.T) {
	echoAddr := startEchoSrv(t)
	srv := NewSOCKS5Server("127.0.0.1:0", nil, "", "", nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)

	conn := dialSOCKS5NoAuth(t, srv.Addr(), echoAddr)
	_, _ = conn.Write([]byte("test"))
	time.Sleep(100 * time.Millisecond)
	conn.Close()
	time.Sleep(100 * time.Millisecond)

	st := srv.Stats()
	if st.Handled < 1 {
		t.Fatalf("en az 1 bağlantı işlenmeli: %+v", st)
	}
}

// TestSOCKS5CustomDialer, özel dialer enjeksiyonunu test eder (relay entegrasyonu
// için kullanılır: standart TCP yerine relay ClientForwarder'ın portuna bağlanır).
func TestSOCKS5CustomDialer(t *testing.T) {
	// "WAN hedef" olarak echo.
	echoAddr := startEchoSrv(t)
	// Özel dialer: tüm istekleri echo'ya yönlendir (relay simülasyonu).
	customDialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Gerçek relay'de burada relay.ClientForwarder.Addr() kullanılır.
		return (&net.Dialer{}).DialContext(ctx, network, echoAddr)
	}
	srv := NewSOCKS5Server("127.0.0.1:0", customDialer, "", "", nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)

	// "hedef" önemli değil — custom dialer echo'ya gönderir.
	conn := dialSOCKS5NoAuth(t, srv.Addr(), "example.com:80")
	defer conn.Close()
	msg := []byte("relay diyalog testi")
	_, _ = conn.Write(msg)
	got := make([]byte, len(msg))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.ReadFull(conn, got)
	if !bytes.Equal(got, msg) {
		t.Fatal("custom dialer üzerinden veri bütünlüğü bozuldu")
	}
}
