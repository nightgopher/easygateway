package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/archive"
)

// ArchiveHandler exposes admin endpoints for querying, exporting, and purging
// conversation archives. It is mounted under /admin/archives and protected by
// the same admin auth middleware as the rest of the admin API.
type ArchiveHandler struct {
	store archive.Store
	log   *log.Logger
}

// NewArchiveHandler constructs an ArchiveHandler. store may be a NoopStore
// when archiving is disabled — the endpoints still respond but always return
// empty result sets, so the admin UI doesn't need a feature flag.
func NewArchiveHandler(store archive.Store, logger *log.Logger) *ArchiveHandler {
	if store == nil {
		store = archive.NoopStore{}
	}
	return &ArchiveHandler{store: store, log: logger}
}

// HandleList GET /admin/archives
// Returns a paginated list of archives, newest first.
func (h *ArchiveHandler) HandleList(c *gin.Context) {
	limit := parsePositiveInt(c.Query("limit"), 50, 500)
	offset := parsePositiveInt(c.Query("offset"), 0, 100000)

	records, total, err := h.store.QueryArchives(limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"archives": records,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
	})
}

// HandleExport GET /admin/archives/export?since=<cursor>&limit=N
// Streams archives as JSONL for incremental knowledge-base ingestion. The
// cursor is a composite "timestamp,id" pair (RFC3339Nano + int64). The response
// includes a "X-Archive-Next-Cursor" header so callers can resume.
//
// Format details:
//   - Each line is a compact JSON object matching archive.Archive.
//   - Rows are ordered by (timestamp ASC, id ASC) for deterministic replay.
//   - The cursor is opaque to clients; pass the value from the response header
//     as the ?since= parameter on the next call. An empty cursor starts from
//     the beginning.
func (h *ArchiveHandler) HandleExport(c *gin.Context) {
	limit := parsePositiveInt(c.Query("limit"), 100, 1000)

	sinceID, sinceTime, err := parseCursor(c.Query("since"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cursor: " + err.Error()})
		return
	}

	records, err := h.store.ExportArchives(sinceID, sinceTime, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Build the next cursor from the last record (if any).
	var nextCursor string
	if n := len(records); n > 0 {
		last := records[n-1]
		nextCursor = formatCursor(last.ID, last.Timestamp)
	}

	c.Header("Content-Type", "application/x-ndjson")
	c.Header("X-Archive-Schema-Version", strconv.Itoa(archive.SchemaVersion))
	if nextCursor != "" {
		c.Header("X-Archive-Next-Cursor", nextCursor)
	}
	c.Header("X-Archive-Count", strconv.Itoa(len(records)))

	c.Writer.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(c.Writer)
	for _, ar := range records {
		if err := enc.Encode(ar); err != nil {
			return
		}
	}
}

// HandleDeleteBefore DELETE /admin/archives?before_days=N  (or ?before=<RFC3339>)
// Purges archives older than the given cutoff. Accepts either a relative
// "before_days" integer or an absolute "before" RFC3339 timestamp. Returns the
// number of rows deleted.
func (h *ArchiveHandler) HandleDeleteBefore(c *gin.Context) {
	cutoff, err := parseDeleteCutoff(c.Query("before_days"), c.Query("before"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	n, err := h.store.DeleteArchivesBefore(cutoff)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": n, "cutoff": cutoff.Format(time.RFC3339)})
}

// HandleDeleteOne DELETE /admin/archives/:id
// Removes a single archive by numeric ID.
func (h *ArchiveHandler) HandleDeleteOne(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid archive id"})
		return
	}
	if err := h.store.DeleteArchive(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": id})
}

// parseCursor decodes a "timestamp,id" cursor. An empty string yields (0, zero time),
// meaning "start from the beginning".
func parseCursor(s string) (int64, time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, time.Time{}, nil
	}
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return 0, time.Time{}, fmt.Errorf("cursor must be 'timestamp,id'")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("cursor timestamp: %w", err)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("cursor id: %w", err)
	}
	return id, ts, nil
}

func formatCursor(id int64, ts time.Time) string {
	return ts.UTC().Format(time.RFC3339Nano) + "," + strconv.FormatInt(id, 10)
}

func parseDeleteCutoff(beforeDays, before string) (time.Time, error) {
	if beforeDays != "" {
		days, err := strconv.Atoi(beforeDays)
		if err != nil || days < 0 {
			return time.Time{}, fmt.Errorf("before_days must be a non-negative integer")
		}
		return time.Now().AddDate(0, 0, -days), nil
	}
	if before != "" {
		t, err := time.Parse(time.RFC3339Nano, before)
		if err != nil {
			return time.Time{}, fmt.Errorf("before must be RFC3339 timestamp: %w", err)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("either before_days or before is required")
}

func parsePositiveInt(s string, defaultVal, maxVal int) int {
	if s == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return defaultVal
	}
	if n > maxVal {
		return maxVal
	}
	return n
}
