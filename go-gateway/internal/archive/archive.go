package archive

import (
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/nightgopher/easygateway/internal/safego"
)

// Archiver is the entry point handlers call to persist conversations. It owns
// a bounded async worker goroutine so archival never performs SQLite I/O on the
// request path. When Config.Enabled is false, Submit is a no-op and the worker
// goroutine is never started.
type Archiver struct {
	store Store
	cfg   Config
	log   *log.Logger

	queue     chan Archive
	wg        sync.WaitGroup
	closeOnce sync.Once
	submitMu  sync.RWMutex
	closed    bool
}

// NewArchiver constructs an Archiver. If cfg.Enabled is false, the returned
// Archiver is a safe no-op (Submit returns immediately, store is never touched).
// Otherwise a background worker is started that drains the queue and writes to
// store. Submit applies backpressure instead of dropping records: this keeps
// the incremental export cursor complete and makes a graceful shutdown able to
// flush every accepted archive.
func NewArchiver(store Store, cfg Config, logger *log.Logger) *Archiver {
	if store == nil {
		store = NoopStore{}
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if cfg.Enabled && cfg.MaxBodyKB <= 0 {
		cfg.MaxBodyKB = DefaultConfig().MaxBodyKB
	}
	a := &Archiver{
		store: store,
		cfg:   cfg,
		log:   logger,
	}
	if cfg.Enabled {
		a.queue = make(chan Archive, 512)
		a.wg.Add(1)
		go a.worker()
		logger.Printf("archive: enabled (max_body_kb=%d, retention_days=%d)", cfg.MaxBodyKB, cfg.RetentionDays)
	} else {
		logger.Printf("archive: disabled")
	}
	return a
}

// Enabled reports whether archival is active. Handlers can short-circuit
// expensive capture work when this returns false.
func (a *Archiver) Enabled() bool {
	return a.cfg.Enabled
}

// Submit enqueues an archive for asynchronous persistence. It waits only for
// queue capacity, never for the SQLite write itself. The HTTP handlers call it
// after the provider has completed (or the stream has terminated), so bounded
// backpressure preserves records without changing the response body.
func (a *Archiver) Submit(ar Archive) {
	if !a.cfg.Enabled {
		return
	}
	// Apply body size cap before queuing so the queue itself never holds
	// oversized payloads.
	ar.RequestBytes = len(ar.RequestBody)
	ar.ResponseBytes = len(ar.ResponseBody)
	ar.RequestBody, ar.Truncated = truncateBody(ar.RequestBody, a.cfg.MaxBodyKB)
	var responseTruncated bool
	ar.ResponseBody, responseTruncated = truncateBody(ar.ResponseBody, a.cfg.MaxBodyKB)
	ar.Truncated = ar.Truncated || responseTruncated || BodyWasTruncated(ar.RequestBody) || BodyWasTruncated(ar.ResponseBody)
	if ar.SchemaVersion == 0 {
		ar.SchemaVersion = SchemaVersion
	}
	// Synchronize the send with Close. A graceful HTTP shutdown can time out
	// while a long stream is still finishing; late submissions must be ignored
	// safely instead of sending to a closed channel.
	a.submitMu.RLock()
	defer a.submitMu.RUnlock()
	if a.closed {
		return
	}
	a.queue <- ar
}

// Close drains the queue and stops the worker. It blocks until all queued
// archives have been written (or the store returns an error) and is safe to
// call from main's defer chain.
func (a *Archiver) Close() {
	if !a.cfg.Enabled {
		return
	}
	a.closeOnce.Do(func() {
		a.submitMu.Lock()
		a.closed = true
		close(a.queue)
		a.submitMu.Unlock()
		a.wg.Wait()
	})
}

// CleanupRetention purges archives older than cfg.RetentionDays. Intended to be
// called periodically by the existing background cleanup task in main.
func (a *Archiver) CleanupRetention() {
	if !a.cfg.Enabled || a.cfg.RetentionDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -a.cfg.RetentionDays)
	n, err := a.store.DeleteArchivesBefore(cutoff)
	if err != nil {
		a.log.Printf("archive: cleanup error: %v", err)
		return
	}
	if n > 0 {
		a.log.Printf("archive: cleaned up %d archives older than %d days", n, a.cfg.RetentionDays)
	}
}

func (a *Archiver) worker() {
	defer a.wg.Done()
	for ar := range a.queue {
		// Each record is isolated: a panic in one save must not kill the worker
		// goroutine, which would silently stop all later archival, and must not
		// reach the runtime either. safego.Run logs and swallows it.
		safego.Run(a.log, "archive save", func() { a.saveWithRetry(ar) })
	}
}

func (a *Archiver) saveWithRetry(ar Archive) {
	const maxAttempts = 3
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = a.store.SaveArchive(ar)
		if err == nil {
			break
		}
		if attempt < maxAttempts {
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
		}
	}
	if err != nil {
		a.log.Printf("archive: save error after %d attempts (request_id=%s): %v", maxAttempts, ar.RequestID, err)
	}
}

// truncateBody caps a JSON body string to maxKB kilobytes. When truncation is
// needed, the body is wrapped in a valid JSON envelope so downstream JSONL
// consumers can always parse the result without encountering broken JSON.
//
// The envelope preserves a preview of the original content and carries a
// "truncated" flag so consumers can distinguish truncated from complete bodies.
func truncateBody(body string, maxKB int) (string, bool) {
	if maxKB <= 0 {
		return body, false
	}
	maxBytes := maxKB * 1024
	if len(body) <= maxBytes {
		return body, false
	}
	// Instead of cutting mid-JSON, wrap the preview in a valid JSON object so
	// downstream JSONL consumers always parse valid JSON. The preview must be
	// trimmed to account for JSON-escaping overhead (each byte can expand up
	// to 6x for control chars like \u0000), so we iteratively shrink until the
	// marshaled envelope fits within maxBytes.
	preview := body[:maxBytes]
	for {
		b, _ := json.Marshal(map[string]any{
			"truncated": true,
			"preview":   preview,
		})
		if len(b) <= maxBytes || len(preview) == 0 {
			return string(b), true
		}
		// Trim by 10% and retry — converges quickly even for pathological input.
		preview = preview[:len(preview)*9/10]
	}
}

// BodyWasTruncated recognizes both the valid JSON envelope used for bounded
// non-stream bodies and the explicit marker used by the stream capture sink.
// It lets the export consumer distinguish a complete transcript from a
// configured size-bound preview.
func BodyWasTruncated(body string) bool {
	if strings.Contains(body, "stream archive middle truncated") {
		return true
	}
	var envelope struct {
		Truncated bool `json:"truncated"`
	}
	return json.Unmarshal([]byte(body), &envelope) == nil && envelope.Truncated
}

// MaxBodyKB returns the configured per-body size limit in KB. Exposed so
// handlers can size their archive sinks to match.
func (a *Archiver) MaxBodyKB() int {
	return a.cfg.MaxBodyKB
}

// MaxBodyBytes returns the configured capture limit used by streaming sinks.
func (a *Archiver) MaxBodyBytes() int {
	return a.cfg.MaxBodyKB * 1024
}
