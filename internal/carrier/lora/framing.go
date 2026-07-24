package lora

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// crc16CCITT, cerceve butunlugu icin CRC-16/CCITT-FALSE hesaplar.
// LoRa modemlerinin donanim CRC'sinden BAGIMSIZ, uygulama seviyesinde
// ek bir butunluk kontroludur; parca birlestirme sonrasi bozulmayi yakalar.
func crc16CCITT(data []byte) uint16 {
	var crc uint16 = 0xFFFF
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// FrameHeader, cozulmus bir cercevenin baslik alanlaridir.
type FrameHeader struct {
	To       byte // RadioHead hedef adresi (0xFF = yayin)
	From     byte // RadioHead kaynak adresi
	MsgID    byte // RadioHead mesaj kimligi (ACK eslesmesi icin)
	Flags    byte // RadioHead bayraklari
	PacketID uint16
	Index    uint8
	Count    uint8
	ChunkLen uint16
}

// encodeFrame, tek bir parca cercevesini telde gidecek bayt dizisine cevirir.
// Duzen: [To|From|MsgID|Flags][PacketID|Index|Count|ChunkLen][chunk][CRC16]
func encodeFrame(h FrameHeader, chunk []byte) ([]byte, error) {
	if len(chunk) > MaxChunk {
		return nil, fmt.Errorf("%w: parca %d bayt, azami %d", ErrFrameTooLarge, len(chunk), MaxChunk)
	}
	buf := make([]byte, 0, rhHeaderSize+fragHeaderSize+len(chunk)+crcSize)
	buf = append(buf, h.To, h.From, h.MsgID, h.Flags)

	var frag [fragHeaderSize]byte
	binary.BigEndian.PutUint16(frag[0:2], h.PacketID)
	frag[2] = h.Index
	frag[3] = h.Count
	binary.BigEndian.PutUint16(frag[4:6], uint16(len(chunk)))
	buf = append(buf, frag[:]...)

	buf = append(buf, chunk...)

	crc := crc16CCITT(buf)
	var crcb [crcSize]byte
	binary.BigEndian.PutUint16(crcb[:], crc)
	buf = append(buf, crcb[:]...)

	if len(buf) > MTU {
		return nil, ErrFrameTooLarge
	}
	return buf, nil
}

// decodeFrame, telden gelen ham baytlari baslik + yuk parcasina cozer.
// CRC dogrulamasi burada yapilir; bozuk cerceve ErrBadCRC/ErrBadFrame doner.
func decodeFrame(frame []byte) (FrameHeader, []byte, error) {
	minLen := rhHeaderSize + fragHeaderSize + crcSize
	if len(frame) < minLen {
		return FrameHeader{}, nil, fmt.Errorf("%w: cerceve %d bayt, asgari %d", ErrBadFrame, len(frame), minLen)
	}

	body := frame[:len(frame)-crcSize]
	gotCRC := binary.BigEndian.Uint16(frame[len(frame)-crcSize:])
	if crc16CCITT(body) != gotCRC {
		return FrameHeader{}, nil, ErrBadCRC
	}

	h := FrameHeader{
		To:    frame[0],
		From:  frame[1],
		MsgID: frame[2],
		Flags: frame[3],
	}
	frag := frame[rhHeaderSize : rhHeaderSize+fragHeaderSize]
	h.PacketID = binary.BigEndian.Uint16(frag[0:2])
	h.Index = frag[2]
	h.Count = frag[3]
	h.ChunkLen = binary.BigEndian.Uint16(frag[4:6])

	chunkStart := rhHeaderSize + fragHeaderSize
	chunkEnd := chunkStart + int(h.ChunkLen)
	if chunkEnd > len(body) {
		return FrameHeader{}, nil, fmt.Errorf("%w: bildirilen parca uzunlugu cerceveye sigmiyor", ErrBadFrame)
	}
	chunk := make([]byte, h.ChunkLen)
	copy(chunk, body[chunkStart:chunkEnd])
	return h, chunk, nil
}

// Fragment, MTU'yu asabilen bir yuku, her biri MTU'ya sigan cercevelere boler.
// packetID, ayni mesajin parcalarini alici tarafta eslestirir.
func Fragment(to, from byte, packetID uint16, payload []byte) ([][]byte, error) {
	if len(payload) == 0 {
		// Bos yuk bile tek (bos) parca olarak gonderilir; alici "0 bayt
		// mesaj" ile "hic mesaj yok" durumunu ayirt edebilsin.
		f, err := encodeFrame(FrameHeader{To: to, From: from, PacketID: packetID, Index: 0, Count: 1}, nil)
		if err != nil {
			return nil, err
		}
		return [][]byte{f}, nil
	}

	total := (len(payload) + MaxChunk - 1) / MaxChunk
	if total > 255 {
		return nil, fmt.Errorf("lora: yuk cok buyuk (%d parca, azami 255)", total)
	}

	frames := make([][]byte, 0, total)
	for i := 0; i < total; i++ {
		start := i * MaxChunk
		end := start + MaxChunk
		if end > len(payload) {
			end = len(payload)
		}
		h := FrameHeader{
			To:       to,
			From:     from,
			MsgID:    byte(packetID), // dusuk bayt; ACK eslesmesi icin yeterli
			PacketID: packetID,
			Index:    uint8(i),
			Count:    uint8(total),
		}
		f, err := encodeFrame(h, payload[start:end])
		if err != nil {
			return nil, err
		}
		frames = append(frames, f)
	}
	return frames, nil
}

// Reassembler, gelen parcalari packetID'ye gore biriktirir ve mesaj tamam
// olunca birlestirir. Parcalar SIRASIZ ve TEKRARLI gelebilir; ikisi de
// dogru islenir. Yarim kalan mesajlar TTL sonunda temizlenir (bellek sizmasi
// onlemi — kayip bir parca yuzunden mesaj sonsuza dek beklemez).
type Reassembler struct {
	mu      sync.Mutex
	pending map[uint16]*partial
	ttl     time.Duration
}

type partial struct {
	count    uint8
	chunks   map[uint8][]byte
	from     byte
	deadline time.Time
}

// NewReassembler, verilen TTL ile bir birlestirici kurar.
// ttl <= 0 ise varsayilan 30 saniye kullanilir.
func NewReassembler(ttl time.Duration) *Reassembler {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Reassembler{pending: make(map[uint16]*partial), ttl: ttl}
}

// Push, tek bir ham cerceveyi isler. Mesaj bu parcayla tamamlandiysa
// (complete=true) birlesmis yuku ve kaynak adresini dondurur.
func (r *Reassembler) Push(frame []byte) (payload []byte, from byte, complete bool, err error) {
	h, chunk, derr := decodeFrame(frame)
	if derr != nil {
		return nil, 0, false, derr
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcLocked()

	p, ok := r.pending[h.PacketID]
	if !ok {
		p = &partial{
			count:    h.Count,
			chunks:   make(map[uint8][]byte, h.Count),
			from:     h.From,
			deadline: time.Now().Add(r.ttl),
		}
		r.pending[h.PacketID] = p
	}
	// Ayni packetID'de celiskili Count gelirse (cok nadir, cakisan ID),
	// en son bildirilen sayiyi esas al ama mevcut parcalari koru.
	if h.Count != 0 {
		p.count = h.Count
	}
	p.chunks[h.Index] = chunk

	if uint8(len(p.chunks)) < p.count {
		return nil, 0, false, nil
	}
	// Tum parcalar geldi mi? Index'ler 0..count-1 eksiksiz olmali.
	for i := uint8(0); i < p.count; i++ {
		if _, ok := p.chunks[i]; !ok {
			return nil, 0, false, nil
		}
	}

	var out []byte
	for i := uint8(0); i < p.count; i++ {
		out = append(out, p.chunks[i]...)
	}
	delete(r.pending, h.PacketID)
	return out, p.from, true, nil
}

// gcLocked, suresi dolmus yarim mesajlari temizler. Cagiran mutex'i tutmali.
func (r *Reassembler) gcLocked() {
	now := time.Now()
	for id, p := range r.pending {
		if now.After(p.deadline) {
			delete(r.pending, id)
		}
	}
}

// Pending, o an birlestirilmeyi bekleyen yarim mesaj sayisidir (gozlem icin).
func (r *Reassembler) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
