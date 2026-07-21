// Package carrier - desteklenen fiziksel tasiyici katmanlari.
//
// HUKUKI NOT: Bu liste bilincli olarak kisitlidir. Lisansli spektrumda yayin
// (AM/FM veri tasima) ve ucuncu taraf uydu sinyallerinin izinsiz dinlenmesi
// BILEREK disarida birakilmistir; 5809 sayili Elektronik Haberlesme Kanunu
// ve TCK 132-140 kapsaminda sucturlar.
package carrier

import "fmt"

type Type string

const (
	StandardInternet  Type = "standard_internet"
	MeshWiFi          Type = "mesh_wifi"
	LoRaISM           Type = "lora_ism"
	OpticalLiFi       Type = "optical_li_fi"
	SatelliteLicensed Type = "satellite_licensed"
)

var allowed = map[Type]struct{}{
	StandardInternet:  {},
	MeshWiFi:          {},
	LoRaISM:           {},
	OpticalLiFi:       {},
	SatelliteLicensed: {},
}

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

func All() []Type {
	out := make([]Type, 0, len(allowed))
	for t := range allowed {
		out = append(out, t)
	}
	return out
}
