package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Bu dosya, mesh AGI ICI sifir-bilgi guvenlik kalkanidir:
//
//   1. Ed25519 dugum kimligi   — her dugum bir anahtar ciftine sahiptir;
//      kimligi ozel anahtarla imzalayarak kanitlar (taklit engellenir).
//   2. Proof-of-Work (anti-Sybil) — aga katilim icin hesaplama maliyeti;
//      saldirgan binlerce sahte dugum uretmeyi ekonomik olarak zorlastirir.
//   3. Nonce Sliding Window + zaman damgasi — paket TEKRAR OYNATMA (replay)
//      saldirilarini matematiksel olarak reddeder.
//
// Hepsi INTERNETSIZ calisir: dogrulama tamamen yereldir, merkezi otorite yok.

var (
	ErrBadSignature   = errors.New("guard: imza dogrulanamadi")
	ErrWeakPoW        = errors.New("guard: yetersiz proof-of-work (sahte dugum suphesi)")
	ErrReplay         = errors.New("guard: nonce tekrari (replay saldirisi)")
	ErrStaleTimestamp = errors.New("guard: zaman damgasi pencere disinda")
	ErrFutureStamp    = errors.New("guard: zaman damgasi gelecekte")
	ErrBadIdentity    = errors.New("guard: gecersiz dugum kimligi")
)

// Identity, bir mesh dugumunun Ed25519 kimligidir.
type Identity struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewIdentity, yeni bir Ed25519 kimligi uretir.
func NewIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{priv: priv, pub: pub}, nil
}

// IdentityFromSeed, deterministik (tohum tabanli) kimlik uretir — test ve
// yeniden uretilebilir dugum kimligi icin.
func IdentityFromSeed(seed []byte) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, ErrBadIdentity
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Identity{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

// NodeID, dugumun ag-genelinde benzersiz kimligidir (public key hex).
func (id *Identity) NodeID() string { return hex.EncodeToString(id.pub) }

// PublicKey, dogrulama icin acik anahtari dondurur.
func (id *Identity) PublicKey() ed25519.PublicKey { return id.pub }

// Sign, veriyi ozel anahtarla imzalar.
func (id *Identity) Sign(msg []byte) []byte { return ed25519.Sign(id.priv, msg) }

// VerifySig, bir NodeID (hex public key) ile imzayi dogrular.
func VerifySig(nodeID string, msg, sig []byte) error {
	pub, err := hex.DecodeString(nodeID)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ErrBadIdentity
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return ErrBadSignature
	}
	return nil
}

// --- Proof-of-Work (anti-Sybil) ---

// SolvePoW, verilen public key icin H(pub || nonce)'un ilk 'bits' bitinin
// sifir oldugu bir nonce bulur. Dugum aga katilmadan ONCE bir kez cozer.
func SolvePoW(pub ed25519.PublicKey, bits int) uint64 {
	var nonce uint64
	buf := make([]byte, len(pub)+8)
	copy(buf, pub)
	for {
		binary.BigEndian.PutUint64(buf[len(pub):], nonce)
		h := sha256.Sum256(buf)
		if leadingZeroBits(h[:]) >= bits {
			return nonce
		}
		nonce++
	}
}

// VerifyPoW, bir nonce'un gerekli zorlukta oldugunu UCUZCA dogrular.
func VerifyPoW(pub ed25519.PublicKey, nonce uint64, bits int) bool {
	buf := make([]byte, len(pub)+8)
	copy(buf, pub)
	binary.BigEndian.PutUint64(buf[len(pub):], nonce)
	h := sha256.Sum256(buf)
	return leadingZeroBits(h[:]) >= bits
}

// leadingZeroBits, bir bayt diziminin bastan kac bitinin sifir oldugunu sayar.
func leadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		for i := 7; i >= 0; i-- {
			if x&(1<<uint(i)) == 0 {
				n++
			} else {
				return n
			}
		}
	}
	return n
}

// --- Nonce Sliding Window (replay koruması) ---

// ReplayGuard, her dugum icin gorulen nonce'lari ve zaman penceresini tutarak
// tekrar oynatma saldirilarini engeller. Eszamanli erisim guvenlidir.
type ReplayGuard struct {
	window    time.Duration // kabul edilen zaman sapmasi
	maxNonces int           // dugum basi saklanan azami nonce (bellek siniri)

	mu   sync.Mutex
	seen map[string]*nodeWindow // nodeID -> pencere
}

type nodeWindow struct {
	nonces map[uint64]int64 // nonce -> ilk gorulme (unix nano); GC icin
	order  []uint64         // ekleme sirasi (kayan pencere icin)
}

// NewReplayGuard, verilen zaman penceresiyle bir koruma olusturur.
func NewReplayGuard(window time.Duration, maxNoncesPerNode int) *ReplayGuard {
	if window <= 0 {
		window = 2 * time.Minute
	}
	if maxNoncesPerNode <= 0 {
		maxNoncesPerNode = 4096
	}
	return &ReplayGuard{
		window:    window,
		maxNonces: maxNoncesPerNode,
		seen:      make(map[string]*nodeWindow),
	}
}

// Check, bir paketin (nodeID, nonce, timestamp) uclusunu dogrular. Gecerliyse
// nonce'u kaydeder ve nil doner; tekrar/eskimis/gelecek ise hata doner.
// now, cari zamandir (test icin enjekte edilebilir).
func (g *ReplayGuard) Check(nodeID string, nonce uint64, ts, now time.Time) error {
	// Zaman penceresi: ne cok eski ne de gelecekte olmali.
	if now.Sub(ts) > g.window {
		return ErrStaleTimestamp
	}
	if ts.Sub(now) > g.window {
		return ErrFutureStamp
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	nw := g.seen[nodeID]
	if nw == nil {
		nw = &nodeWindow{nonces: make(map[uint64]int64)}
		g.seen[nodeID] = nw
	}

	// Once eskimis nonce'lari temizle (kayan pencere): penceresi gecmis
	// olanlari bastan at; ardindan bellek siniri asildiysa en eskileri at.
	cutoff := now.Add(-g.window).UnixNano()
	for len(nw.order) > 0 {
		oldest := nw.order[0]
		t, ok := nw.nonces[oldest]
		if !ok {
			nw.order = nw.order[1:]
			continue
		}
		if t < cutoff {
			delete(nw.nonces, oldest)
			nw.order = nw.order[1:]
			continue
		}
		break
	}
	for len(nw.nonces) > g.maxNonces && len(nw.order) > 0 {
		oldest := nw.order[0]
		delete(nw.nonces, oldest)
		nw.order = nw.order[1:]
	}

	// Tekrar mi?
	if _, dup := nw.nonces[nonce]; dup {
		return ErrReplay
	}

	nw.nonces[nonce] = now.UnixNano()
	nw.order = append(nw.order, nonce)
	return nil
}

// --- Bütünleşik kalkan ---

// Guard, kimlik + PoW + replay korumasini tek bir kapida birlestirir.
type Guard struct {
	powBits int
	replay  *ReplayGuard
}

// NewGuard, verilen PoW zorlugu ve replay penceresiyle bir kalkan olusturur.
func NewGuard(powBits int, replayWindow time.Duration) *Guard {
	if powBits < 0 {
		powBits = 0
	}
	return &Guard{
		powBits: powBits,
		replay:  NewReplayGuard(replayWindow, 8192),
	}
}

// JoinRequest, bir dugumun aga katilma talebidir.
type JoinRequest struct {
	NodeID   string // hex public key
	PoWNonce uint64
	Sig      []byte // NodeID'nin ozel anahtariyla imzasi (kimlik kaniti)
}

// VerifyJoin, katilim talebini dogrular: kimlik imzasi gecerli VE PoW yeterli.
// Sybil saldirisi icin saldirgan HER sahte dugum icin PoW cozmek zorundadir.
func (gd *Guard) VerifyJoin(req JoinRequest) error {
	pub, err := hex.DecodeString(req.NodeID)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ErrBadIdentity
	}
	// Kimlik kaniti: dugum kendi NodeID'sini imzalamis olmali.
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(req.NodeID), req.Sig) {
		return ErrBadSignature
	}
	// Anti-Sybil: PoW yeterli mi?
	if gd.powBits > 0 && !VerifyPoW(ed25519.PublicKey(pub), req.PoWNonce, gd.powBits) {
		return ErrWeakPoW
	}
	return nil
}

// MakeJoinRequest, bir kimlik icin gecerli katilim talebi uretir (PoW cozer).
func MakeJoinRequest(id *Identity, powBits int) JoinRequest {
	nonce := uint64(0)
	if powBits > 0 {
		nonce = SolvePoW(id.pub, powBits)
	}
	return JoinRequest{
		NodeID:   id.NodeID(),
		PoWNonce: nonce,
		Sig:      id.Sign([]byte(id.NodeID())),
	}
}

// VerifyPacket, imzali bir mesh paketini dogrular: imza gecerli VE replay
// degil. msg, imzalanan kanonik bayttir.
func (gd *Guard) VerifyPacket(nodeID string, msg, sig []byte, nonce uint64, ts, now time.Time) error {
	if err := VerifySig(nodeID, msg, sig); err != nil {
		return err
	}
	return gd.replay.Check(nodeID, nonce, ts, now)
}
