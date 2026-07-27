package bearer

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// Lisanssiz mock adaptorleri Available()=true dondurmeli ve saglikli Probe etmeli.
func TestLicenseFreeBearersActive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cases := []struct {
		name string
		b    Bearer
		kind Kind
	}{
		{"WiGig60GHz", NewWiGigBearer("test"), KindWiGig60GHz},
		{"FSO", NewFSOBearer("test"), KindFSO},
		{"HaLow", NewHaLowBearer("test"), KindHaLow},
	}
	for _, c := range cases {
		if c.b.Kind() != c.kind {
			t.Errorf("%s: kind %q beklendi, %q geldi", c.name, c.kind, c.b.Kind())
		}
		if !c.b.Available() {
			t.Errorf("%s: lisanssiz mock Available()=true olmali", c.name)
		}
		rtt, err := c.b.Probe(ctx)
		if err != nil {
			t.Errorf("%s: Probe hatasi: %v", c.name, err)
		}
		if rtt <= 0 {
			t.Errorf("%s: pozitif RTT beklendi, %v geldi", c.name, rtt)
		}
	}
}

// Lisans/donanim bekleyen tasiyicilar stub (Available=false) kalmali.
func TestLicensedBearersRemainStub(t *testing.T) {
	bs := DefaultBearers([]string{"1.1.1.1:53"})
	want := map[Kind]bool{
		KindEthernet: true, KindWiFiWAN: true, KindWiGig60GHz: true, KindFSO: true, KindHaLow: true,
		KindTVWS: false, KindLoRaUSB: false, KindSoftAPMesh: false, KindUSBTethering: false, KindBLEMesh: false,
	}
	got := map[Kind]bool{}
	for _, b := range bs {
		got[b.Kind()] = b.Available()
	}
	for k, exp := range want {
		if got[k] != exp {
			t.Errorf("%s: Available()=%v beklendi, %v geldi", k, exp, got[k])
		}
	}
	if len(bs) != 10 {
		t.Errorf("10 tasiyici beklendi, %d geldi", len(bs))
	}
}

// Manager, lisanssiz mock tasiyicilardan birini secebilmeli (failover zinciri gercek).
func TestManagerElectsLicenseFreeBearer(t *testing.T) {
	m := New(slog.Default(), nil, time.Second)
	m.Register(NewHardwareStub(KindTVWS, "stub"))
	m.Register(NewWiGigBearer("test"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.elect(ctx)
	if m.Active() != KindWiGig60GHz {
		t.Fatalf("WiGig secilmeliydi (stub atlanir), aktif: %q", m.Active())
	}
}
