package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PostgresStore, kalıcı defterdir. Sunucu yeniden başlasa dahi
// faturalama verisi korunur.
//
// VERİ MODELİ:
//   - aetheris_ledgers          : istemci başına birikimli toplamlar
//   - aetheris_ledger_carriers  : taşıyıcı kırılımı
//   - aetheris_ledger_events    : SALT-EKLEME (append-only) olay defteri
//
// Olay defteri neden var? Birikimli toplam, faturalama itirazında
// "bu rakam nereden çıktı" sorusunu yanıtlayamaz. Her geçiş ayrı satır
// olarak, makbuz özeti ve imzasıyla saklanır. Bu satırlar hiçbir zaman
// GÜNCELLENMEZ veya SİLİNMEZ; denetlenebilirliğin temeli budur.
//
// Yükün kendisi ASLA saklanmaz — yalnızca SHA-256 özeti. Sıfır bilgi
// sözleşmesi veritabanı katmanında da geçerlidir.
type PostgresStore struct {
	db *sql.DB
}

// migrations, sırayla uygulanan şema sürümleridir.
// Yeni şema değişikliği eklerken MEVCUT girdileri ASLA değiştirme;
// listenin sonuna yeni bir sürüm ekle.
var migrations = []struct {
	Version int
	SQL     string
}{
	{
		Version: 1,
		SQL: `
CREATE TABLE IF NOT EXISTS aetheris_ledgers (
    client_id   TEXT PRIMARY KEY,
    bytes_in    BIGINT      NOT NULL DEFAULT 0,
    bytes_out   BIGINT      NOT NULL DEFAULT 0,
    requests    BIGINT      NOT NULL DEFAULT 0,
    first_seen  TIMESTAMPTZ NOT NULL,
    last_seen   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS aetheris_ledger_carriers (
    client_id    TEXT   NOT NULL REFERENCES aetheris_ledgers(client_id) ON DELETE CASCADE,
    carrier_type TEXT   NOT NULL,
    bytes        BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (client_id, carrier_type)
);

CREATE TABLE IF NOT EXISTS aetheris_ledger_events (
    id           BIGSERIAL PRIMARY KEY,
    client_id    TEXT        NOT NULL,
    carrier_type TEXT        NOT NULL,
    bytes_in     BIGINT      NOT NULL,
    bytes_out    BIGINT      NOT NULL,
    payload_sha  TEXT        NOT NULL,
    signature    TEXT        NOT NULL,
    destination  TEXT        NOT NULL DEFAULT '',
    occurred_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ledger_events_client_time
    ON aetheris_ledger_events (client_id, occurred_at);
`,
	},
}

// NewPostgres, verilen DSN ile bağlanır, bağlantıyı doğrular ve
// bekleyen migration'ları uygular.
func NewPostgres(ctx context.Context, driverName, dsn string) (*PostgresStore, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: baglanti acilamadi: %w", err)
	}

	// Bağlantı havuzu sınırları — sınırsız havuz, veritabanını
	// yoğun yük altında bağlantı tükenmesine sürükler.
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: veritabanina ulasilamadi: %w", err)
	}

	s := &PostgresStore{db: db}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// NewPostgresWithDB, hazır bir *sql.DB ile store oluşturur.
// Testlerde sahte (mock) sürücü enjekte etmek için kullanılır.
func NewPostgresWithDB(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Migrate, uygulanmamış şema sürümlerini sırayla uygular.
// Her sürüm kendi transaction'ında çalışır; yarım kalmış şema oluşmaz.
func (p *PostgresStore) Migrate(ctx context.Context) error {
	const createVersions = `
CREATE TABLE IF NOT EXISTS aetheris_schema_migrations (
    version    INT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`
	if _, err := p.db.ExecContext(ctx, createVersions); err != nil {
		return fmt.Errorf("store: migration tablosu olusturulamadi: %w", err)
	}

	for _, m := range migrations {
		var exists bool
		err := p.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM aetheris_schema_migrations WHERE version = $1)`,
			m.Version).Scan(&exists)
		if err != nil {
			return fmt.Errorf("store: migration %d durumu okunamadi: %w", m.Version, err)
		}
		if exists {
			continue
		}

		tx, err := p.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: migration %d transaction acilamadi: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %d uygulanamadi: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO aetheris_schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %d kaydedilemedi: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %d commit edilemedi: %w", m.Version, err)
		}
	}
	return nil
}

// Record, tek bir geçişi ATOMİK olarak işler.
// Üç tablo tek transaction'da güncellenir: ya hepsi ya hiçbiri.
func (p *PostgresStore) Record(ctx context.Context, u Usage) error {
	if u.OccurredAt.IsZero() {
		u.OccurredAt = time.Now().UTC()
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: transaction acilamadi: %w", err)
	}
	// Erken dönüşlerde sızıntı olmaması için savunmacı rollback.
	// Commit başarılıysa bu çağrı etkisizdir.
	defer func() { _ = tx.Rollback() }()

	const upsertLedger = `
INSERT INTO aetheris_ledgers (client_id, bytes_in, bytes_out, requests, first_seen, last_seen)
VALUES ($1, $2, $3, 1, $4, $4)
ON CONFLICT (client_id) DO UPDATE SET
    bytes_in  = aetheris_ledgers.bytes_in  + EXCLUDED.bytes_in,
    bytes_out = aetheris_ledgers.bytes_out + EXCLUDED.bytes_out,
    requests  = aetheris_ledgers.requests  + 1,
    last_seen = GREATEST(aetheris_ledgers.last_seen, EXCLUDED.last_seen)`

	if _, err := tx.ExecContext(ctx, upsertLedger,
		u.ClientID, int64(u.BytesIn), int64(u.BytesOut), u.OccurredAt); err != nil {
		return fmt.Errorf("store: defter guncellenemedi: %w", err)
	}

	const upsertCarrier = `
INSERT INTO aetheris_ledger_carriers (client_id, carrier_type, bytes)
VALUES ($1, $2, $3)
ON CONFLICT (client_id, carrier_type) DO UPDATE SET
    bytes = aetheris_ledger_carriers.bytes + EXCLUDED.bytes`

	if _, err := tx.ExecContext(ctx, upsertCarrier,
		u.ClientID, u.CarrierType, int64(u.BytesIn+u.BytesOut)); err != nil {
		return fmt.Errorf("store: tasiyici kirilimi guncellenemedi: %w", err)
	}

	const insertEvent = `
INSERT INTO aetheris_ledger_events
    (client_id, carrier_type, bytes_in, bytes_out, payload_sha, signature, destination, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	if _, err := tx.ExecContext(ctx, insertEvent,
		u.ClientID, u.CarrierType, int64(u.BytesIn), int64(u.BytesOut),
		u.PayloadSHA, u.Signature, u.Destination, u.OccurredAt); err != nil {
		return fmt.Errorf("store: olay kaydedilemedi: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit basarisiz: %w", err)
	}
	return nil
}

func (p *PostgresStore) ClientUsage(ctx context.Context, clientID string) (*Entry, error) {
	const q = `
SELECT bytes_in, bytes_out, requests, first_seen, last_seen
FROM aetheris_ledgers WHERE client_id = $1`

	var (
		bytesIn, bytesOut, requests int64
		firstSeen, lastSeen         time.Time
	)
	err := p.db.QueryRowContext(ctx, q, clientID).
		Scan(&bytesIn, &bytesOut, &requests, &firstSeen, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: defter okunamadi: %w", err)
	}

	e := &Entry{
		BytesIn:   uint64(bytesIn),
		BytesOut:  uint64(bytesOut),
		Requests:  uint64(requests),
		ByCarrier: make(map[string]uint64),
		FirstSeen: firstSeen.UTC(),
		LastSeen:  lastSeen.UTC(),
	}

	rows, err := p.db.QueryContext(ctx,
		`SELECT carrier_type, bytes FROM aetheris_ledger_carriers WHERE client_id = $1`, clientID)
	if err != nil {
		return nil, fmt.Errorf("store: tasiyici kirilimi okunamadi: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ct string
		var b int64
		if err := rows.Scan(&ct, &b); err != nil {
			return nil, fmt.Errorf("store: tasiyici satiri okunamadi: %w", err)
		}
		e.ByCarrier[ct] = uint64(b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tasiyici sonuc kumesi hatasi: %w", err)
	}
	return e, nil
}

func (p *PostgresStore) Snapshot(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{
		Clients:     make(map[string]*Entry),
		GeneratedAt: time.Now().UTC(),
	}

	rows, err := p.db.QueryContext(ctx,
		`SELECT client_id, bytes_in, bytes_out, requests, first_seen, last_seen FROM aetheris_ledgers`)
	if err != nil {
		return snap, fmt.Errorf("store: anlik goruntu alinamadi: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id                          string
			bytesIn, bytesOut, requests int64
			firstSeen, lastSeen         time.Time
		)
		if err := rows.Scan(&id, &bytesIn, &bytesOut, &requests, &firstSeen, &lastSeen); err != nil {
			return snap, fmt.Errorf("store: defter satiri okunamadi: %w", err)
		}
		snap.Clients[id] = &Entry{
			BytesIn:   uint64(bytesIn),
			BytesOut:  uint64(bytesOut),
			Requests:  uint64(requests),
			ByCarrier: make(map[string]uint64),
			FirstSeen: firstSeen.UTC(),
			LastSeen:  lastSeen.UTC(),
		}
		snap.TotalBytesIn += uint64(bytesIn)
		snap.TotalBytesOut += uint64(bytesOut)
		snap.TotalRequests += uint64(requests)
	}
	if err := rows.Err(); err != nil {
		return snap, fmt.Errorf("store: sonuc kumesi hatasi: %w", err)
	}

	cRows, err := p.db.QueryContext(ctx,
		`SELECT client_id, carrier_type, bytes FROM aetheris_ledger_carriers`)
	if err != nil {
		return snap, fmt.Errorf("store: tasiyici kirilimlari okunamadi: %w", err)
	}
	defer cRows.Close()

	for cRows.Next() {
		var id, ct string
		var b int64
		if err := cRows.Scan(&id, &ct, &b); err != nil {
			return snap, fmt.Errorf("store: tasiyici satiri okunamadi: %w", err)
		}
		if e, ok := snap.Clients[id]; ok {
			e.ByCarrier[ct] = uint64(b)
		}
	}
	if err := cRows.Err(); err != nil {
		return snap, fmt.Errorf("store: tasiyici sonuc kumesi hatasi: %w", err)
	}
	return snap, nil
}

func (p *PostgresStore) Kind() string { return "postgres" }

func (p *PostgresStore) Close() error { return p.db.Close() }
