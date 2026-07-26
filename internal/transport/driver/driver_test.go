package driver

import (
	"context"
	"testing"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	ble := NewStub(Capabilities{Kind: KindBLE, Name: "ble0", Mesh: true, Broadcast: true}, true)
	if err := r.Register(ble); err != nil {
		t.Fatal(err)
	}
	// Ayni tur ikinci kez: hata.
	if err := r.Register(NewStub(Capabilities{Kind: KindBLE, Name: "ble1"}, true)); err != ErrAlreadyExists {
		t.Fatalf("cift kayit reddedilmeliydi: %v", err)
	}
	got, err := r.Get(KindBLE)
	if err != nil || got.Capabilities().Name != "ble0" {
		t.Fatalf("kayitli surucu geri alinmaliydi: %v", err)
	}
	if _, err := r.Get(KindSoftAP); err != ErrNotFound {
		t.Fatalf("olmayan surucu ErrNotFound vermeliydi: %v", err)
	}
}

// TestAvailableDriversAutoActivate, yalnizca donanimsal olarak MEVCUT
// suruculerin otomatik devreye alinacagini dogrular.
func TestAvailableDriversAutoActivate(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(NewStub(Capabilities{Kind: KindBLE, Name: "ble-var"}, true))        // mevcut
	_ = r.Register(NewStub(Capabilities{Kind: KindSoftAP, Name: "softap-yok"}, false)) // mevcut degil

	avail := r.AvailableDrivers()
	if len(avail) != 1 || avail[0].Capabilities().Name != "ble-var" {
		t.Fatalf("yalnizca mevcut surucu devreye alinmaliydi, %d bulundu", len(avail))
	}
	// Liste ikisini de bildirir (telemetri icin).
	if len(r.List()) != 2 {
		t.Fatalf("List tum kayitli suruculeri bildirmeli, %d", len(r.List()))
	}
}

func TestStubSendReceive(t *testing.T) {
	d := NewStub(Capabilities{Kind: KindBLE, Name: "ble", MaxMTU: 244}, true)
	ctx := context.Background()

	// Acilmadan gonderim hata vermeli.
	if err := d.Send(ctx, "peer", []byte("x")); err != ErrNotAvailable {
		t.Fatalf("acilmadan gonderim reddedilmeliydi: %v", err)
	}
	if err := d.Open(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Send(ctx, "peer", []byte("merhaba")); err != nil {
		t.Fatal(err)
	}
	if d.SentCount() != 1 {
		t.Fatalf("1 cerceve gonderilmeliydi, %d", d.SentCount())
	}

	// Gelen cerceve.
	d.Inject("peerX", []byte("selam"))
	select {
	case f := <-d.Receive():
		if f.Src != "peerX" || string(f.Data) != "selam" {
			t.Fatalf("gelen cerceve yanlis: %+v", f)
		}
	default:
		t.Fatal("enjekte edilen cerceve alinmaliydi")
	}
}

func TestUnavailableDriverOpenFails(t *testing.T) {
	d := NewStub(Capabilities{Kind: KindSoftAP, Name: "yok"}, false)
	if err := d.Open(context.Background()); err != ErrNotAvailable {
		t.Fatalf("mevcut olmayan surucu Open'da hata vermeliydi: %v", err)
	}
}
