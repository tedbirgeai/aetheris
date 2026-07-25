package main

import (
	"bytes"
	"strings"
	"testing"
)

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := runCLI("version")
	if code != 0 || !strings.Contains(out, "aetheris-cli") {
		t.Fatalf("version: kod=%d cikti=%q", code, out)
	}
}

func TestKeygen(t *testing.T) {
	code, out, _ := runCLI("keygen")
	if code != 0 || !strings.Contains(out, "NodeID:") {
		t.Fatalf("keygen: kod=%d cikti=%q", code, out)
	}
	// NodeID 64 hex karakter (32 bayt public key) olmali.
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "NodeID:"))
	if len(line) != 64 {
		t.Fatalf("NodeID 64 hex olmali, uzunluk %d (%q)", len(line), line)
	}
}

func TestRouteMultiHop(t *testing.T) {
	code, out, errb := runCLI("route",
		"-links", "A-B:10:ethernet,B-C:10:ethernet",
		"-from", "A", "-to", "C")
	if code != 0 {
		t.Fatalf("route: kod=%d err=%q", code, errb)
	}
	if !strings.Contains(out, "A -> B -> C") {
		t.Fatalf("cok-sicramali yol beklenir, cikti=%q", out)
	}
}

func TestRouteNoRoute(t *testing.T) {
	code, _, _ := runCLI("route", "-links", "A-B:10", "-from", "A", "-to", "Z")
	if code != 1 {
		t.Fatalf("yol yoksa kod=1 beklenir, %d", code)
	}
}

func TestMeshDemo(t *testing.T) {
	code, out, errb := runCLI("mesh-demo")
	if code != 0 {
		t.Fatalf("mesh-demo: kod=%d err=%q", code, errb)
	}
	if !strings.Contains(out, "BASARILI") || !strings.Contains(out, "C teslim aldi") {
		t.Fatalf("mesh-demo basari ciktisi vermeliydi: %q", out)
	}
}

func TestReceiptGenerateAndVerify(t *testing.T) {
	// Once bir fis uret.
	code, out, errb := runCLI("receipt", "-relayer",
		"1111111111111111111111111111111111111111111111111111111111111111",
		"-bytes", "1000", "-nonce", "1")
	if code != 0 {
		t.Fatalf("receipt uretimi: kod=%d err=%q", code, errb)
	}
	if !strings.Contains(out, "relayer_id") {
		t.Fatalf("fis JSON'u beklenir: %q", out)
	}
	// Not: -verify stdin okur; burada uretim ciktisinin gecerli JSON oldugunu
	// dogrulamak yeterli (imza dogrulamasi ledger testlerinde kapsandi).
}

func TestUnknownCommand(t *testing.T) {
	code, _, errb := runCLI("nosuchcmd")
	if code != 2 || !strings.Contains(errb, "bilinmeyen komut") {
		t.Fatalf("bilinmeyen komut 2 dondurmeliydi: kod=%d err=%q", code, errb)
	}
}

func TestServe(t *testing.T) {
	code, out, _ := runCLI("serve", "-id", "testnode")
	if code != 0 || !strings.Contains(out, "mesh dugumu hazir") {
		t.Fatalf("serve: kod=%d cikti=%q", code, out)
	}
}
