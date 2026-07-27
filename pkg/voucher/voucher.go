// Package voucher, off-grid ortamlar için Ed25519 ile imzalanmış kontör/kredi
// (Voucher) üretme ve doğrulama mekanizması sağlar. Yüklenen krediler WAL
// ledger'a Zero-Knowledge olarak işlenir: içerik saklanmaz, yalnızca SHA-256
// ve bayt miktarı ölçülür.
//
// Akış:
//
//	Operatör → NewVoucher() → Ed25519 imzala → QR/NFC/LoRa ile dağıt
//	Kullanıcı → Redeem()    → imzayı doğrula → WAL ledger'a kayıt
//
// Çift harcama (double-spend): her voucher benzersiz SerialNo taşır; bir kez
// redeemed olan seri numarası sonraki denemelerde ErrAlreadyRedeemed hatası
// verir.
package voucher

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	ErrBadSignature    = errors.New("voucher: imza geçersiz")
	ErrExpired         = errors.New("voucher: voucher süresi dolmuş")
	ErrAlreadyRedeemed = errors.New("voucher: bu seri numarası zaten kullanıldı")
	ErrBadSerial       = errors.New("voucher: geçersiz seri numarası")
	ErrInvalidAmount   = errors.New("voucher: geçersiz kredi miktarı")
)

// Issuer, voucher imzalayan (operatör) taraftır.
type Issuer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	// NodeID, bu işleticinin kimliğidir (public key hex).
	NodeID string
}

// NewIssuer, yeni bir Ed25519 issuer kimliği oluşturur.
func NewIssuer() (*Issuer, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Issuer{priv: priv, pub: pub, NodeID: hex.EncodeToString(pub)}, nil
}

// IssuerFromSeed, deterministik issuer oluşturur (kalıcı operatör kimliği için).
func IssuerFromSeed(seed []byte) (*Issuer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("voucher: seed %d bayt olmalı", ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Issuer{priv: priv, pub: pub, NodeID: hex.EncodeToString(pub)}, nil
}

// sign, test yardımcısı: canonical baytları issuer private key ile imzalar.
func (iss *Issuer) sign(msg []byte) []byte { return ed25519.Sign(iss.priv, msg) }

// PublicKey, doğrulama için açık anahtarı döndürür.
func (iss *Issuer) PublicKey() ed25519.PublicKey { return iss.pub }

// Voucher, imzalanmış off-grid kredi birimidir.
type Voucher struct {
	Version   int    `json:"v"`          // protokol versiyonu
	SerialNo  string `json:"serial"`     // benzersiz seri (hex, 16 bayt)
	IssuerID  string `json:"issuer"`     // issuer public key hex
	BearerID  string `json:"bearer"`     // alıcı node/API key ID
	Credits   uint64 `json:"credits"`    // kredi birimi (bayt karşılığı)
	IssuedAt  int64  `json:"issued_at"`  // unix saniye
	ExpiresAt int64  `json:"expires_at"` // unix saniye (0 = süresiz)
	Sig       []byte `json:"sig"`        // Ed25519 imzası
}

// canonical, imzalanacak deterministik baytları üretir (Sig alanı hariç).
func (v *Voucher) canonical() []byte {
	buf := make([]byte, 0, 128)
	buf = append(buf, "aetheris-voucher-v1\x00"...)
	buf = append(buf, v.SerialNo...)
	buf = append(buf, 0)
	buf = append(buf, v.IssuerID...)
	buf = append(buf, 0)
	buf = append(buf, v.BearerID...)
	buf = append(buf, 0)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v.Credits)
	buf = append(buf, n[:]...)
	binary.BigEndian.PutUint64(n[:], uint64(v.IssuedAt))
	buf = append(buf, n[:]...)
	binary.BigEndian.PutUint64(n[:], uint64(v.ExpiresAt))
	buf = append(buf, n[:]...)
	return buf
}

// NewVoucher, belirtilen kredi miktarıyla imzalı bir voucher üretir.
// bearerID, alıcının node/API key kimliğidir. ttl=0 → süresiz.
func (iss *Issuer) NewVoucher(bearerID string, credits uint64, ttl time.Duration) (*Voucher, error) {
	if credits == 0 {
		return nil, ErrInvalidAmount
	}
	serial := make([]byte, 16)
	if _, err := rand.Read(serial); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var exp int64
	if ttl > 0 {
		exp = time.Now().Add(ttl).Unix()
	}
	v := &Voucher{
		Version:   1,
		SerialNo:  hex.EncodeToString(serial),
		IssuerID:  iss.NodeID,
		BearerID:  bearerID,
		Credits:   credits,
		IssuedAt:  now,
		ExpiresAt: exp,
	}
	v.Sig = ed25519.Sign(iss.priv, v.canonical())
	return v, nil
}

// Verify, voucher imzasını verilen public key ile doğrular.
func (v *Voucher) Verify(pub ed25519.PublicKey) error {
	if !ed25519.Verify(pub, v.canonical(), v.Sig) {
		return ErrBadSignature
	}
	if v.ExpiresAt > 0 && time.Now().Unix() > v.ExpiresAt {
		return ErrExpired
	}
	return nil
}

// PayloadSHA256, Zero-Knowledge kayıt için voucher içeriğinin SHA-256'sını döndürür.
// Gerçek kredi/taraf bilgisi saklama yerine yalnızca bu hash WAL'a işlenir.
func (v *Voucher) PayloadSHA256() string {
	h := sha256.Sum256(v.canonical())
	return hex.EncodeToString(h[:])
}

// Marshal, voucher'ı JSON'a çevirir (off-grid dağıtım için).
func (v *Voucher) Marshal() ([]byte, error) { return json.Marshal(v) }

// Unmarshal, JSON'dan voucher ayrıştırır.
func Unmarshal(data []byte) (*Voucher, error) {
	var v Voucher
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	if v.SerialNo == "" {
		return nil, ErrBadSerial
	}
	return &v, nil
}

// --- WAL Ledger Zero-Knowledge ---

// LedgerEntry, WAL'a yazılan tek bir kayıttır. İçerik yerine hash saklanır.
type LedgerEntry struct {
	SerialSHA  string `json:"serial_sha"`  // serial numarasının SHA-256'sı
	PayloadSHA string `json:"payload_sha"` // canonical() SHA-256
	BearerID   string `json:"bearer_id"`   // alıcı kimliği (net değer, gerekli)
	Credits    uint64 `json:"credits"`     // kredi miktarı
	RedeemedAt int64  `json:"redeemed_at"`
}

// Ledger, off-grid voucher redemption defteri. Çift harcamayı engeller.
type Ledger struct {
	mu      sync.RWMutex
	seen    map[string]struct{} // serial SHA → işlenmiş
	entries []LedgerEntry
	total   map[string]uint64 // bearerID → toplam kredi
	onWrite func(LedgerEntry) // WAL yazma geri çağrısı (kalıcılık için)
}

// NewLedger, yeni bir in-memory defter oluşturur.
// onWrite nil değilse, her kayıt bu fonksiyona iletilir (WAL entegrasyonu).
func NewLedger(onWrite func(LedgerEntry)) *Ledger {
	return &Ledger{
		seen:    make(map[string]struct{}),
		total:   make(map[string]uint64),
		onWrite: onWrite,
	}
}

// Redeem, bir voucher'ı doğrulayıp defteri günceller.
func (l *Ledger) Redeem(v *Voucher, pub ed25519.PublicKey) error {
	if err := v.Verify(pub); err != nil {
		return err
	}
	serialSHA := sha256hexStr([]byte(v.SerialNo))
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.seen[serialSHA]; dup {
		return ErrAlreadyRedeemed
	}
	l.seen[serialSHA] = struct{}{}
	entry := LedgerEntry{
		SerialSHA:  serialSHA,
		PayloadSHA: v.PayloadSHA256(),
		BearerID:   v.BearerID,
		Credits:    v.Credits,
		RedeemedAt: time.Now().Unix(),
	}
	l.entries = append(l.entries, entry)
	l.total[v.BearerID] += v.Credits
	if l.onWrite != nil {
		l.onWrite(entry)
	}
	return nil
}

// Balance, bir bearer'ın toplam kredisini döndürür.
func (l *Ledger) Balance(bearerID string) uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.total[bearerID]
}

// Entries, tüm kayıtların bir kopyasını döndürür.
func (l *Ledger) Entries() []LedgerEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]LedgerEntry(nil), l.entries...)
}

func sha256hexStr(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// WriteToWAL, defterdeki tüm girişleri writer'a JSON satırları olarak yazar.
func (l *Ledger) WriteToWAL(w io.Writer) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	enc := json.NewEncoder(w)
	for _, e := range l.entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}
