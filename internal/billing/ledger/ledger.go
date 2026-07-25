// Package ledger, INTERNETSIZ ortamlarda gecerli, Ed25519 dijital imzali
// bakiye/fis (voucher) ve role kredisi muhasebesidir.
//
// Senaryo: Off-grid mesh'te A dugumu, B dugumunun trafigini C'ye tasir. B,
// A'ya "senin benim icin N bayt tasidigini kabul ediyorum" diyen KRIPTOGRAFIK
// bir fis (Local Signed Receipt) imzalar. A bu fisleri biriktirir; her fis,
// A'nin role kredisinin (Relay Credit) matematiksel kanitidir. Hicbir merkezi
// sunucu veya internet gerekmez — dogrulama tamamen yereldir.
package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/tedbirgeai/aetheris/internal/security"
)

var (
	ErrBadReceiptSig = errors.New("ledger: fis imzasi gecersiz")
	ErrDuplicate     = errors.New("ledger: fis zaten islenmis (cift harcama)")
	ErrSelfRelay     = errors.New("ledger: dugum kendi trafigi icin kredi kazanamaz")
	ErrBadVoucher    = errors.New("ledger: voucher imzasi gecersiz")
	ErrVoucherSpent  = errors.New("ledger: voucher zaten harcanmis")
)

// Receipt, "OriginID, RelayerID'nin kendisi icin Bytes bayt tasidigini kabul
// eder" beyanidir. Origin tarafindan Ed25519 ile imzalanir.
type Receipt struct {
	RelayerID string `json:"relayer_id"` // trafigi tasiyan (kredi kazanan)
	OriginID  string `json:"origin_id"`  // trafigin sahibi (fisi imzalayan)
	Bytes     uint64 `json:"bytes"`      // tasinan bayt
	Nonce     uint64 `json:"nonce"`      // cift-harcama engelleme
	IssuedAt  int64  `json:"issued_at"`  // unix saniye
	Sig       []byte `json:"sig"`        // OriginID'nin imzasi
}

// canonical, imzalanacak/dogrulanacak deterministik baytlari uretir (imza
// alani HARIC). Alan sirasi ve bicimi sabittir; platformdan bagimsizdir.
func (r Receipt) canonical() []byte {
	buf := make([]byte, 0, 128)
	buf = append(buf, "aetheris-receipt-v1\x00"...)
	buf = append(buf, r.RelayerID...)
	buf = append(buf, 0)
	buf = append(buf, r.OriginID...)
	buf = append(buf, 0)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], r.Bytes)
	buf = append(buf, n[:]...)
	binary.BigEndian.PutUint64(n[:], r.Nonce)
	buf = append(buf, n[:]...)
	binary.BigEndian.PutUint64(n[:], uint64(r.IssuedAt))
	buf = append(buf, n[:]...)
	return buf
}

// ID, fisin benzersiz kimligi (cift-harcama anahtari): origin + nonce.
func (r Receipt) ID() string {
	h := sha256.Sum256([]byte(r.OriginID + ":" + hex.EncodeToString(u64(r.Nonce))))
	return hex.EncodeToString(h[:])
}

func u64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// SignReceipt, origin kimligiyle bir fis imzalar. relayerID, krediyi kazanacak
// dugumdur. Bytes ve nonce cagiran tarafindan verilir.
func SignReceipt(origin *security.Identity, relayerID string, bytesRelayed, nonce uint64) Receipt {
	r := Receipt{
		RelayerID: relayerID,
		OriginID:  origin.NodeID(),
		Bytes:     bytesRelayed,
		Nonce:     nonce,
		IssuedAt:  time.Now().Unix(),
	}
	r.Sig = origin.Sign(r.canonical())
	return r
}

// VerifyReceipt, fisin imzasini OriginID'ye karsi dogrular.
func VerifyReceipt(r Receipt) error {
	if err := security.VerifySig(r.OriginID, r.canonical(), r.Sig); err != nil {
		return ErrBadReceiptSig
	}
	if r.RelayerID == r.OriginID {
		return ErrSelfRelay
	}
	return nil
}

// Ledger, bir dugumun (genelde relayer) topladigi fisleri dogrulayip role
// kredisini biriktiren, cift-harcamayi engelleyen yerel defterdir. Tamamen
// offline calisir. Eszamanli erisim guvenlidir.
type Ledger struct {
	mu       sync.Mutex
	seen     map[string]struct{} // receipt.ID() -> islenmis
	credit   map[string]uint64   // relayerID -> toplam kredi (bayt)
	byOrigin map[string]uint64   // originID -> bu origin'e borclu bayt
	vouchers map[string]struct{} // harcanan voucher kimlikleri
}

// New, bos bir defter olusturur.
func New() *Ledger {
	return &Ledger{
		seen:     make(map[string]struct{}),
		credit:   make(map[string]uint64),
		byOrigin: make(map[string]uint64),
		vouchers: make(map[string]struct{}),
	}
}

// Submit, imzali bir fisi dogrular ve kabul edilirse relayer'in kredisine
// ekler. Ayni fis (origin+nonce) iki kez islenmez (cift-harcama korumasi).
func (l *Ledger) Submit(r Receipt) error {
	if err := VerifyReceipt(r); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	id := r.ID()
	if _, dup := l.seen[id]; dup {
		return ErrDuplicate
	}
	l.seen[id] = struct{}{}
	l.credit[r.RelayerID] += r.Bytes
	l.byOrigin[r.OriginID] += r.Bytes
	return nil
}

// Credit, bir relayer'in topladigi toplam role kredisini (bayt) dondurur.
func (l *Ledger) Credit(relayerID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.credit[relayerID]
}

// OwedBy, bir origin'in (kendi trafigi icin) toplam borcunu dondurur.
func (l *Ledger) OwedBy(originID string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byOrigin[originID]
}

// Snapshot, tum relayer kredilerinin bir kopyasini dondurur.
func (l *Ledger) Snapshot() map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]uint64, len(l.credit))
	for k, v := range l.credit {
		out[k] = v
	}
	return out
}

// --- Voucher (devredilebilir imzali bakiye fisi) ---

// Voucher, bir dugumun baska bir dugume kredi/bakiye devrettigi, imzali ve
// tek-kullanimlik bir fistir. Internet olmadan el degistirir.
type Voucher struct {
	Issuer   string `json:"issuer"` // veren (imzalayan)
	Bearer   string `json:"bearer"` // alan
	Amount   uint64 `json:"amount"` // devredilen kredi
	Serial   uint64 `json:"serial"` // tek-kullanim (double-spend engelleme)
	IssuedAt int64  `json:"issued_at"`
	Sig      []byte `json:"sig"`
}

func (v Voucher) canonical() []byte {
	buf := make([]byte, 0, 128)
	buf = append(buf, "aetheris-voucher-v1\x00"...)
	buf = append(buf, v.Issuer...)
	buf = append(buf, 0)
	buf = append(buf, v.Bearer...)
	buf = append(buf, 0)
	buf = append(buf, u64(v.Amount)...)
	buf = append(buf, u64(v.Serial)...)
	buf = append(buf, u64(uint64(v.IssuedAt))...)
	return buf
}

// ID, voucher'in tek-kullanim kimligi (issuer + serial).
func (v Voucher) ID() string {
	h := sha256.Sum256([]byte(v.Issuer + ":" + hex.EncodeToString(u64(v.Serial))))
	return hex.EncodeToString(h[:])
}

// IssueVoucher, issuer kimligiyle bearer'a Amount kredi devreden imzali bir
// voucher uretir.
func IssueVoucher(issuer *security.Identity, bearerID string, amount, serial uint64) Voucher {
	v := Voucher{
		Issuer:   issuer.NodeID(),
		Bearer:   bearerID,
		Amount:   amount,
		Serial:   serial,
		IssuedAt: time.Now().Unix(),
	}
	v.Sig = issuer.Sign(v.canonical())
	return v
}

// Redeem, bir voucher'i dogrular ve bearer'in kredisine ekler. Ayni voucher
// iki kez harcanamaz.
func (l *Ledger) Redeem(v Voucher) error {
	if err := security.VerifySig(v.Issuer, v.canonical(), v.Sig); err != nil {
		return ErrBadVoucher
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	id := v.ID()
	if _, spent := l.vouchers[id]; spent {
		return ErrVoucherSpent
	}
	l.vouchers[id] = struct{}{}
	l.credit[v.Bearer] += v.Amount
	return nil
}

// Marshal / Unmarshal: fisleri off-grid takas icin (LoRa/dosya) serilestir.
func (r Receipt) Marshal() ([]byte, error) { return json.Marshal(r) }
func UnmarshalReceipt(b []byte) (Receipt, error) {
	var r Receipt
	err := json.Unmarshal(b, &r)
	return r, err
}
