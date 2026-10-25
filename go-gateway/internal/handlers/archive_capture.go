package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/archive"
	"github.com/nightgopher/easygateway/internal/provider"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

// defaultSinkMaxBytes caps how much of a streamed SSE transcript the archive
// sink retains in memory. The enabled archiver passes ARCHIVE_MAX_BODY_KB so
// the capture limit and persisted-body limit stay aligned.
//
// Strategy: when the cap is reached, the buffer switches to ring mode — it
// keeps the head (first headBytes) and tail (last tailBytes) of the transcript,
// discarding the middle. This ensures the terminal SSE event (which arrives
// at the end) is always captured for endState detection, while preventing
// unbounded memory growth from long streams.
const (
	defaultSinkMaxBytes = 16 * 1024 * 1024 // 16 MB per stream
	sinkHeadBytes       = 32 * 1024        // 32 KB head (request start context)
)

// archiveSink is a thread-safe, bounded buffer used to tee streamed SSE data
// so it can be archived after the stream completes. It implements io.Writer.
//
// The buffer has a hard capacity (maxBytes). Once the cap is reached, the
// buffer switches to ring mode: it retains the first headBytes and last
// tailBytes, discarding the middle. This preserves the terminal SSE event
// (always at the tail) for endState detection while staying memory-bounded.
type archiveSink struct {
	mu        sync.Mutex
	head      []byte // first headLimit bytes of the transcript
	tail      []byte // ring buffer for the tail (overwrites old data)
	headLimit int
	tailLimit int
	capped    bool // true once the cap was hit
}

func newArchiveSink(maxBytes ...int) *archiveSink {
	limit := defaultSinkMaxBytes
	if len(maxBytes) > 0 && maxBytes[0] > 0 {
		limit = maxBytes[0]
	}
	// Always reserve tail capacity when the configured limit permits it. Using
	// the whole limit for the head at limits <= sinkHeadBytes drops terminal SSE
	// events from long streams and falsely classifies them as interrupted.
	headLimit := limit
	if limit > 1 {
		headLimit = limit / 2
		if headLimit > sinkHeadBytes {
			headLimit = sinkHeadBytes
		}
	}
	tailLimit := limit - headLimit
	return &archiveSink{
		headLimit: headLimit,
		tailLimit: tailLimit,
		head:      make([]byte, 0, headLimit),
		tail:      make([]byte, 0, tailLimit),
	}
}

func (s *archiveSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	originalLen := len(p)

	// Phase 1: fill the head buffer.
	if !s.capped && len(s.head) < s.headLimit {
		space := s.headLimit - len(s.head)
		if len(p) <= space {
			s.head = append(s.head, p...)
			return originalLen, nil
		}
		s.head = append(s.head, p[:space]...)
		p = p[space:]
		// Fall through to write remaining bytes to tail.
	}

	// Phase 2: write to the tail ring buffer. The tail has fixed capacity;
	// when full, it overwrites old data (classic ring buffer).
	if len(p) > 0 {
		if s.tailLimit <= 0 {
			s.capped = true
		} else if len(s.tail)+len(p) <= s.tailLimit {
			s.tail = append(s.tail, p...)
		} else {
			// Ring overwrite: keep only the last tailLimit bytes of the
			// combined old tail and new input.
			combined := make([]byte, s.tailLimit)
			if len(p) >= s.tailLimit {
				copy(combined, p[len(p)-s.tailLimit:])
			} else {
				keepOld := s.tailLimit - len(p)
				if keepOld > len(s.tail) {
					keepOld = len(s.tail)
				}
				copy(combined, s.tail[len(s.tail)-keepOld:])
				copy(combined[keepOld:], p)
			}
			s.tail = combined
			s.capped = true
		}
	}
	return originalLen, nil
}

func (s *archiveSink) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	if s.capped {
		// Insert truncation marker between head and tail.
		out = make([]byte, 0, len(s.head)+len(s.tail)+60)
		out = append(out, s.head...)
		out = append(out, []byte("\n...[stream archive middle truncated at capacity]...\n")...)
		out = append(out, s.tail...)
	} else {
		out = make([]byte, 0, len(s.head)+len(s.tail))
		out = append(out, s.head...)
		out = append(out, s.tail...)
	}
	return out
}

// Len returns the number of bytes currently in the buffer (thread-safe).
func (s *archiveSink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.head) + len(s.tail)
}

// teeStreamWriter wraps an io.Writer (typically the deferredStreamWriter that
// writes to the HTTP client) and copies every byte into an archiveSink, so the
// handler can archive the full SSE transcript without delaying the live stream.
//
// It also forwards Flush calls to the underlying writer so that streaming
// semantics (incremental SSE delivery) are preserved when the archive tee is
// active. Without this, http.Flusher type assertions in provider streaming
// code would fail and net/http would buffer events.
type teeStreamWriter struct {
	dst  io.Writer
	sink *archiveSink
}

func newTeeStreamWriter(dst io.Writer, sink *archiveSink) *teeStreamWriter {
	return &teeStreamWriter{dst: dst, sink: sink}
}

func (w *teeStreamWriter) Write(p []byte) (int, error) {
	if w.sink != nil {
		_, _ = w.sink.Write(p)
	}
	return w.dst.Write(p)
}

// Flush forwards the flush to the destination if it implements http.Flusher.
// This is critical: provider streaming code checks for http.Flusher to deliver
// SSE events incrementally. Without this method, the tee wrapper would break
// that contract and cause events to be buffered until the stream ends.
func (w *teeStreamWriter) Flush() {
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

// submitArchive is the shared post-request archival helper used by all three
// protocol handlers. It reads the request/response metadata that middleware
// and handlers already set on the gin.Context, fills in the bodies, and pushes
// the record to the archiver. It is safe to call even when archiving is
// disabled (the archiver will no-op).
func submitArchive(
	c *gin.Context,
	archiver *archive.Archiver,
	protocol archive.Protocol,
	requestBody []byte,
	responseBody []byte,
	status archive.Status,
	statusCode int,
	errorReason string,
) {
	if archiver == nil || !archiver.Enabled() {
		return
	}
	ar := archive.NewArchive()
	ar.RequestID = c.GetString(requestmeta.RequestIDKey)
	ar.Timestamp = nowFunc()
	ar.Protocol = protocol
	ar.Source = sanitizeArchiveHeader(c.GetHeader("X-AI-Source"))
	ar.ConversationID = sanitizeArchiveHeader(c.GetHeader("X-Conversation-ID"))
	ar.SessionID = sanitizeArchiveHeader(c.GetHeader("X-Session-ID"))
	ar.Model = c.GetString(requestmeta.ModelKey)
	ar.Provider = c.GetString(requestmeta.ProviderKey)
	ar.IsStream = c.GetBool(requestmeta.RequestIsStreamKey)
	ar.Status = status
	ar.StatusCode = statusCode
	ar.InputTokens = c.GetInt(requestmeta.InputTokensKey)
	ar.OutputTokens = c.GetInt(requestmeta.OutputTokensKey)
	ar.RequestBody = string(archive.Redact(requestBody))

	// For streaming responses, responseBody is an SSE transcript — a sequence
	// of "data: {json}\n\n" lines, not a single JSON document. archive.Redact
	// would fail to parse it and return the raw bytes, leaving sensitive data
	// or inline multimedia un-sanitized. We redact each SSE data payload
	// individually instead.
	if ar.IsStream {
		ar.ResponseBody = string(redactSSETranscript(responseBody))
	} else {
		ar.ResponseBody = string(archive.Redact(responseBody))
	}

	// Sanitize error reasons: upstream error messages may contain credentials
	// (e.g. "invalid api key: sk-secret"). Apply the same redaction logic.
	ar.ErrorReason = sanitizeErrorReason(errorReason)
	archiver.Submit(ar)
}

// submitRoutingErrorArchive records failures that happen before a streaming
// provider starts. Without this path, request metrics include the rejected
// request while conversation archives silently omit it.
func submitRoutingErrorArchive(
	c *gin.Context,
	archiver *archive.Archiver,
	protocol archive.Protocol,
	requestBody []byte,
	err error,
) {
	if archiver == nil || !archiver.Enabled() || err == nil {
		return
	}
	reason := archiveErrorReason(err)
	submitArchive(
		c,
		archiver,
		protocol,
		requestBody,
		[]byte(fmt.Sprintf(`{"error":%q}`, reason)),
		archive.StatusError,
		routingErrorStatus(err),
		reason,
	)
}

// sanitizeArchiveHeader bounds caller-provided correlation metadata before it
// reaches SQLite/export. It is intentionally conservative: these fields are
// identifiers for joining external histories, never untrusted prose.
func sanitizeArchiveHeader(value string) string {
	value = strings.TrimSpace(string(archive.RedactText([]byte(value))))
	if len(value) > 256 {
		value = value[:256]
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return -1
		}
		return r
	}, value)
}

// redactSSETranscript parses an SSE byte stream event-by-event and redacts the
// complete data payload. SSE permits one JSON payload to span multiple data:
// lines; joining those lines before parsing prevents multiline payloads from
// bypassing the JSON redactor.
func redactSSETranscript(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	blocks := strings.Split(text, "\n\n")
	for i, block := range blocks {
		blocks[i] = redactSSEBlock(block)
	}
	return []byte(strings.Join(blocks, "\n\n"))
}

func redactSSEBlock(block string) string {
	lines := strings.Split(block, "\n")
	dataPayloads := make([]string, 0, 1)
	dataLine := make([]bool, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		dataLine[i] = true
		dataPayloads = append(dataPayloads, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
	}
	if len(dataPayloads) == 0 {
		return block
	}

	payload := strings.Join(dataPayloads, "\n")
	if payload != "" && payload != "[DONE]" {
		payload = string(archive.Redact([]byte(payload)))
	}
	result := make([]string, 0, len(lines)-len(dataPayloads)+1)
	inserted := false
	for i, line := range lines {
		if dataLine[i] {
			if !inserted {
				result = append(result, "data: "+payload)
				inserted = true
			}
			continue
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// sanitizeErrorReason redacts any credential-like content from an error
// message before it is persisted in error_reason. Upstream errors can contain
// echoed secrets (e.g. "invalid key: sk-xxx"), so we must treat the entire
// error string as untrusted.
func sanitizeErrorReason(reason string) string {
	if reason == "" {
		return ""
	}
	redacted := archive.Redact([]byte(reason))
	// RedactText also catches credential-shaped text embedded in otherwise
	// valid JSON error messages, such as {"message":"api_key=..."}.
	return truncateReason(string(archive.RedactText(redacted)))
}

// archiveErrorReason deliberately omits ProviderError.Message. That message
// comes directly from an upstream response and may contain a provider API key
// that has no recognizable prefix. The archive keeps provider and status
// context while preserving the no-secrets-at-rest guarantee.
func archiveErrorReason(err error) string {
	if err == nil {
		return ""
	}
	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) {
		providerName := providerErr.Provider
		if providerName == "" {
			providerName = "upstream"
		}
		if providerErr.StatusCode > 0 {
			return fmt.Sprintf("provider %s returned HTTP %d", providerName, providerErr.StatusCode)
		}
		return fmt.Sprintf("provider %s request failed", providerName)
	}
	return sanitizeErrorReason(err.Error())
}

// parseStreamEndState inspects raw SSE bytes to determine whether the stream
// reached a terminal event. It parses SSE event/data lines to match terminal
// event types precisely, rather than using fragile substring searches.
//
// Recognized terminal events:
//   - OpenAI chat.completions: data: [DONE]
//   - Responses API: event: response.completed / response.failed / response.incomplete
//   - Anthropic Messages: event: message_stop
func parseStreamEndState(raw []byte, streamErr error) (archive.Status, string) {
	if streamErr != nil {
		if len(bytes.TrimSpace(raw)) == 0 {
			return archive.StatusError, archiveErrorReason(streamErr)
		}
		return archive.StatusInterrupted, archiveErrorReason(streamErr)
	}
	text := string(raw)

	// Parse SSE structure: look at "event:" and "data:" lines
	hasDone := false
	hasCompleted := false
	hasMessageStop := false
	hasFailed := false
	hasIncomplete := false

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "data: [DONE]":
			hasDone = true
		case strings.HasPrefix(trimmed, "event: response.completed"):
			hasCompleted = true
		case strings.HasPrefix(trimmed, "event: response.failed"):
			hasFailed = true
		case strings.HasPrefix(trimmed, "event: response.incomplete"):
			hasIncomplete = true
		case strings.HasPrefix(trimmed, "event: message_stop"):
			hasMessageStop = true
		case strings.Contains(trimmed, "\"type\":\"response.completed\""):
			// Some Responses SSE payloads embed the type in data JSON
			hasCompleted = true
		case strings.Contains(trimmed, "\"type\":\"message_stop\""):
			hasMessageStop = true
		}
	}

	if hasDone || hasCompleted || hasMessageStop {
		return archive.StatusCompleted, ""
	}
	if hasFailed {
		return archive.StatusError, "upstream returned response.failed"
	}
	if hasIncomplete {
		return archive.StatusInterrupted, "upstream returned response.incomplete"
	}

	// No terminal marker found → the stream was interrupted before completion.
	return archive.StatusInterrupted, "stream ended without terminal event"
}

func truncateReason(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return s[:512] + "..."
	}
	return s
}

// marshalCompact serializes v to compact JSON, ignoring errors (returns empty
// string on failure). Used when archiving non-stream responses where we want
// the exact JSON the client received.
func marshalCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
