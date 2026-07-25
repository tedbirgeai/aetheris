package security

import (
	"testing"
	"time"
)

func TestIdentitySignVerify(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("mesh paketi")
	sig := id.Sign(msg)
	if err := VerifySig(id.NodeID(), msg, sig); err != nil {
		t.Fatalf("gecerli imza dogrulanmaliydi: %v", err)
	}
	// Degistirilmis mesaj reddedilmeli.
	if err := VerifySig(id.NodeID(), []byte("baska"), sig); err != ErrBadSignature {
		t.Fatalf("degistirilmis mesaj reddedilmeliydi: %v", err)
	}
}

func TestForgedIdentityRejected(t *testing.T) {
	a, _ := NewIdentity()
	b, _ := NewIdentity()
	msg := []byte("x")
	sig := a.Sign(msg)
	// a'nin imzasini b'nin kimligiyle dogrulamak basarisiz olmali.
	if err := VerifySig(b.NodeID(), msg, sig); err != ErrBadSignature {
		t.Fatalf("sahte kimlik reddedilmeliydi: %v", err)
	}
}

func TestProofOfWork(t *testing.T) {
	id, _ := NewIdentity()
	bits := 12 // makul: hizli cozulur ama sifir degil
	nonce := SolvePoW(id.PublicKey(), bits)
	if !VerifyPoW(id.PublicKey(), nonce, bits) {
		t.Fatal("cozulen PoW dogrulanmaliydi")
	}
	// nonce+1 neredeyse kesinlikle gecersiz.
	if VerifyPoW(id.PublicKey(), nonce+1, bits) {
		t.Skip("nadir: nonce+1 de gecerli cikti, atlaniyor")
	}
}

func TestJoinAntiSybil(t *testing.T) {
	gd := NewGuard(12, time.Minute)
	id, _ := NewIdentity()

	// Gecerli katilim: kimlik imzasi + PoW.
	req := MakeJoinRequest(id, 12)
	if err := gd.VerifyJoin(req); err != nil {
		t.Fatalf("gecerli katilim kabul edilmeliydi: %v", err)
	}

	// PoW'suz (nonce=0) katilim reddedilmeli.
	bad := req
	bad.PoWNonce = 0
	if err := gd.VerifyJoin(bad); err != ErrWeakPoW {
		// nonce=0 nadiren gecerli olabilir; o durumda baska bir gecersiz dene.
		bad.PoWNonce = 123456789
		if err2 := gd.VerifyJoin(bad); err2 != ErrWeakPoW {
			t.Fatalf("yetersiz PoW reddedilmeliydi: %v / %v", err, err2)
		}
	}

	// Sahte imza reddedilmeli.
	forged := MakeJoinRequest(id, 12)
	forged.Sig[0] ^= 0xFF
	if err := gd.VerifyJoin(forged); err != ErrBadSignature {
		t.Fatalf("sahte imzali katilim reddedilmeliydi: %v", err)
	}
}

func TestReplayGuardRejectsDuplicate(t *testing.T) {
	rg := NewReplayGuard(2*time.Minute, 1024)
	now := time.Now()
	node := "abc123"

	if err := rg.Check(node, 1, now, now); err != nil {
		t.Fatalf("ilk nonce kabul edilmeliydi: %v", err)
	}
	// Ayni nonce tekrar: replay.
	if err := rg.Check(node, 1, now, now); err != ErrReplay {
		t.Fatalf("tekrar eden nonce reddedilmeliydi: %v", err)
	}
	// Farkli nonce kabul.
	if err := rg.Check(node, 2, now, now); err != nil {
		t.Fatalf("yeni nonce kabul edilmeliydi: %v", err)
	}
	// Farkli dugum ayni nonce: sorun yok (nonce dugum-basi).
	if err := rg.Check("baska", 1, now, now); err != nil {
		t.Fatalf("baska dugumun ayni nonce'u kabul edilmeliydi: %v", err)
	}
}

func TestReplayGuardTimestampWindow(t *testing.T) {
	rg := NewReplayGuard(1*time.Minute, 1024)
	now := time.Now()

	// Cok eski zaman damgasi (pencere disi).
	if err := rg.Check("n", 1, now.Add(-2*time.Minute), now); err != ErrStaleTimestamp {
		t.Fatalf("eski zaman damgasi reddedilmeliydi: %v", err)
	}
	// Gelecekteki zaman damgasi.
	if err := rg.Check("n", 2, now.Add(2*time.Minute), now); err != ErrFutureStamp {
		t.Fatalf("gelecek zaman damgasi reddedilmeliydi: %v", err)
	}
	// Pencere icinde kabul.
	if err := rg.Check("n", 3, now.Add(-30*time.Second), now); err != nil {
		t.Fatalf("pencere ici zaman damgasi kabul edilmeliydi: %v", err)
	}
}

func TestReplayGuardSlidingEviction(t *testing.T) {
	rg := NewReplayGuard(1*time.Minute, 1024)
	base := time.Now()

	// t=0'da nonce 100.
	if err := rg.Check("n", 100, base, base); err != nil {
		t.Fatal(err)
	}
	// t=90sn sonra: pencere kaydi, nonce 100 artik pencere disinda kaldigi
	// icin TEKRAR kullanilabilir olmali (eski kayit tahliye edildi).
	later := base.Add(90 * time.Second)
	if err := rg.Check("n", 100, later, later); err != nil {
		t.Fatalf("pencere kayinca eski nonce yeniden kabul edilmeliydi: %v", err)
	}
}

func TestVerifyPacketIntegration(t *testing.T) {
	gd := NewGuard(0, time.Minute) // PoW'suz, sadece imza + replay
	id, _ := NewIdentity()
	now := time.Now()
	msg := []byte("kanonik paket")
	sig := id.Sign(msg)

	if err := gd.VerifyPacket(id.NodeID(), msg, sig, 42, now, now); err != nil {
		t.Fatalf("gecerli paket kabul edilmeliydi: %v", err)
	}
	// Ayni nonce tekrar: replay.
	if err := gd.VerifyPacket(id.NodeID(), msg, sig, 42, now, now); err != ErrReplay {
		t.Fatalf("replay reddedilmeliydi: %v", err)
	}
}
