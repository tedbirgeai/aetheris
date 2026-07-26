// Package ephemeral, RF/Seri (LoRa) katmaninda IP ve MAC adresini TAMAMEN
// baypas eden, SIFIR-KVKK / Zero-Knowledge anonim cerceveleme saglar.
//
// Havaya cikan her cerceve su bicimdedir:
//
//	+--------+------------------+-----------+------------------+
//	| Magic  | Ephemeral Dest   | AES Nonce | AES-256-GCM      |
//	| /Flags | Hash (8 bayt)    | (12 bayt) | sifreli payload  |
//	| 1 bayt | DONEN            |           | (auth tag dahil) |
//	+--------+------------------+-----------+------------------+
//
// Hedef Hash, dugum kimligi + ZAMAN EPOCH'undan turetilir ve her epoch'ta
// DONER. Boylece telde kalici bir tanimlayici (IP/MAC/sabit kimlik) bulunmaz;
// bir dinleyici yalnizca cari epoch icin kendi hash'ini hesaplayip eslesirse
// paketin kendisine ait oldugunu anlar. Disaridan bakan biri icin ayni
// dugumun ardisik paketleri BAGLANAMAZ (unlinkability) — kisisel veri zirhi.
package ephemeral

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

const (
	// MagicV1, cerceve surum/protokol imzasi (Flags ust 4 bit icin ayrilmis).
	MagicV1 byte = 0xAE

	destHashLen = 8
	nonceLen    = 12
	headerLen   = 1 + destHashLen + nonceLen // magic + dest + nonce

	// KeySize, AES-256 anahtar boyutu.
	KeySize = 32

	// EpochDuration, hedef hash'inin donme periyodu. Kisa = daha iyi
	// unlinkability, ama alici saat kaymasina daha az tolerans.
	EpochDuration = 60 * time.Second
)

var (
	ErrBadKey     = errors.New("ephemeral: anahtar 32 bayt olmali")
	ErrShortFrame = errors.New("ephemeral: cerceve cok kisa")
	ErrBadMagic   = errors.New("ephemeral: gecersiz magic")
	ErrNotForMe   = errors.New("ephemeral: hedef hash eslesmiyor")
	ErrAuth       = errors.New("ephemeral: kimlik dogrulama basarisiz (bozuk/sahte)")
)

// Epoch, verilen zaman icin epoch numarasini dondurur.
func Epoch(t time.Time) int64 {
	return t.UnixNano() / int64(EpochDuration)
}

// DestHash, bir dugum kimligi ve epoch icin DONEN 8-baytlik hedef hash'i
// uretir. IP/MAC yok; yalnizca kimlik + zaman.
func DestHash(nodeID string, epoch int64) [destHashLen]byte {
	var e [8]byte
	binary.BigEndian.PutUint64(e[:], uint64(epoch))
	h := sha256.New()
	_, _ = io.WriteString(h, "aetheris-ephemeral-v1:")
	_, _ = io.WriteString(h, nodeID)
	_, _ = h.Write(e[:])
	sum := h.Sum(nil)
	var out [destHashLen]byte
	copy(out[:], sum[:destHashLen])
	return out
}

// Seal, payload'i AES-256-GCM ile sifreleyip anonim cerceveye sarar. destID
// hedef dugumun kimligidir; hedef hash cari epoch'a gore hesaplanir.
func Seal(key []byte, destID string, payload []byte, now time.Time) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrBadKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	dh := DestHash(destID, Epoch(now))

	out := make([]byte, 0, headerLen+len(payload)+aead.Overhead())
	out = append(out, MagicV1)
	out = append(out, dh[:]...)
	out = append(out, nonce...)
	// AAD olarak basligi (magic+dest+nonce) baglayarak butunlugu koru.
	aad := out[:headerLen]
	out = aead.Seal(out, nonce, payload, aad)
	return out, nil
}

// Open, gelen ham cerceveyi cozer. myID alicinin kimligidir. Cerceve alicinin
// cari (veya bir onceki, saat kaymasi toleransi) epoch hash'iyle eslesirse
// payload cozulur. Eslesmezse ErrNotForMe.
func Open(key []byte, myID string, raw []byte, now time.Time) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrBadKey
	}
	if len(raw) < headerLen {
		return nil, ErrShortFrame
	}
	if raw[0] != MagicV1 {
		return nil, ErrBadMagic
	}
	dst := raw[1 : 1+destHashLen]
	nonce := raw[1+destHashLen : headerLen]
	ct := raw[headerLen:]

	// Cari ve bir onceki epoch icin hedef hash'i dene (saat kaymasi toleransi).
	epoch := Epoch(now)
	match := false
	for _, e := range []int64{epoch, epoch - 1} {
		mh := DestHash(myID, e)
		if subtle.ConstantTimeCompare(dst, mh[:]) == 1 {
			match = true
			break
		}
	}
	if !match {
		return nil, ErrNotForMe
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	aad := raw[:headerLen]
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrAuth
	}
	return pt, nil
}

// Overhead, bir payload'a eklenen toplam ek yuk (baslik + GCM tag).
func Overhead() int { return headerLen + 16 }
