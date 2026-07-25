package tunnel

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Bu dosya, canli TCP/UDP tunel proxy motorudur. Istemciden gelen ham bayt
// akislarini AES-256-GCM ile sifreleyip PARCALI (chunked) cerceveler halinde
// bir mesh baglantisi (Link) uzerinden hedef uca iletir.
//
// ZERO-KNOWLEDGE ILKESI (v0.5a kabul kriteri 3):
// Motor, tasidigi verinin ICERIGINI asla saklamaz, loglamaz veya muhasebeye
// dahil etmez. Yalnizca iki sey olculur:
//   - PayloadSHA : akisin duz-metninin SHA-256 ozeti (butunluk kaniti)
//   - Bayt sayimi: BytesIn / BytesOut
// Duz metin yalnizca gecici bir tampon icinde bulunur ve sifreleme/desifre
// disinda hicbir yere yazilmaz.

const (
	// DefaultChunkSize, tek bir cercevede tasinacak azami duz-metin boyutu.
	DefaultChunkSize = 16 * 1024
	// GCMNonceSize, AES-GCM standart nonce boyutu (12 bayt).
	GCMNonceSize = 12
	// AES256KeySize, AES-256 anahtar boyutu.
	AES256KeySize = 32
)

var (
	ErrKeySize     = errors.New("tunnel: AES-256 anahtari tam 32 bayt olmalidir")
	ErrShortCipher = errors.New("tunnel: sifreli metin nonce'dan kisa")
	ErrBadFrame    = errors.New("tunnel: bozuk cerceve")
	ErrClosedLink  = errors.New("tunnel: baglanti kapali")
)

// frameType, cerceve turudur.
type frameType uint8

const (
	frameData  frameType = 1 // sifreli veri parcasi
	frameClose frameType = 2 // akis sonu (yarim-kapatma)
)

// Cipher, AES-256-GCM sarmalayicisidir. Her Seal cagrisi TAZE rastgele bir
// nonce uretir ve sifreli metnin onune ekler: cikti = nonce || ciphertext.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher, 32 baytlik anahtardan bir AES-256-GCM Cipher olusturur.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != AES256KeySize {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Seal, duz metni sifreler. Cikti: nonce(12) || ciphertext+tag.
func (c *Cipher) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, GCMNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// Seal, ciktinin ilk 12 baytini nonce yapar, uzerine ekler.
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

// Open, Seal ciktisini cozer.
func (c *Cipher) Open(blob []byte) ([]byte, error) {
	if len(blob) < GCMNonceSize {
		return nil, ErrShortCipher
	}
	nonce := blob[:GCMNonceSize]
	ct := blob[GCMNonceSize:]
	return c.aead.Open(nil, nonce, ct, nil)
}

// Link, iki mesh ucu arasinda cerceve tasiyan cift-yonlu bir kanaldir.
// Gercek uygulama mesh/TCP uzerinden olabilir; testler ve surec-ici mesh
// icin NewPipeLink kullanilir. Send/Recv eszamanli cagrilabilir olmalidir.
type Link interface {
	Send(frame []byte) error
	Recv() ([]byte, error)
	Close() error
}

// encodeFrame, [type:1][len:4][payload] bicimini uretir.
func encodeFrame(t frameType, payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	buf[0] = byte(t)
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

func decodeFrame(b []byte) (frameType, []byte, error) {
	if len(b) < 5 {
		return 0, nil, ErrBadFrame
	}
	n := binary.BigEndian.Uint32(b[1:5])
	if int(n) != len(b)-5 {
		return 0, nil, ErrBadFrame
	}
	return frameType(b[0]), b[5:], nil
}

// halfCloser, yalnizca YAZMA yonunu kapatabilen baglantilardir (TCP
// net.TCPConn.CloseWrite gibi). Egress, istemci akisi bitince hedefe EOF
// sinyali vermek icin kullanir; boylece hedef (veya echo dongusu) okuma
// tarafinda EOF gorur ve yanit yonu duzgun sonlanir.
type halfCloser interface{ CloseWrite() error }

func halfCloseWrite(c io.Closer) {
	if hc, ok := c.(halfCloser); ok {
		_ = hc.CloseWrite()
		return
	}
	// Yarim-kapatma desteklenmiyorsa tam kapat (son care).
	_ = c.Close()
}

// Stats, bir tunel akisinin zero-knowledge muhasebesidir. Icerik YOKTUR.
type Stats struct {
	BytesIn    uint64 // istemciden okunan (yukari yon) duz-metin bayt
	BytesOut   uint64 // istemciye yazilan (asagi yon) duz-metin bayt
	PayloadSHA string // yukari yon duz-metnin SHA-256'si (hex)
}

// Proxy, tunel proxy motorudur. AES-256-GCM Cipher tutar ve akislari
// sifreleyip Link uzerinden tasir.
type Proxy struct {
	cipher    *Cipher
	chunkSize int
}

// NewProxy, 32 baytlik AES-256 anahtariyla bir Proxy olusturur.
func NewProxy(key []byte, chunkSize int) (*Proxy, error) {
	c, err := NewCipher(key)
	if err != nil {
		return nil, err
	}
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	return &Proxy{cipher: c, chunkSize: chunkSize}, nil
}

// pump, src'den okur, her parcayi hasher'a besler (varsa), sifreler ve Data
// cercevesi olarak link'e yazar. Kaynak bitince frameClose gonderir.
// Okunan toplam duz-metin bayt sayisini dondurur.
func (p *Proxy) pump(src io.Reader, link Link, hasher io.Writer) (uint64, error) {
	buf := make([]byte, p.chunkSize)
	var total uint64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			total += uint64(n)
			if hasher != nil {
				_, _ = hasher.Write(buf[:n])
			}
			sealed, err := p.cipher.Seal(buf[:n])
			if err != nil {
				return total, err
			}
			if err := link.Send(encodeFrame(frameData, sealed)); err != nil {
				return total, err
			}
		}
		if rerr == io.EOF {
			_ = link.Send(encodeFrame(frameClose, nil))
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// drain, link'ten cerceve okur, Data'yi cozup dst'ye yazar; frameClose
// gelince durur. dst'ye yazilan toplam duz-metin bayt sayisini dondurur.
func (p *Proxy) drain(link Link, dst io.Writer) (uint64, error) {
	var total uint64
	for {
		raw, err := link.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, ErrClosedLink) {
				return total, nil
			}
			return total, err
		}
		t, payload, derr := decodeFrame(raw)
		if derr != nil {
			return total, derr
		}
		switch t {
		case frameClose:
			return total, nil
		case frameData:
			plain, oerr := p.cipher.Open(payload)
			if oerr != nil {
				return total, oerr
			}
			if _, werr := dst.Write(plain); werr != nil {
				return total, werr
			}
			total += uint64(len(plain))
		default:
			return total, ErrBadFrame
		}
	}
}

// ServeIngress, ISTEMCI tarafidir: yerel baglantidan (local) gelen ham baytlari
// sifreleyip link uzerinden hedefe iletir; donen sifreli baytlari cozup
// local'e yazar. Cift-yonlu akis biter bitmez zero-knowledge Stats doner.
//
// local: istemci baglantisi (TCP net.Conn veya benzeri ReadWriteCloser).
func (p *Proxy) ServeIngress(ctx context.Context, local io.ReadWriteCloser, link Link) (Stats, error) {
	hasher := sha256.New()
	var (
		wg                sync.WaitGroup
		upErr, downErr    error
		bytesIn, bytesOut uint64
	)

	wg.Add(2)
	// yukari: local -> (sifrele) -> link
	go func() {
		defer wg.Done()
		bytesIn, upErr = p.pump(local, link, hasher)
	}()
	// asagi: link -> (coz) -> local
	go func() {
		defer wg.Done()
		bytesOut, downErr = p.drain(link, local)
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		_ = local.Close()
		_ = link.Close()
		<-done
		return Stats{}, ctx.Err()
	case <-done:
	}
	_ = local.Close()

	st := Stats{
		BytesIn:    bytesIn,
		BytesOut:   bytesOut,
		PayloadSHA: hex.EncodeToString(hasher.Sum(nil)),
	}
	if upErr != nil {
		return st, upErr
	}
	return st, downErr
}

// Dialer, egress tarafinda hedef uca baglanti acar. Testlerde bir echo/pipe
// ucu; uretimde net.Dial ile gercek hedef.
type Dialer func(ctx context.Context) (io.ReadWriteCloser, error)

// ServeEgress, HEDEF (cikis) tarafidir: link'ten gelen sifreli baytlari cozup
// hedefe yazar; hedeften donen baytlari sifreleyip link'e geri gonderir.
func (p *Proxy) ServeEgress(ctx context.Context, link Link, dial Dialer) (Stats, error) {
	target, err := dial(ctx)
	if err != nil {
		_ = link.Close()
		return Stats{}, fmt.Errorf("tunnel: hedefe baglanilamadi: %w", err)
	}

	var (
		wg                 sync.WaitGroup
		inErr, outErr      error
		toTarget, toClient uint64
	)
	wg.Add(2)
	// link -> (coz) -> hedef
	go func() {
		defer wg.Done()
		toTarget, inErr = p.drain(link, target)
		// Istemci akisi bitti: hedefe EOF sinyali ver ki yanit yonu (echo
		// veya gercek sunucu) okuma tarafinda sonlanabilsin.
		halfCloseWrite(target)
	}()
	// hedef -> (sifrele) -> link
	go func() {
		defer wg.Done()
		toClient, outErr = p.pump(target, link, nil)
		_ = target.Close()
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		_ = target.Close()
		_ = link.Close()
		<-done
		return Stats{}, ctx.Err()
	case <-done:
	}

	st := Stats{BytesIn: toTarget, BytesOut: toClient}
	if inErr != nil {
		return st, inErr
	}
	return st, outErr
}

// --- Surec-ici mesh baglantisi (test ve tek-surec mesh icin) ---

// pipeLink, iki ucu birbirine bagli, goroutine-guvenli bir Link ciftidir.
type pipeLink struct {
	out    chan []byte // bu ucun yazdigi
	in     chan []byte // bu ucun okudugu
	closed chan struct{}
	once   sync.Once
}

// NewPipeLink, birbirine bagli iki Link ucu dondurur (a'nin Send'i b'nin
// Recv'ine gider ve tersi). Surec-ici mesh koprusu gibi davranir.
func NewPipeLink() (Link, Link) {
	ab := make(chan []byte, 64)
	ba := make(chan []byte, 64)
	closed := make(chan struct{})
	a := &pipeLink{out: ab, in: ba, closed: closed}
	b := &pipeLink{out: ba, in: ab, closed: closed}
	return a, b
}

func (l *pipeLink) Send(frame []byte) error {
	// Cerceveyi kopyala: cagiran tamponu yeniden kullanabilir.
	cp := make([]byte, len(frame))
	copy(cp, frame)
	select {
	case l.out <- cp:
		return nil
	case <-l.closed:
		return ErrClosedLink
	}
}

func (l *pipeLink) Recv() ([]byte, error) {
	select {
	case f := <-l.in:
		return f, nil
	case <-l.closed:
		// Kapanmadan once kuyrukta kalan varsa onlari da ver.
		select {
		case f := <-l.in:
			return f, nil
		default:
			return nil, ErrClosedLink
		}
	}
}

func (l *pipeLink) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// SealPacket / OpenPacket: UDP datagram modu. Her datagram TEK bir chunk
// olarak bagimsizca sifrelenir; mesaj sinirlari korunur. UDP proxy'si bu
// iki islevi kullanir (istemci datagrami sifrele -> mesh -> hedefte coz).
func (p *Proxy) SealPacket(datagram []byte) ([]byte, error) { return p.cipher.Seal(datagram) }
func (p *Proxy) OpenPacket(blob []byte) ([]byte, error)     { return p.cipher.Open(blob) }
