package ledger

import (
	"testing"

	"github.com/tedbirgeai/aetheris/internal/security"
)

func mustID(t *testing.T) *security.Identity {
	t.Helper()
	id, err := security.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRelayReceiptCredit, temel akis: origin fis imzalar, relayer'in defteri
// dogrular ve krediyi ekler.
func TestRelayReceiptCredit(t *testing.T) {
	origin := mustID(t)  // B: trafigin sahibi
	relayer := mustID(t) // A: tasiyan

	rc := SignReceipt(origin, relayer.NodeID(), 100_000, 1)
	if err := VerifyReceipt(rc); err != nil {
		t.Fatalf("gecerli fis dogrulanmaliydi: %v", err)
	}

	l := New()
	if err := l.Submit(rc); err != nil {
		t.Fatalf("fis kabul edilmeliydi: %v", err)
	}
	if got := l.Credit(relayer.NodeID()); got != 100_000 {
		t.Fatalf("role kredisi 100000 olmali, %d", got)
	}
	if got := l.OwedBy(origin.NodeID()); got != 100_000 {
		t.Fatalf("origin borcu 100000 olmali, %d", got)
	}
}

// TestDoubleSpendRejected, ayni fisin iki kez islenemedigini dogrular.
func TestDoubleSpendRejected(t *testing.T) {
	origin := mustID(t)
	relayer := mustID(t)
	rc := SignReceipt(origin, relayer.NodeID(), 500, 7)

	l := New()
	if err := l.Submit(rc); err != nil {
		t.Fatal(err)
	}
	if err := l.Submit(rc); err != ErrDuplicate {
		t.Fatalf("cift harcama reddedilmeliydi: %v", err)
	}
	// Kredi tek sayilmali.
	if l.Credit(relayer.NodeID()) != 500 {
		t.Fatalf("kredi cift sayildi: %d", l.Credit(relayer.NodeID()))
	}
}

// TestTamperedReceiptRejected, imzadan sonra alan degistirilirse reddedilir.
func TestTamperedReceiptRejected(t *testing.T) {
	origin := mustID(t)
	relayer := mustID(t)
	rc := SignReceipt(origin, relayer.NodeID(), 100, 1)

	// Bayt miktarini sisir.
	rc.Bytes = 999_999
	if err := VerifyReceipt(rc); err != ErrBadReceiptSig {
		t.Fatalf("kurcalanmis fis reddedilmeliydi: %v", err)
	}
	l := New()
	if err := l.Submit(rc); err != ErrBadReceiptSig {
		t.Fatalf("kurcalanmis fis Submit'te reddedilmeliydi: %v", err)
	}
}

// TestSelfRelayRejected, dugum kendi trafigi icin fis imzalayip kredi
// kazanamaz.
func TestSelfRelayRejected(t *testing.T) {
	id := mustID(t)
	rc := SignReceipt(id, id.NodeID(), 1000, 1) // relayer == origin
	if err := VerifyReceipt(rc); err != ErrSelfRelay {
		t.Fatalf("self-relay reddedilmeliydi: %v", err)
	}
}

// TestForgedSignatureRejected, baska bir kimligin imzasiyla uretilen sahte
// fis reddedilir.
func TestForgedSignatureRejected(t *testing.T) {
	origin := mustID(t)
	attacker := mustID(t)
	relayer := mustID(t)

	// Saldirgan, origin adina fis uydurmaya calisir ama kendi anahtariyla imzalar.
	rc := Receipt{
		RelayerID: relayer.NodeID(),
		OriginID:  origin.NodeID(), // origin gibi gorunuyor
		Bytes:     1_000_000,
		Nonce:     1,
	}
	rc.Sig = attacker.Sign(rc.canonical()) // ama yanlis anahtar

	if err := VerifyReceipt(rc); err != ErrBadReceiptSig {
		t.Fatalf("sahte imzali fis reddedilmeliydi: %v", err)
	}
}

// TestOffGridSerialization, fisin off-grid takas icin serilestirilip geri
// okunabildigini ve hala gecerli oldugunu dogrular (LoRa/dosya ile tasima).
func TestOffGridSerialization(t *testing.T) {
	origin := mustID(t)
	relayer := mustID(t)
	rc := SignReceipt(origin, relayer.NodeID(), 42_000, 9)

	blob, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalReceipt(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyReceipt(back); err != nil {
		t.Fatalf("serilestirilmis fis hala gecerli olmaliydi: %v", err)
	}
	l := New()
	if err := l.Submit(back); err != nil {
		t.Fatal(err)
	}
	if l.Credit(relayer.NodeID()) != 42_000 {
		t.Fatal("serilestirme sonrasi kredi yanlis")
	}
}

// TestVoucherRedeem, imzali voucher'in devredilebildigini ve iki kez
// harcanamadigini dogrular.
func TestVoucherRedeem(t *testing.T) {
	issuer := mustID(t)
	bearer := mustID(t)

	v := IssueVoucher(issuer, bearer.NodeID(), 5000, 1)
	l := New()
	if err := l.Redeem(v); err != nil {
		t.Fatalf("gecerli voucher kabul edilmeliydi: %v", err)
	}
	if l.Credit(bearer.NodeID()) != 5000 {
		t.Fatalf("voucher kredisi 5000 olmali, %d", l.Credit(bearer.NodeID()))
	}
	// Ikinci kez: harcanmis.
	if err := l.Redeem(v); err != ErrVoucherSpent {
		t.Fatalf("harcanmis voucher reddedilmeliydi: %v", err)
	}

	// Kurcalanmis voucher reddedilmeli.
	v2 := IssueVoucher(issuer, bearer.NodeID(), 100, 2)
	v2.Amount = 999999
	if err := l.Redeem(v2); err != ErrBadVoucher {
		t.Fatalf("kurcalanmis voucher reddedilmeliydi: %v", err)
	}
}

// TestMultiRelayerAccounting, birden cok relayer icin ayri kredi tutuldugunu
// dogrular (off-grid ag muhasebesi).
func TestMultiRelayerAccounting(t *testing.T) {
	origin := mustID(t)
	a := mustID(t)
	b := mustID(t)

	l := New()
	_ = l.Submit(SignReceipt(origin, a.NodeID(), 1000, 1))
	_ = l.Submit(SignReceipt(origin, b.NodeID(), 2000, 2))
	_ = l.Submit(SignReceipt(origin, a.NodeID(), 500, 3))

	if l.Credit(a.NodeID()) != 1500 {
		t.Fatalf("A kredisi 1500 olmali, %d", l.Credit(a.NodeID()))
	}
	if l.Credit(b.NodeID()) != 2000 {
		t.Fatalf("B kredisi 2000 olmali, %d", l.Credit(b.NodeID()))
	}
	if l.OwedBy(origin.NodeID()) != 3500 {
		t.Fatalf("origin toplam borcu 3500 olmali, %d", l.OwedBy(origin.NodeID()))
	}
}
