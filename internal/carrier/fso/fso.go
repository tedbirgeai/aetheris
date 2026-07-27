// Package fso, Serbest Uzay Optik (Free Space Optical) iletişim katmanını
// implement eder. Lazer veya kızılötesi ışık ile bina-bina veya açık alanda
// gigabit hızında veri iletimi sağlar.
//
// Özellikler:
//   - Frekans lisansı YOK (ışık spektrumu serbesttir)
//   - 100m - 5km menzil
//   - 100 Mbps - 10 Gbps bant genişliği
//   - Görüş hattı (LOS) gerektirir
//   - Hava koşullarından (sis, yağmur) etkilenir
//
// Gerçek donanım: Lightpointe, GeoDesy, MRV Communications FSO birim.
// Mock: Hava durumu simülasyonu ile gerçekçi zayıflama modeli.
package fso

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrNotOpen      = errors.New("fso: adaptör açılmadı")
	ErrNoLOS        = errors.New("fso: görüş hattı (LOS) yok")
	ErrWeatherBlock = errors.New("fso: hava koşulları bağlantıyı kesiyor")
	ErrTooBig       = errors.New("fso: çerçeve maksimum boyutu aşıyor")
)

// Wavelength, FSO lazer dalga boyudur.
type Wavelength int

const (
	Wave785nm  Wavelength = 785  // Görünür kızılötesi — kısa menzil, ucuz
	Wave850nm  Wavelength = 850  // Standart — yaygın kullanım
	Wave1550nm Wavelength = 1550 // Telekom bandı — en uzun menzil, göz güvenli
)

// Weather, hava koşullarını simüle eder.
type Weather int

const (
	WeatherClear Weather = iota // Açık — tam güç
	WeatherHaze                 // Hafif sis — %20 kayıp
	WeatherFog                  // Yoğun sis — %60 kayıp
	WeatherRain                 // Yağmur — %40 kayıp
	WeatherStorm                // Fırtına — bağlantı kopabilir
)

func (w Weather) AttenuationDB() float64 {
	switch w {
	case WeatherClear:
		return 0
	case WeatherHaze:
		return 3
	case WeatherFog:
		return 12
	case WeatherRain:
		return 6
	case WeatherStorm:
		return 20
	default:
		return 0
	}
}

func (w Weather) String() string {
	return []string{"açık", "hafif-sis", "yoğun-sis", "yağmur", "fırtına"}[w]
}

// Config, FSO adaptör yapılandırmasıdır.
type Config struct {
	NodeID       string
	Wavelength   Wavelength
	MaxDistanceM int     // maksimum menzil (metre)
	TxPowerMW    float64 // verici gücü (mW)
	ApertureMMsq float64 // alıcı açıklığı (mm²)
}

// DefaultConfig, güvenli varsayılan FSO yapılandırması.
func DefaultConfig(nodeID string) Config {
	return Config{
		NodeID:       nodeID,
		Wavelength:   Wave1550nm,
		MaxDistanceM: 2000,
		TxPowerMW:    100,
		ApertureMMsq: 400,
	}
}

// LinkBudget, menzil ve hava koşuluna göre bağlantı bütçesini hesaplar.
func LinkBudget(cfg Config, distM int, weather Weather) (availableDB, requiredDB float64, ok bool) {
	// Serbest uzay yol kaybı (Friis formülü optik versiyonu).
	freespaceDB := 20 * math.Log10(float64(distM)/1000.0) // basitleştirilmiş
	weatherLoss := weather.AttenuationDB()
	totalLoss := freespaceDB + weatherLoss
	// Verici gücü → dBm
	txDBm := 10 * math.Log10(cfg.TxPowerMW)
	// Minimum alıcı hassasiyeti (tipik -40 dBm)
	rxSensDBm := -40.0
	available := txDBm - totalLoss
	required := rxSensDBm
	return available, required, available > required
}

// Frame, FSO üzerinden iletilen veri çerçevesidir.
type Frame struct {
	Payload   []byte
	SizeKB    float64
	Weather   Weather
	RSSI_DBm  float64
	LatencyUs int // mikrosaniye cinsinden gecikme
}

// MTU, optik çerçeve başına maksimum yük (Ethernet MTU ile uyumlu).
const MTU = 9000 // Jumbo frame

// FSODriver, bir FSO biriminin soyutlamasıdır.
type FSODriver interface {
	Open(ctx context.Context) error
	Send(ctx context.Context, f Frame) error
	Receive(ctx context.Context) (Frame, error)
	LinkQuality() LinkQuality
	Available() bool
	SetWeather(w Weather) // test/simülasyon için
	Close() error
}

// LinkQuality, anlık bağlantı kalitesini özetler.
type LinkQuality struct {
	RSSI_DBm     float64
	Weather      Weather
	EstBWMbps    float64
	LinkOK       bool
	Availability float64 // 0-1 arası (1 = %100)
}

// --- Mock FSO Sürücüsü ---

// SharedOptical, birden fazla MockFSO'nun iletişim kurduğu sanal optik ortam.
type SharedOptical struct {
	mu      sync.Mutex
	devices []*MockFSO
	weather Weather
}

func NewSharedOptical(weather Weather) *SharedOptical {
	return &SharedOptical{weather: weather}
}

func (o *SharedOptical) SetWeather(w Weather) {
	o.mu.Lock()
	o.weather = w
	o.mu.Unlock()
}

func (o *SharedOptical) attach(d *MockFSO) {
	o.mu.Lock()
	o.devices = append(o.devices, d)
	o.mu.Unlock()
}

func (o *SharedOptical) detach(d *MockFSO) {
	o.mu.Lock()
	out := o.devices[:0]
	for _, dev := range o.devices {
		if dev != d {
			out = append(out, dev)
		}
	}
	o.devices = out
	o.mu.Unlock()
}

func (o *SharedOptical) broadcast(src *MockFSO, f Frame) error {
	o.mu.Lock()
	devs := append([]*MockFSO(nil), o.devices...)
	w := o.weather
	o.mu.Unlock()

	// Hava koşulu bağlantıyı kesebilir.
	_, _, ok := LinkBudget(src.cfg, src.cfg.MaxDistanceM, w)
	if !ok {
		return ErrWeatherBlock
	}
	f.Weather = w
	f.RSSI_DBm = -20 - float64(w)*5 // simüle RSSI

	for _, d := range devs {
		if d == src || !d.opened.Load() {
			continue
		}
		select {
		case d.inbox <- f:
		default:
		}
	}
	return nil
}

type MockFSO struct {
	cfg     Config
	optical *SharedOptical
	inbox   chan Frame
	opened  atomic.Bool
	mu      sync.RWMutex
	tx, rx  atomic.Uint64
	weather Weather
	logger  *slog.Logger
}

func NewMockFSO(cfg Config, optical *SharedOptical, logger *slog.Logger) *MockFSO {
	if logger == nil {
		logger = slog.Default()
	}
	d := &MockFSO{
		cfg:     cfg,
		optical: optical,
		inbox:   make(chan Frame, 128),
		logger:  logger,
	}
	if optical != nil {
		optical.attach(d)
	}
	return d
}

func (d *MockFSO) Available() bool { return true }

func (d *MockFSO) Open(_ context.Context) error {
	d.opened.Store(true)
	d.logger.Info("FSO lazer köprü aktif",
		"dalga_boyu_nm", int(d.cfg.Wavelength),
		"menzil_m", d.cfg.MaxDistanceM)
	return nil
}

func (d *MockFSO) SetWeather(w Weather) {
	d.mu.Lock()
	d.weather = w
	d.mu.Unlock()
	if d.optical != nil {
		d.optical.SetWeather(w)
	}
}

func (d *MockFSO) LinkQuality() LinkQuality {
	d.mu.RLock()
	w := d.weather
	d.mu.RUnlock()
	_, _, ok := LinkBudget(d.cfg, d.cfg.MaxDistanceM, w)
	bw := 1000.0 // 1 Gbps varsayılan
	if !ok {
		bw = 0
	}
	avail := 1.0 - float64(w)*0.15
	if avail < 0 {
		avail = 0
	}
	return LinkQuality{
		RSSI_DBm:     -20 - float64(w)*5,
		Weather:      w,
		EstBWMbps:    bw,
		LinkOK:       ok,
		Availability: avail,
	}
}

func (d *MockFSO) Send(ctx context.Context, f Frame) error {
	if !d.opened.Load() {
		return ErrNotOpen
	}
	if len(f.Payload) > MTU {
		return ErrTooBig
	}
	f.SizeKB = float64(len(f.Payload)) / 1024
	f.LatencyUs = 1 // ~1µs gecikme (ışık hızı, 300m mesafe)
	if err := d.optical.broadcast(d, f); err != nil {
		return err
	}
	d.tx.Add(1)
	return nil
}

func (d *MockFSO) Receive(ctx context.Context) (Frame, error) {
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case f := <-d.inbox:
		d.rx.Add(1)
		return f, nil
	}
}

func (d *MockFSO) Close() error {
	d.opened.Store(false)
	if d.optical != nil {
		d.optical.detach(d)
	}
	return nil
}

// OpenHAL, FSO sürücüsünü açar.
func OpenHAL(cfg Config, optical *SharedOptical) (FSODriver, bool) {
	return NewMockFSO(cfg, optical, nil), false
}

// EstimateAvailability, yıllık ortalama FSO erişilebilirliğini tahmin eder.
// İklim verisine dayanır — İstanbul için tipik değerler.
func EstimateAvailability(distM int) float64 {
	switch {
	case distM <= 500:
		return 0.9999 // 99.99%
	case distM <= 1000:
		return 0.9995 // 99.95%
	case distM <= 2000:
		return 0.998 // 99.8%
	default:
		return 0.99 // 99%
	}
}

// RangeSummary, konfigürasyona göre özet bilgi döndürür.
func RangeSummary(cfg Config) string {
	return fmt.Sprintf("FSO %dnm | menzil %dm | %.0fmW | tahmini erişilebilirlik %%%.2f",
		int(cfg.Wavelength), cfg.MaxDistanceM, cfg.TxPowerMW,
		EstimateAvailability(cfg.MaxDistanceM)*100)
}

// WeatherSchedule, İstanbul iklimine göre saatlik hava durumu simülasyonu.
func SimulateWeather(hour int) Weather {
	// Basit model: sabah sis, öğlen açık, akşam yağmur ihtimali.
	switch {
	case hour >= 6 && hour <= 9:
		return WeatherHaze
	case hour >= 10 && hour <= 17:
		return WeatherClear
	case hour >= 18 && hour <= 21:
		if time.Now().Unix()%3 == 0 {
			return WeatherRain
		}
		return WeatherClear
	default:
		return WeatherClear
	}
}
