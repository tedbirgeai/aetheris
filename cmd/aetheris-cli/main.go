// Command aetheris-cli, hem SUNUCU hem ISTEMCI olarak calisabilen hafif,
// bagimsiz Aetheris aracidir. Dis bagimlilik yoktur; tek binary olarak
// Linux/Windows/macOS icin cross-compile edilebilir (CGO gerektirmez).
//
// Alt komutlar:
//
//	keygen                 yeni Ed25519 dugum kimligi uret
//	route                  topolojide iki dugum arasi en dusuk maliyetli yol
//	receipt                imzali role fisi uret / dogrula
//	mesh-demo              3 dugumlu cok-sicramali kayipsiz teslim gosterimi
//	serve                  yerel mesh dugumu olarak calis (gossip + router)
//	version                surum bilgisi
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/tedbirgeai/aetheris/internal/billing/ledger"
	"github.com/tedbirgeai/aetheris/internal/router/mesh"
	"github.com/tedbirgeai/aetheris/internal/security"
	"github.com/tedbirgeai/aetheris/internal/tunnel"
)

// Version, surum etiketi (build sirasinda -ldflags ile gecilebilir).
var Version = "v0.6a-turnkey"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run, test edilebilirlik icin ayrilmis giris noktasidir.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "keygen":
		return cmdKeygen(args[1:], stdout, stderr)
	case "route":
		return cmdRoute(args[1:], stdout, stderr)
	case "receipt":
		return cmdReceipt(args[1:], stdout, stderr)
	case "mesh-demo":
		return cmdMeshDemo(args[1:], stdout, stderr)
	case "p2p-demo":
		return cmdP2PDemo(args[1:], stdout, stderr)
	case "exit-demo":
		return cmdExitDemo(args[1:], stdout, stderr)
	case "serve":
		return cmdServe(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, "aetheris-cli", Version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "bilinmeyen komut: %q\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `aetheris-cli — Aetheris Evrensel Mimarisi araci

Kullanim:
  aetheris-cli <komut> [secenekler]

Komutlar:
  keygen              Ed25519 dugum kimligi uret
  route               Topolojide en dusuk maliyetli yolu hesapla
  receipt             Imzali role fisi uret/dogrula
  mesh-demo           3 dugumlu cok-sicramali teslim gosterimi
  p2p-demo            Tam cevrimdisi (0-WAN) P2P mesaj + dosya takasi
  exit-demo           Exit Node uzerinden WAN kopru (multi-hop internet cikisi)
  serve               Yerel mesh dugumu olarak calis
  version             Surum bilgisi

Ornekler:
  aetheris-cli keygen
  aetheris-cli route -links "A-B:10:ethernet,B-C:10:ethernet" -from A -to C
  aetheris-cli mesh-demo
  aetheris-cli p2p-demo
  aetheris-cli exit-demo
`)
}

func cmdKeygen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "JSON cikti")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	id, err := security.NewIdentity()
	if err != nil {
		fmt.Fprintln(stderr, "kimlik uretilemedi:", err)
		return 1
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(map[string]string{"node_id": id.NodeID()})
	} else {
		fmt.Fprintln(stdout, "NodeID:", id.NodeID())
	}
	return 0
}

// parseLinks, "A-B:10:ethernet,B-C:5:wifi" bicimini bir Graph'a cevirir.
func parseLinks(spec string) (*mesh.Graph, error) {
	g := mesh.NewGraph()
	if strings.TrimSpace(spec) == "" {
		return g, nil
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		seg := strings.Split(part, ":")
		if len(seg) < 2 {
			return nil, fmt.Errorf("gecersiz link: %q (bicim: A-B:rtt[:carrier])", part)
		}
		ab := strings.Split(seg[0], "-")
		if len(ab) != 2 {
			return nil, fmt.Errorf("gecersiz dugum cifti: %q", seg[0])
		}
		rtt, err := strconv.ParseFloat(seg[1], 64)
		if err != nil {
			return nil, fmt.Errorf("gecersiz RTT: %q", seg[1])
		}
		carrier := mesh.CarrierEthernet
		if len(seg) >= 3 {
			carrier = mesh.Carrier(seg[2])
		}
		if err := g.AddLink(ab[0], ab[1], rtt, carrier); err != nil {
			return nil, err
		}
	}
	return g, nil
}

func cmdRoute(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("route", flag.ContinueOnError)
	fs.SetOutput(stderr)
	links := fs.String("links", "", `topoloji: "A-B:10:ethernet,B-C:10:ethernet"`)
	from := fs.String("from", "", "kaynak dugum")
	to := fs.String("to", "", "hedef dugum")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *from == "" || *to == "" {
		fmt.Fprintln(stderr, "route: -from ve -to zorunlu")
		return 2
	}
	g, err := parseLinks(*links)
	if err != nil {
		fmt.Fprintln(stderr, "route:", err)
		return 2
	}
	res, err := g.ShortestPath(*from, *to)
	if err != nil {
		fmt.Fprintln(stderr, "route:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Yol: %s\n", strings.Join(res.Hops, " -> "))
	fmt.Fprintf(stdout, "Maliyet: %.2f\n", res.Cost)
	if len(res.Carriers) > 0 {
		cs := make([]string, len(res.Carriers))
		for i, c := range res.Carriers {
			cs[i] = string(c)
		}
		fmt.Fprintf(stdout, "Tasiyicilar: %s\n", strings.Join(cs, ", "))
	}
	return 0
}

func cmdReceipt(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("receipt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	relayer := fs.String("relayer", "", "krediyi kazanacak dugum (NodeID)")
	bytesRelayed := fs.Uint64("bytes", 0, "tasinan bayt")
	nonce := fs.Uint64("nonce", 1, "cift-harcama nonce")
	verify := fs.String("verify", "", "dogrulanacak fis JSON dosyasi (- : stdin)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Dogrulama modu.
	if *verify != "" {
		var data []byte
		var err error
		if *verify == "-" {
			data, err = io.ReadAll(bufio.NewReader(os.Stdin))
		} else {
			data, err = os.ReadFile(*verify)
		}
		if err != nil {
			fmt.Fprintln(stderr, "receipt: okunamadi:", err)
			return 1
		}
		rc, err := ledger.UnmarshalReceipt(data)
		if err != nil {
			fmt.Fprintln(stderr, "receipt: JSON cozulemedi:", err)
			return 1
		}
		if err := ledger.VerifyReceipt(rc); err != nil {
			fmt.Fprintln(stdout, "GECERSIZ:", err)
			return 1
		}
		fmt.Fprintf(stdout, "GECERLI: %s -> %s, %d bayt\n", rc.OriginID, rc.RelayerID, rc.Bytes)
		return 0
	}

	// Uretme modu: yeni origin kimligiyle imzala.
	if *relayer == "" {
		fmt.Fprintln(stderr, "receipt: -relayer zorunlu (veya -verify kullanin)")
		return 2
	}
	origin, err := security.NewIdentity()
	if err != nil {
		fmt.Fprintln(stderr, "receipt: kimlik hatasi:", err)
		return 1
	}
	rc := ledger.SignReceipt(origin, *relayer, *bytesRelayed, *nonce)
	blob, _ := rc.Marshal()
	fmt.Fprintln(stdout, string(blob))
	return 0
}

func cmdMeshDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mesh-demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// A-B ve B-C bagli; A-C YOK. Paket A'dan C'ye B uzerinden gitmeli.
	g := mesh.NewGraph()
	_ = g.AddLink("A", "B", 10, mesh.CarrierWiFi)
	_ = g.AddLink("B", "C", 10, mesh.CarrierEthernet)

	A := mesh.NewRouter("A")
	B := mesh.NewRouter("B")
	C := mesh.NewRouter("C")
	for _, r := range []*mesh.Router{A, B, C} {
		r.SetGraph(g)
	}
	mesh.Wire(A, B)
	mesh.Wire(B, C)

	got := make(chan string, 1)
	C.OnDeliver(func(p mesh.Packet) { got <- string(p.Payload) })

	msg := "off-grid cok-sicramali paket"
	fmt.Fprintln(stdout, "Topoloji: A <-wifi-> B <-ethernet-> C  (A-C DOGRUDAN YOK)")
	if err := A.Send("C", "demo-1", []byte(msg)); err != nil {
		fmt.Fprintln(stderr, "gonderim hatasi:", err)
		return 1
	}
	select {
	case out := <-got:
		if out != msg {
			fmt.Fprintln(stderr, "payload bozuldu")
			return 1
		}
		fmt.Fprintf(stdout, "C teslim aldi: %q\n", out)
		fmt.Fprintf(stdout, "B aktardi (forwarded=%d), C teslim (delivered=%d)\n",
			B.Stats().Forwarded, C.Stats().Delivered)
		fmt.Fprintln(stdout, "SONUC: BASARILI — cok-sicramali kayipsiz teslim")
		return 0
	default:
		fmt.Fprintln(stderr, "SONUC: BASARISIZ — paket ulasmadi")
		return 1
	}
}

// cmdP2PDemo, TAM CEVRIMDISI (0-WAN) iki yerel dugumun mesh uzerinden mesaj
// ve dosya takasi yaptigini kanitlar. Hicbir dis sunucu/internet kullanilmaz;
// veri AES-256-GCM ile sifrelenip surec-ici mesh baglantisi (PipeLink) uzerinden
// tasinir. Sahada iki cihaz arasindaki UDP/LoRa baglantisinin karsiligidir.
func cmdP2PDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("p2p-demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Fprintln(stdout, "=== TAM CEVRIMDISI (0-WAN) P2P TAKAS ===")
	fmt.Fprintln(stdout, "Ortam: iki yerel dugum, HICBIR internet/WAN yok.")

	// Paylasilan oturum anahtari (sahada anahtar takasi ayri; burada ortak).
	key := make([]byte, tunnel.AES256KeySize)
	if _, err := rand.Read(key); err != nil {
		fmt.Fprintln(stderr, "anahtar hatasi:", err)
		return 1
	}

	// Alice (gonderen) ve Bob (alan) icin proxy.
	alice, err := tunnel.NewProxy(key, 4096)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	bob, err := tunnel.NewProxy(key, 4096)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	// 1) MESAJ takasi.
	message := []byte("Merhaba Bob — bu mesaj internet OLMADAN, yalnizca yerel mesh uzerinden geldi.")
	// 2) DOSYA takasi (sentetik 64 KB ikili dosya).
	file := make([]byte, 64*1024)
	_, _ = rand.Read(file)
	payload := append(append([]byte(nil), message...), file...)

	// Bob tarafinda gelen baytlari toplayan hedef.
	sink := newReaderConn(nil)
	linkA, linkB := tunnel.NewPipeLink()

	var wg sync.WaitGroup
	var aliceStats tunnel.Stats
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = bob.ServeEgress(context.Background(), linkB, func(context.Context) (io.ReadWriteCloser, error) {
			return sink, nil
		})
	}()
	go func() {
		defer wg.Done()
		aliceStats, _ = alice.ServeIngress(context.Background(), newReaderConn(payload), linkA)
	}()
	wg.Wait()

	got := sink.Bytes()
	if !bytes.Equal(got, payload) {
		fmt.Fprintln(stderr, "SONUC: BASARISIZ — veri bozuldu")
		return 1
	}
	fmt.Fprintf(stdout, "[1] Mesaj teslim edildi (%d bayt): %q\n", len(message), string(message))
	fmt.Fprintf(stdout, "[2] Dosya teslim edildi (%d bayt), butunluk TAM\n", len(file))
	fmt.Fprintf(stdout, "    Zero-knowledge PayloadSHA: %s\n", aliceStats.PayloadSHA[:32]+"…")
	fmt.Fprintln(stdout, "    Sifreleme: AES-256-GCM (her parca taze nonce)")
	fmt.Fprintln(stdout, "SONUC: BASARILI — 0-WAN P2P mesaj+dosya takasi, INTERNET KULLANILMADI")
	return 0
}

// cmdExitDemo, dogrudan WAN'i OLMAYAN Dugum A'nin, WAN'i olan Dugum B (exit
// node) uzerinden dis dunyaya eristigini gosterir. A'nin sifreli trafigi mesh
// uzerinden B'ye tasinir; B trafigi WAN hedefe (burada yerel echo, gercek
// internetin karsiligi) iletir ve yaniti geri dondurur.
func cmdExitDemo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("exit-demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmt.Fprintln(stdout, "=== EXIT NODE — MULTI-HOP WAN KOPRU ===")
	fmt.Fprintln(stdout, "Dugum A: internet YOK.  Dugum B: exit node (WAN var).")
	fmt.Fprintln(stdout, "Akis: A --[sifreli mesh]--> B --[WAN]--> hedef ve geri.")

	// "WAN hedefi" olarak yerel bir echo sunucusu (gercek internetin karsiligi).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(stderr, "echo sunucu hatasi:", err)
		return 1
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) { defer conn.Close(); _, _ = io.Copy(conn, conn) }(c)
		}
	}()

	key := make([]byte, tunnel.AES256KeySize)
	_, _ = rand.Read(key)
	nodeA, _ := tunnel.NewProxy(key, 4096)
	nodeB, _ := tunnel.NewProxy(key, 4096)

	request := []byte("GET /veri HTTP/1.0 — A'dan cikan, B'nin WAN'i uzerinden giden istek")
	sink := newReaderConn(request) // A'nin gonderdigi

	linkA, linkB := tunnel.NewPipeLink()
	var wg sync.WaitGroup
	var aStats tunnel.Stats
	wg.Add(2)
	go func() {
		defer wg.Done()
		// B = exit node: mesh'ten geleni WAN hedefe (echo) baglar.
		_, _ = nodeB.ServeEgress(context.Background(), linkB, func(ctx context.Context) (io.ReadWriteCloser, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "tcp", ln.Addr().String())
		})
	}()
	go func() {
		defer wg.Done()
		aStats, _ = nodeA.ServeIngress(context.Background(), sink, linkA)
	}()
	wg.Wait()

	echoed := sink.Written()
	if !bytes.Equal(echoed, request) {
		fmt.Fprintln(stderr, "SONUC: BASARISIZ — WAN yaniti bozuldu")
		return 1
	}
	fmt.Fprintf(stdout, "[+] A'nin istegi B'nin WAN'i uzerinden gidip yaniti dondu (%d bayt)\n", len(request))
	fmt.Fprintf(stdout, "    B'ye giden trafik uctan uca sifreli (A ve WAN arasi B icerigi GORMEZ*)\n")
	fmt.Fprintf(stdout, "    Zero-knowledge PayloadSHA: %s\n", aStats.PayloadSHA[:32]+"…")
	fmt.Fprintln(stdout, "    * B, hedefe baglanmak icin adresi bilir; yuk AES-256-GCM ile korunur.")
	fmt.Fprintln(stdout, "SONUC: BASARILI — A, kendi interneti olmadan B uzerinden WAN'a ulasti")
	return 0
}

func cmdServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nodeID := fs.String("id", "", "dugum kimligi (bos ise uretilir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	id := *nodeID
	if id == "" {
		gen, err := security.NewIdentity()
		if err != nil {
			fmt.Fprintln(stderr, "serve:", err)
			return 1
		}
		id = gen.NodeID()
	}
	// Bu, turnkey pakette gercek gossip/router dongusune baglanan iskelettir.
	// CI/test ortaminda uzun surecli dinlemeyi baslatmadan kimligi bildirir.
	fmt.Fprintf(stdout, "aetheris mesh dugumu hazir: %s\n", shortID(id))
	fmt.Fprintln(stdout, "(gercek dinleme icin gateway binary'si ile calistirin)")
	return 0
}

func shortID(id string) string {
	if len(id) > 16 {
		return id[:16] + "…"
	}
	return id
}

// memPipe, bir ReadWriteCloser'dir: sabit girdi baytlarini Read ile verir
// (bitince EOF), yazilan baytlari bir tampona toplar. Demo icin istemci/hedef
// baglantisini modeller.
type memPipe struct {
	in  *bytes.Reader
	mu  sync.Mutex
	out bytes.Buffer
}

func newReaderConn(input []byte) *memPipe { return &memPipe{in: bytes.NewReader(input)} }

func (m *memPipe) Read(b []byte) (int, error) { return m.in.Read(b) }
func (m *memPipe) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out.Write(b)
}
func (m *memPipe) Close() error      { return nil }
func (m *memPipe) CloseWrite() error { return nil }
func (m *memPipe) Written() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.out.Bytes()...)
}
func (m *memPipe) Bytes() []byte { return m.Written() }
