package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Bu testler CANLI VERITABANI GEREKTIRMEZ. sqlmock, database/sql
// surucusunun yerine gecerek hangi SQL'in hangi sirayla ve hangi
// transaction sinirlari icinde calistigini dogrular.
//
// Boylece "atomik transaction" iddiasi bir yorum satiri olmaktan cikip
// test edilmis bir davraniga donusur.

func newMockStore(t *testing.T) (*PostgresStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock olusturulamadi: %v", err)
	}
	return NewPostgresWithDB(db), mock, func() { _ = db.Close() }
}

// TestPostgresRecordIsAtomic, Record'un TEK transaction icinde uc tabloyu
// da guncelledigini ve commit ettigini dogrular.
func TestPostgresRecordIsAtomic(t *testing.T) {
	st, mock, closeFn := newMockStore(t)
	defer closeFn()

	now := time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_ledgers")).
		WithArgs("acme", int64(64), int64(304), now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_ledger_carriers")).
		WithArgs("acme", "optical_li_fi", int64(368)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_ledger_events")).
		WithArgs("acme", "optical_li_fi", int64(64), int64(304), "sha", "sig", "edge-1", now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	err := st.Record(context.Background(), Usage{
		ClientID:    "acme",
		CarrierType: "optical_li_fi",
		BytesIn:     64,
		BytesOut:    304,
		PayloadSHA:  "sha",
		Signature:   "sig",
		Destination: "edge-1",
		OccurredAt:  now,
	})
	if err != nil {
		t.Fatalf("Record hata dondu: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("beklenen SQL akisi gerceklesmedi: %v", err)
	}
}

// TestPostgresRecordRollsBackOnFailure, ara adimda hata olustugunda
// transaction'in ROLLBACK edildigini dogrular. Yarim yazilmis defter
// faturalamayi bozar; bu davranis pazarlik konusu degildir.
func TestPostgresRecordRollsBackOnFailure(t *testing.T) {
	st, mock, closeFn := newMockStore(t)
	defer closeFn()

	boom := errors.New("disk dolu")

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_ledgers")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_ledger_carriers")).
		WillReturnError(boom)
	mock.ExpectRollback()

	err := st.Record(context.Background(), Usage{
		ClientID:    "acme",
		CarrierType: "mesh_wifi",
		BytesIn:     10,
		BytesOut:    10,
	})
	if err == nil {
		t.Fatal("hata bekleniyordu, nil dondu")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("altta yatan hata sarilmamis: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("rollback gerceklesmedi: %v", err)
	}
}

// TestPostgresRecordRollsBackOnEventFailure, olay defteri yazilamazsa
// birikimli toplamlarin da geri alindigini dogrular.
func TestPostgresRecordRollsBackOnEventFailure(t *testing.T) {
	st, mock, closeFn := newMockStore(t)
	defer closeFn()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_ledgers")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_ledger_carriers")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_ledger_events")).
		WillReturnError(errors.New("olay yazilamadi"))
	mock.ExpectRollback()

	if err := st.Record(context.Background(), Usage{ClientID: "acme", CarrierType: "direct"}); err == nil {
		t.Fatal("hata bekleniyordu")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("rollback gerceklesmedi: %v", err)
	}
}

// TestPostgresMigrateAppliesOnce, uygulanmis migration'in tekrar
// calistirilmadigini dogrular.
func TestPostgresMigrateAppliesOnce(t *testing.T) {
	st, mock, closeFn := newMockStore(t)
	defer closeFn()

	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS aetheris_schema_migrations")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate hata dondu: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("beklenen akis gerceklesmedi: %v", err)
	}
}

// TestPostgresMigrateAppliesPending, uygulanmamis migration'in
// transaction icinde calistirilip surum tablosuna yazildigini dogrular.
func TestPostgresMigrateAppliesPending(t *testing.T) {
	st, mock, closeFn := newMockStore(t)
	defer closeFn()

	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS aetheris_schema_migrations")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS")).
		WithArgs(1).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS aetheris_ledgers")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO aetheris_schema_migrations")).
		WithArgs(1).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate hata dondu: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("beklenen akis gerceklesmedi: %v", err)
	}
}

// TestPostgresClientUsageNotFound, kaydi olmayan istemci icin
// ErrNotFound dondugunu dogrular.
func TestPostgresClientUsageNotFound(t *testing.T) {
	st, mock, closeFn := newMockStore(t)
	defer closeFn()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT bytes_in, bytes_out, requests")).
		WithArgs("yok").
		WillReturnError(sql.ErrNoRows)

	_, err := st.ClientUsage(context.Background(), "yok")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ErrNotFound bekleniyordu, alinan: %v", err)
	}
}

// TestPostgresClientUsageAggregates, defter ve tasiyici kirilimlarinin
// birlestirilerek dondugunu dogrular.
func TestPostgresClientUsageAggregates(t *testing.T) {
	st, mock, closeFn := newMockStore(t)
	defer closeFn()

	now := time.Now().UTC().Truncate(time.Second)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT bytes_in, bytes_out, requests")).
		WithArgs("acme").
		WillReturnRows(sqlmock.NewRows(
			[]string{"bytes_in", "bytes_out", "requests", "first_seen", "last_seen"}).
			AddRow(int64(500), int64(120), int64(3), now, now))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT carrier_type, bytes")).
		WithArgs("acme").
		WillReturnRows(sqlmock.NewRows([]string{"carrier_type", "bytes"}).
			AddRow("mesh_wifi", int64(400)).
			AddRow("lora_ism", int64(220)))

	e, err := st.ClientUsage(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ClientUsage hata dondu: %v", err)
	}
	if e.BytesIn != 500 || e.BytesOut != 120 || e.Requests != 3 {
		t.Fatalf("beklenmeyen toplamlar: %+v", e)
	}
	if e.ByCarrier["mesh_wifi"] != 400 || e.ByCarrier["lora_ism"] != 220 {
		t.Fatalf("tasiyici kirilimi hatali: %v", e.ByCarrier)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("beklenen akis gerceklesmedi: %v", err)
	}
}

// TestPostgresSnapshotTotals, anlik goruntunun toplamlari dogru
// hesapladigini dogrular.
func TestPostgresSnapshotTotals(t *testing.T) {
	st, mock, closeFn := newMockStore(t)
	defer closeFn()

	now := time.Now().UTC()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT client_id, bytes_in, bytes_out")).
		WillReturnRows(sqlmock.NewRows(
			[]string{"client_id", "bytes_in", "bytes_out", "requests", "first_seen", "last_seen"}).
			AddRow("acme", int64(100), int64(10), int64(1), now, now).
			AddRow("globex", int64(400), int64(40), int64(2), now, now))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT client_id, carrier_type, bytes")).
		WillReturnRows(sqlmock.NewRows([]string{"client_id", "carrier_type", "bytes"}).
			AddRow("acme", "mesh_wifi", int64(110)))

	snap, err := st.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot hata dondu: %v", err)
	}
	if snap.TotalBytesIn != 500 || snap.TotalBytesOut != 50 || snap.TotalRequests != 3 {
		t.Fatalf("toplamlar hatali: %+v", snap)
	}
	if len(snap.Clients) != 2 {
		t.Fatalf("istemci sayisi = %d, beklenen 2", len(snap.Clients))
	}
	if snap.Clients["acme"].ByCarrier["mesh_wifi"] != 110 {
		t.Fatal("tasiyici kirilimi istemciye baglanmadi")
	}
}

// TestPostgresKind, store turunun dogru raporlandigini dogrular.
func TestPostgresKind(t *testing.T) {
	st, _, closeFn := newMockStore(t)
	defer closeFn()
	if st.Kind() != "postgres" {
		t.Fatalf("Kind() = %q", st.Kind())
	}
}

// StoreArayuzuUyumu, her iki implementasyonun da Store arayuzunu
// karsiladigini DERLEME ZAMANINDA garanti eder.
var (
	_ Store = (*MemoryStore)(nil)
	_ Store = (*PostgresStore)(nil)
)
