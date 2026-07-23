//go:build integration

package store

import (
	"context"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// TestIntegrationWALToPostgres, WAL katmaninin kayitlari GERCEK
// PostgreSQL'e bastigini ve yeniden baslatmada kurtardigini dogrular.
//
//	AETHERIS_TEST_DSN="postgres://..." go test -race -tags=integration \
//	  -run TestIntegrationWAL ./internal/store/
func TestIntegrationWALToPostgres(t *testing.T) {
	dsn := os.Getenv("AETHERIS_TEST_DSN")
	if dsn == "" {
		t.Skip("AETHERIS_TEST_DSN tanimsiz")
	}
	ctx := context.Background()

	pg, err := NewPostgres(ctx, "postgres", dsn)
	if err != nil {
		t.Fatalf("postgres baglantisi: %v", err)
	}
	defer pg.Close()

	// Temiz zemin.
	for _, tbl := range []string{"aetheris_ledger_events", "aetheris_ledger_carriers", "aetheris_ledgers"} {
		if _, err := pg.db.ExecContext(ctx, "TRUNCATE TABLE "+tbl+" CASCADE"); err != nil {
			t.Fatalf("%s temizlenemedi: %v", tbl, err)
		}
	}

	wal, err := NewWAL(ctx, pg, WALConfig{
		Dir:           t.TempDir(),
		FlushInterval: 50 * time.Millisecond,
		BatchSize:     16,
		Logger:        quietLog(),
	})
	if err != nil {
		t.Fatalf("WAL kurulamadi: %v", err)
	}

	const n = 100
	for i := 0; i < n; i++ {
		if err := wal.Record(ctx, Usage{
			ClientID:    "acme",
			CarrierType: "optical_li_fi",
			BytesIn:     10,
			BytesOut:    4,
			PayloadSHA:  "sha",
			Signature:   "sig",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Close, kalan kuyrugu Postgres'e bosaltir.
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	// Yeni bir Postgres baglantisindan oku (diskten geldigini garanti et).
	fresh, err := NewPostgres(ctx, "postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	e, err := fresh.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatalf("WAL sonrasi Postgres'te kayit yok: %v", err)
	}
	if e.Requests != n {
		t.Fatalf("Requests = %d, beklenen %d", e.Requests, n)
	}
	if e.BytesIn != n*10 {
		t.Fatalf("BytesIn = %d, beklenen %d", e.BytesIn, n*10)
	}

	// Olay defterinde n satir olmali (append-only).
	var events int
	if err := fresh.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM aetheris_ledger_events WHERE client_id=$1", "acme").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != n {
		t.Fatalf("olay sayisi = %d, beklenen %d", events, n)
	}
}
