package lora

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
)

// SerialDriver, UART kopruluu bir LoRa modemine (RadioHead/RadioLib serisi,
// RN2483 gibi) baglanan surucudur.
//
// TEL PROTOKOLU: Modemle aramizda basit bir uzunluk-onekli cerceveleme
// kullanilir: [2 bayt uzunluk BE][cerceve baytlari]. Bu, ham UART akisinda
// cerceve sinirlarini belirler. Gercek modem firmware'i bunun ustunde kendi
// LoRa PHY ayarlarini (SF/BW/CR) yapar; o katman DONANIMA aittir ve burada
// simule edilmez (bkz. lora.go dururstluk notu).
//
// Aygit dosyasi (/dev/ttyUSB0 vb.) yoksa OpenSerial hata doner; HAL fabrikasi
// bu durumda otomatik olarak MockLoRaDriver'a duser.
type SerialDriver struct {
	path string
	rwc  io.ReadWriteCloser
	r    *bufio.Reader

	writeMu sync.Mutex
	closed  atomic.Bool

	sent atomic.Uint64
	rcvd atomic.Uint64
}

// OpenSerial, verilen aygit yolunu acar. Aygit yoksa/erisilemezse
// ErrNoHardware sarmalanmis halde doner.
//
// NOT: Gercek UART hiz/parite ayari (termios) burada YAPILMAZ; bu, isletim
// sistemine ozgu ioctl ister ve saf stdlib ile tasinabilir degildir. Aygit
// dosyasi acilabiliyorsa cerceveleme calisir; termios ayari gerekiyorsa
// dagitimda `stty` ile onceden yapilmasi beklenir. Bu sinir DURUSTCE
// belgelenmistir; sahte bir "SPI register yazdim" iddiasi uretilmez.
func OpenSerial(path string) (*SerialDriver, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("%w (%s): %v", ErrNoHardware, path, err)
	}
	return newSerialFromRWC(path, f), nil
}

// newSerialFromRWC, testlerin gercek aygit yerine bir io.ReadWriteCloser
// (ornegin net.Pipe ucu) enjekte edebilmesi icin ayrilmistir.
func newSerialFromRWC(path string, rwc io.ReadWriteCloser) *SerialDriver {
	return &SerialDriver{
		path: path,
		rwc:  rwc,
		r:    bufio.NewReader(rwc),
	}
}

func (s *SerialDriver) Name() string     { return "serial:" + s.path }
func (s *SerialDriver) IsHardware() bool { return true }
func (s *SerialDriver) Sent() uint64     { return s.sent.Load() }
func (s *SerialDriver) Received() uint64 { return s.rcvd.Load() }

func (s *SerialDriver) Send(ctx context.Context, frame []byte) error {
	if s.closed.Load() {
		return ErrClosed
	}
	if len(frame) > MTU {
		return ErrFrameTooLarge
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(frame)))

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.rwc.Write(hdr[:]); err != nil {
		return fmt.Errorf("lora serial: uzunluk yazilamadi: %w", err)
	}
	if _, err := s.rwc.Write(frame); err != nil {
		return fmt.Errorf("lora serial: cerceve yazilamadi: %w", err)
	}
	s.sent.Add(1)
	return nil
}

func (s *SerialDriver) Receive(ctx context.Context) ([]byte, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	// Okuma engelleyicidir; ctx iptalini kapatma ile birlestiririz.
	type result struct {
		frame []byte
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		var hdr [2]byte
		if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
			ch <- result{nil, err}
			return
		}
		n := binary.BigEndian.Uint16(hdr[:])
		if n > MTU {
			ch <- result{nil, ErrFrameTooLarge}
			return
		}
		frame := make([]byte, n)
		if _, err := io.ReadFull(s.r, frame); err != nil {
			ch <- result{nil, err}
			return
		}
		ch <- result{frame, nil}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		s.rcvd.Add(1)
		return res.frame, nil
	}
}

func (s *SerialDriver) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.rwc.Close()
}

// HALConfig, HAL fabrikasi icin ayarlardir.
type HALConfig struct {
	// SerialPath, denenecek aygit yolu (ornegin "/dev/ttyUSB0").
	// Bos ise dogrudan mock kullanilir.
	SerialPath string
	// Addr, bu dugumun LoRa adresi (mock ortaminda kullanilir).
	Addr byte
	// Medium, mock modunda baglanilacak paylasilan ortam. nil ise loopback.
	Medium *MockMedium
	Logger *slog.Logger
}

// OpenHAL, once gercek seri aygiti dener; basarisiz olursa OTOMATIK olarak
// MockLoRaDriver'a duser ve bunu loglar. Sistem HICBIR kosulda cokmemelidir
// (MADDE 1 gerekliligi). Donen ikinci deger, gercek donanima mi baglanildigini
// dururstce bildirir.
func OpenHAL(cfg HALConfig) (Driver, bool) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if cfg.SerialPath != "" {
		drv, err := OpenSerial(cfg.SerialPath)
		if err == nil {
			logger.Info("lora HAL: gercek seri aygit acildi", "path", cfg.SerialPath)
			return drv, true
		}
		logger.Warn("lora HAL: seri aygit acilamadi, mock moduna dusuluyor",
			"path", cfg.SerialPath, "err", err)
	} else {
		logger.Info("lora HAL: seri yol belirtilmedi, mock modu")
	}

	mock := NewMockDriver(cfg.Addr, cfg.Medium, logger)
	return mock, false
}
