// Package lora, LoRa ISM 868/915 MHz icin donanim soyutlama katmanidir (HAL).
//
// # DURUSTLUK NOTU — NE VAR, NE YOK
//
// Bu paket iki sey saglar:
//
//  1. GERCEK bir cerceveleme (framing) katmani: RadioHead RH_RF95 uyumlu
//     4 baytlik baslik + Aetheris parca basligi + CRC-16. Bu kod, telde
//     gidecek baytlarin bicimini URETIR ve DOGRULAR; tamamen test edilmistir.
//
//  2. Iki surucu yolu:
//     - SerialDriver: UART kopruluu LoRa modemleri (RadioHead/RadioLib
//     serisi, RN2483 gibi) icin. Gercek bir /dev/tty aygitini acar.
//     - MockLoRaDriver: fiziksel aygit YOKKEN otomatik devreye girer.
//     Paylasilan RF ortamini (herkes herkesi duyar) bellek-ici simule
//     eder, tum cerceveleri loglar.
//
// NE YAPILMADI: SX1262'nin SPI register programlamasi (spidev ioctl ile
// frekans, yayilma faktoru SF, bant genisligi BW ayari) BURADA YOKTUR.
// O katman gercek donanim ve spidev erisimi ister; simulasyonda anlamsizdir.
// SerialDriver, register programlamasini modemin kendi firmware'ine birakan
// UART-kopruluu modulleri hedefler. Bu, dimport edildiginde CGO/root
// gerektirmeyen, konteynerde -race altinda kosabilen dururst bir sinirdir.
//
// # NEDEN 222 BAYT
//
// LoRa fiziksel katmaninda tek bir paketin tasiyabilecegi azami yuk,
// yayilma faktoru ve bant genisligine gore degisir; SF7/125kHz'de pratik
// ust sinir ~222 bayttir. Daha uzun mesajlar telde tek pakete SIGMAZ;
// bu yuzden HAL, MTU'yu asan yukleri donanim seviyesinde otomatik boler
// (bkz. Framer / Transceiver) ve alici tarafta yeniden birlestirir.
package lora

import (
	"context"
	"errors"
)

// MTU, tek bir LoRa cercevesinin telde tasiyabilecegi azami toplam bayt.
// Baslik + parca basligi + yuk + CRC bu sinira SIGMALIDIR.
const MTU = 222

const (
	// rhHeaderSize, RadioHead RH_RF95 uyumlu baslik: To, From, MsgID, Flags.
	rhHeaderSize = 4
	// fragHeaderSize, Aetheris parca basligi:
	//   PacketID uint16 | Index uint8 | Count uint8 | ChunkLen uint16
	fragHeaderSize = 6
	// crcSize, cercevenin sonundaki CRC-16 (baslik+yuk uzerinden).
	crcSize = 2

	// MaxChunk, tek cercevede tasinabilecek azami yuk parcasi.
	// 222 - 4 - 6 - 2 = 210 bayt.
	MaxChunk = MTU - rhHeaderSize - fragHeaderSize - crcSize
)

// RadioHead bayrak sabitleri (RH_RF95 ile uyumlu).
const (
	// BroadcastAddr, tum dugumlere yayin adresi.
	BroadcastAddr byte = 0xFF
)

var (
	// ErrNoHardware, fiziksel LoRa aygiti bulunamadiginda doner.
	ErrNoHardware = errors.New("lora: fiziksel aygit bulunamadi")
	// ErrFrameTooLarge, tek cerceve MTU'yu astiginda doner.
	ErrFrameTooLarge = errors.New("lora: cerceve MTU sinirini asiyor")
	// ErrBadCRC, alinan cercevenin CRC dogrulamasi basarisiz oldugunda.
	ErrBadCRC = errors.New("lora: CRC dogrulamasi basarisiz")
	// ErrBadFrame, cerceve bicimi bozuk oldugunda.
	ErrBadFrame = errors.New("lora: bozuk cerceve")
	// ErrClosed, surucu kapatildiktan sonra kullanimda.
	ErrClosed = errors.New("lora: surucu kapatildi")
)

// Driver, ham LoRa cerceve gonderme/alma sozlesmesidir.
//
// Send'e verilen cerceve MTU'ya SIGMALIDIR; fragmentation Framer'in isidir,
// surucunun degil. Surucu yalnizca "bir cerceveyi tele koy / telden al"
// isini yapar. Bu ayrim, gercek donanim ile mock arasinda ayni arayuzun
// kullanilmasini saglar.
type Driver interface {
	// Name, surucu turunu dondurur ("mock" | "serial:/dev/ttyUSB0" ...).
	Name() string

	// IsHardware, gercek donanima mi yoksa simulasyona mi bagli oldugunu
	// DURUSTCE bildirir. Cagiran taraf, metriklerin gercek mi simule mi
	// oldugunu bu bayraga bakarak etiketler — sahte "gercek donanim"
	// iddiasi uretilmez.
	IsHardware() bool

	// Send, tek bir cerceveyi (<= MTU) tele koyar.
	Send(ctx context.Context, frame []byte) error

	// Receive, telden tek bir cerceve alir. Baglam iptalinde ctx.Err() doner.
	Receive(ctx context.Context) ([]byte, error)

	// Close, surucuyu kapatir ve kaynaklari serbest birakir.
	Close() error
}
