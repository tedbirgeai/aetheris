package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteBasicFormat(t *testing.T) {
	var buf bytes.Buffer
	err := Write(&buf, []Sample{
		{Name: "aetheris_bytes_total", Type: "counter", Help: "Toplam bayt.", Value: 1234},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"# HELP aetheris_bytes_total Toplam bayt.",
		"# TYPE aetheris_bytes_total counter",
		"aetheris_bytes_total 1234",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("cikti %q icermiyor:\n%s", want, out)
		}
	}
}

// TestSameNameGroupedUnderOneHelp, ayni ada sahip orneklerin TEK
// HELP/TYPE blogu altinda gruplandigini dogrular.
//
// Prometheus bunu ZORUNLU kilar: ayni metrik adi icin ikinci bir
// HELP satiri scrape hatasina yol acar.
func TestSameNameGroupedUnderOneHelp(t *testing.T) {
	var buf bytes.Buffer
	err := Write(&buf, []Sample{
		{Name: "m", Type: "gauge", Help: "aciklama", Labels: map[string]string{"a": "1"}, Value: 1},
		{Name: "m", Type: "gauge", Help: "aciklama", Labels: map[string]string{"a": "2"}, Value: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "# HELP m "); n != 1 {
		t.Fatalf("HELP satiri %d kez gorundu, beklenen 1:\n%s", n, out)
	}
	if n := strings.Count(out, "# TYPE m "); n != 1 {
		t.Fatalf("TYPE satiri %d kez gorundu, beklenen 1:\n%s", n, out)
	}
}

func TestLabelsSortedAndEscaped(t *testing.T) {
	var buf bytes.Buffer
	_ = Write(&buf, []Sample{{
		Name: "m", Type: "gauge",
		Labels: map[string]string{"z": "son", "a": `tirnak"icerir`},
		Value:  1,
	}})
	out := buf.String()
	// Etiketler alfabetik siralanmali (deterministik cikti).
	if !strings.Contains(out, `m{a="tirnak\"icerir",z="son"} 1`) {
		t.Fatalf("etiket bicimi hatali:\n%s", out)
	}
}

func TestFloatFormatting(t *testing.T) {
	var buf bytes.Buffer
	_ = Write(&buf, []Sample{
		{Name: "tam", Type: "gauge", Value: 42},
		{Name: "ondalik", Type: "gauge", Value: 0.125},
	})
	out := buf.String()
	if !strings.Contains(out, "tam 42\n") {
		t.Fatalf("tam sayi gereksiz ondalik aldi:\n%s", out)
	}
	if !strings.Contains(out, "ondalik 0.125\n") {
		t.Fatalf("ondalik deger bozuldu:\n%s", out)
	}
}

func TestEmptyInput(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("bos girdi cikti uretti: %q", buf.String())
	}
}
