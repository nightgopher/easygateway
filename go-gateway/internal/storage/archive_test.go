package storage

import (
	"database/sql"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/internal/archive"
	"github.com/nightgopher/easygateway/internal/metrics"
)

func TestArchiveSchemaMigratesLegacyRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE conversation_archives (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			request_id TEXT DEFAULT '',
			protocol TEXT DEFAULT '',
			model TEXT DEFAULT '',
			provider TEXT DEFAULT '',
			is_stream BOOLEAN DEFAULT 0,
			status TEXT DEFAULT '',
			status_code INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			request_body TEXT DEFAULT '',
			response_body TEXT DEFAULT '',
			error_reason TEXT DEFAULT '',
			schema_version INTEGER DEFAULT 1
		)
	`)
	if err != nil {
		db.Close()
		t.Fatalf("create legacy table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	store, err := NewSQLiteStore(dbPath, log.Default())
	if err != nil {
		t.Fatalf("NewSQLiteStore migration: %v", err)
	}
	defer store.Close()

	ar := archive.NewArchive()
	ar.Timestamp = time.Now().UTC()
	ar.RequestID = "migrated-1"
	ar.Source = "gateway"
	ar.ConversationID = "conv-1"
	ar.RequestBytes = 12
	ar.ResponseBytes = 34
	ar.Truncated = true
	if err := store.SaveArchive(ar); err != nil {
		t.Fatalf("SaveArchive after migration: %v", err)
	}
	records, _, err := store.QueryArchives(10, 0)
	if err != nil {
		t.Fatalf("QueryArchives after migration: %v", err)
	}
	if len(records) != 1 || records[0].Source != "gateway" || !records[0].Truncated {
		t.Fatalf("migrated archive metadata mismatch: %#v", records)
	}
}

func TestSaveArchiveAndQueryArchives(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "archives.db"), log.Default())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ar := archive.NewArchive()
	ar.RequestID = "req-archive-1"
	ar.Timestamp = time.Now().UTC()
	ar.Protocol = archive.ProtocolMessages
	ar.Source = "h w j-code"
	ar.ConversationID = "conversation-1"
	ar.SessionID = "session-1"
	ar.Model = "coding"
	ar.Provider = "glm"
	ar.IsStream = false
	ar.Status = archive.StatusCompleted
	ar.StatusCode = 200
	ar.InputTokens = 10
	ar.OutputTokens = 20
	ar.RequestBody = `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`
	ar.ResponseBody = `{"content":[{"type":"text","text":"hello"}]}`
	ar.RequestBytes = len(ar.RequestBody)
	ar.ResponseBytes = len(ar.ResponseBody)
	ar.Truncated = true

	if err := store.SaveArchive(ar); err != nil {
		t.Fatalf("SaveArchive: %v", err)
	}

	records, total, err := store.QueryArchives(10, 0)
	if err != nil {
		t.Fatalf("QueryArchives: %v", err)
	}
	if total != 1 || len(records) != 1 {
		t.Fatalf("expected 1 record, got total=%d len=%d", total, len(records))
	}
	got := records[0]
	if got.RequestID != "req-archive-1" || got.Model != "coding" || got.Status != archive.StatusCompleted {
		t.Fatalf("queried record mismatch: %#v", got)
	}
	if got.Source != ar.Source || got.ConversationID != ar.ConversationID || got.SessionID != ar.SessionID {
		t.Fatalf("correlation metadata mismatch: %#v", got)
	}
	if got.RequestBytes != ar.RequestBytes || got.ResponseBytes != ar.ResponseBytes || !got.Truncated {
		t.Fatalf("archive size metadata mismatch: %#v", got)
	}
}

func TestSQLiteStoreConcurrentMetricAndArchiveWrites(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "concurrent.db"), log.Default())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	const writes = 40
	errs := make(chan error, writes*2)
	var wg sync.WaitGroup
	for i := 0; i < writes; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			errs <- store.SaveRecord(metrics.RequestRecord{
				Timestamp: time.Now().UTC(), RequestID: "metric-" + string(rune('A'+i)),
				Method: "POST", Path: "/v1/chat/completions", Model: "qwen3.8-max",
				Provider: "ali", StatusCode: 200,
			})
		}(i)
		go func(i int) {
			defer wg.Done()
			ar := archive.NewArchive()
			ar.Timestamp = time.Now().UTC()
			ar.RequestID = "archive-" + string(rune('A'+i))
			ar.Protocol = archive.ProtocolChatCompletions
			ar.Model = "qwen3.8-max"
			ar.Provider = "ali"
			ar.Status = archive.StatusCompleted
			ar.StatusCode = 200
			ar.RequestBody = `{}`
			ar.ResponseBody = `{}`
			errs <- store.SaveArchive(ar)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent SQLite write: %v", err)
		}
	}

	archives, total, err := store.QueryArchives(writes, 0)
	if err != nil {
		t.Fatalf("QueryArchives: %v", err)
	}
	if total != writes || len(archives) != writes {
		t.Fatalf("archive count = total %d, rows %d; want %d", total, len(archives), writes)
	}
	logs, err := store.GetRecentLogs(writes)
	if err != nil {
		t.Fatalf("GetRecentLogs: %v", err)
	}
	if len(logs) != writes {
		t.Fatalf("metric count = %d, want %d", len(logs), writes)
	}
}

func TestExportArchivesCursorContinuity(t *testing.T) {
	store, _ := NewSQLiteStore(filepath.Join(t.TempDir(), "export.db"), log.Default())
	defer store.Close()

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		ar := archive.NewArchive()
		ar.RequestID = "batch-1-" + string(rune('A'+i))
		ar.Timestamp = base.Add(time.Duration(i) * time.Second)
		ar.Protocol = archive.ProtocolChatCompletions
		ar.Model = "coding"
		ar.Status = archive.StatusCompleted
		ar.StatusCode = 200
		ar.RequestBody = `{}`
		ar.ResponseBody = `{}`
		store.SaveArchive(ar)
	}

	// Page 1: limit 2, cursor at zero
	page1, err := store.ExportArchives(0, time.Time{}, 2)
	if err != nil {
		t.Fatalf("export page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}
	if page1[0].RequestID != "batch-1-A" {
		t.Errorf("page1[0] = %s, want batch-1-A", page1[0].RequestID)
	}

	// Page 2: resume from last of page1
	last := page1[len(page1)-1]
	page2, err := store.ExportArchives(last.ID, last.Timestamp, 2)
	if err != nil {
		t.Fatalf("export page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2))
	}
	// The first item of page2 must NOT be the same as last item of page1
	if page2[0].ID == last.ID {
		t.Errorf("cursor did not advance: page2[0].ID == page1[-1].ID")
	}
	if page2[0].Timestamp.Before(last.Timestamp) {
		t.Errorf("page2[0] timestamp before cursor")
	}

	// Page 3: only one left
	page3, err := store.ExportArchives(page2[1].ID, page2[1].Timestamp, 2)
	if err != nil {
		t.Fatalf("export page3: %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 len = %d, want 1 (cursor continuity)", len(page3))
	}
}

func TestExportArchivesSameTimestampUsesID(t *testing.T) {
	store, _ := NewSQLiteStore(filepath.Join(t.TempDir(), "ts.db"), log.Default())
	defer store.Close()

	// Three records with identical timestamps — ordering must fall back to ID.
	ts := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		store.SaveArchive(archive.Archive{
			RequestID: "same-ts-" + string(rune('A'+i)),
			Timestamp: ts,
			Protocol:  archive.ProtocolResponses,
			Status:    archive.StatusCompleted,
		})
	}

	all, err := store.ExportArchives(0, time.Time{}, 10)
	if err != nil {
		t.Fatalf("export all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}

	// Resume from the second record: must get exactly the third.
	page2, err := store.ExportArchives(all[1].ID, all[1].Timestamp, 10)
	if err != nil {
		t.Fatalf("export page2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != all[2].ID {
		t.Fatalf("same-timestamp cursor did not advance correctly: got %#v", page2)
	}
}

func TestDeleteArchivesBefore(t *testing.T) {
	store, _ := NewSQLiteStore(filepath.Join(t.TempDir(), "purge.db"), log.Default())
	defer store.Close()

	old := time.Now().AddDate(0, 0, -100)
	recent := time.Now()

	store.SaveArchive(archive.Archive{RequestID: "old", Timestamp: old, Protocol: archive.ProtocolMessages})
	store.SaveArchive(archive.Archive{RequestID: "recent", Timestamp: recent, Protocol: archive.ProtocolMessages})

	n, err := store.DeleteArchivesBefore(time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("DeleteArchivesBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}

	_, total, _ := store.QueryArchives(10, 0)
	if total != 1 {
		t.Fatalf("after purge total = %d, want 1", total)
	}
}
