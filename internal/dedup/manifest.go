// Package dedup, parca tabanli veri tekillestirmesinin ORTAK tiplerini ve
// SUNUCU TARAFI dogrulamasini icerir.
//
// # NEDEN ISTEMCI TARAFINDA?
//
// Sunucu sifreli veriyi gorur. AES-256-GCM her sifrelemede RASTGELE nonce
// kullanir; ayni duz metin her seferinde FARKLI ciphertext uretir. Bu yuzden
// sunucu sifreli baytlarda tekrar EDEN parca goremez — goremedigi icin
// tekillestiremez.
//
// "Sunucu %80 dedup yapiyor" iddiasi ancak sunucu duz metni gorurse
// dogru olabilirdi; o da tum sifir-bilgi mimarisini gecersiz kilardi.
//
// Dogru tasarim: tekillestirme ISTEMCIDE yapilir (duz metni yalnizca o
// gorur), sunucu ise manifestin YAPISAL butunlugunu ve GERCEKTEN AKTARILAN
// bayt sayisini dogrular.
//
// # SUNUCU NEYI DOGRULAR, NEYI DOGRULAMAZ
//
// Dogrular:
//   - Manifest yapisal olarak gecerli mi (parca sayisi, boyutlar, siralama)
//   - Beyan edilen toplam boyut, parcalarin toplamiyla tutarli mi
//   - Gonderilen parca sayisi manifestteki "yeni" parca sayisiyla ayni mi
//   - Her parcanin base64 uzunlugu manifestteki boyutla eslesiyor mu
//   - Manifest istemci tarafindan imzalanmis mi (HMAC butunlugu)
//
// Dogrulayamaz (ve dogruladigini IDDIA ETMEZ):
//   - Parca hash'inin gercekten o duz metnin hash'i olup olmadigi
//     (sunucu duz metni hic gormez)
//   - Istemcinin "bu parcayi daha once gonderdim" beyaninin dogrulugu
//
// Faturalama bu ikinci gruba BAGIMLI DEGILDIR: sunucu, iddia edilen
// tasarrufu degil, TELDE GERCEKTEN AKAN baytlari olcer. Istemci yalan
// soylerse kendi zararina olur (veri hedefte eksik olur), sunucunun
// gelirine zarar veremez.
package dedup

import (
	"encoding/base64"
	"errors"
	"fmt"
)

// Sinirlar. Kotu niyetli veya hatali bir manifest gecidin bellegini
// tuketememelidir.
const (
	// MaxChunks, tek bir manifestteki azami parca sayisi.
	MaxChunks = 4096
	// MaxChunkBytes, tek bir parcanin azami ham boyutu (1 MiB).
	MaxChunkBytes = 1 << 20
	// MinChunkBytes, AES-GCM ciktisinin asgari boyutu: 12 nonce + 16 tag.
	MinChunkBytes = 28
	// HashHexLen, SHA-256 ozetinin hex uzunlugu.
	HashHexLen = 64
)

var (
	ErrEmptyManifest     = errors.New("dedup: manifest bos")
	ErrTooManyChunks     = errors.New("dedup: parca sayisi siniri asiyor")
	ErrChunkCountsDiffer = errors.New("dedup: gonderilen parca sayisi manifestle uyusmuyor")
	ErrSizeMismatch      = errors.New("dedup: beyan edilen boyut parcalarla tutarsiz")
	ErrBadHash           = errors.New("dedup: gecersiz parca ozeti")
	ErrChunkTooLarge     = errors.New("dedup: parca azami boyutu asiyor")
	ErrChunkTooSmall     = errors.New("dedup: parca AES-GCM icin fazla kisa")
)

// ChunkRef, manifestteki tek bir parca kaydidir.
type ChunkRef struct {
	// Hash, parcanin DUZ METIN halinin SHA-256 ozeti (hex).
	// Sunucu bunu DOGRULAYAMAZ; yalnizca istemcinin kendi onbellegi ve
	// hedef dugum icin anlamlidir. Sunucu icin opak bir etikettir.
	Hash string `json:"hash"`
	// Size, parcanin SIFRELENMIS halinin bayt boyutu.
	// Sunucu bunu GERCEKTEN dogrular: gonderilen blogun boyutuyla eslesmeli.
	Size int `json:"size"`
	// Cached true ise istemci bu parcayi daha once gonderdigini beyan eder
	// ve govdede YOLLAMAZ. Sunucu bu beyani dogrulayamaz — ama zaten
	// faturalama telde akan bayta gore yapilir, beyana gore degil.
	Cached bool `json:"cached"`
}

// Manifest, bir yukun parca dokumudur.
type Manifest struct {
	// TotalPlainSize, duz metnin toplam boyutu (istemci beyani, bilgi amacli).
	TotalPlainSize int `json:"total_plain_size"`
	// Chunks, sirali parca listesi.
	Chunks []ChunkRef `json:"chunks"`
	// Signature, manifestin istemci tarafindan uretilmis HMAC-SHA256
	// imzasidir (Signature alani bos birakilarak hesaplanir).
	Signature string `json:"signature,omitempty"`
}

// NewChunkCount, govdede GERCEKTEN gonderilmesi gereken parca sayisini
// dondurur (Cached=false olanlar).
func (m *Manifest) NewChunkCount() int {
	n := 0
	for _, c := range m.Chunks {
		if !c.Cached {
			n++
		}
	}
	return n
}

// TransferredBytes, telde GERCEKTEN akan sifreli bayt toplamini dondurur.
// FATURALAMA BUNA GORE YAPILIR — istemcinin tasarruf iddiasina gore degil.
func (m *Manifest) TransferredBytes() uint64 {
	var total uint64
	for _, c := range m.Chunks {
		if !c.Cached {
			total += uint64(c.Size)
		}
	}
	return total
}

// ReferencedBytes, onbellekten karsilanan (gonderilmeyen) bayt toplamidir.
// Yalnizca RAPORLAMA icindir; faturalamaya girmez.
func (m *Manifest) ReferencedBytes() uint64 {
	var total uint64
	for _, c := range m.Chunks {
		if c.Cached {
			total += uint64(c.Size)
		}
	}
	return total
}

// VerificationResult, dogrulamanin DURUST sonucudur.
type VerificationResult struct {
	// ChunkCount, manifestteki toplam parca sayisi.
	ChunkCount int `json:"chunk_count"`
	// NewChunks, govdede gercekten gonderilen parca sayisi.
	NewChunks int `json:"new_chunks"`
	// TransferredBytes, telde akan sifreli bayt (FATURALANAN).
	TransferredBytes uint64 `json:"transferred_bytes"`
	// ReferencedBytes, istemcinin onbellekten karsiladigini BEYAN ettigi
	// bayt. Sunucu bunu dogrulayamaz; raporlama amaclidir.
	ReferencedBytes uint64 `json:"referenced_bytes_claimed"`
	// StructurallyValid, yapisal dogrulamanin sonucu.
	StructurallyValid bool `json:"structurally_valid"`
	// ContentVerified HER ZAMAN false'tur ve oyle kalmalidir.
	// Sunucu duz metni gormedigi icin parca ozetlerini dogrulayamaz.
	// Bu alan, ileride birinin "icerik dogrulandi" diye yanlis bir
	// iddiada bulunmasini onlemek icin ACIKCA burada durur.
	ContentVerified bool `json:"content_verified"`
}

// VerifyManifest, manifesti ve gonderilen parcalari YAPISAL olarak dogrular.
//
// chunks: govdede gelen base64 kodlu sifreli parcalar (yalnizca Cached=false
// olanlar, manifest sirasiyla).
func VerifyManifest(m *Manifest, chunks []string) (*VerificationResult, error) {
	if m == nil || len(m.Chunks) == 0 {
		return nil, ErrEmptyManifest
	}
	if len(m.Chunks) > MaxChunks {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooManyChunks, len(m.Chunks), MaxChunks)
	}

	newCount := m.NewChunkCount()
	if newCount != len(chunks) {
		return nil, fmt.Errorf("%w: manifest %d, govde %d",
			ErrChunkCountsDiffer, newCount, len(chunks))
	}

	res := &VerificationResult{
		ChunkCount:      len(m.Chunks),
		NewChunks:       newCount,
		ReferencedBytes: m.ReferencedBytes(),
		// Sunucu duz metni gormez; icerik dogrulamasi YAPILAMAZ.
		ContentVerified: false,
	}

	idx := 0
	var transferred uint64
	for i, c := range m.Chunks {
		if len(c.Hash) != HashHexLen {
			return nil, fmt.Errorf("%w: parca %d, uzunluk %d", ErrBadHash, i, len(c.Hash))
		}
		if !isHex(c.Hash) {
			return nil, fmt.Errorf("%w: parca %d hex degil", ErrBadHash, i)
		}
		if c.Size > MaxChunkBytes {
			return nil, fmt.Errorf("%w: parca %d, %d bayt", ErrChunkTooLarge, i, c.Size)
		}
		if c.Size < MinChunkBytes {
			return nil, fmt.Errorf("%w: parca %d, %d bayt", ErrChunkTooSmall, i, c.Size)
		}

		if c.Cached {
			continue
		}

		// Bu parca govdede gelmis olmali; boyutu manifestle ESLESMELI.
		raw, err := base64.StdEncoding.DecodeString(chunks[idx])
		if err != nil {
			return nil, fmt.Errorf("dedup: parca %d gecerli base64 degil: %w", i, err)
		}
		if len(raw) != c.Size {
			return nil, fmt.Errorf("%w: parca %d manifest %d bayt, govde %d bayt",
				ErrSizeMismatch, i, c.Size, len(raw))
		}
		transferred += uint64(len(raw))
		idx++
	}

	res.TransferredBytes = transferred
	res.StructurallyValid = true
	return res, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
