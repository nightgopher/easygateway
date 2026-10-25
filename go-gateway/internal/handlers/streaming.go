package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

// deferredStreamWriter delays the HTTP 200/SSE headers until the first byte
// is actually available. This leaves the response uncommitted when an
// upstream provider fails before producing output, so the handler can safely
// try a fallback provider or return the original HTTP status.
type deferredStreamWriter struct {
	dst       io.Writer
	commit    func()
	committed bool
}

func newDeferredStreamWriter(c *gin.Context) *deferredStreamWriter {
	return &deferredStreamWriter{
		dst: c.Writer,
		commit: func() {
			if c.Writer.Written() {
				return
			}
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("X-Accel-Buffering", "no")
			c.Writer.WriteHeader(http.StatusOK)
		},
	}
}

func (w *deferredStreamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.Commit()
	return w.dst.Write(p)
}

func (w *deferredStreamWriter) Commit() {
	if w.committed {
		return
	}
	w.commit()
	w.committed = true
}

func (w *deferredStreamWriter) Started() bool {
	return w.committed
}

func (w *deferredStreamWriter) Flush() {
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

// usageTrackingWriter observes SSE data while forwarding it unchanged. It is
// used on direct/pass-through streams where no protocol converter gets a
// chance to extract usage metadata for the archive.
type usageTrackingWriter struct {
	dst       io.Writer
	ctx       *gin.Context
	pending   string
	event     string
	dataLines []string
}

func newUsageTrackingWriter(dst io.Writer, ctx *gin.Context) *usageTrackingWriter {
	return &usageTrackingWriter{dst: dst, ctx: ctx}
}

func (w *usageTrackingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.consume(p[:n])
	}
	return n, err
}

func (w *usageTrackingWriter) Flush() {
	if flusher, ok := w.dst.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func (w *usageTrackingWriter) Finish() {
	if w.pending != "" {
		w.consume([]byte("\n"))
	}
	w.flushEvent()
}

func (w *usageTrackingWriter) consume(p []byte) {
	w.pending += string(p)
	for {
		idx := strings.IndexByte(w.pending, '\n')
		if idx < 0 {
			return
		}
		line := strings.TrimSuffix(w.pending[:idx], "\r")
		w.pending = w.pending[idx+1:]
		switch {
		case line == "":
			w.flushEvent()
		case strings.HasPrefix(line, "event:"):
			w.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			w.dataLines = append(w.dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}

func (w *usageTrackingWriter) flushEvent() {
	if len(w.dataLines) == 0 {
		w.event = ""
		return
	}
	payload := strings.Join(w.dataLines, "\n")
	w.event = ""
	w.dataLines = nil
	if payload == "" || payload == "[DONE]" {
		return
	}
	var value any
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return
	}
	usage, ok := findUsageDetails(value)
	if !ok || w.ctx == nil {
		return
	}
	currentInput := w.ctx.GetInt(requestmeta.InputTokensKey)
	currentOutput := w.ctx.GetInt(requestmeta.OutputTokensKey)
	if usage.InputFound {
		currentInput = usage.InputTokens
	}
	if usage.OutputFound {
		currentOutput = usage.OutputTokens
	}
	if usage.InputFound || usage.OutputFound {
		setUsageMetadata(w.ctx, currentInput, currentOutput)
	}
	if usage.CacheUsageKnown {
		cacheInput := usage.CacheInputTokens
		if !usage.CacheInputFound {
			cacheInput = w.ctx.GetInt(requestmeta.CacheInputTokensKey)
			if usage.CacheDenominatorAddsCache {
				cacheInput = currentInput + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
			} else if cacheInput <= 0 {
				cacheInput = currentInput
			}
		}
		setCacheUsageMetadata(w.ctx, usage.CacheReadInputTokens, usage.CacheCreationInputTokens, cacheInput, true)
	}
}

// findUsage accepts Anthropic message usage, OpenAI chat usage, and native
// Responses usage. Walking nested objects also covers response.completed
// payloads without coupling the tracker to one protocol's response shape.
func findUsage(value any) (int, int, bool) {
	usage, found := findUsageDetails(value)
	return usage.InputTokens, usage.OutputTokens, found
}

type parsedUsage struct {
	InputTokens               int
	OutputTokens              int
	InputFound                bool
	OutputFound               bool
	CacheReadInputTokens      int
	CacheCreationInputTokens  int
	CacheInputTokens          int
	CacheInputFound           bool
	CacheDenominatorAddsCache bool
	CacheUsageKnown           bool
}

func findUsageDetails(value any) (parsedUsage, bool) {
	switch current := value.(type) {
	case map[string]any:
		if rawUsage, ok := current["usage"]; ok {
			if usage, ok := rawUsage.(map[string]any); ok {
				parsed, found := parseUsage(usage)
				if found {
					return parsed, true
				}
			}
		}
		if parsed, found := parseUsage(current); found {
			return parsed, true
		}
		if rawUsage, ok := current["usageMetadata"].(map[string]any); ok {
			if parsed, found := parseUsage(rawUsage); found {
				return parsed, true
			}
		}
		for _, child := range current {
			if usage, ok := findUsageDetails(child); ok {
				return usage, true
			}
		}
	case []any:
		for _, child := range current {
			if usage, ok := findUsageDetails(child); ok {
				return usage, true
			}
		}
	}
	return parsedUsage{}, false
}

func usageCounts(usage map[string]any) (int, int, bool) {
	parsed, found := parseUsage(usage)
	return parsed.InputTokens, parsed.OutputTokens, found
}

func parseUsage(usage map[string]any) (parsedUsage, bool) {
	input, inputFound := usageNumber(usage, "input_tokens", "prompt_tokens", "inputTokens", "promptTokens", "promptTokenCount", "prompt_token_count")
	output, outputFound := usageNumber(usage, "output_tokens", "completion_tokens", "outputTokens", "completionTokens", "candidatesTokenCount", "candidates_token_count")
	cacheRead, cacheReadFound := usageNumber(usage, "cache_read_input_tokens", "cacheReadInputTokens", "cachedContentTokenCount", "cached_content_token_count")
	cacheCreation, cacheCreationFound := usageNumber(usage, "cache_creation_input_tokens", "cacheCreationInputTokens")

	openAICacheDetails := false
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details", "promptTokensDetails", "inputTokensDetails"} {
		if details, ok := usage[key].(map[string]any); ok {
			if cached, found := usageNumber(details, "cached_tokens", "cachedTokens"); found {
				cacheRead = cached
				cacheReadFound = true
				openAICacheDetails = true
			}
		}
	}
	known := cacheReadFound || cacheCreationFound
	denominatorAddsCache := !openAICacheDetails && usageHasKey(usage,
		"cache_read_input_tokens", "cache_creation_input_tokens", "cacheReadInputTokens", "cacheCreationInputTokens",
	)
	cacheInput := 0
	if known && inputFound {
		cacheInput = input
		if denominatorAddsCache {
			// Anthropic reports uncached, cache-read, and cache-write input
			// counts separately. OpenAI and Gemini input counts already include
			// cached tokens, so their denominators are just the input count.
			cacheInput += cacheRead + cacheCreation
		}
	}

	parsed := parsedUsage{
		InputTokens:               input,
		OutputTokens:              output,
		InputFound:                inputFound,
		OutputFound:               outputFound,
		CacheReadInputTokens:      cacheRead,
		CacheCreationInputTokens:  cacheCreation,
		CacheInputTokens:          cacheInput,
		CacheInputFound:           known && inputFound,
		CacheDenominatorAddsCache: denominatorAddsCache,
		CacheUsageKnown:           known,
	}
	return parsed, inputFound || outputFound || known
}

func usageHasKey(usage map[string]any, keys ...string) bool {
	for _, key := range keys {
		if _, ok := usage[key]; ok {
			return true
		}
	}
	return false
}

func usageNumber(usage map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		value, ok := usage[key]
		if !ok {
			continue
		}
		switch number := value.(type) {
		case float64:
			return int(number), true
		case int:
			return number, true
		}
	}
	return 0, false
}

func writeSSEEvent(w io.Writer, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return err
}

func streamErrorPayload(err error) map[string]any {
	payload := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "server_error",
			"message": err.Error(),
		},
	}
	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) {
		errorPayload := payload["error"].(map[string]any)
		if providerErr.StatusCode > 0 {
			errorPayload["code"] = providerErr.StatusCode
		}
		if providerErr.RequestID != "" {
			errorPayload["request_id"] = providerErr.RequestID
		}
	}
	return payload
}

func writeAnthropicStreamError(w io.Writer, err error) error {
	return writeSSEEvent(w, "error", streamErrorPayload(err))
}

func writeResponsesStreamError(w io.Writer, err error) error {
	return writeSSEEvent(w, "error", streamErrorPayload(err))
}

func writeOpenAIStreamError(w io.Writer, err error) error {
	payload := map[string]any{
		"error": map[string]any{
			"type":    "server_error",
			"message": err.Error(),
		},
	}
	if err := writeSSEEvent(w, "", payload); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if flusher, ok := w.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return nil
}
