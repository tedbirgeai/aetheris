package ephemeral

import (
	"bytes"
	"crypto/rand"
	"testing"
	"time"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestSealOpenRoundTrip, hedef dugumun kendi cercevesini cozebildigini
// dogrular (IP/MAC olmadan, yalnizca kimlik+epoch).
func TestSealOpenRoundTrip(t *testing.T) {
	key := mustKey(t)
	now := time.Now()
	payload := []byte("off-grid RF mesaji — IP/MAC yok")

	frame, err := Seal(key, "node-B", payload, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(key, "node-B", frame, now)
	if err != nil {
		t.Fatalf("hedef kendi cercevesini cozmeliydi: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("cozulen payload bozuk")
	}
}

// TestNoIPNoMAC, cerceve baytlarinda IP/MAC benzeri sabit tanimlayici
// OLMADIGINI ve baslik boyutunun beklenen (1+8+12) oldugunu dogrular.
func TestFrameLayout(t *testing.T) {
	key := mustKey(t)
	frame, _ := Seal(key, "x", []byte("abc"), time.Now())
	if frame[0] != MagicV1 {
		t.Fatal("ilk bayt Magic olmali")
	}
	// baslik(21) + payload(3) + GCM tag(16) = 40
	if len(frame) != headerLen+3+16 {
		t.Fatalf("cerceve boyutu beklenmedik: %d", len(frame))
	}
}

// TestNotForMe, baska bir dugume ait cercevenin cozulmedigini (hedef hash
// eslesmiyor) dogrular.
func TestNotForMe(t *testing.T) {
	key := mustKey(t)
	now := time.Now()
	frame, _ := Seal(key, "node-B", []byte("gizli"), now)

	if _, err := Open(key, "node-C", frame, now); err != ErrNotForMe {
		t.Fatalf("baska dugumun cercevesi ErrNotForMe vermeliydi: %v", err)
	}
}

// TestEphemeralRotation, ayni dugumun hedef hash'inin epoch'lar arasinda
// DEGISTIGINI (unlinkability) dogrular.
func TestEphemeralRotation(t *testing.T) {
	e1 := Epoch(time.Unix(0, 0))
	e2 := e1 + 5
	h1 := DestHash("node-B", e1)
	h2 := DestHash("node-B", e2)
	if h1 == h2 {
		t.Fatal("hedef hash epoch'lar arasi degismeli (donen kimlik)")
	}
}

// TestClockSkewTolerance, bir onceki epoch'ta muhurlenen cercevenin cari
// epoch'ta hala cozulebildigini (saat kaymasi toleransi) dogrular.
func TestClockSkewTolerance(t *testing.T) {
	key := mustKey(t)
	past := time.Now().Add(-EpochDuration) // bir onceki epoch
	frame, _ := Seal(key, "node-B", []byte("zaman"), past)

	// Alici cari epoch'ta acmayi dener; onceki epoch da denendigi icin cozmeli.
	if _, err := Open(key, "node-B", frame, time.Now()); err != nil {
		t.Fatalf("bir onceki epoch cercevesi tolere edilmeliydi: %v", err)
	}
}

// TestTamperRejected, sifreli payload kurcalanirsa GCM kimlik dogrulamasinin
// reddettigini dogrular.
func TestTamperRejected(t *testing.T) {
	key := mustKey(t)
	now := time.Now()
	frame, _ := Seal(key, "node-B", []byte("dokunma"), now)

	frame[len(frame)-1] ^= 0xFF // son baytı boz
	if _, err := Open(key, "node-B", frame, now); err != ErrAuth {
		t.Fatalf("kurcalanmis cerceve ErrAuth vermeliydi: %v", err)
	}
}

// TestBadMagic, yanlis magic baytinin reddedildigini dogrular.
func TestBadMagic(t *testing.T) {
	key := mustKey(t)
	frame, _ := Seal(key, "node-B", []byte("x"), time.Now())
	frame[0] = 0x00
	if _, err := Open(key, "node-B", frame, time.Now()); err != ErrBadMagic {
		t.Fatalf("yanlis magic ErrBadMagic vermeliydi: %v", err)
	}
}

// TestWrongKey, farkli anahtarla muhurlenen cercevenin (hedef eslesse bile)
// cozulemedigini dogrular.
func TestWrongKey(t *testing.T) {
	k1 := mustKey(t)
	k2 := mustKey(t)
	now := time.Now()
	frame, _ := Seal(k1, "node-B", []byte("sir"), now)
	if _, err := Open(k2, "node-B", frame, now); err != ErrAuth {
		t.Fatalf("yanlis anahtar ErrAuth vermeliydi: %v", err)
	}
}
