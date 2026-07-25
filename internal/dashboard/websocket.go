package dashboard

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Bu dosya, DIS BAGIMLILIK OLMADAN (gorilla/websocket vb. yok) sunucu->istemci
// telemetri yayini icin yeterli, minimal ama dogru bir RFC 6455 WebSocket
// uygulamasidir. Yalnizca metin cerceveleri yazar; istemci kontrol
// cercevelerini (close/ping) okuyup uygun sekilde yanitlar. Tek binary ve
// offline-first ilkesine sadik kalir.

// wsGUID, RFC 6455 el sıkışma sabiti.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var (
	errNotWebSocket = errors.New("dashboard: WebSocket yukseltme istegi degil")
	errNoHijack     = errors.New("dashboard: ResponseWriter hijack desteklemiyor")
)

// wsConn, yukseltilmis bir WebSocket baglantisidir.
type wsConn struct {
	conn   io.ReadWriteCloser
	br     *bufio.Reader
	writeM sync.Mutex
	closed chan struct{}
	once   sync.Once
}

// wsUpgrade, HTTP baglantisini WebSocket'e yukseltir (el sıkışma + hijack).
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !headerContains(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errNotWebSocket
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errNotWebSocket
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errNoHijack
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	accept := wsAcceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &wsConn{
		conn:   conn,
		br:     rw.Reader,
		closed: make(chan struct{}),
	}, nil
}

func wsAcceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerContains(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// WriteText, tek bir metin cercevesi (opcode 0x1, FIN=1, maskesiz) yazar.
func (c *wsConn) WriteText(payload []byte) error {
	select {
	case <-c.closed:
		return io.ErrClosedPipe
	default:
	}

	c.writeM.Lock()
	defer c.writeM.Unlock()

	var header []byte
	n := len(payload)
	switch {
	case n <= 125:
		header = []byte{0x81, byte(n)}
	case n <= 0xFFFF:
		header = []byte{0x81, 126, 0, 0}
		binary.BigEndian.PutUint16(header[2:], uint16(n))
	default:
		header = make([]byte, 10)
		header[0] = 0x81
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:], uint64(n))
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

// writeControl, kontrol cercevesi (close/pong) yazar.
func (c *wsConn) writeControl(opcode byte, payload []byte) error {
	c.writeM.Lock()
	defer c.writeM.Unlock()
	if len(payload) > 125 {
		payload = payload[:125]
	}
	frame := append([]byte{0x80 | opcode, byte(len(payload))}, payload...)
	_, err := c.conn.Write(frame)
	return err
}

// readLoop, istemci cercevelerini okur. Yalnizca kontrol cerceveleriyle
// (close/ping) ilgilenir; close gelince veya hata olunca baglantiyi kapatir.
// Telemetri yalnizca sunucu->istemci aktigi icin veri cerceveleri yok sayilir.
func (c *wsConn) readLoop() {
	defer c.Close()
	for {
		opcode, payload, err := c.readFrame()
		if err != nil {
			return
		}
		switch opcode {
		case 0x8: // close
			_ = c.writeControl(0x8, payload)
			return
		case 0x9: // ping -> pong
			_ = c.writeControl(0xA, payload)
		default:
			// text/binary/pong: telemetri tek yonlu, yok say.
		}
	}
}

// readFrame, tek bir istemci cercevesini okur ve maskeyi cozer.
func (c *wsConn) readFrame() (opcode byte, payload []byte, err error) {
	h := make([]byte, 2)
	if _, err = io.ReadFull(c.br, h); err != nil {
		return 0, nil, err
	}
	opcode = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	length := uint64(h[1] & 0x7F)

	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(c.br, ext); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(c.br, ext); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext)
	}

	// Guvenlik: asiri buyuk cerceveleri reddet (istemci kontrol amacli
	// kucuk cerceveler gonderir).
	if length > 1<<20 {
		return 0, nil, errors.New("dashboard: cerceve cok buyuk")
	}

	var mask []byte
	if masked {
		mask = make([]byte, 4)
		if _, err = io.ReadFull(c.br, mask); err != nil {
			return 0, nil, err
		}
	}

	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

// Close, baglantiyi kapatir (idempotent).
func (c *wsConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.conn.Close()
}

// Done, baglanti kapandiginda kapanan kanali dondurur.
func (c *wsConn) Done() <-chan struct{} { return c.closed }
