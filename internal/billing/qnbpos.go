// QNB Finansbank Sanal POS (NestPay/EST 3D Secure) ödeme adaptörü.
//
// Bu dosyayı internal/billing/qnbpos.go olarak koyun. Yalnızca standart Go.
//
// QNB Finansbank sanal POS altyapısı NestPay (Asseco/Payten EST) tabanlıdır.
// 3D Secure (3D Pay Hosting) akışı:
//
//   1. Sunucu, sipariş için imzalı form alanları üretir (Build3DFields).
//   2. Tarayıcı bu alanları GatewayURL'e POST eder (otomatik-submit form).
//   3. Kullanıcı bankada 3D doğrulaması yapar.
//   4. Banka, OkURL/FailURL'e POST ile döner; sunucu VerifyCallback ile
//      HASH'i yeniden hesaplayıp doğrular ve mdStatus'ü kontrol eder.
//
// CANLIYA ALMA (yalnızca bunlar kalır):
//   AETHERIS_QNBPOS_CLIENTID   QNB'den verilen üye işyeri (clientid)
//   AETHERIS_QNBPOS_STOREKEY   3D anahtarı (storekey)
//   AETHERIS_QNBPOS_URL        prod: https://vpos.qnbfinansbank.com/fim/est3Dgate
//                              test: https://vpostest.qnbfinansbank.com/fim/est3Dgate
//   AETHERIS_QNBPOS_OK_URL     başarı dönüş adresi (https://.../billing/qnb/ok)
//   AETHERIS_QNBPOS_FAIL_URL   hata dönüş adresi

package billing

import (
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// randHex, n bayt kriptografik rastgele veriyi hex olarak döndürür (rnd alanı).
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NestPayConfig, QNB sanal POS 3D ayarlarıdır.
type NestPayConfig struct {
	ClientID   string
	StoreKey   string
	GatewayURL string
	OkURL      string
	FailURL    string
	Currency   string // ISO 4217 sayısal; TRY = "949"
}

// NestPayFromEnv, ayarları ortam değişkenlerinden yükler.
func NestPayFromEnv() NestPayConfig {
	return NestPayConfig{
		ClientID:   os.Getenv("AETHERIS_QNBPOS_CLIENTID"),
		StoreKey:   os.Getenv("AETHERIS_QNBPOS_STOREKEY"),
		GatewayURL: os.Getenv("AETHERIS_QNBPOS_URL"),
		OkURL:      os.Getenv("AETHERIS_QNBPOS_OK_URL"),
		FailURL:    os.Getenv("AETHERIS_QNBPOS_FAIL_URL"),
		Currency:   "949",
	}
}

// Ready, canlıya alınmaya hazır mı (zorunlu alanlar dolu mu).
func (c NestPayConfig) Ready() bool {
	return c.ClientID != "" && c.StoreKey != "" && c.GatewayURL != ""
}

// hashEscape, NestPay v3 kaçış kuralı: '\' -> '\\', '|' -> '\|'.
func hashEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}

// computeHashV3, NestPay HashVersion 3.0 imzasını üretir:
// tüm parametreler (hash ve encoding hariç) anahtar adına göre
// büyük/küçük harf duyarsız sıralanır, değerleri '|' ile birleştirilir,
// sona storekey eklenir, SHA-512 alınır ve Base64'lenir.
func (c NestPayConfig) computeHashV3(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		lk := strings.ToLower(k)
		if lk == "hash" || lk == "encoding" {
			continue
		}
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return strings.ToLower(keys[i]) < strings.ToLower(keys[j])
	})
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(hashEscape(params[k]))
		sb.WriteString("|")
	}
	sb.WriteString(hashEscape(c.StoreKey))
	sum := sha512.Sum512([]byte(sb.String()))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Build3DFields, tarayıcının GatewayURL'e POST edeceği imzalı form alanlarını
// üretir. amountKurus kuruş cinsindendir (₺100,00 = 10000). orderID sipariş
// numarasıdır (tekil olmalı).
func (c NestPayConfig) Build3DFields(orderID string, amountKurus int64, email string) map[string]string {
	amount := fmt.Sprintf("%d.%02d", amountKurus/100, amountKurus%100)
	rnd := randHex(16)
	p := map[string]string{
		"clientid":                        c.ClientID,
		"storetype":                       "3d_pay_hosting",
		"hashAlgorithm":                   "ver3",
		"trantype":                        "Auth",
		"amount":                          amount,
		"currency":                        c.Currency,
		"oid":                             orderID,
		"okUrl":                           c.OkURL,
		"failUrl":                         c.FailURL,
		"lang":                            "tr",
		"rnd":                             rnd,
		"email":                           email,
		"refreshtime":                     "5",
		"encoding":                        "UTF-8",
	}
	p["hash"] = c.computeHashV3(p)
	return p
}

// VerifyCallback, banka dönüş POST'unu doğrular: HASH'i yeniden hesaplar,
// gelen HASH ile sabit-zamanda karşılaştırır ve 3D sonucunu (mdStatus)
// kontrol eder. mdStatus ∈ {1,2,3,4} → 3D doğrulama başarılı.
// Ayrıca Response=Approved / ProcReturnCode=00 finansal onayı gösterir.
func (c NestPayConfig) VerifyCallback(params map[string]string) (ok bool, reason string) {
	got := params["HASH"]
	if got == "" {
		got = params["hash"]
	}
	if got == "" {
		return false, "HASH yok"
	}
	want := c.computeHashV3(params)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return false, "HASH uyuşmuyor (bütünlük ihlali)"
	}
	switch params["mdStatus"] {
	case "1", "2", "3", "4":
		// 3D doğrulama tamam.
	default:
		return false, "3D doğrulama başarısız (mdStatus=" + params["mdStatus"] + ")"
	}
	if r := strings.ToLower(params["Response"]); r != "" && r != "approved" {
		return false, "banka onayı yok (Response=" + params["Response"] + ")"
	}
	if pc := params["ProcReturnCode"]; pc != "" && pc != "00" {
		return false, "işlem reddedildi (ProcReturnCode=" + pc + ")"
	}
	return true, "onaylandı"
}
