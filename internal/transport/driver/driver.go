// Package driver, YEREL DONANIM tasima suruculeri icin bir soyutlama katmani
// (HAL — Hardware Abstraction Layer) tanimlar. Bluetooth/BLE Mesh, Wi-Fi
// SoftAP/Direct gibi ileride eklenecek yerel cip kutuphaneleri bu arayuzu
// uygulayarak sisteme takilir. Mimari zemin %100 hazirdir; gercek cip
// suruculeri (gomulu C kutuphaneleri, platform API'leri) bu sozlesmeye gore
// yazilir.
//
// DURUSTLUK NOTU: Bu paket ARAYUZ + kayit defteri + stub saglar. Gercek BLE/
// SoftAP radyo kontrolu, platforma ozel cip kutuphaneleri gerektirir ve bu
// surumde YOKTUR (v0.6b+ entegrasyonu). Amac, o suruculerin takilacagi
// standart yuvayi tanimlamaktir.
package driver

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Kind, tasima surucusunun turudur.
type Kind string

const (
	KindBLE      Kind = "ble"      // Bluetooth Low Energy / BLE Mesh
	KindSoftAP   Kind = "softap"   // Wi-Fi SoftAP (erisim noktasi modu)
	KindWiFiP2P  Kind = "wifi_p2p" // Wi-Fi Direct
	KindSerial   Kind = "serial"   // UART/seri (LoRa modem vb.)
	KindEthernet Kind = "ethernet" // kablolu
)

// Capabilities, bir surucunun yeteneklerini bildirir.
type Capabilities struct {
	Kind         Kind
	Name         string
	MaxMTU       int  // azami cerceve boyutu (bayt)
	Broadcast    bool // yayin destegi
	Mesh         bool // cok-sicramali mesh destegi
	Duplex       bool // tam cift yonlu
	NeedsPairing bool // eslesme/kimlik gerektiriyor mu
}

// Driver, bir yerel tasima surucusunun sozlesmesidir. Gercek uygulamalar
// (BLE cipi, SoftAP yoneticisi) bunu karsilar.
type Driver interface {
	// Capabilities, surucunun yeteneklerini dondurur.
	Capabilities() Capabilities
	// Open, surucuyu (donanimi) hazir hale getirir.
	Open(ctx context.Context) error
	// Send, bir cerceveyi hedefe (veya yayina) gonderir.
	Send(ctx context.Context, dst string, frame []byte) error
	// Receive, gelen cerceveleri dondurur (dst = kaynak).
	Receive() <-chan Frame
	// Available, donanimin fiziksel olarak mevcut/hazir oldugunu bildirir.
	Available() bool
	// Close, surucuyu kapatir.
	Close() error
}

// Frame, alinan bir cercevedir.
type Frame struct {
	Src  string
	Data []byte
}

var (
	ErrNotAvailable  = errors.New("driver: donanim mevcut degil")
	ErrAlreadyExists = errors.New("driver: bu tur zaten kayitli")
	ErrNotFound      = errors.New("driver: surucu bulunamadi")
)

// Registry, kullanilabilir tasima suruculerini tutar. Gateway, kayitli ve
// donanimsal olarak MEVCUT suruculeri otomatik devreye alir.
type Registry struct {
	mu      sync.RWMutex
	drivers map[Kind]Driver
}

// NewRegistry, bos bir kayit defteri olusturur.
func NewRegistry() *Registry {
	return &Registry{drivers: make(map[Kind]Driver)}
}

// Register, bir surucuyu kaydeder.
func (r *Registry) Register(d Driver) error {
	k := d.Capabilities().Kind
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.drivers[k]; ok {
		return ErrAlreadyExists
	}
	r.drivers[k] = d
	return nil
}

// Get, bir turdeki surucuyu dondurur.
func (r *Registry) Get(k Kind) (Driver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[k]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

// AvailableDrivers, donanimsal olarak MEVCUT (hazir) suruculeri dondurur.
// Gateway bunlari ikincil/off-grid katman olarak otomatik devreye alir.
func (r *Registry) AvailableDrivers() []Driver {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Driver
	for _, d := range r.drivers {
		if d.Available() {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Capabilities().Name < out[j].Capabilities().Name
	})
	return out
}

// List, kayitli tum suruculerin yeteneklerini dondurur (telemetri icin).
func (r *Registry) List() []Capabilities {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Capabilities, 0, len(r.drivers))
	for _, d := range r.drivers {
		out = append(out, d.Capabilities())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
