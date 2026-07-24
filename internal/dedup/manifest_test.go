package dedup

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func mkChunk(t *testing.T, size int) (ChunkRef, string) {
	t.Helper()
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = byte(i % 251)
	}
	sum := sha256.Sum256(raw)
	return ChunkRef{
			Hash: hex.EncodeToString(sum[:]),
			Size: size,
		},
		base64.StdEncoding.EncodeToString(raw)
}

func TestVerifyManifestHappyPath(t *testing.T) {
	r1, b1 := mkChunk(t, 100)
	r2, b2 := mkChunk(t, 200)

	m := &Manifest{TotalPlainSize: 300, Chunks: []ChunkRef{r1, r2}}
	res, err := VerifyManifest(m, []string{b1, b2})
	if err != nil {
		t.Fatalf("hata dondu: %v", err)
	}
	if res.TransferredBytes != 300 {
		t.Fatalf("TransferredBytes = %d, beklenen 300", res.TransferredBytes)
	}
	if res.NewChunks != 2 || res.ChunkCount != 2 {
		t.Fatalf("parca sayilari hatali: %+v", res)
	}
	if !res.StructurallyValid {
		t.Fatal("StructurallyValid false")
	}
}

// TestContentVerifiedIsAlwaysFalse, sunucunun icerik dogrulamasi
// YAPMADIGINI ve yapmadigini ACIKCA bildirdigini kanitlar.
//
// Bu test SILINMEMELIDIR. ContentVerified alaninin true olmasi,
// sunucunun duz metni gordugu (sifir-bilgi ihlali) veya dogrulayamadigi
// bir seyi dogruladigini iddia ettigi (musteriyi yaniltma) anlamina gelir.
func TestContentVerifiedIsAlwaysFalse(t *testing.T) {
	r1, b1 := mkChunk(t, 100)
	m := &Manifest{TotalPlainSize: 100, Chunks: []ChunkRef{r1}}

	res, err := VerifyManifest(m, []string{b1})
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentVerified {
		t.Fatal("ContentVerified true dondu - sunucu duz metni goremez, " +
			"icerik dogrulamasi IDDIA EDILEMEZ")
	}
}

// TestCachedChunksAreNotBilled, onbellekten karsilandigi BEYAN edilen
// parcalarin faturalanmadigini dogrular — telde akmadilar.
func TestCachedChunksAreNotBilled(t *testing.T) {
	r1, b1 := mkChunk(t, 100)
	r2, _ := mkChunk(t, 500)
	r2.Cached = true // istemci "bunu daha once gonderdim" diyor

	m := &Manifest{TotalPlainSize: 600, Chunks: []ChunkRef{r1, r2}}
	res, err := VerifyManifest(m, []string{b1}) // yalnizca 1 parca govdede
	if err != nil {
		t.Fatalf("hata dondu: %v", err)
	}
	if res.TransferredBytes != 100 {
		t.Fatalf("TransferredBytes = %d, beklenen 100 (yalnizca akan bayt)", res.TransferredBytes)
	}
	if res.ReferencedBytes != 500 {
		t.Fatalf("ReferencedBytes = %d, beklenen 500", res.ReferencedBytes)
	}
}

// TestChunkSizeMismatchRejected, manifestte beyan edilen boyutla
// govdedeki gercek boyut uyusmazsa reddedildigini dogrular.
// Bu, faturayi dusuk gostermeye calisan bir istemciyi engeller.
func TestChunkSizeMismatchRejected(t *testing.T) {
	r1, b1 := mkChunk(t, 100)
	r1.Size = 50 // YALAN: gercekte 100 bayt gonderiliyor

	m := &Manifest{TotalPlainSize: 100, Chunks: []ChunkRef{r1}}
	_, err := VerifyManifest(m, []string{b1})
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("ErrSizeMismatch bekleniyordu, alinan: %v", err)
	}
}

func TestVerifyManifestRejectsInvalid(t *testing.T) {
	valid, vb := mkChunk(t, 100)

	cases := map[string]struct {
		m      *Manifest
		chunks []string
	}{
		"bos manifest": {
			&Manifest{}, nil,
		},
		"parca sayisi uyusmuyor": {
			&Manifest{Chunks: []ChunkRef{valid, valid}}, []string{vb},
		},
		"gecersiz hash uzunlugu": {
			&Manifest{Chunks: []ChunkRef{{Hash: "kisa", Size: 100}}}, []string{vb},
		},
		"hash hex degil": {
			&Manifest{Chunks: []ChunkRef{{Hash: strings.Repeat("z", 64), Size: 100}}}, []string{vb},
		},
		"parca cok kucuk": {
			&Manifest{Chunks: []ChunkRef{{Hash: valid.Hash, Size: 10}}}, []string{vb},
		},
		"parca cok buyuk": {
			&Manifest{Chunks: []ChunkRef{{Hash: valid.Hash, Size: MaxChunkBytes + 1}}}, []string{vb},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyManifest(c.m, c.chunks); err == nil {
				t.Fatalf("%s icin hata bekleniyordu", name)
			}
		})
	}
}

func TestTooManyChunksRejected(t *testing.T) {
	r, _ := mkChunk(t, 100)
	chunks := make([]ChunkRef, MaxChunks+1)
	for i := range chunks {
		c := r
		c.Cached = true
		chunks[i] = c
	}
	m := &Manifest{Chunks: chunks}
	if _, err := VerifyManifest(m, nil); !errors.Is(err, ErrTooManyChunks) {
		t.Fatalf("ErrTooManyChunks bekleniyordu, alinan: %v", err)
	}
}
