package relay

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// startEcho, WAN hedefinin karsiligi olan yerel bir TCP echo sunucusu baslatir.
func startEcho(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) { defer conn.Close(); _, _ = io.Copy(conn, conn) }(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// TestRelayEndToEnd, KABUL KRITERI cekirdegi: WAN'i olmayan istemci (A),
// exit node (B) uzerinden "WAN" hedefe (echo) baglanir ve baytlar kayipsiz
// gidip gelir. Gercek TCP baglantilari kullanilir.
func TestRelayEndToEnd(t *testing.T) {
	wanAddr, stopEcho := startEcho(t)
	defer stopEcho()

	key := DeriveKey("ortak-sir")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// B: exit node.
	exit, err := NewExitServer(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := exit.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go exit.Serve(ctx)

	// A: istemci yonlendirici — exit uzerinden WAN echo'ya.
	client, err := NewClientForwarder(key, exit.Addr(), wanAddr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go client.Serve(ctx)

	// Yerel uygulama A'nin yonlendiricisine baglanir (sanki dogrudan internete
	// cikiyormus gibi), ama trafik B'nin WAN'i uzerinden gider.
	app, err := net.Dial("tcp", client.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	payload := bytes.Repeat([]byte("AETHERIS otomatik exit yonlendirme testi — "), 300)
	if _, err := app.Write(payload); err != nil {
		t.Fatal(err)
	}
	// Yazma yonunu kapat ki echo tam donsun.
	if cw, ok := app.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	got := make([]byte, 0, len(payload))
	br := bufio.NewReader(app)
	_ = app.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	for len(got) < len(payload) {
		n, err := br.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			break
		}
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("bayt aktarim bozuk: gonderilen %d, gelen %d", len(payload), len(got))
	}
	if exit.Stats().Served < 1 {
		t.Fatalf("exit en az 1 baglanti servis etmeliydi, %d", exit.Stats().Served)
	}
}

// TestNetLinkFraming, netLink cerceve yaz/oku dogrulugunu net.Pipe uzerinde
// dogrular.
func TestNetLinkFraming(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	la := NewNetLink(a)
	lb := NewNetLink(b)

	frames := [][]byte{[]byte("bir"), bytes.Repeat([]byte("x"), 5000), []byte("")}
	go func() {
		for _, f := range frames {
			_ = la.Send(f)
		}
	}()
	for _, want := range frames {
		got, err := lb.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("cerceve uyusmuyor: %d vs %d bayt", len(got), len(want))
		}
	}
}

func TestDeriveKeyDeterministic(t *testing.T) {
	k1 := DeriveKey("secret")
	k2 := DeriveKey("secret")
	k3 := DeriveKey("baska")
	if len(k1) != 32 {
		t.Fatalf("anahtar 32 bayt olmali, %d", len(k1))
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("ayni sir ayni anahtari uretmeli")
	}
	if bytes.Equal(k1, k3) {
		t.Fatal("farkli sir farkli anahtar uretmeli")
	}
}
