// Command mobiletest, gecidin telefondan test edilmesini kolaylastirir.
//
// # SORUN
//
// Gelistirici telefonundan gecidi test etmek isteyince su adimlari elle
// yapmak zorunda kalir: yerel IP'yi bul, Docker port yonlendirmesini
// dogrula, guvenlik duvarini ac, URL'yi telefona yaz. Her adimda hata
// olabilir ve hata mesajlari yaniltici olur ("baglanamiyor" - neden?).
//
// # BU ARAC NE YAPAR
//
//  1. Makinenin LAN IP adres(ler)ini otomatik bulur (Windows/Linux/macOS).
//  2. Gecidin gercekten dinledigini localhost uzerinden dogrular.
//  3. LAN IP uzerinden de erisilebildigini dogrular — erisilemiyorsa
//     SEBEBINI soyler (guvenlik duvari mi, Docker port mapping mi).
//  4. Telefonla taranabilir QR kodu terminale basar.
//
// # NEDEN GO, NEDEN BASH DEGIL
//
// Bash betigi Windows'ta calismaz (ip/ifconfig yok, Git Bash sinirli).
// Go ile yazilinca ayni kod uc platformda da calisir ve gelistirici
// zaten Go kurulu.
//
// # TUNEL HAKKINDA
//
// Varsayilan olarak SADECE LAN uzerinden erisim onerilir: telefon ve
// bilgisayar ayni WiFi'da olmalidir. Bu guvenlidir.
//
// --tunnel bayragi public bir tunel acar (cloudflared gerekir) ve gecidi
// TUM INTERNETE acar. Gecidinizde gecerli API anahtarlari vardir; tunel
// URL'i sizarsa herkes istek atabilir. Bu yuzden opt-in'dir ve acik
// uyari basar.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func main() {
	var (
		port    = flag.Int("port", 8080, "gecidin dinledigi port")
		path    = flag.String("path", "/healthz", "test edilecek yol")
		tunnel  = flag.Bool("tunnel", false, "public tunel ac (INTERNETE ACAR - dikkat)")
		noQR    = flag.Bool("no-qr", false, "QR kodu basma, sadece URL yaz")
		timeout = flag.Duration("timeout", 3*time.Second, "erisim kontrolu zaman asimi")
	)
	flag.Parse()

	fmt.Println()
	fmt.Println("  AETHERIS MOBIL SAHA TESTI")
	fmt.Println("  " + strings.Repeat("=", 46))
	fmt.Println()

	// --- 1. Gecit localhost'ta ayakta mi? ---
	localURL := fmt.Sprintf("http://127.0.0.1:%d%s", *port, *path)
	fmt.Printf("  [1/4] Gecit yerelde calisiyor mu?  %s\n", localURL)

	if err := probe(localURL, *timeout); err != nil {
		fail("Gecide localhost uzerinden ULASILAMIYOR.", []string{
			"Gecit calisiyor mu?  docker compose ps",
			"Loglara bakin:       docker compose logs gateway --tail 30",
			fmt.Sprintf("Port dogru mu?       compose'da \"%d:8080\" bekleniyor", *port),
		}, err)
		os.Exit(1)
	}
	fmt.Println("        OK - gecit ayakta")
	fmt.Println()

	// --- 2. LAN IP adreslerini bul ---
	fmt.Println("  [2/4] Yerel ag adresleri araniyor...")
	ips := lanIPs()
	if len(ips) == 0 {
		fail("Hicbir LAN IP adresi bulunamadi.", []string{
			"Kabloya veya WiFi'ya bagli misiniz?",
			"VPN acikken sanal adapterler gercek IP'yi gizleyebilir.",
		}, nil)
		os.Exit(1)
	}
	for _, ip := range ips {
		fmt.Printf("        bulundu: %s\n", ip)
	}
	fmt.Println()

	// --- 3. LAN uzerinden gercekten erisilebiliyor mu? ---
	fmt.Println("  [3/4] LAN adresi kendi makineden yoklaniyor...")
	var reachable []string
	var lastErr error
	for _, ip := range ips {
		u := fmt.Sprintf("http://%s:%d%s", ip, *port, *path)
		if err := probe(u, *timeout); err != nil {
			fmt.Printf("        %s  ERISILEMIYOR\n", ip)
			lastErr = err
			continue
		}
		fmt.Printf("        %s  yanit verdi\n", ip)
		reachable = append(reachable, ip)
	}
	fmt.Println()

	if len(reachable) == 0 {
		fail("Gecit localhost'ta calisiyor ama LAN'dan ERISILEMIYOR.", []string{
			"En olasi sebep: Windows Guvenlik Duvari baglantiyi engelliyor.",
			"Cozum (Yonetici PowerShell):",
			fmt.Sprintf("  New-NetFirewallRule -DisplayName \"Aetheris %d\" \\", *port),
			fmt.Sprintf("    -Direction Inbound -LocalPort %d -Protocol TCP \\", *port),
			"    -Action Allow -Profile Private",
			"",
			"NOT: -Profile Private kullanin. Public profile eklemeyin;",
			"     kafe/havaalani aglarinda gecidiniz herkese acilir.",
			"",
			"Docker kullaniyorsaniz port yonlendirmesini de kontrol edin:",
			"  docker compose ps   ->  0.0.0.0:8080->8080/tcp gormelisiniz",
		}, lastErr)
		os.Exit(1)
	}

	// KRITIK UYARI — YANLIS POZITIF RISKI
	//
	// Yukaridaki kontrol kendi makinemizden kendi LAN IP'mize yapildi.
	// Bu trafik isletim sisteminin ag yigininda kalir ve GUVENLIK
	// DUVARININ GELEN KURALINA HIC TAKILMAZ. Telefon ise gercekten
	// disaridan gelir.
	//
	// Yani bu adim "yanit verdi" dese bile telefon baglanamayabilir.
	// Bunu SESSIZ GECMEK, gelistiriciyi korlemesine denemeye iter —
	// aracin onlemek icin yazildigi sey tam olarak budur.
	fmt.Println("  " + strings.Repeat("-", 46))
	fmt.Println("  NOT: Yukaridaki kontrol KENDI makinenizden yapildi.")
	fmt.Println("  Bu, disaridan erisimi KANITLAMAZ — kendi kendine")
	fmt.Println("  yapilan istek guvenlik duvarinin gelen kuralina")
	fmt.Println("  takilmaz. Telefondan acilmazsa asagidaki adimlar:")
	fmt.Println()
	printFirewallHelp(*port)
	fmt.Println("  " + strings.Repeat("-", 46))
	fmt.Println()

	// --- 4. QR kodu bas ---
	primary := reachable[0]
	phoneURL := fmt.Sprintf("http://%s:%d%s", primary, *port, *path)

	fmt.Println("  [4/4] Telefon erisim adresi hazir")
	fmt.Println()
	fmt.Println("  " + strings.Repeat("-", 46))
	fmt.Printf("   URL:  %s\n", phoneURL)
	fmt.Println("  " + strings.Repeat("-", 46))
	fmt.Println()

	if !*noQR {
		printQR(phoneURL)
	}

	fmt.Println("  Telefonunuz bilgisayarla AYNI WiFi agina bagli olmali.")
	fmt.Println()

	if len(reachable) > 1 {
		fmt.Println("  Alternatif adresler:")
		for _, ip := range reachable[1:] {
			fmt.Printf("    http://%s:%d%s\n", ip, *port, *path)
		}
		fmt.Println()
	}

	if *tunnel {
		runTunnel(*port)
	} else {
		fmt.Println("  Farkli agdan (mobil veri) test etmek icin: --tunnel")
		fmt.Println("  UYARI: tunel gecidi TUM INTERNETE acar.")
		fmt.Println()
	}
}

// probe, URL'ye GET atar ve 5xx olmayan yanit bekler.
func probe(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("sunucu %d dondu", resp.StatusCode)
	}
	return nil
}

// lanIPs, gercek LAN IPv4 adreslerini dondurur.
// Loopback, kapali arayuzler ve Docker/WSL sanal adapterleri elenir.
func lanIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualIface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			// Yalnizca ozel ag araliklari: 10/8, 172.16/12, 192.168/16
			if !ip4.IsPrivate() {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	return out
}

// isVirtualIface, sanal adapter adlarini tanir.
// Docker/WSL/VirtualBox adapterleri LAN IP'si gibi gorunur ama
// telefondan erisilemez; onerirsek gelistirici bosuna ugrasir.
func isVirtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, p := range []string{
		"docker", "veth", "br-", "vmnet", "vboxnet",
		"wsl", "hyper-v", "vethernet", "virtual", "tailscale", "zt",
	} {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

func fail(title string, hints []string, err error) {
	fmt.Println()
	fmt.Println("  HATA: " + title)
	if err != nil {
		fmt.Printf("  Ayrinti: %v\n", err)
	}
	if len(hints) > 0 {
		fmt.Println()
		fmt.Println("  Ne yapmali:")
		for _, h := range hints {
			if h == "" {
				fmt.Println()
				continue
			}
			fmt.Println("    " + h)
		}
	}
	fmt.Println()
}

// runTunnel, cloudflared ile gecici public tunel acar.
func runTunnel(port int) {
	fmt.Println()
	fmt.Println("  " + strings.Repeat("!", 46))
	fmt.Println("  UYARI: PUBLIC TUNEL ACILIYOR")
	fmt.Println()
	fmt.Println("  Gecidiniz TUM INTERNETE acilacak. Gecerli API")
	fmt.Println("  anahtarlariniz var; tunel URL'i sizarsa herkes")
	fmt.Println("  istek atabilir ve faturaniza yazilir.")
	fmt.Println()
	fmt.Println("  Testi bitirince Ctrl+C ile KAPATIN.")
	fmt.Println("  " + strings.Repeat("!", 46))
	fmt.Println()

	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		fmt.Println("  cloudflared bulunamadi. Kurulum:")
		switch runtime.GOOS {
		case "windows":
			fmt.Println("    winget install --id Cloudflare.cloudflared")
		case "darwin":
			fmt.Println("    brew install cloudflared")
		default:
			fmt.Println("    https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/")
		}
		fmt.Println()
		fmt.Println("  (Hesap veya token gerektirmez: quick tunnel modu)")
		fmt.Println()
		return
	}

	fmt.Println("  Tunel aciliyor... URL asagida gorunecek.")
	fmt.Println()

	cmd := exec.Command(bin, "tunnel", "--url",
		fmt.Sprintf("http://localhost:%d", port))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("\n  Tunel kapandi: %v\n", err)
	}
}

// printFirewallHelp, Windows Guvenlik Duvari icin gereken adimlari basar.
//
// # NEDEN IKI KURAL GEREKIYOR
//
//  1. Port kurali: 8080'e gelen TCP baglantilarina izin verir.
//  2. Docker Desktop Backend izni: Docker ilk acilista guvenlik duvari
//     izni sorar. "Iptal" denirse Windows o program icin ENGELLEME kurali
//     olusturur ve engelleme kurallari, sonradan eklenen izin
//     kurallarindan ONCELIKLIDIR. Port kurali dogru olsa bile baglanti
//     kurulamaz. Bu, sahada en cok vakit kaybettiren tuzaktir.
func printFirewallHelp(port int) {
	fmt.Println("  Yonetici PowerShell'de:")
	fmt.Println()
	fmt.Println("  # 1) Porta izin ver (-Profile Private: yalnizca ev/is agi)")
	fmt.Printf("  New-NetFirewallRule -DisplayName \"Aetheris %d\" \\\n", port)
	fmt.Printf("    -Direction Inbound -LocalPort %d -Protocol TCP \\\n", port)
	fmt.Println("    -Action Allow -Profile Private")
	fmt.Println()
	fmt.Println("  # 2) Docker Desktop icin ENGELLEME kurali var mi bak")
	fmt.Println("  #    (ilk acilista \"Iptal\" dendiyse olusur ve izinleri EZER)")
	fmt.Println("  Get-NetFirewallRule -Action Block -Enabled True |")
	fmt.Println("    Where-Object { $_.DisplayName -like \"*docker*\" } |")
	fmt.Println("    Select-Object DisplayName, Direction")
	fmt.Println()
	fmt.Println("  # Varsa sil:")
	fmt.Println("  Get-NetFirewallRule -Action Block -Enabled True |")
	fmt.Println("    Where-Object { $_.DisplayName -like \"*docker*\" } |")
	fmt.Println("    Remove-NetFirewallRule")
	fmt.Println()
	fmt.Println("  # 3) Docker port yonlendirmesini dogrula")
	fmt.Println("  #    0.0.0.0:8080->8080/tcp gormelisiniz")
	fmt.Println("  docker compose ps")
}
