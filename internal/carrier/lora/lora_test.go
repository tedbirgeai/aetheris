package lora

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	chunk := []byte("aetheris mesh test yuku")
	h := FrameHeader{To: 0x02, From: 0x01, PacketID: 4242, Index: 0, Count: 1}
	frame, err := encodeFrame(h, chunk)
	if err != nil {
		t.Fatalf("encodeFrame: %v", err)
	}
	if len(frame) > MTU {
		t.Fatalf("cerceve MTU'yu asti: %d > %d", len(frame), MTU)
	}
	gotH, gotChunk, err := decodeFrame(frame)
	if err != nil {
		t.Fatalf("decodeFrame: %v", err)
	}
	if gotH.PacketID != 4242 || gotH.From != 0x01 || gotH.To != 0x02 {
		t.Fatalf("baslik uyusmadi: %+v", gotH)
	}
	if !bytes.Equal(gotChunk, chunk) {
		t.Fatalf("yuk uyusmadi: %q != %q", gotChunk, chunk)
	}
}

func TestCRCDetectsCorruption(t *testing.T) {
	frame, err := encodeFrame(FrameHeader{To: 1, From: 2, PacketID: 1, Count: 1}, []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	frame[rhHeaderSize+fragHeaderSize] ^= 0xFF // yuku boz
	if _, _, err := decodeFrame(frame); err != ErrBadCRC {
		t.Fatalf("bozuk cerceve ErrBadCRC vermeliydi, verdi: %v", err)
	}
}

func TestFragmentReassembleLargePayload(t *testing.T) {
	// MTU'nun ~10 kati bir yuk: coklu parca zorunlu.
	payload := make([]byte, MaxChunk*10+37)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	frames, err := Fragment(BroadcastAddr, 0x01, 7, payload)
	if err != nil {
		t.Fatalf("Fragment: %v", err)
	}
	if len(frames) != 11 {
		t.Fatalf("11 parca bekleniyordu, %d uretildi", len(frames))
	}
	for i, f := range frames {
		if len(f) > MTU {
			t.Fatalf("parca %d MTU'yu asti: %d", i, len(f))
		}
	}

	// Parcalari SIRASIZ ver: reassembler sirayi kendi duzeltmeli.
	reasm := NewReassembler(time.Second)
	order := []int{5, 0, 10, 3, 1, 9, 2, 8, 4, 7, 6}
	var out []byte
	for _, idx := range order {
		msg, from, complete, err := reasm.Push(frames[idx])
		if err != nil {
			t.Fatalf("Push[%d]: %v", idx, err)
		}
		if complete {
			out = msg
			if from != 0x01 {
				t.Fatalf("kaynak adres yanlis: %d", from)
			}
		}
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("birlestirilmis yuk orijinalle uyusmuyor (%d vs %d bayt)", len(out), len(payload))
	}
	if reasm.Pending() != 0 {
		t.Fatalf("tamamlanan mesaj sonrasi pending 0 olmali, %d", reasm.Pending())
	}
}

func TestReassemblerToleratesDuplicates(t *testing.T) {
	payload := bytes.Repeat([]byte("X"), MaxChunk+10)
	frames, _ := Fragment(1, 2, 9, payload)
	reasm := NewReassembler(time.Second)
	// Ilk parcayi iki kez gonder.
	reasm.Push(frames[0])
	reasm.Push(frames[0])
	msg, _, complete, err := reasm.Push(frames[1])
	if err != nil || !complete {
		t.Fatalf("tekrarli parcaya ragmen tamamlanmaliydi: complete=%v err=%v", complete, err)
	}
	if !bytes.Equal(msg, payload) {
		t.Fatal("tekrar sonrasi yuk bozuldu")
	}
}

func TestMockLoopbackTransceiver(t *testing.T) {
	drv := NewMockDriver(0x01, nil, nil) // loopback
	tr := NewTransceiver(drv, 0x01, time.Second)
	defer tr.Close()

	payload := bytes.Repeat([]byte("mesh"), 200) // > MTU, fragment gerekir
	ctx := context.Background()
	if err := tr.SendMessage(ctx, BroadcastAddr, payload); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	got, _, err := tr.ReceiveMessage(rctx)
	if err != nil {
		t.Fatalf("ReceiveMessage: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("loopback yuku uyusmadi")
	}
}

func TestMockMediumTwoNodes(t *testing.T) {
	medium := NewMockMedium()
	a := NewMockDriver(0x01, medium, nil)
	b := NewMockDriver(0x02, medium, nil)
	ta := NewTransceiver(a, 0x01, time.Second)
	tb := NewTransceiver(b, 0x02, time.Second)
	defer ta.Close()
	defer tb.Close()

	payload := []byte("komsu dugume merhaba")
	ctx := context.Background()

	done := make(chan []byte, 1)
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		msg, _, err := tb.ReceiveMessage(rctx)
		if err != nil {
			t.Errorf("B receive: %v", err)
			done <- nil
			return
		}
		done <- msg
	}()

	if err := ta.SendMessage(ctx, 0x02, payload); err != nil {
		t.Fatalf("A send: %v", err)
	}
	select {
	case got := <-done:
		if !bytes.Equal(got, payload) {
			t.Fatal("ortam uzerinden yuk uyusmadi")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("zaman asimi: B mesaji almadi")
	}
}

func TestMockMediumPartition(t *testing.T) {
	medium := NewMockMedium()
	a := NewMockDriver(0x01, medium, nil)
	b := NewMockDriver(0x02, medium, nil)
	defer a.Close()
	defer b.Close()

	medium.Partition(0x01, 0x02) // A'nin yaydigini B duymaz

	frames, _ := Fragment(0x02, 0x01, 1, []byte("bu ulasmamali"))
	_ = a.Send(context.Background(), frames[0])

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := b.Receive(ctx); err == nil {
		t.Fatal("bolunme sirasinda B cerceve almamaliydi")
	}

	medium.Heal(0x01, 0x02)
	_ = a.Send(context.Background(), frames[0])
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := b.Receive(ctx2); err != nil {
		t.Fatalf("iyilesme sonrasi B almaliydi: %v", err)
	}
}

func TestOpenHALFallsBackToMock(t *testing.T) {
	// Var olmayan aygit yolu: sistem cokmemeli, mock'a dusmeli.
	drv, isHW := OpenHAL(HALConfig{SerialPath: "/dev/does-not-exist-xyz", Addr: 0x05})
	defer drv.Close()
	if isHW {
		t.Fatal("var olmayan aygit icin isHardware=false olmaliydi")
	}
	if drv.Name() != "mock" {
		t.Fatalf("mock surucu bekleniyordu, gelen: %s", drv.Name())
	}
	if drv.IsHardware() {
		t.Fatal("mock surucu IsHardware()=false dururst olmali")
	}
}

func TestSerialDriverOverPipe(t *testing.T) {
	// Gercek aygit yerine net.Pipe: seri cerceveleme protokolunu dogrula.
	c1, c2 := net.Pipe()
	tx := newSerialFromRWC("/dev/fake", c1)
	rx := newSerialFromRWC("/dev/fake", c2)
	defer tx.Close()
	defer rx.Close()

	if !tx.IsHardware() {
		t.Fatal("SerialDriver IsHardware()=true olmali")
	}

	payload := []byte("seri cerceve testi")
	frames, _ := Fragment(2, 1, 1, payload)

	var wg sync.WaitGroup
	wg.Add(1)
	var got []byte
	var rerr error
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got, rerr = rx.Receive(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tx.Send(ctx, frames[0]); err != nil {
		t.Fatalf("Send: %v", err)
	}
	wg.Wait()
	if rerr != nil {
		t.Fatalf("Receive: %v", rerr)
	}
	if !bytes.Equal(got, frames[0]) {
		t.Fatal("seri cerceve uyusmadi")
	}
}

// io.ReadWriteCloser'in net.Conn tarafindan karsilandigini derleme
// zamaninda dogrula (SerialDriver enjeksiyon testinin dayanagi).
var _ io.ReadWriteCloser = net.Conn(nil)
