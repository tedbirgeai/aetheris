package config

import (
	"strings"
	"testing"
)

func TestParseAPIKeysValid(t *testing.T) {
	const k1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const k2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	got, err := parseAPIKeys("acme:" + k1 + ", globex:" + k2)
	if err != nil {
		t.Fatalf("hata dondu: %v", err)
	}
	if got[k1] != "acme" || got[k2] != "globex" {
		t.Fatalf("esleme hatali: %v", got)
	}
}

// TestParseAPIKeysRejectsShortKey, zayif anahtarin BASLANGICTA
// reddedildigini dogrular. Uretimde 8 karakterlik anahtarla calisan
// bir gecit, kimlik dogrulamasi olmayan bir gecittir.
func TestParseAPIKeysRejectsShortKey(t *testing.T) {
	if _, err := parseAPIKeys("acme:kisa"); err == nil {
		t.Fatal("kisa anahtar kabul edildi")
	}
}

func TestParseAPIKeysRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"bos":                "",
		"ayirac yok":         "acmeaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bos clientID":       ":aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"yinelenen anahtar":  "a:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa,b:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"sadece bosluk ayir": "   ",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAPIKeys(raw); err == nil {
				t.Fatalf("%s icin hata bekleniyordu", name)
			}
		})
	}
}

func TestParseRoutesValid(t *testing.T) {
	got, err := parseRoutes("eu-edge=edge@https://edge.example.com/ingest,peer1=peering@http://peer.example.net")
	if err != nil {
		t.Fatalf("hata dondu: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rota sayisi = %d, beklenen 2", len(got))
	}
	if got[0].Name != "eu-edge" || got[0].Kind != "edge" {
		t.Fatalf("ilk rota hatali: %+v", got[0])
	}
	if got[0].Upstream.Host != "edge.example.com" || got[0].Upstream.Path != "/ingest" {
		t.Fatalf("upstream ayristirilmadi: %v", got[0].Upstream)
	}
	if got[1].Kind != "peering" {
		t.Fatalf("ikinci rota turu = %q", got[1].Kind)
	}
}

func TestParseRoutesEmptyMeansDisabled(t *testing.T) {
	got, err := parseRoutes("")
	if err != nil {
		t.Fatalf("bos rota listesi hata vermemeli: %v", err)
	}
	if got != nil {
		t.Fatalf("bos liste icin nil bekleniyordu, alinan: %v", got)
	}
}

func TestParseRoutesRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"tur yok":            "a=https://x.example",
		"gecersiz tur":       "a=sihirli@https://x.example",
		"gecersiz sema":      "a=edge@ftp://x.example",
		"host yok":           "a=edge@https://",
		"ad yok":             "=edge@https://x.example",
		"yinelenen ad":       "a=edge@https://x.example,a=direct@https://y.example",
		"esittir yok":        "aedge@https://x.example",
		"sadece ayiraclarla": ",,,",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRoutes(raw); err == nil {
				t.Fatalf("%s icin hata bekleniyordu", name)
			}
		})
	}
}

// clearEnv, testin miras alinan ortam degiskenlerinden ETKILENMEMESINI
// saglar. Gelistiricinin kabuginda AETHERIS_LISTEN gibi bir degisken
// export edilmisse, hermetik olmayan bir test sahte basarisizlik verir.
// t.Setenv, test bitiminde otomatik geri alir.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"AETHERIS_LISTEN",
		"AETHERIS_API_KEYS",
		"AETHERIS_RECEIPT_SECRET",
		"AETHERIS_STORE",
		"AETHERIS_DATABASE_DSN",
		"AETHERIS_ROUTES",
		"AETHERIS_MAX_PAYLOAD_BYTES",
		"AETHERIS_RATE_LIMIT_PER_MIN",
		"AETHERIS_RATE_LIMIT_BURST",
		"AETHERIS_REDIS_ADDR",
		"AETHERIS_WAL_ENABLED",
		"AETHERIS_WAL_DIR",
		"AETHERIS_HEALTHPROBE",
		"AETHERIS_FORWARD_TIMEOUT_SEC",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadRejectsPostgresWithoutDSN(t *testing.T) {
	clearEnv(t)
	t.Setenv("AETHERIS_API_KEYS", "acme:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("AETHERIS_STORE", "postgres")
	t.Setenv("AETHERIS_DATABASE_DSN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("DSN olmadan postgres kabul edildi")
	}
	if !strings.Contains(err.Error(), "AETHERIS_DATABASE_DSN") {
		t.Fatalf("hata mesaji yol gostermiyor: %v", err)
	}
}

func TestLoadRejectsUnknownStore(t *testing.T) {
	clearEnv(t)
	t.Setenv("AETHERIS_API_KEYS", "acme:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("AETHERIS_STORE", "mongodb")

	if _, err := Load(); err == nil {
		t.Fatal("bilinmeyen store turu kabul edildi")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("AETHERIS_API_KEYS", "acme:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	t.Setenv("AETHERIS_STORE", "")
	t.Setenv("AETHERIS_ROUTES", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("varsayilan konfigurasyon reddedildi: %v", err)
	}
	if cfg.StoreKind != "memory" {
		t.Fatalf("StoreKind = %q, beklenen memory", cfg.StoreKind)
	}
	if cfg.Listen != ":8080" {
		t.Fatalf("Listen = %q", cfg.Listen)
	}
	if cfg.MaxPayloadBytes != 1<<20 {
		t.Fatalf("MaxPayloadBytes = %d", cfg.MaxPayloadBytes)
	}
	if len(cfg.Routes) != 0 {
		t.Fatalf("rota bekleniyordu yok, alinan %d", len(cfg.Routes))
	}
}

// TestLoadRejectsMissingKeys, hicbir anahtar tanimlanmadiginda sunucunun
// AYAGA KALKMAMASINI dogrular. Acik bir gecit, gecit degildir.
func TestLoadRejectsMissingKeys(t *testing.T) {
	clearEnv(t)
	t.Setenv("AETHERIS_API_KEYS", "")
	if _, err := Load(); err == nil {
		t.Fatal("anahtarsiz konfigurasyon kabul edildi")
	}
}
