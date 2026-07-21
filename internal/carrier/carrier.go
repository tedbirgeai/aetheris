// Package carrier - desteklenen fiziksel tasiyici katmanlari.
//
// HUKUKI NOT: Bu liste bilincli olarak kisitlidir. Lisansli spektrumda yayin
// (AM/FM veri tasima) ve ucuncu taraf uydu sinyallerinin izinsiz dinlenmesi
// BILEREK disarida birakilmistir; 5809 sayili Elektronik Haberlesme Kanunu
// ve TCK 132-140 kapsaminda sucturlar.
//
// Yeni tasiyici eklemeden once ilgili lisans/izin belgesi alinmali ve
// ruhsat referansi buraya yorum olarak islenmelidir.
package carrier

import (
	"fmt"
	"sort"
)

type Type string

const (
	// StandardInternet - mevcut TCP/IP hatlari (fiber, xDSL, mobil veri).
	StandardInternet Type = "standard_internet"

	// MeshWiFi - 2.4/5 GHz ISM bandi, lisanssiz kullanima acik.
	MeshWiFi Type = "mesh_wifi"

	// LoRaISM - 868/915 MHz ISM bandi, gorev dongusu sinirlarina tabi.
	LoRaISM Type = "lora_ism"

	// OpticalLiFi - gorunur isik / kizilotesi. Spektrum lisansi gerekmez.
	OpticalLiFi Type = "optical_li_fi"

	// SatelliteLicensed - YALNIZCA aboneligi bulunan uydu terminalleri.
	SatelliteLicensed Type = "satellite_licensed"
)

var allowed = map[Type]struct{}{
	StandardInternet:  {},
	MeshWiFi:          {},
	LoRaISM:           {},
	OpticalLiFi:       {},
	SatelliteLicensed: {},
}

// Normalize, bos degeri varsayilana ceker ve tasiyiciyi dogrular.
// Bilinmeyen tasiyici sessizce kabul edilmez - acik hata doner.
func Normalize(raw string) (Type, error) {
	if raw == "" {
		return StandardInternet, nil
	}
	t := Type(raw)
	if _, ok := allowed[t]; !ok {
		return "", fmt.Errorf("desteklenmeyen veya yasal olarak izinsiz tasiyici: %q", raw)
	}
	return t, nil
}

// IsAllowed, tasiyicinin izin listesinde olup olmadigini bildirir.
func IsAllowed(t Type) bool {
	_, ok := allowed[t]
	return ok
}

// All, izin verilen tum tasiyicilari alfabetik sirada dondurur.
// Sirali olmasi, health cikisinin deterministik olmasini saglar.
func All() []Type {
	out := make([]Type, 0, len(allowed))
	for t := range allowed {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
