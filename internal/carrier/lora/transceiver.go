package lora

import (
	"context"
	"sync/atomic"
	"time"
)

// Transceiver, ham Driver'in ustunde MTU'dan uzun mesajlari otomatik
// bolup (fragment) birlestiren (reassemble) yuksek seviyeli arayuzdur.
//
// Cagiran taraf istedigi uzunlukta yuk verir; Transceiver bunu MTU'ya
// sigan cercevelere boler, tek tek gonderir; alici tarafta parcalari
// birlestirip tam mesaji dondurur. Donanim MTU sinirini ust katmandan
// gizler.
type Transceiver struct {
	drv    Driver
	reasm  *Reassembler
	pktCtr atomic.Uint32
	addr   byte
}

// NewTransceiver, bir surucuyu sarmalayarak transceiver olusturur.
// addr, gonderilen cercevelerin RadioHead "From" alani olur.
// reasmTTL, yarim mesajlarin bellek temizleme suresi.
func NewTransceiver(drv Driver, addr byte, reasmTTL time.Duration) *Transceiver {
	return &Transceiver{
		drv:   drv,
		reasm: NewReassembler(reasmTTL),
		addr:  addr,
	}
}

// nextPacketID, artan (wrap-around) 16-bit paket kimligi uretir.
func (t *Transceiver) nextPacketID() uint16 {
	return uint16(t.pktCtr.Add(1))
}

// SendMessage, herhangi bir uzunluktaki yuku otomatik bolerek gonderir.
// to = hedef adres (yayin icin BroadcastAddr).
func (t *Transceiver) SendMessage(ctx context.Context, to byte, payload []byte) error {
	frames, err := Fragment(to, t.addr, t.nextPacketID(), payload)
	if err != nil {
		return err
	}
	for _, f := range frames {
		if err := t.drv.Send(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// ReceiveMessage, tam bir mesaj birlesene kadar cerceve alir. Ara parcalar
// biriktirilir; yalnizca tamamlanmis mesaj dondurulur. Bozuk/CRC-hatali
// cerceveler sessizce atlanir (gercek radyoda gurultu beklenir).
func (t *Transceiver) ReceiveMessage(ctx context.Context) (payload []byte, from byte, err error) {
	for {
		frame, rerr := t.drv.Receive(ctx)
		if rerr != nil {
			return nil, 0, rerr
		}
		msg, src, complete, perr := t.reasm.Push(frame)
		if perr != nil {
			// Gurultu/bozuk cerceve: yut, dinlemeye devam et.
			continue
		}
		if complete {
			return msg, src, nil
		}
	}
}

// Driver, alttaki surucuyu dondurur (gozlem/kapatma icin).
func (t *Transceiver) Driver() Driver { return t.drv }

// Close, alttaki surucuyu kapatir.
func (t *Transceiver) Close() error { return t.drv.Close() }
