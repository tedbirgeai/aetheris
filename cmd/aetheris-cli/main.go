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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/tedbirgeai/aetheris/internal/billing/ledger"
	"github.com/tedbirgeai/aetheris/internal/router/mesh"
	"github.com/tedbirgeai/aetheris/internal/security"
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
  serve               Yerel mesh dugumu olarak calis
  version             Surum bilgisi

Ornekler:
  aetheris-cli keygen
  aetheris-cli route -links "A-B:10:ethernet,B-C:10:ethernet" -from A -to C
  aetheris-cli mesh-demo
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
