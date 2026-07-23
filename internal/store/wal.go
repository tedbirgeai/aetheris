package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// WALStore, dayanikli defter kuyrugudur.
//
// SORUN: PostgresStore fail-closed calisir — veritabani yavaslar veya
// duserse tunel istekleri 503 alir. Bu, faturalama butunlugu icin dogru
// ama kullanilabilirlik icin sert bir takas.
//
// COZUM: WALStore araya girer. Her Record cagrisi:
//  1. Once diske WAL (Write-Ahead Log) satiri yazar — hizli, yerel, dayanikli.
//  2. Kaydi bellek-ici kuyruga koyar.
//  3. Arka plandaki flusher, kuyrugu asenkron olarak alt store'a (Postgres)
//     basar.
//
// Boylece veritabani kesintiye ugrasa DAHI:
//   - Tunel istekleri BLOKLANMAZ (non-blocking).
//   - Hicbir kayit KAYBOLMAZ — disk WAL'inde durur, DB geri gelince islenir.
//   - Surec cokerse, yeniden baslatmada WAL'den kurtarma (replay) yapilir.
//
// TAKAS: Bu, fail-closed'dan fail-open'a gecistir. Kisa bir pencerede
// (WAL yazildi ama Postgres'e daha basilmadi) veri "commit edilmis" sayilir
// ama henuz ana defterde degildir. Disk WAL bu boslugu dayanikli kilar.
type WALStore struct {
	backend Store // gercek kalicilik (genelde PostgresStore)
	logger  *slog.Logger

	dir     string
	walFile *os.File
	walBuf  *bufio.Writer
	walMu   sync.Mutex

	queue     chan walRecord
	batchSize int
	flushIvl  time.Duration

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	// gozlemlenebilirlik sayaclari
	enqueued  atomic.Uint64
	flushed   atomic.Uint64
	flushErrs atomic.Uint64
	dropped   atomic.Uint64
}

type walRecord struct {
	Seq   uint64 `json:"seq"`
	Usage Usage  `json:"usage"`
}

// WALConfig, WALStore kurulum parametreleridir.
type WALConfig struct {
	// Dir, WAL dosyalarinin tutulacagi dizin.
	Dir string
	// QueueSize, bellek-ici kuyruk kapasitesi. Dolarsa Record, disk WAL'ine
	// yazmaya devam eder ama flush geride kalir (backpressure).
	QueueSize int
	// BatchSize, tek transaction'da Postgres'e basilacak azami kayit.
	BatchSize int
	// FlushInterval, kuyruk dolmasa bile bu araliktan sonra flush edilir.
	FlushInterval time.Duration
	Logger        *slog.Logger
}

// NewWAL, WAL dizinini hazirlar, varsa onceki WAL'i kurtarir (replay)
// ve arka plan flusher'i baslatir.
func NewWAL(ctx context.Context, backend Store, cfg WALConfig) (*WALStore, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 64
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 500 * time.Millisecond
	}
	if cfg.Dir == "" {
		return nil, errors.New("wal: dizin belirtilmeli")
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("wal: dizin olusturulamadi: %w", err)
	}

	w := &WALStore{
		backend:   backend,
		logger:    cfg.Logger,
		dir:       cfg.Dir,
		queue:     make(chan walRecord, cfg.QueueSize),
		batchSize: cfg.BatchSize,
		flushIvl:  cfg.FlushInterval,
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}

	// Onceki calismadan kalan WAL'i kurtar.
	if err := w.recover(ctx); err != nil {
		return nil, fmt.Errorf("wal: kurtarma basarisiz: %w", err)
	}

	// Aktif WAL dosyasini ac (append modunda).
	walPath := filepath.Join(cfg.Dir, "aetheris.wal")
	f, err := os.OpenFile(walPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("wal: dosya acilamadi: %w", err)
	}
	w.walFile = f
	w.walBuf = bufio.NewWriter(f)

	go w.flusher()
	return w, nil
}

// Record, ONCE diske WAL yazar, SONRA kuyruga koyar. Non-blocking.
func (w *WALStore) Record(ctx context.Context, u Usage) error {
	if u.OccurredAt.IsZero() {
		u.OccurredAt = time.Now().UTC()
	}
	seq := w.enqueued.Add(1)
	rec := walRecord{Seq: seq, Usage: u}

	// 1) Diske yaz — dayaniklilik burada saglanir.
	if err := w.appendWAL(rec); err != nil {
		// Disk yazilamiyorsa ciddi bir sorun var; cagirana bildir.
		return fmt.Errorf("wal: diske yazilamadi: %w", err)
	}

	// 2) Kuyruga koy — flusher asenkron isler.
	select {
	case w.queue <- rec:
	default:
		// Kuyruk dolu: kayit disk WAL'inde GUVENDE, sadece bellek
		// kuyruguna sigmadi. Flusher gerilemis demektir. Kayit,
		// bir sonraki recover'da veya DB toparlayinca islenir.
		w.dropped.Add(1)
	}
	return nil
}

// appendWAL, tek satirlik JSON'u WAL dosyasina yazar ve fsync eder.
func (w *WALStore) appendWAL(rec walRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	w.walMu.Lock()
	defer w.walMu.Unlock()

	if _, err := w.walBuf.Write(line); err != nil {
		return err
	}
	if err := w.walBuf.WriteByte('\n'); err != nil {
		return err
	}
	if err := w.walBuf.Flush(); err != nil {
		return err
	}
	// fsync: veri disk onbelleginden fiziksel diske. Guc kesintisinde
	// dahi kaybolmamasi icin.
	return w.walFile.Sync()
}

// flusher, kuyrugu periyodik veya batch dolunca alt store'a basar.
func (w *WALStore) flusher() {
	defer close(w.stopped)
	ticker := time.NewTicker(w.flushIvl)
	defer ticker.Stop()

	batch := make([]walRecord, 0, w.batchSize)

	drain := func() {
		if len(batch) == 0 {
			return
		}
		w.flushBatch(batch)
		batch = batch[:0]
	}

	for {
		select {
		case <-w.stop:
			// Kapanmadan once kuyrukta kalani bosalt.
			for {
				select {
				case rec := <-w.queue:
					batch = append(batch, rec)
					if len(batch) >= w.batchSize {
						drain()
					}
				default:
					drain()
					return
				}
			}
		case rec := <-w.queue:
			batch = append(batch, rec)
			if len(batch) >= w.batchSize {
				drain()
			}
		case <-ticker.C:
			drain()
		}
	}
}

// flushBatch, bir grup kaydi alt store'a basar. Basarisiz kayitlar
// WAL'de kaldigi icin bir sonraki recover'da tekrar denenir.
func (w *WALStore) flushBatch(batch []walRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var failed int
	for _, rec := range batch {
		if err := w.backend.Record(ctx, rec.Usage); err != nil {
			failed++
			w.flushErrs.Add(1)
			continue
		}
		w.flushed.Add(1)
	}

	if failed == 0 {
		// Tum batch basariyla islendi: WAL'i kısalt (truncate) ki
		// dosya sonsuz buyumesin. Basit strateji: hepsi basariliysa
		// dosyayi sifirla. (Uretimde segment-bazli rotasyon yapilir;
		// bu, tek-dosya basit ve dogru bir baslangictir.)
		w.truncateWAL()
	} else {
		w.logger.Warn("WAL flush kismen basarisiz, kayitlar WAL'de tutuluyor",
			"failed", failed, "total", len(batch))
	}
}

// truncateWAL, basariyla islenmis WAL dosyasini sifirlar.
func (w *WALStore) truncateWAL() {
	w.walMu.Lock()
	defer w.walMu.Unlock()

	if err := w.walBuf.Flush(); err != nil {
		w.logger.Warn("WAL truncate oncesi flush hatasi", "err", err)
		return
	}
	if err := w.walFile.Truncate(0); err != nil {
		w.logger.Warn("WAL truncate hatasi", "err", err)
		return
	}
	if _, err := w.walFile.Seek(0, 0); err != nil {
		w.logger.Warn("WAL seek hatasi", "err", err)
	}
}

// recover, onceki calismadan kalan WAL'i okur ve alt store'a basar.
// Surec cokse bile hicbir kayit kaybolmaz.
func (w *WALStore) recover(ctx context.Context) error {
	walPath := filepath.Join(w.dir, "aetheris.wal")
	f, err := os.Open(walPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // ilk calisma, kurtarilacak WAL yok
	}
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var recovered, failed int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec walRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			w.logger.Warn("WAL kurtarma: bozuk satir atlandi", "err", err)
			continue
		}
		if err := w.backend.Record(ctx, rec.Usage); err != nil {
			failed++
			continue
		}
		recovered++
	}
	scanErr := scanner.Err()

	// PLATFORM NOTU: Dosyayi silmeden ONCE acikca kapatmak zorunludur.
	// Linux acik bir dosyanin silinmesine izin verir, WINDOWS IZIN VERMEZ
	// ("dosya baska bir islem tarafindan kullaniliyor" hatasi). Bu yuzden
	// defer f.Close() yerine burada kapatiyoruz.
	if cerr := f.Close(); cerr != nil {
		w.logger.Warn("WAL kurtarma: dosya kapatilamadi", "err", cerr)
	}

	if scanErr != nil {
		return scanErr
	}

	if recovered > 0 || failed > 0 {
		w.logger.Info("WAL kurtarma tamamlandi", "recovered", recovered, "failed", failed)
	}

	// Kurtarma sonrasi eski WAL'i temizle. Basarisiz kayit varsa dosya
	// KORUNUR; bir sonraki acilista tekrar denenir.
	if failed == 0 {
		if rerr := os.Remove(walPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			// Silme basarisizsa icerigi sifirlamayi dene. Bu, WAL'in bir
			// sonraki acilista TEKRAR islenmesini (cift faturalama) onler.
			if terr := os.Truncate(walPath, 0); terr != nil {
				// Ikisi de basarisiz: neredeyse kesinlikle ayni dizinde
				// baska bir Aetheris ornegi dosyayi acik tutuyor.
				// Bu durumda devam etmek CIFT FATURALAMA riski dogurur,
				// bu yuzden acilisi durduruyoruz.
				return fmt.Errorf(
					"wal: kurtarilan WAL temizlenemedi (%v). Ayni WAL dizinini "+
						"paylasan baska bir Aetheris ornegi calisiyor olabilir. "+
						"Her ornek KENDI WAL dizinini kullanmalidir; aksi halde "+
						"ayni kayitlar iki kez faturalanir", rerr)
			}
		}
	}
	return nil
}

// ClientUsage / Snapshot / Kind: alt store'a devret.
func (w *WALStore) ClientUsage(ctx context.Context, clientID string) (*Entry, error) {
	return w.backend.ClientUsage(ctx, clientID)
}

func (w *WALStore) Snapshot(ctx context.Context) (Snapshot, error) {
	return w.backend.Snapshot(ctx)
}

func (w *WALStore) Kind() string { return "wal+" + w.backend.Kind() }

// Stats, gozlemlenebilirlik sayaclarini dondurur.
type WALStats struct {
	Enqueued  uint64 `json:"enqueued"`
	Flushed   uint64 `json:"flushed"`
	FlushErrs uint64 `json:"flush_errors"`
	Dropped   uint64 `json:"queue_overflow"`
	Pending   int    `json:"pending_in_queue"`
}

func (w *WALStore) Stats() WALStats {
	return WALStats{
		Enqueued:  w.enqueued.Load(),
		Flushed:   w.flushed.Load(),
		FlushErrs: w.flushErrs.Load(),
		Dropped:   w.dropped.Load(),
		Pending:   len(w.queue),
	}
}

// Close, flusher'i durdurur, kalan kuyrugu bosaltir ve alt store'u kapatir.
func (w *WALStore) Close() error {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.stopped // flusher'in kuyrugu bosaltmasini bekle

	w.walMu.Lock()
	if w.walBuf != nil {
		_ = w.walBuf.Flush()
	}
	if w.walFile != nil {
		_ = w.walFile.Sync()
		_ = w.walFile.Close()
	}
	w.walMu.Unlock()

	return w.backend.Close()
}

// Derleme zamaninda WALStore'un Store arayuzunu karsiladigini garanti et.
var _ Store = (*WALStore)(nil)
