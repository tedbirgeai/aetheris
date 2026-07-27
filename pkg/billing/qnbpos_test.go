package billing

import "testing"

func testCfg() NestPayConfig {
	return NestPayConfig{
		ClientID: "100000000", StoreKey: "TEST_STOREKEY_123",
		GatewayURL: "https://vpostest.qnbfinansbank.com/fim/est3Dgate",
		OkURL:      "https://x/ok", FailURL: "https://x/fail", Currency: "949",
	}
}

// NestPay v3 hash deterministik ve girdiye duyarlı olmalı.
func TestNestPayHashDeterministic(t *testing.T) {
	c := testCfg()
	p := map[string]string{"clientid": "100000000", "oid": "ORD-1", "amount": "100.00", "currency": "949"}
	h1 := c.computeHashV3(p)
	h2 := c.computeHashV3(p)
	if h1 != h2 || h1 == "" {
		t.Fatalf("hash deterministik değil: %q vs %q", h1, h2)
	}
	p["amount"] = "200.00" // kurcala
	if c.computeHashV3(p) == h1 {
		t.Fatal("tutar değişince hash değişmeliydi")
	}
}

// hash/encoding alanları imzaya dahil edilmemeli.
func TestNestPayHashExcludesHashField(t *testing.T) {
	c := testCfg()
	base := map[string]string{"clientid": "1", "oid": "x", "amount": "1.00"}
	withHash := map[string]string{"clientid": "1", "oid": "x", "amount": "1.00", "hash": "ZZZ", "encoding": "UTF-8"}
	if c.computeHashV3(base) != c.computeHashV3(withHash) {
		t.Fatal("hash/encoding alanları imzaya dahil edilmemeli")
	}
}

// Build3DFields imzalı olmalı ve VerifyCallback doğrulamalı; kurcalama reddedilmeli.
func TestNestPayVerifyCallback(t *testing.T) {
	c := testCfg()
	fields := c.Build3DFields("ORD-42", 12345, "a@b.co") // ₺123,45
	if fields["amount"] != "123.45" {
		t.Fatalf("tutar biçimi hatalı: %s", fields["amount"])
	}
	if fields["hash"] == "" {
		t.Fatal("form imzalanmadı")
	}
	// Banka dönüşünü simüle et: aynı alanlar + başarılı 3D + onay.
	cb := map[string]string{}
	for k, v := range fields {
		cb[k] = v
	}
	delete(cb, "hash")
	cb["mdStatus"] = "1"
	cb["Response"] = "Approved"
	cb["ProcReturnCode"] = "00"
	cb["HASH"] = c.computeHashV3(cb)
	if ok, reason := c.VerifyCallback(cb); !ok {
		t.Fatalf("geçerli callback reddedildi: %s", reason)
	}
	// Kurcala: tutarı değiştir, HASH güncellenmeden.
	cb["amount"] = "999.00"
	if ok, _ := c.VerifyCallback(cb); ok {
		t.Fatal("kurcalanmış callback kabul edildi (bütünlük ihlali yakalanmadı)")
	}
}
