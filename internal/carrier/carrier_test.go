package carrier

import "testing"

func TestNormalizeDefaultsToStandardInternet(t *testing.T) {
	got, err := Normalize("")
	if err != nil {
		t.Fatalf("bos tasiyici hata verdi: %v", err)
	}
	if got != StandardInternet {
		t.Fatalf("varsayilan = %q, beklenen %q", got, StandardInternet)
	}
}

func TestNormalizeAcceptsAllowed(t *testing.T) {
	for _, c := range All() {
		t.Run(string(c), func(t *testing.T) {
			got, err := Normalize(string(c))
			if err != nil {
				t.Fatalf("izinli tasiyici reddedildi: %v", err)
			}
			if got != c {
				t.Fatalf("Normalize(%q) = %q", c, got)
			}
		})
	}
}

// TestNormalizeRejectsIllegalCarriers, hukuki kapsam korumasini
// regresyona karsi kilitler.
//
// Bu test SILINMEMELIDIR. Asagidaki tasiyicilarin kabul edilmesi,
// 5809 sayili Elektronik Haberlesme Kanunu ve TCK 132-140 kapsaminda
// suc olusturan bir kullanimin onunu acar. Yeni bir tasiyici eklenecekse
// once lisans/izin belgesi alinmali, sonra bu listeden cikarilmalidir.
func TestNormalizeRejectsIllegalCarriers(t *testing.T) {
	illegal := []string{
		"radio_rf",          // lisansli spektrumda veri yayini
		"satellite_dish",    // ucuncu taraf uydu sinyali dinleme
		"am_fm_data",        // lisansli yayin bandi
		"uydu_sizintisi",    // izinsiz dinleme
		"pirate_radio",      // kacak telsiz istasyonu
		"STANDARD_INTERNET", // buyuk harf varyanti da kabul edilmemeli
	}
	for _, raw := range illegal {
		t.Run(raw, func(t *testing.T) {
			if _, err := Normalize(raw); err == nil {
				t.Fatalf("yasal olmayan tasiyici %q kabul edildi", raw)
			}
		})
	}
}

func TestIsAllowed(t *testing.T) {
	if !IsAllowed(MeshWiFi) {
		t.Fatal("mesh_wifi izinli olmali")
	}
	if IsAllowed(Type("radio_rf")) {
		t.Fatal("radio_rf izinli gorunuyor")
	}
}

// TestAllIsSorted, health ciktisinin deterministik oldugunu dogrular.
func TestAllIsSorted(t *testing.T) {
	got := All()
	if len(got) != 5 {
		t.Fatalf("tasiyici sayisi = %d, beklenen 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("sirali degil: %v", got)
		}
	}
}
