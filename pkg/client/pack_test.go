package client

import (
	"bytes"
	"crypto/rand"
	"strings"
	"sync"
	"testing"

	"github.com/tedbirgeai/aetheris/internal/dedup"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestPackUnpackRoundTrip, sifrelenip parcalanan verinin BOZULMADAN
// geri alindigini dogrular.
func TestPackUnpackRoundTrip(t *testing.T) {
	key := testKey(t)
	plain := []byte(strings.Repeat("Aetheris veri blogu. ", 500))

	p := &Packer{Key: key, ChunkSize: 1024}
	packed, err := p.Pack(plain)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Unpack(key, packed.Manifest, packed.Chunks, nil)
	if err != nil {
		t.Fatalf("Unpack hata dondu: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("tur sonunda veri bozuldu")
	}
}

// TestDedupSavesOnRepeatedContent, TEKRAR EDEN icerikte gercek tasarruf
// oldugunu OLCEREK dogrular. Vaat degil, olcum.
func TestDedupSavesOnRepeatedContent(t *testing.T) {
	key := testKey(t)
	cache := NewChunkCache(0)
	p := &Packer{Key: key, ChunkSize: 1024, Cache: cache}

	// Her parcasi FARKLI olan veri: yuk ici tekillestirme devreye
	// girmesin, olctugumuz sey GONDERIMLER ARASI tasarruf olsun.
	plain := distinctData(1024, 8)

	first, err := p.Pack(plain)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stats.CachedChunks != 0 {
		t.Fatalf("ilk gonderimde onbellek isabeti olmamali: %d", first.Stats.CachedChunks)
	}
	if first.Stats.SavedRatio != 0 {
		t.Fatalf("ilk gonderimde tasarruf 0 olmali: %f", first.Stats.SavedRatio)
	}

	second, err := p.Pack(plain)
	if err != nil {
		t.Fatal(err)
	}
	// Ikinci gonderimde TUM parcalar onbellekte olmali.
	if second.Stats.NewChunks != 0 {
		t.Fatalf("ikinci gonderimde yeni parca olmamali: %d", second.Stats.NewChunks)
	}
	if second.Stats.TransferredBytes != 0 {
		t.Fatalf("ikinci gonderimde bayt akmamali: %d", second.Stats.TransferredBytes)
	}
	if second.Stats.SavedRatio != 1.0 {
		t.Fatalf("ikinci gonderimde tasarruf 1.0 olmali: %f", second.Stats.SavedRatio)
	}
}

// TestDedupSavesNothingOnRandomData, RASTGELE veride tasarruf
// OLMADIGINI dogrular.
//
// Bu test, "her durumda %80 tasarruf" gibi bir vaadin yalan oldugunu
// kanitlar. Tekillestirme yalnizca tekrar eden veride ise yarar.
func TestDedupSavesNothingOnRandomData(t *testing.T) {
	key := testKey(t)
	cache := NewChunkCache(0)
	p := &Packer{Key: key, ChunkSize: 1024, Cache: cache}

	for i := 0; i < 3; i++ {
		plain := make([]byte, 8192)
		if _, err := rand.Read(plain); err != nil {
			t.Fatal(err)
		}
		packed, err := p.Pack(plain)
		if err != nil {
			t.Fatal(err)
		}
		if packed.Stats.SavedRatio != 0 {
			t.Fatalf("rastgele veride tasarruf olmamali, olculen: %f",
				packed.Stats.SavedRatio)
		}
		if packed.Stats.CachedChunks != 0 {
			t.Fatalf("rastgele veride onbellek isabeti olmamali: %d",
				packed.Stats.CachedChunks)
		}
	}
}

// TestPartialDedup, kismi tekrar eden veride ORANTILI tasarruf oldugunu
// dogrular.
func TestPartialDedup(t *testing.T) {
	key := testKey(t)
	cache := NewChunkCache(0)
	p := &Packer{Key: key, ChunkSize: 1024, Cache: cache}

	// Ilk gonderim: 4 farkli parca
	base := distinctData(1024, 4)
	if _, err := p.Pack(base); err != nil {
		t.Fatal(err)
	}

	// Ikinci gonderim: ayni 4 parca + 4 YENI farkli parca
	extra := make([]byte, 0, 4096)
	for i := 4; i < 8; i++ {
		block := bytes.Repeat([]byte{byte('a' + i)}, 1024)
		copy(block, []byte("yeni-"+string(rune('0'+i))+"-blok"))
		extra = append(extra, block...)
	}
	extended := append(append([]byte{}, base...), extra...)
	second, err := p.Pack(extended)
	if err != nil {
		t.Fatal(err)
	}

	if second.Stats.CachedChunks != 4 {
		t.Fatalf("4 parca onbellekten gelmeliydi: %d", second.Stats.CachedChunks)
	}
	if second.Stats.NewChunks != 4 {
		t.Fatalf("4 yeni parca olmaliydi: %d", second.Stats.NewChunks)
	}
	// Tasarruf ~%50 olmali.
	if second.Stats.SavedRatio < 0.45 || second.Stats.SavedRatio > 0.55 {
		t.Fatalf("tasarruf orani ~0.5 olmaliydi: %f", second.Stats.SavedRatio)
	}
}

// TestUnpackWithCache, onbellekten karsilanan parcalarla birlestirmenin
// dogru calistigini dogrular.
func TestUnpackWithCache(t *testing.T) {
	key := testKey(t)
	cache := NewChunkCache(0)
	p := &Packer{Key: key, ChunkSize: 1024, Cache: cache}

	// Her parcasi farkli, boylece hepsi ilk gonderimde govdede gider.
	plain := distinctData(1024, 4)

	first, err := p.Pack(plain)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stats.NewChunks != 4 {
		t.Fatalf("ilk gonderimde 4 parca gitmeliydi: %d", first.Stats.NewChunks)
	}

	// Alici taraf parcalari onbellegine alir.
	recvCache := make(map[string][]byte)
	for i, ref := range first.Manifest.Chunks {
		recvCache[ref.Hash] = plain[i*1024 : (i+1)*1024]
	}

	// Ikinci gonderim: hepsi onbellekte, govde bos.
	second, err := p.Pack(plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Chunks) != 0 {
		t.Fatalf("govde bos olmaliydi: %d parca", len(second.Chunks))
	}

	got, err := Unpack(key, second.Manifest, second.Chunks, recvCache)
	if err != nil {
		t.Fatalf("onbellekli Unpack hata dondu: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("onbellekten birlestirilen veri bozuk")
	}
}

// TestManifestSignature, manifest imzasinin dogrulanabilir oldugunu ve
// degistirilirse tutmadigini dogrular.
func TestManifestSignature(t *testing.T) {
	key := testKey(t)
	secret := []byte("manifest-imza-anahtari-0123456789")
	p := &Packer{Key: key, ChunkSize: 1024, ManifestSecret: secret}

	packed, err := p.Pack([]byte(strings.Repeat("Z", 2048)))
	if err != nil {
		t.Fatal(err)
	}
	if packed.Manifest.Signature == "" {
		t.Fatal("manifest imzalanmadi")
	}
	if !VerifyManifestSignature(packed.Manifest, secret) {
		t.Fatal("imza dogrulanamadi")
	}

	// Boyutu degistir -> imza tutmamali.
	packed.Manifest.Chunks[0].Size = 1
	if VerifyManifestSignature(packed.Manifest, secret) {
		t.Fatal("degistirilmis manifest ayni imzayi uretti")
	}
}

// TestPackedManifestPassesServerVerification, istemcinin urettigi
// manifestin SUNUCU dogrulamasindan gectigini uctan uca kanitlar.
func TestPackedManifestPassesServerVerification(t *testing.T) {
	key := testKey(t)
	cache := NewChunkCache(0)
	p := &Packer{Key: key, ChunkSize: 2048, Cache: cache}

	plain := []byte(strings.Repeat("Aetheris ", 2000))
	packed, err := p.Pack(plain)
	if err != nil {
		t.Fatal(err)
	}

	res, err := dedup.VerifyManifest(packed.Manifest, packed.Chunks)
	if err != nil {
		t.Fatalf("sunucu dogrulamasi basarisiz: %v", err)
	}
	if res.TransferredBytes != packed.Stats.TransferredBytes {
		t.Fatalf("sunucu %d bayt olctu, istemci %d gonderdi",
			res.TransferredBytes, packed.Stats.TransferredBytes)
	}
	if res.ContentVerified {
		t.Fatal("sunucu icerik dogrulamasi iddia ediyor - IMKANSIZ")
	}
}

// TestChunkCacheConcurrent, onbellegin es zamanli kullanimda
// bozulmadigini dogrular. -race ile calistirilmalidir.
func TestChunkCacheConcurrent(t *testing.T) {
	cache := NewChunkCache(0)
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := strings.Repeat("a", 60) + string(rune('0'+i%10)) + "xyz"
			cache.Add(h)
			_ = cache.Has(h)
			_ = cache.Size()
		}(i)
	}
	wg.Wait()

	if cache.Size() == 0 {
		t.Fatal("onbellek bos kaldi")
	}
}

// TestChunkCacheEviction, onbellek tavaninin calistigini dogrular.
func TestChunkCacheEviction(t *testing.T) {
	cache := NewChunkCache(5)
	for i := 0; i < 20; i++ {
		cache.Add(strings.Repeat("b", 63) + string(rune('a'+i)))
	}
	if cache.Size() > 5 {
		t.Fatalf("onbellek tavani asildi: %d", cache.Size())
	}
}

func TestPackRejectsBadKey(t *testing.T) {
	p := &Packer{Key: []byte("kisa")}
	if _, err := p.Pack([]byte("veri")); err != ErrKeySize {
		t.Fatalf("ErrKeySize bekleniyordu: %v", err)
	}
}

func TestPackRejectsEmpty(t *testing.T) {
	p := &Packer{Key: testKey(t)}
	if _, err := p.Pack(nil); err != ErrEmptyPayload {
		t.Fatalf("ErrEmptyPayload bekleniyordu: %v", err)
	}
}

// distinctData, her parcasi FARKLI olan veri uretir.
// Ayni icerikli parcalar yuk ICINDE de tekillestirildigi icin (bkz.
// TestIntraPayloadDedup), parcalar arasi tasarrufu olcmek isteyen
// testlerin ayirt edilebilir veri kullanmasi gerekir.
func distinctData(chunkSize, chunks int) []byte {
	out := make([]byte, 0, chunkSize*chunks)
	for i := 0; i < chunks; i++ {
		block := bytes.Repeat([]byte{byte('A' + i%26)}, chunkSize)
		// Her blogun basina benzersiz bir imza koy.
		copy(block, []byte(strings.Repeat("#", 1)+string(rune('0'+i%10))+"-blok-"))
		out = append(out, block...)
	}
	return out
}

// TestIntraPayloadDedup, TEK BIR yuk icinde tekrar eden parcalarin da
// tekillestirildigini dogrular.
//
// Bu davranis testler yazilirken kesfedildi: 8 KiB'lik ayni karakterden
// olusan veri, 1 KiB parcalarla bolununce 8 OZDES parca uretir ve ilk
// gonderimde bile 7'si onbellekten karsilanir. Bu dogrudur ve degerlidir
// (tekrar eden blok yapisi olan dosyalarda tasarruf saglar), ama testler
// bunu hesaba katmalidir.
func TestIntraPayloadDedup(t *testing.T) {
	key := testKey(t)
	cache := NewChunkCache(0)
	p := &Packer{Key: key, ChunkSize: 1024, Cache: cache}

	// 8 KiB ayni karakter -> 8 ozdes parca
	packed, err := p.Pack(bytes.Repeat([]byte("X"), 8192))
	if err != nil {
		t.Fatal(err)
	}
	if packed.Stats.TotalChunks != 8 {
		t.Fatalf("TotalChunks = %d, beklenen 8", packed.Stats.TotalChunks)
	}
	if packed.Stats.NewChunks != 1 {
		t.Fatalf("NewChunks = %d, beklenen 1 (ilki disindakiler tekrar)",
			packed.Stats.NewChunks)
	}
	if packed.Stats.CachedChunks != 7 {
		t.Fatalf("CachedChunks = %d, beklenen 7", packed.Stats.CachedChunks)
	}
	// 8 parcanin 7'si gonderilmedi -> ~%87.5 tasarruf
	if packed.Stats.SavedRatio < 0.8 {
		t.Fatalf("SavedRatio = %f, beklenen >0.8", packed.Stats.SavedRatio)
	}
}
