package billing

import (
	"sync"
	"testing"
	"time"
)

func TestCreditEngineAwardsCredits(t *testing.T) {
	rec := &recordingEmitter{}
	b := New([]Emitter{rec}, Config{Logger: quietLogger(), RetryDelay: time.Millisecond})
	defer b.Close()

	// 1 bayt = 0.001 kredi birimi
	ce := NewCreditEngine(0.001, 0, b)

	granted := ce.RecordRelay("relayer-a", "musteri-b", 10000)
	if granted != 10 {
		t.Fatalf("kazanilan kredi = %d, beklenen 10", granted)
	}

	bal, ok := ce.Balance("relayer-a")
	if !ok {
		t.Fatal("kredi kaydi bulunamadi")
	}
	if bal.RelayedBytes != 10000 || bal.CreditUnits != 10 || bal.RelayCount != 1 {
		t.Fatalf("kredi kaydi hatali: %+v", bal)
	}
}

// TestSelfRelayEarnsNothing, kendi trafigini role etmenin kredi
// KAZANDIRMADIGINI dogrular.
//
// Bu, sistemin en bariz kotuye kullanim yolunu kapatir: kendi kendine
// trafik uretip kredi toplamak.
func TestSelfRelayEarnsNothing(t *testing.T) {
	ce := NewCreditEngine(0.001, 0, nil)

	if got := ce.RecordRelay("acme", "acme", 1000000); got != 0 {
		t.Fatalf("kendi trafigini role etmek %d kredi kazandirdi, beklenen 0", got)
	}
	if _, ok := ce.Balance("acme"); ok {
		t.Fatal("self-relay icin kayit olusturuldu")
	}
}

// TestCreditCapEnforced, donem basina kredi tavaninin uygulandigini
// dogrular. Tavansiz bir sistem, sahte trafikle sinirsiz indirim
// toplanmasina acik olurdu.
func TestCreditCapEnforced(t *testing.T) {
	ce := NewCreditEngine(1.0, 100, nil) // 1 bayt = 1 kredi, tavan 100

	first := ce.RecordRelay("a", "b", 60)
	if first != 60 {
		t.Fatalf("ilk kazanc = %d, beklenen 60", first)
	}

	// Ikinci islem tavani asiyor: yalnizca 40 verilmeli.
	second := ce.RecordRelay("a", "b", 60)
	if second != 40 {
		t.Fatalf("ikinci kazanc = %d, beklenen 40 (tavan 100)", second)
	}

	// Ucuncu islem hic kredi kazandirmamali.
	third := ce.RecordRelay("a", "b", 60)
	if third != 0 {
		t.Fatalf("ucuncu kazanc = %d, beklenen 0", third)
	}

	bal, _ := ce.Balance("a")
	if bal.CreditUnits != 100 {
		t.Fatalf("toplam kredi = %d, tavan 100 asildi", bal.CreditUnits)
	}
	if bal.CappedUnits != 80 {
		t.Fatalf("tavan nedeniyle kesilen = %d, beklenen 80", bal.CappedUnits)
	}
}

func TestCreditEngineEmitsEvent(t *testing.T) {
	rec := &recordingEmitter{}
	b := New([]Emitter{rec}, Config{Logger: quietLogger(), RetryDelay: time.Millisecond})

	ce := NewCreditEngine(0.01, 0, b)
	ce.RecordRelay("relayer", "origin", 1000)

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	events := rec.got()
	if len(events) != 1 {
		t.Fatalf("olay sayisi = %d, beklenen 1", len(events))
	}
	if events[0].Type != CreditEarned {
		t.Fatalf("olay turu = %s", events[0].Type)
	}
	if events[0].CreditUnits != 10 {
		t.Fatalf("CreditUnits = %d, beklenen 10", events[0].CreditUnits)
	}
}

func TestCreditEngineConcurrent(t *testing.T) {
	ce := NewCreditEngine(1.0, 0, nil)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ce.RecordRelay("relayer", "origin", 10)
		}()
	}
	wg.Wait()

	bal, _ := ce.Balance("relayer")
	if bal.CreditUnits != 1000 {
		t.Fatalf("CreditUnits = %d, beklenen 1000 - YARIS KOSULU", bal.CreditUnits)
	}
	if bal.RelayCount != 100 {
		t.Fatalf("RelayCount = %d, beklenen 100", bal.RelayCount)
	}
}

func TestCreditResetPeriod(t *testing.T) {
	ce := NewCreditEngine(1.0, 0, nil)
	ce.RecordRelay("a", "b", 100)
	ce.ResetPeriod()
	if _, ok := ce.Balance("a"); ok {
		t.Fatal("donem sifirlamasi sonrasi kayit kaldi")
	}
}

// --- Esik izleyici ---

// TestThresholdFiresOncePerTier, her esik icin YALNIZCA BIR KEZ olay
// uretildigini dogrular. Aksi halde esigin ustundeki her istek yeni
// olay dogurur ve alici sistem bogulur.
func TestThresholdFiresOncePerTier(t *testing.T) {
	rec := &recordingEmitter{}
	b := New([]Emitter{rec}, Config{Logger: quietLogger(), RetryDelay: time.Millisecond})

	tw := NewThresholdWatcher([]uint64{1000, 5000}, b)

	// Esigin altinda: olay yok
	if got := tw.Check("acme", 500); got != 0 {
		t.Fatalf("esik altinda olay uretildi: %d", got)
	}
	// Ilk esigi ast: olay
	if got := tw.Check("acme", 1500); got != 1000 {
		t.Fatalf("ilk esik = %d, beklenen 1000", got)
	}
	// Ayni esik tekrar: olay YOK
	if got := tw.Check("acme", 2000); got != 0 {
		t.Fatalf("ayni esik icin tekrar olay uretildi: %d", got)
	}
	// Ikinci esigi ast: olay
	if got := tw.Check("acme", 6000); got != 5000 {
		t.Fatalf("ikinci esik = %d, beklenen 5000", got)
	}
	// Tekrar: olay YOK
	if got := tw.Check("acme", 9000); got != 0 {
		t.Fatalf("ikinci esik icin tekrar olay uretildi: %d", got)
	}

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if len(rec.got()) != 2 {
		t.Fatalf("toplam olay = %d, beklenen 2", len(rec.got()))
	}
}

func TestThresholdIsolatesClients(t *testing.T) {
	rec := &recordingEmitter{}
	b := New([]Emitter{rec}, Config{Logger: quietLogger(), RetryDelay: time.Millisecond})
	defer b.Close()

	tw := NewThresholdWatcher([]uint64{1000}, b)

	if got := tw.Check("acme", 2000); got != 1000 {
		t.Fatal("acme icin esik olayi uretilmedi")
	}
	// Farkli musteri kendi esigini asmali.
	if got := tw.Check("globex", 2000); got != 1000 {
		t.Fatal("globex icin esik olayi uretilmedi")
	}
}

func TestThresholdConcurrent(t *testing.T) {
	rec := &recordingEmitter{}
	b := New([]Emitter{rec}, Config{Logger: quietLogger(), RetryDelay: time.Millisecond})

	tw := NewThresholdWatcher([]uint64{1000}, b)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tw.Check("acme", 5000)
		}()
	}
	wg.Wait()

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// 50 es zamanli kontrol olsa da YALNIZCA 1 olay uretilmeli.
	if n := len(rec.got()); n != 1 {
		t.Fatalf("es zamanli kontrolde %d olay uretildi, beklenen 1", n)
	}
}
