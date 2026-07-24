// Package metrics, Aetheris metriklerini Prometheus text exposition
// formatinda disa aktarir.
//
// TASARIM KARARI — NEDEN HARICI KUTUPHANE YOK:
// Prometheus text format acik ve basit bir metindir. prometheus/client_golang
// bagimliligi eklemek 20+ dolayli bagimlilik getirir. Ihtiyacimiz olan
// yalnizca dogru bicimde metin uretmek; bu dosya onu yapar.
//
// Cikti, OpenTelemetry Collector'in prometheus receiver'i tarafindan da
// dogrudan okunabilir (OTel, Prometheus scrape'i yerel olarak destekler).
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Sample, tek bir metrik olcumudur.
type Sample struct {
	Name   string
	Help   string
	Type   string // "counter" | "gauge"
	Labels map[string]string
	Value  float64
}

// Write, ornekleri Prometheus text exposition formatinda yazar.
//
// Bicim:
//
//	# HELP metrik_adi Aciklama
//	# TYPE metrik_adi gauge
//	metrik_adi{etiket="deger"} 1.23
//
// Ayni ada sahip ornekler tek HELP/TYPE blogu altinda gruplanir —
// Prometheus bunu zorunlu kilar, aksi halde scrape hata verir.
func Write(w io.Writer, samples []Sample) error {
	// Ada gore grupla, cikti deterministik olsun.
	byName := make(map[string][]Sample)
	var order []string
	for _, s := range samples {
		if _, seen := byName[s.Name]; !seen {
			order = append(order, s.Name)
		}
		byName[s.Name] = append(byName[s.Name], s)
	}
	sort.Strings(order)

	for _, name := range order {
		group := byName[name]
		if len(group) == 0 {
			continue
		}
		if group[0].Help != "" {
			if _, err := fmt.Fprintf(w, "# HELP %s %s\n", name, group[0].Help); err != nil {
				return err
			}
		}
		typ := group[0].Type
		if typ == "" {
			typ = "gauge"
		}
		if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", name, typ); err != nil {
			return err
		}
		for _, s := range group {
			if _, err := fmt.Fprintf(w, "%s%s %s\n",
				s.Name, formatLabels(s.Labels), formatValue(s.Value)); err != nil {
				return err
			}
		}
	}
	return nil
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		// DIKKAT: %q KULLANMAYIN. escapeLabel zaten kacis uyguluyor;
		// %q ustune bir kez daha kacirir ve `\"` yerine `\\\"` uretir.
		// Bozuk etiket degeri Prometheus scrape'ini kirar.
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escapeLabel(labels[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabel, Prometheus etiket degeri kacislarini uygular.
func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

// formatValue, ondalik degerleri Prometheus'un bekledigi bicimde yazar.
// Tam sayilar gereksiz ondalik almaz; float'lar yeterli hassasiyette kalir.
func formatValue(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
