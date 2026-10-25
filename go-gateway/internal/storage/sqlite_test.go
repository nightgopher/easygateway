package storage

import (
	"database/sql"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/requestmeta"
	_ "modernc.org/sqlite"
)

func TestSQLiteStorePersistsRequestCorrelationMetadata(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "metrics.db"), log.Default())
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	defer store.Close()

	want := metrics.RequestRecord{
		Timestamp: time.Now().UTC(), RequestID: "req-123", Method: "POST", Path: "/v1/messages",
		Model: "coding", Provider: "glm", ProviderAttempts: []requestmeta.ProviderAttempt{{Provider: "glm", Status: "success"}},
		StatusCode: 200, Latency: 12.5, IsStream: true,
	}
	if err := store.SaveRecord(want); err != nil {
		t.Fatalf("SaveRecord() error = %v", err)
	}
	records, err := store.GetRecentLogs(1)
	if err != nil {
		t.Fatalf("GetRecentLogs() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	got := records[0]
	if got.RequestID != want.RequestID || got.Provider != want.Provider || len(got.ProviderAttempts) != 1 || got.ProviderAttempts[0].Status != "success" {
		t.Fatalf("record metadata = %#v", got)
	}
}

func TestSQLiteStoreMigratesLegacyRequestLogsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	_, err = db.Exec(`CREATE TABLE request_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		method TEXT NOT NULL,
		path TEXT NOT NULL,
		model TEXT DEFAULT '',
		provider TEXT DEFAULT '',
		status_code INTEGER DEFAULT 0,
		latency_ms REAL DEFAULT 0,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		is_stream BOOLEAN DEFAULT 0,
		error TEXT DEFAULT ''
	)`)
	if err != nil {
		db.Close()
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	store, err := NewSQLiteStore(path, log.Default())
	if err != nil {
		t.Fatalf("NewSQLiteStore() migration error = %v", err)
	}
	defer store.Close()
	if err := store.SaveRecord(metrics.RequestRecord{Timestamp: time.Now(), RequestID: "after-migration", Method: "GET", Path: "/health", StatusCode: 200}); err != nil {
		t.Fatalf("SaveRecord() after migration error = %v", err)
	}
}

func TestSQLiteStoreCountsInterruptedHTTP200AsError(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "metrics.db"), log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.SaveRecord(metrics.RequestRecord{Timestamp: now, Method: "POST", Path: "/v1/messages", Model: "mock", StatusCode: 200, IsStream: true, Error: "upstream stream interrupted"}); err != nil {
		t.Fatal(err)
	}
	stats, err := store.GetDailyStats(now.Format("2006-01-02"))
	if err != nil || len(stats) != 1 || stats[0].Successes != 0 || stats[0].Errors != 1 {
		t.Fatalf("daily stats=%+v, err=%v", stats, err)
	}
}

func TestSQLiteStoreRollsBackRecordWhenDailyStatsFail(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "metrics.db"), log.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`CREATE TRIGGER fail_daily_stats BEFORE INSERT ON daily_stats BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRecord(metrics.RequestRecord{Timestamp: time.Now(), Model: "mock"}); err == nil {
		t.Fatal("injected daily stats failure was ignored")
	}
	logs, err := store.GetRecentLogs(10)
	if err != nil || len(logs) != 0 {
		t.Fatalf("partial transaction retained request log: logs=%v, err=%v", logs, err)
	}
}
