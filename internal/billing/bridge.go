// Package billing, olcum kayitlarini dis faturalama sistemlerine
// (Stripe, e-Fatura, egress kredi dususu) ileten koprudur.
//
// # TASARIM: ASENKRON VE ARIZAYA DAYANIKLI
//
// Faturalama koprusu tunel trafigini ASLA bloklamaz. WAL nasil defteri
// istegin onunden cekiyorsa, bu kopru de dis sistem cagrilarini istegin
// onunden ceker:
//
//	tunel istegi -> WAL (disk) -> [asenkron] -> Postgres
//	                            -> [asenkron] -> billing emitter -> Stripe/e-Fatura
//
// Stripe yavaslarsa veya duserse musteri istegi etkilenmez. Olaylar
// kuyrukta bekler, tekrar denenir.
//
// # DURUSTLUK NOTU — NE TEST EDILDI, NE EDILMEDI
//
// Bu paket ve WebhookEmitter GERCEKTEN test edilmistir (httptest ile).
// Ancak Stripe ve e-Fatura'nin CANLI API'lerine karsi dogrulama
// yapilmamistir; bunun icin gercek hesap kimlik bilgileri gerekir.
// StripeEmitter, Stripe'in "meter events" API'sinin bekledigi govde
// bicimini uretir ama uc noktaya karsi dogrulanmamistir. Uretime
// almadan once test anahtariyla bir kez dogrulanmalidir.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// EventType, fatura olayinin turudur.
type EventType string

const (
	// ReceiptGenerated, bir tunel gecisi olculup makbuz uretildiginde.
	ReceiptGenerated EventType = "receipt.generated"
	// CreditEarned, bir dugum baskasinin trafigini role ettiginde
	// kazanilan fatura indirim kredisi.
	CreditEarned EventType = "credit.earned"
	// UsageThresholdExceeded, istemci belirlenen kullanim esigini astiginda.
	UsageThresholdExceeded EventType = "usage.threshold_exceeded"
)

// Event, dis sistemlere iletilen fatura olayidir.
type Event struct {
	// ID, olayin benzersiz kimligi. TEKRARLI GONDERIM (retry) durumunda
	// alici tarafin ayni olayi iki kez faturalamamasi icin idempotency
	// anahtari olarak kullanilir.
	ID   string    `json:"id"`
	Type EventType `json:"type"`

	ClientID    string `json:"client_id"`
	CarrierType string `json:"carrier_type,omitempty"`
	Destination string `json:"destination,omitempty"`

	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`

	// PayloadSHA ve Signature, makbuzun denetim izidir.
	// YUKUN KENDISI ASLA GONDERILMEZ — yalnizca ozet.
	PayloadSHA string `json:"payload_sha256,omitempty"`
	Signature  string `json:"signature,omitempty"`

	// CreditUnits, CreditEarned olaylarinda kazanilan kredi birimi.
	CreditUnits uint64 `json:"credit_units,omitempty"`

	// ThresholdBytes, UsageThresholdExceeded olaylarinda asilan esik.
	ThresholdBytes uint64 `json:"threshold_bytes,omitempty"`

	OccurredAt time.Time `json:"occurred_at"`
}

// Emitter, tek bir dis hedefe olay ileten arayuzdur.
// Stripe, e-Fatura, dahili muhasebe — hepsi bu arayuzu uygular.
type Emitter interface {
	// Emit, olayi hedefe iletir. Hata donerse Bridge tekrar dener.
	Emit(ctx context.Context, ev Event) error
	// Name, log ve metrikler icin hedef adi.
	Name() string
}

// Bridge, olaylari kuyruga alip kayitli emitter'lara asenkron dagitir.
type Bridge struct {
	emitters []Emitter
	logger   *slog.Logger

	queue chan Event

	maxRetries int
	retryDelay time.Duration

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	// Gozlemlenebilirlik
	enqueued  atomic.Uint64
	delivered atomic.Uint64
	failed    atomic.Uint64
	dropped   atomic.Uint64
}

// Config, Bridge kurulum parametreleridir.
type Config struct {
	QueueSize  int
	MaxRetries int
	RetryDelay time.Duration
	Logger     *slog.Logger
}

// New, koprüyu kurar ve arka plan dagiticiyi baslatir.
func New(emitters []Emitter, cfg Config) *Bridge {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 2048
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryDelay <= 0 {
		cfg.RetryDelay = 2 * time.Second
	}

	b := &Bridge{
		emitters:   emitters,
		logger:     cfg.Logger,
		queue:      make(chan Event, cfg.QueueSize),
		maxRetries: cfg.MaxRetries,
		retryDelay: cfg.RetryDelay,
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	go b.dispatch()
	return b
}

// Publish, olayi kuyruga koyar. ASLA BLOKLAMAZ.
//
// Kuyruk doluysa olay DUSURULUR ve sayac artirilir. Bu bilincli bir
// tercihtir: faturalama koprusu, musteri istegini bekletmemelidir.
// Dusen olaylar `dropped` sayacinda gorulur; kalici kayit zaten WAL ve
// Postgres'te durur — kopru yalnizca DIS SISTEME BILDIRIM katmanidir,
// tek gercek kaynak degildir.
func (b *Bridge) Publish(ev Event) {
	if len(b.emitters) == 0 {
		return // kayitli hedef yok, sessizce gec
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	b.enqueued.Add(1)

	select {
	case b.queue <- ev:
	default:
		b.dropped.Add(1)
		b.logger.Warn("faturalama olayi dusuruldu: kuyruk dolu",
			"event_type", ev.Type, "client_id", ev.ClientID)
	}
}

func (b *Bridge) dispatch() {
	defer close(b.stopped)
	for {
		select {
		case <-b.stop:
			// Kapanmadan once kuyrukta kalani bosalt.
			for {
				select {
				case ev := <-b.queue:
					b.deliver(ev)
				default:
					return
				}
			}
		case ev := <-b.queue:
			b.deliver(ev)
		}
	}
}

// deliver, olayi tum emitter'lara iletir; hata olursa tekrar dener.
//
// KAPANIS DAVRANISI — ONEMLI:
// Ilk uygulamada geri cekilme beklemesi `case <-b.stop: return` iceriyordu.
// Bu, kapanis sirasinda TEKRAR DENEME BEKLEYEN OLAYLARIN SESSIZCE
// DUSMESINE yol aciyordu: dispatcher kuyrugu bosaltirken deliver()
// cagriliyor, ilk deneme basarisiz oluyor, ikinci denemeden once
// b.stop kapali oldugu icin fonksiyon donuyor ve olay kayboluyordu.
// Faturalama olaylarinin zarif kapanista kaybolmasi kabul edilemez.
//
// Duzeltme: kapanista denemeler TERK EDILMEZ, yalnizca geri cekilme
// suresi kisaltilir. Boylece Close() sinirli surede biter ama olaylar
// da elden gecirilmis olur. Close()'un kendi 30 saniyelik ust siniri
// kalici arizali bir emitter'da askida kalmayi onler.
func (b *Bridge) deliver(ev Event) {
	stopping := func() bool {
		select {
		case <-b.stop:
			return true
		default:
			return false
		}
	}

	for _, em := range b.emitters {
		var lastErr error
		for attempt := 0; attempt <= b.maxRetries; attempt++ {
			if attempt > 0 {
				// Ustel geri cekilme: 1x, 2x, 4x ...
				delay := b.retryDelay * time.Duration(1<<(attempt-1))
				if stopping() {
					// Kapaniyoruz: denemeyi BIRAKMA, sadece hizlandir.
					delay = time.Millisecond
				}
				time.Sleep(delay)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := em.Emit(ctx, ev)
			cancel()
			if err == nil {
				b.delivered.Add(1)
				lastErr = nil
				break
			}
			lastErr = err
		}
		if lastErr != nil {
			b.failed.Add(1)
			b.logger.Error("faturalama olayi iletilemedi",
				"emitter", em.Name(), "event_id", ev.ID,
				"event_type", ev.Type, "err", lastErr)
		}
	}
}

// Stats, gozlemlenebilirlik sayaclaridir.
type Stats struct {
	Enqueued  uint64 `json:"enqueued"`
	Delivered uint64 `json:"delivered"`
	Failed    uint64 `json:"failed"`
	Dropped   uint64 `json:"dropped"`
	Pending   int    `json:"pending"`
	Emitters  int    `json:"emitters"`
}

func (b *Bridge) Stats() Stats {
	return Stats{
		Enqueued:  b.enqueued.Load(),
		Delivered: b.delivered.Load(),
		Failed:    b.failed.Load(),
		Dropped:   b.dropped.Load(),
		Pending:   len(b.queue),
		Emitters:  len(b.emitters),
	}
}

// Close, kuyrugu bosaltip dagiticiyi durdurur.
func (b *Bridge) Close() error {
	b.stopOnce.Do(func() { close(b.stop) })
	select {
	case <-b.stopped:
	case <-time.After(30 * time.Second):
		return errors.New("billing: kapanis zaman asimina ugradi")
	}
	return nil
}

// MarshalEvent, olayi JSON'a cevirir (emitter'lar icin yardimci).
func MarshalEvent(ev Event) ([]byte, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("billing: olay kodlanamadi: %w", err)
	}
	return b, nil
}
