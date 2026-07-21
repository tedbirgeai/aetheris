//go:build integration

// Bu dosya YALNIZCA `-tags=integration` ile derlenir ve CANLI bir
// PostgreSQL ornegi gerektirir. Varsayilan `go test ./...` calistirmasi
// bu testleri atlar; boylece gelistirici makinesinde veritabani zorunlu
// olmaz ama CI'da gercek surucu davranisi dogrulanabilir.
//
// Calistirmak icin:
//
//	AETHERIS_TEST_DSN="postgres://user:pass@localhost:5432/aetheris?sslmode=disable" \
//	  go test -race -tags=integration ./internal/store/
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func integrationStore(t *testing.T) *PostgresStore {
	t.Helper()
	dsn := os.Getenv("AETHERIS_TEST_DSN")
	if dsn == "" {
		t.Skip("AETHERIS_TEST_DSN tanimsiz - entegrasyon testi atlandi")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := NewPostgres(ctx, "postgres", dsn)
	if err != nil {
		t.Fatalf("baglanti kurulamadi: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Her test kendi temiz zeminini kurar.
	for _, tbl := range []string{
		"aetheris_ledger_events",
		"aetheris_ledger_carriers",
		"aetheris_ledgers",
	} {
		if _, err := st.db.ExecContext(ctx, "TRUNCATE TABLE "+tbl+" CASCADE"); err != nil {
			t.Fatalf("%s temizlenemedi: %v", tbl, err)
		}
	}
	return st
}

// TestIntegrationMigrationsAreIdempotent, migration'in tekrar tekrar
// calistirilabildigini dogrular.
func TestIntegrationMigrationsAreIdempotent(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("%d. migration calistirmasi basarisiz: %v", i+1, err)
		}
	}

	var count int
	if err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM aetheris_schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) {
		t.Fatalf("migration kaydi = %d, beklenen %d", count, len(migrations))
	}
}

// TestIntegrationRecordPersists, kaydin gercekten diske yazildigini ve
// YENI bir baglantidan okunabildigini dogrular. Kalicilik iddiasinin
// somut kaniti budur.
func TestIntegrationRecordPersists(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	usage := Usage{
		ClientID:    "acme",
		CarrierType: "optical_li_fi",
		BytesIn:     64,
		BytesOut:    304,
		PayloadSHA:  "abc123",
		Signature:   "sig456",
		Destination: "edge-1",
		OccurredAt:  now,
	}
	if err := st.Record(ctx, usage); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Ayri bir baglantidan oku - onbellekten degil, diskten geldigini
	// garanti etmek icin.
	fresh, err := NewPostgres(ctx, "postgres", os.Getenv("AETHERIS_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	e, err := fresh.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatalf("ClientUsage: %v", err)
	}
	if e.BytesIn != 64 || e.BytesOut != 304 || e.Requests != 1 {
		t.Fatalf("kalici veri hatali: %+v", e)
	}
	if e.ByCarrier["optical_li_fi"] != 368 {
		t.Fatalf("tasiyici kirilimi = %v", e.ByCarrier)
	}
}

// TestIntegrationAccumulates, ayni istemcinin birden fazla geciste
// dogru birikim yaptigini dogrular.
func TestIntegrationAccumulates(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := st.Record(ctx, Usage{
			ClientID:    "acme",
			CarrierType: "mesh_wifi",
			BytesIn:     10,
			BytesOut:    5,
			PayloadSHA:  fmt.Sprintf("sha-%d", i),
			Signature:   "sig",
		}); err != nil {
			t.Fatal(err)
		}
	}

	e, err := st.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if e.BytesIn != 50 || e.BytesOut != 25 || e.Requests != 5 {
		t.Fatalf("birikim hatali: %+v", e)
	}
	if e.ByCarrier["mesh_wifi"] != 75 {
		t.Fatalf("tasiyici birikimi = %d, beklenen 75", e.ByCarrier["mesh_wifi"])
	}
}

// TestIntegrationEventLedgerIsAppendOnly, her gecisin ayri bir olay
// satiri urettigini dogrular. Faturalama itirazlarinda dayanak budur.
func TestIntegrationEventLedgerIsAppendOnly(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()

	const n = 7
	for i := 0; i < n; i++ {
		if err := st.Record(ctx, Usage{
			ClientID:    "acme",
			CarrierType: "lora_ism",
			BytesIn:     1,
			BytesOut:    1,
			PayloadSHA:  fmt.Sprintf("sha-%d", i),
			Signature:   fmt.Sprintf("sig-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	var events int
	if err := st.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM aetheris_ledger_events WHERE client_id = $1", "acme").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != n {
		t.Fatalf("olay sayisi = %d, beklenen %d", events, n)
	}

	// Yuk icerigi ASLA saklanmamali - yalnizca ozet.
	var cols int
	if err := st.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'aetheris_ledger_events'
		  AND column_name IN ('payload', 'ciphertext', 'body', 'content')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatal("SIFIR BILGI IHLALI: olay tablosunda yuk sutunu var")
	}
}

// TestIntegrationConcurrentRecords, es zamanli yazmalarda tek bir
// baytin bile kaybolmadigini gercek veritabaninda dogrular.
// UPSERT yarislari burada ortaya cikar.
func TestIntegrationConcurrentRecords(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()

	const goroutines = 20
	const perGoroutine = 25

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := st.Record(ctx, Usage{
					ClientID:    "acme",
					CarrierType: "standard_internet",
					BytesIn:     3,
					BytesOut:    2,
					PayloadSHA:  fmt.Sprintf("sha-%d-%d", g, i),
					Signature:   "sig",
				}); err != nil {
					errCh <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("es zamanli yazma hatasi: %v", err)
	}

	e, err := st.ClientUsage(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	const total = goroutines * perGoroutine
	if e.Requests != total {
		t.Fatalf("Requests = %d, beklenen %d - KAYIP VAR", e.Requests, total)
	}
	if e.BytesIn != total*3 {
		t.Fatalf("BytesIn = %d, beklenen %d - KAYIP VAR", e.BytesIn, total*3)
	}
	if e.BytesOut != total*2 {
		t.Fatalf("BytesOut = %d, beklenen %d - KAYIP VAR", e.BytesOut, total*2)
	}
}

// TestIntegrationSnapshotAcrossClients, cok istemcili anlik goruntunun
// dogru toplandigini dogrular.
func TestIntegrationSnapshotAcrossClients(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()

	_ = st.Record(ctx, Usage{ClientID: "acme", CarrierType: "mesh_wifi", BytesIn: 100, BytesOut: 10})
	_ = st.Record(ctx, Usage{ClientID: "globex", CarrierType: "lora_ism", BytesIn: 400, BytesOut: 40})

	snap, err := st.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.TotalBytesIn != 500 || snap.TotalBytesOut != 50 || snap.TotalRequests != 2 {
		t.Fatalf("toplamlar hatali: %+v", snap)
	}
	if len(snap.Clients) != 2 {
		t.Fatalf("istemci sayisi = %d", len(snap.Clients))
	}
	if snap.Clients["globex"].ByCarrier["lora_ism"] != 440 {
		t.Fatalf("globex tasiyici kirilimi hatali: %v", snap.Clients["globex"].ByCarrier)
	}
}

// TestIntegrationNotFound, kaydi olmayan istemci icin ErrNotFound
// dondugunu gercek veritabaninda dogrular.
func TestIntegrationNotFound(t *testing.T) {
	st := integrationStore(t)
	if _, err := st.ClientUsage(context.Background(), "hic-olmayan"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ErrNotFound bekleniyordu, alinan: %v", err)
	}
}
