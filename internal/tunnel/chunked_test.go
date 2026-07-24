package tunnel_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tedbirgeai/aetheris/internal/dedup"
	"github.com/tedbirgeai/aetheris/internal/store"
	"github.com/tedbirgeai/aetheris/internal/tunnel"
	"github.com/tedbirgeai/aetheris/pkg/client"
)

func postChunked(t *testing.T, env *testEnv, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tunnel/chunked", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+testKey)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

// TestChunkedEndToEnd, istemci paketleme -> sunucu dogrulama -> defter
// zincirini uctan uca dogrular.
func TestChunkedEndToEnd(t *testing.T) {
	st := store.NewMemory()
	env := newEnv(t, st, nil)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	p := &client.Packer{Key: key, ChunkSize: 512, Cache: client.NewChunkCache(0)}

	// Test ortaminin MaxPayloadBytes siniri 4096; base64 sismesini de
	// hesaba katarak kucuk tutuyoruz.
	plain := []byte(strings.Repeat("Aetheris. ", 100))
	packed, err := p.Pack(plain)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(tunnel.ChunkedRequest{
		CarrierType: "mesh_wifi",
		Manifest:    packed.Manifest,
		Chunks:      packed.Chunks,
	})

	rec := postChunked(t, env, string(body), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d, govde = %s", rec.Code, rec.Body.String())
	}

	var receipt tunnel.ChunkedReceipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ClientID != "acme" {
		t.Fatalf("ClientID = %q", receipt.ClientID)
	}
	// FATURALANAN = telde akan bayt
	if receipt.MeteredBytes != packed.Stats.TransferredBytes {
		t.Fatalf("MeteredBytes = %d, istemci %d gonderdi",
			receipt.MeteredBytes, packed.Stats.TransferredBytes)
	}
	if receipt.Verification == nil || !receipt.Verification.StructurallyValid {
		t.Fatal("yapisal dogrulama basarisiz")
	}

	e, err := st.ClientUsage(context.Background(), "acme")
	if err != nil {
		t.Fatalf("defter guncellenmedi: %v", err)
	}
	if e.BytesIn != packed.Stats.TransferredBytes {
		t.Fatalf("deftere %d bayt yazildi, telde %d aktı",
			e.BytesIn, packed.Stats.TransferredBytes)
	}
}

// TestChunkedNeverClaimsContentVerification, makbuzun icerik
// dogrulamasi IDDIA ETMEDIGINI kanitlar.
func TestChunkedNeverClaimsContentVerification(t *testing.T) {
	env := newEnv(t, store.NewMemory(), nil)

	key := make([]byte, 32)
	p := &client.Packer{Key: key, ChunkSize: 512}
	packed, err := p.Pack([]byte(strings.Repeat("veri", 200)))
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(tunnel.ChunkedRequest{
		Manifest: packed.Manifest, Chunks: packed.Chunks,
	})
	rec := postChunked(t, env, string(body), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d: %s", rec.Code, rec.Body.String())
	}

	var receipt tunnel.ChunkedReceipt
	_ = json.Unmarshal(rec.Body.Bytes(), &receipt)

	if receipt.Verification.ContentVerified {
		t.Fatal("makbuz icerik dogrulamasi IDDIA EDIYOR - sunucu duz metni goremez")
	}
	// Alan adi da durust olmali: "claimed" icermeli.
	raw := rec.Body.String()
	if !strings.Contains(raw, "savings_claimed_bytes") {
		t.Fatal("tasarruf alani 'claimed' ibaresi tasimiyor - yaniltici olur")
	}
}

// TestChunkedBillsOnlyTransferredBytes, onbellekten karsilandigi
// BEYAN edilen parcalarin FATURALANMADIGINI dogrular.
func TestChunkedBillsOnlyTransferredBytes(t *testing.T) {
	st := store.NewMemory()
	env := newEnv(t, st, nil)

	key := make([]byte, 32)
	cache := client.NewChunkCache(0)
	p := &client.Packer{Key: key, ChunkSize: 1024, Cache: cache}

	plain := []byte(strings.Repeat("Q", 4096))

	// Ilk gonderim
	first, _ := p.Pack(plain)
	b1, _ := json.Marshal(tunnel.ChunkedRequest{
		Manifest: first.Manifest, Chunks: first.Chunks,
	})
	if rec := postChunked(t, env, string(b1), true); rec.Code != http.StatusOK {
		t.Fatalf("ilk gonderim = %d: %s", rec.Code, rec.Body.String())
	}

	e1, _ := st.ClientUsage(context.Background(), "acme")
	afterFirst := e1.BytesIn

	// Ikinci gonderim: hepsi onbellekte, govde BOS
	second, _ := p.Pack(plain)
	if len(second.Chunks) != 0 {
		t.Fatalf("ikinci gonderimde govde bos olmaliydi: %d", len(second.Chunks))
	}
	b2, _ := json.Marshal(tunnel.ChunkedRequest{
		Manifest: second.Manifest, Chunks: second.Chunks,
	})
	if rec := postChunked(t, env, string(b2), true); rec.Code != http.StatusOK {
		t.Fatalf("ikinci gonderim = %d: %s", rec.Code, rec.Body.String())
	}

	e2, _ := st.ClientUsage(context.Background(), "acme")
	// Ikinci gonderimde hicbir parca akmadi -> BytesIn ARTMAMALI.
	if e2.BytesIn != afterFirst {
		t.Fatalf("akmayan veri faturalandi: %d -> %d", afterFirst, e2.BytesIn)
	}
}

// TestChunkedRejectsSizeLie, manifestte boyut yalani soyleyen istemcinin
// REDDEDILDIGINI dogrular.
func TestChunkedRejectsSizeLie(t *testing.T) {
	env := newEnv(t, store.NewMemory(), nil)

	key := make([]byte, 32)
	p := &client.Packer{Key: key, ChunkSize: 1024}
	packed, err := p.Pack([]byte(strings.Repeat("R", 1024)))
	if err != nil {
		t.Fatal(err)
	}
	// YALAN: boyutu kucuk goster
	packed.Manifest.Chunks[0].Size = 30

	body, _ := json.Marshal(tunnel.ChunkedRequest{
		Manifest: packed.Manifest, Chunks: packed.Chunks,
	})
	rec := postChunked(t, env, string(body), true)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("durum = %d, beklenen 422", rec.Code)
	}
}

func TestChunkedRejectsMissingAuth(t *testing.T) {
	env := newEnv(t, store.NewMemory(), nil)
	m := &dedup.Manifest{Chunks: []dedup.ChunkRef{{Hash: strings.Repeat("a", 64), Size: 100, Cached: true}}}
	body, _ := json.Marshal(tunnel.ChunkedRequest{Manifest: m})
	if rec := postChunked(t, env, string(body), false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("durum = %d, beklenen 401", rec.Code)
	}
}

func TestChunkedRejectsIllegalCarrier(t *testing.T) {
	env := newEnv(t, store.NewMemory(), nil)
	m := &dedup.Manifest{Chunks: []dedup.ChunkRef{{Hash: strings.Repeat("a", 64), Size: 100, Cached: true}}}
	body, _ := json.Marshal(tunnel.ChunkedRequest{CarrierType: "radio_rf", Manifest: m})
	if rec := postChunked(t, env, string(body), true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("durum = %d, beklenen 422", rec.Code)
	}
}
