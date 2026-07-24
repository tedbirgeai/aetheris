package main

import (
	"fmt"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// QR uretimi — NEDEN KUTUPHANE KULLANIYORUZ
//
// QR kodlamayi elle yazmak mumkun ama hataya cok acik: maske secimi,
// bicim bilgisi BCH kodlamasi ve veri yerlesimi zigzagi gibi detaylarda
// bir bit hatasi kodu SESSIZCE taranamaz hale getirir.
//
// Bu proje icin once elle bir kodlayici yazildi ve referans uygulamayla
// karsilastirildi: ciktilar UYUSMADI. Taranmayan bir QR, sahada
// gelistiriciye "neden calismiyor" diye vakit kaybettirir — hic QR
// olmamasindan daha kotudur.
//
// skip2/go-qrcode kucuk, bagimliliksiz ve yaygin kullanilan bir
// kutuphanedir. Dogru olan, calistigi kanitlanmis kodu kullanmaktir.

// printQR, veriyi QR kod olarak terminale basar.
func printQR(data string) {
	q, err := qrcode.New(data, qrcode.Low)
	if err != nil {
		// QR uretilemezse arac islevini kaybetmez; URL zaten basildi.
		fmt.Printf("  (QR uretilemedi: %v - yukaridaki URL'yi elle girin)\n\n", err)
		return
	}
	// Sessiz bolge QR standardinin gerektirdigi 4 modul.
	renderQR(q.Bitmap())
}

// renderQR, matrisi yarim blok karakterlerle basar.
// Her karakter iki satiri temsil eder; boylece QR kare gorunur, dikeyde
// uzamaz. Uzamis bir QR bircok telefon tarayicisinda okunmaz.
func renderQR(m [][]bool) {
	if len(m) == 0 {
		return
	}
	size := len(m)

	// go-qrcode Bitmap()'inde true = KOYU modul.
	get := func(y, x int) bool {
		if y < 0 || x < 0 || y >= size || x >= size {
			return false
		}
		return m[y][x]
	}

	var sb strings.Builder
	for y := 0; y < size; y += 2 {
		sb.WriteString("  ")
		for x := 0; x < size; x++ {
			top, bot := get(y, x), get(y+1, x)
			switch {
			case top && bot:
				sb.WriteString("\u2588") // tam blok
			case top && !bot:
				sb.WriteString("\u2580") // ust yarim
			case !top && bot:
				sb.WriteString("\u2584") // alt yarim
			default:
				sb.WriteString(" ")
			}
		}
		sb.WriteString("\n")
	}
	fmt.Println(sb.String())
}
