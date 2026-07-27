package voucher

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestIssuerNewVoucher(t *testing.T) {
	iss, err := NewIssuer()
	if err != nil {
		t.Fatal(err)
	}
	v, err := iss.NewVoucher("bearer-abc", 1000, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if v.Credits != 1000 {
		t.Fatalf("kredi 1000 olmalı: %d", v.Credits)
	}
	if v.SerialNo == "" {
		t.Fatal("seri no boş olmamalı")
	}
	if err := v.Verify(iss.PublicKey()); err != nil {
		t.Fatalf("geçerli voucher doğrulanmalı: %v", err)
	}
}

func TestVoucherTamperedRejected(t *testing.T) {
	iss, _ := NewIssuer()
	v, _ := iss.NewVoucher("b", 500, time.Hour)
	v.Credits = 9999 // kurcalama
	if err := v.Verify(iss.PublicKey()); err != ErrBadSignature {
		t.Fatalf("kurcalanmış voucher reddedilmeli: %v", err)
	}
}

func TestVoucherExpired(t *testing.T) {
	iss, _ := NewIssuer()
	// Süresi dolmuş voucher: NewVoucher ile üret, sonra ExpiresAt'ı geçmişe çek
	// ve yeniden imzala (test helper metodu).
	v, _ := iss.NewVoucher("b", 100, time.Hour)
	v.ExpiresAt = time.Now().Add(-time.Second).Unix()
	// İmzayı yenile (issuer'ın Sign metoduyla canonical üzerinden).
	v.Sig = iss.sign(v.canonical())
	if err := v.Verify(iss.PublicKey()); err != ErrExpired {
		t.Fatalf("süresi dolmuş voucher reddedilmeli: %v", err)
	}
}

func TestLedgerDoubleSpend(t *testing.T) {
	iss, _ := NewIssuer()
	l := NewLedger(nil)
	v, _ := iss.NewVoucher("bearer-1", 200, time.Hour)

	if err := l.Redeem(v, iss.PublicKey()); err != nil {
		t.Fatalf("ilk redeem kabul edilmeli: %v", err)
	}
	if err := l.Redeem(v, iss.PublicKey()); err != ErrAlreadyRedeemed {
		t.Fatalf("ikinci redeem reddedilmeli (çift harcama): %v", err)
	}
}

func TestLedgerBalance(t *testing.T) {
	iss, _ := NewIssuer()
	l := NewLedger(nil)

	v1, _ := iss.NewVoucher("user-A", 300, time.Hour)
	v2, _ := iss.NewVoucher("user-A", 200, time.Hour)
	v3, _ := iss.NewVoucher("user-B", 500, time.Hour)

	_ = l.Redeem(v1, iss.PublicKey())
	_ = l.Redeem(v2, iss.PublicKey())
	_ = l.Redeem(v3, iss.PublicKey())

	if l.Balance("user-A") != 500 {
		t.Fatalf("user-A bakiyesi 500 olmalı: %d", l.Balance("user-A"))
	}
	if l.Balance("user-B") != 500 {
		t.Fatalf("user-B bakiyesi 500 olmalı: %d", l.Balance("user-B"))
	}
}

func TestVoucherZeroKnowledge(t *testing.T) {
	iss, _ := NewIssuer()
	v, _ := iss.NewVoucher("zk-bearer", 1000, time.Hour)

	// PayloadSHA deterministik olmalı.
	h1 := v.PayloadSHA256()
	h2 := v.PayloadSHA256()
	if h1 != h2 {
		t.Fatal("PayloadSHA deterministik değil")
	}
	// SHA, raw kredi miktarını içermemeli (ZK prensip).
	if strings.Contains(h1, "1000") {
		t.Fatal("PayloadSHA ham kredi değeri içermemeli")
	}
}

func TestVoucherMarshalUnmarshal(t *testing.T) {
	iss, _ := NewIssuer()
	v, _ := iss.NewVoucher("bearer-serial", 750, time.Hour)
	data, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := v2.Verify(iss.PublicKey()); err != nil {
		t.Fatalf("seriden sonra doğrulama başarısız: %v", err)
	}
	if v2.Credits != v.Credits {
		t.Fatalf("kredi farklı: %d vs %d", v2.Credits, v.Credits)
	}
}

func TestLedgerWriteToWAL(t *testing.T) {
	iss, _ := NewIssuer()
	l := NewLedger(nil)
	v, _ := iss.NewVoucher("wal-test", 100, time.Hour)
	_ = l.Redeem(v, iss.PublicKey())

	var buf bytes.Buffer
	if err := l.WriteToWAL(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "payload_sha") {
		t.Fatal("WAL çıktısı payload_sha içermeli")
	}
	if strings.Contains(buf.String(), `"credits":100`) {
		// Kredi miktarı WAL'da visible — bu acceptable; ZK yalnızca payload içeriği için.
		t.Log("not: kredi miktarı WAL'da görünür (bu beklenen davranış)")
	}
}

func TestIssuerFromSeed(t *testing.T) {
	seed := make([]byte, 32)
	iss1, err := IssuerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	iss2, _ := IssuerFromSeed(seed)
	if iss1.NodeID != iss2.NodeID {
		t.Fatal("aynı seed aynı NodeID üretmeli")
	}
	v, _ := iss1.NewVoucher("b", 1, time.Hour)
	if err := v.Verify(iss2.PublicKey()); err != nil {
		t.Fatalf("farklı instance'dan doğrulama: %v", err)
	}
}

func TestZeroCreditsRejected(t *testing.T) {
	iss, _ := NewIssuer()
	if _, err := iss.NewVoucher("b", 0, time.Hour); err != ErrInvalidAmount {
		t.Fatalf("sıfır kredi reddedilmeli: %v", err)
	}
}
