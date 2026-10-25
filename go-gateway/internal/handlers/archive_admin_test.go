package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/archive"
)

func TestArchiveExportEmitsJSONLWithCursor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeArchiveStore{}
	// Seed 3 archives
	for i, id := range []string{"a", "b", "c"} {
		store.records = append(store.records, archive.Archive{
			ID:        int64(i + 1),
			RequestID: id,
		})
	}
	handler := NewArchiveHandler(store, testLogger())

	engine := gin.New()
	engine.GET("/admin/archives/export", handler.HandleExport)

	// Page 1 — store returns all records; we check the header + JSONL body shape
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/archives/export?limit=2", nil)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %s, want application/x-ndjson", ct)
	}
	cursor := w.Header().Get("X-Archive-Next-Cursor")
	if cursor == "" {
		t.Fatal("missing X-Archive-Next-Cursor header")
	}
	if v := w.Header().Get("X-Archive-Schema-Version"); v != "2" {
		t.Errorf("schema version = %s, want 2", v)
	}

	// Verify JSONL: each line must be a standalone JSON object
	lines := splitJSONL(w.Body.String())
	if len(lines) < 1 {
		t.Fatalf("page1 lines = %d, want >= 1", len(lines))
	}
	var first archive.Archive
	json.Unmarshal([]byte(lines[0]), &first)
	if first.RequestID != "a" {
		t.Errorf("first record = %s, want a", first.RequestID)
	}

	// Page 2 using cursor
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/admin/archives/export?limit=2&since="+cursor, nil)
	engine.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("page2 status = %d", w2.Code)
	}
	lines2 := splitJSONL(w2.Body.String())
	if len(lines2) < 1 {
		t.Fatalf("page2 lines = %d, want >= 1 (continuity)", len(lines2))
	}
}

func TestArchiveListReturnsPaginatedPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &fakeArchiveStore{}
	store.total = 42
	store.records = []archive.Archive{{ID: 1, RequestID: "only"}}

	handler := NewArchiveHandler(store, testLogger())
	engine := gin.New()
	engine.GET("/admin/archives", handler.HandleList)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/archives?limit=10&offset=5", nil)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Archives []archive.Archive `json:"archives"`
		Total    int               `json:"total"`
		Limit    int               `json:"limit"`
		Offset   int               `json:"offset"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Total != 42 || resp.Limit != 10 || resp.Offset != 5 {
		t.Errorf("pagination payload mismatch: %#v", resp)
	}
}

func TestArchiveDeleteRequiresCutoff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewArchiveHandler(&fakeArchiveStore{}, testLogger())
	engine := gin.New()
	engine.DELETE("/admin/archives", handler.HandleDeleteBefore)

	// No cutoff → 400
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/admin/archives", nil)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing cutoff status = %d, want 400", w.Code)
	}

	// With before_days → 200
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodDelete, "/admin/archives?before_days=30", nil)
	engine.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("before_days=30 status = %d, want 200", w2.Code)
	}
}

func TestArchiveInvalidCursorReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewArchiveHandler(&fakeArchiveStore{}, testLogger())
	engine := gin.New()
	engine.GET("/admin/archives/export", handler.HandleExport)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/archives/export?since=garbage", nil)
	engine.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid cursor status = %d, want 400", w.Code)
	}
}

func splitJSONL(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// fakeArchiveStore is an in-memory archive.Store for handler tests.
type fakeArchiveStore struct {
	records []archive.Archive
	total   int
}

func (s *fakeArchiveStore) SaveArchive(a archive.Archive) error { return nil }
func (s *fakeArchiveStore) QueryArchives(limit, offset int) ([]archive.Archive, int, error) {
	return s.records, s.total, nil
}
func (s *fakeArchiveStore) ExportArchives(sinceID int64, sinceTime time.Time, limit int) ([]archive.Archive, error) {
	return s.records, nil
}
func (s *fakeArchiveStore) DeleteArchivesBefore(cutoff time.Time) (int64, error) { return 1, nil }
func (s *fakeArchiveStore) DeleteArchive(int64) error                            { return nil }
func (s *fakeArchiveStore) Close() error                                         { return nil }
