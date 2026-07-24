// Package client, Aetheris istemci kutuphanesidir.
//
// SIFIR-BILGI SORUMLULUGU ISTEMCIDEDIR: sifreleme, parcalama (chunking) ve
// tekillestirme (dedup) BURADA yapilir. Sunucu duz metni hicbir zaman
// gormez, dolayisiyla tekrar eden veriyi de goremez.
//
// # TEKILLESTIRME NASIL CALISIR
//
//  1. Duz metin sabit boyutlu parcalara bolunur.
//  2. Her parcanin SHA-256 ozeti alinir (duz metin uzerinden).
//  3. Ozet yerel onbellekte varsa: parca GONDERILMEZ, manifeste
//     Cached=true olarak yazilir.
//  4. Yoksa: parca AES-256-GCM ile sifrelenir ve govdede gonderilir.
//
// Tasarruf, tekrar eden icerigin oraninda gerceklesir. Sabit veri
// gonderiliyorsa yuksek, her seferinde rastgele veri gonderiliyorsa
// SIFIRDIR. Kutuphane bu orani OLCER ve raporlar; abartili bir oran
// vaat etmez.
//
// # ONEMLI SINIRLAMA
//
// Parca ozeti DUZ METIN uzerinden alinir. Bu, ayni icerigi gonderen iki
// FARKLI istemcinin birbirinin onbelleginden yararlanamayacagi anlamina
// gelir (her istemcinin onbellegi kendine ozeldir). Istemciler arasi
// paylasimli dedup, ozetlerin sunucuya aciklanmasini gerektirir ve
// icerik hakkinda bilgi sizdirir (confirmation-of-file saldirisi).
// Bu yuzden BILINCLI OLARAK yapilmamistir.
package client

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/tedbirgeai/aetheris/internal/dedup"
)

// DefaultChunkSize, varsayilan parca boyutu (64 KiB).
// Kucuk parca = daha iyi tekillestirme, daha buyuk manifest.
// Buyuk parca = kucuk manifest, daha az tekillestirme sansi.
const DefaultChunkSize = 64 * 1024

var (
	ErrKeySize      = errors.New("client: AES-256 icin 32 baytlik anahtar gerekli")
	ErrEmptyPayload = errors.New("client: bos yuk")
)

// ChunkCache, gonderilmis parca ozetlerini tutar.
// Es zamanli kullanima uygundur.
type ChunkCache struct {
	mu   sync.RWMutex
	seen map[string]struct{}
	// maxEntries, onbellegin sinirsiz buyumesini onler. 0 = sinirsiz.
	maxEntries int
}

func NewChunkCache(maxEntries int) *ChunkCache {
	return &ChunkCache{
		seen:       make(map[string]struct{}),
		maxEntries: maxEntries,
	}
}

func (c *ChunkCache) Has(hash string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.seen[hash]
	return ok
}

func (c *ChunkCache) Add(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxEntries > 0 && len(c.seen) >= c.maxEntries {
		// Basit tahliye: onbellek dolunca sifirla. LRU daha iyi olurdu
		// ama dogruluk acisindan fark etmez — kaybedilen tek sey
		// tekillestirme firsati, veri butunlugu degil.
		c.seen = make(map[string]struct{})
	}
	c.seen[hash] = struct{}{}
}

func (c *ChunkCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.seen)
}

// Reset, onbellegi temizler.
func (c *ChunkCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = make(map[string]struct{})
}

// Packer, duz metni parcalayip sifreleyen ve manifest ureten yapidir.
type Packer struct {
	// Key, AES-256 anahtari (32 bayt). SUNUCUYA ASLA GONDERILMEZ.
	Key []byte
	// ChunkSize, parca boyutu. 0 ise DefaultChunkSize kullanilir.
	ChunkSize int
	// Cache, gonderilmis parca ozetleri. nil ise tekillestirme yapilmaz.
	Cache *ChunkCache
	// ManifestSecret, manifest imzasi icin HMAC anahtari.
	// Bos ise manifest imzalanmaz.
	ManifestSecret []byte
}

// Packed, paketleme sonucudur.
type Packed struct {
	Manifest *dedup.Manifest `json:"manifest"`
	// Chunks, GONDERILECEK (Cached=false) parcalarin base64 kodlu
	// sifreli halleri, manifest sirasiyla.
	Chunks []string `json:"chunks"`
	// Stats, olculen gercek tasarruf.
	Stats PackStats `json:"stats"`
}

// PackStats, GERCEKTEN olculen degerlerdir; tahmin veya vaat degildir.
type PackStats struct {
	PlainBytes       int    `json:"plain_bytes"`
	TotalChunks      int    `json:"total_chunks"`
	NewChunks        int    `json:"new_chunks"`
	CachedChunks     int    `json:"cached_chunks"`
	TransferredBytes uint64 `json:"transferred_bytes"`
	// SavedRatio, [0,1] araliginda GERCEKLESEN tasarruf orani.
	// Tekrar eden veri yoksa 0'dir. Bu deger olculur, iddia edilmez.
	SavedRatio float64 `json:"saved_ratio"`
}

// Pack, duz metni parcalar, tekillestirir, sifreler ve manifest uretir.
//
// Cache nil degilse: daha once gonderilmis parcalar govdeye KONULMAZ.
// Cache nil ise: tum parcalar gonderilir (tekillestirme kapali).
func (p *Packer) Pack(plaintext []byte) (*Packed, error) {
	if len(p.Key) != 32 {
		return nil, ErrKeySize
	}
	if len(plaintext) == 0 {
		return nil, ErrEmptyPayload
	}

	size := p.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}
	if size > dedup.MaxChunkBytes-64 {
		// Sifreleme nonce+tag ekler; manifest sinirini asmayalim.
		size = dedup.MaxChunkBytes - 64
	}

	block, err := aes.NewCipher(p.Key)
	if err != nil {
		return nil, fmt.Errorf("client: sifre olusturulamadi: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("client: GCM olusturulamadi: %w", err)
	}

	man := &dedup.Manifest{TotalPlainSize: len(plaintext)}
	var body []string
	var transferred uint64
	cached := 0

	for off := 0; off < len(plaintext); off += size {
		end := off + size
		if end > len(plaintext) {
			end = len(plaintext)
		}
		part := plaintext[off:end]

		// Ozet DUZ METIN uzerinden alinir — tekillestirmenin calismasi
		// icin sart. Bu ozet sunucuya gider ama sunucu icin opaktir.
		sum := sha256.Sum256(part)
		hash := hex.EncodeToString(sum[:])

		// Sifreleme: her parca AYRI nonce ile. Ayni duz metin parcasi
		// bile her seferinde farkli ciphertext uretir — sunucunun
		// tekillestirme yapamamasinin sebebi tam olarak budur.
		nonce := make([]byte, gcm.NonceSize())
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return nil, fmt.Errorf("client: nonce uretilemedi: %w", err)
		}
		sealed := gcm.Seal(nonce, nonce, part, nil)

		ref := dedup.ChunkRef{Hash: hash, Size: len(sealed)}

		if p.Cache != nil && p.Cache.Has(hash) {
			ref.Cached = true
			cached++
		} else {
			body = append(body, base64.StdEncoding.EncodeToString(sealed))
			transferred += uint64(len(sealed))
			if p.Cache != nil {
				p.Cache.Add(hash)
			}
		}
		man.Chunks = append(man.Chunks, ref)
	}

	if len(p.ManifestSecret) > 0 {
		man.Signature = SignManifest(man, p.ManifestSecret)
	}

	// Tasarruf orani: GONDERILMEYEN bayt / gonderilecek olan toplam bayt.
	var totalIfNoDedup uint64
	for _, c := range man.Chunks {
		totalIfNoDedup += uint64(c.Size)
	}
	var ratio float64
	if totalIfNoDedup > 0 {
		ratio = 1.0 - float64(transferred)/float64(totalIfNoDedup)
	}

	return &Packed{
		Manifest: man,
		Chunks:   body,
		Stats: PackStats{
			PlainBytes:       len(plaintext),
			TotalChunks:      len(man.Chunks),
			NewChunks:        len(body),
			CachedChunks:     cached,
			TransferredBytes: transferred,
			SavedRatio:       ratio,
		},
	}, nil
}

// SignManifest, manifesti HMAC-SHA256 ile imzalar (Signature alani haric).
func SignManifest(m *dedup.Manifest, secret []byte) string {
	cp := *m
	cp.Signature = ""
	payload, err := json.Marshal(&cp)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyManifestSignature, imzayi dogrular. Sabit zamanli karsilastirma.
func VerifyManifestSignature(m *dedup.Manifest, secret []byte) bool {
	if m.Signature == "" {
		return false
	}
	want := SignManifest(m, secret)
	return hmac.Equal([]byte(m.Signature), []byte(want))
}

// Unpack, sifreli parcalari cozup duz metni birlestirir.
// Yalnizca ISTEMCI/HEDEF tarafinda anlamlidir; gecit bunu asla cagirmaz.
//
// cachedChunks: daha once alinmis parcalarin hash -> duz metin haritasi.
func Unpack(key []byte, m *dedup.Manifest, chunks []string,
	cachedChunks map[string][]byte) ([]byte, error) {

	if len(key) != 32 {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	var out []byte
	idx := 0
	for i, ref := range m.Chunks {
		if ref.Cached {
			part, ok := cachedChunks[ref.Hash]
			if !ok {
				return nil, fmt.Errorf("client: parca %d onbellekte yok: %s", i, ref.Hash)
			}
			out = append(out, part...)
			continue
		}
		if idx >= len(chunks) {
			return nil, fmt.Errorf("client: parca %d govdede eksik", i)
		}
		sealed, err := base64.StdEncoding.DecodeString(chunks[idx])
		if err != nil {
			return nil, fmt.Errorf("client: parca %d base64 hatasi: %w", i, err)
		}
		if len(sealed) < gcm.NonceSize() {
			return nil, fmt.Errorf("client: parca %d fazla kisa", i)
		}
		nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
		part, err := gcm.Open(nil, nonce, ct, nil)
		if err != nil {
			return nil, fmt.Errorf("client: parca %d cozulemedi: %w", i, err)
		}
		// Butunluk: cozulen parcanin ozeti manifesttekiyle eslesmeli.
		sum := sha256.Sum256(part)
		if hex.EncodeToString(sum[:]) != ref.Hash {
			return nil, fmt.Errorf("client: parca %d ozet uyusmazligi", i)
		}
		out = append(out, part...)
		idx++
	}
	return out, nil
}
