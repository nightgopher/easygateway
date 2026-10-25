package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/archive"
	"github.com/nightgopher/easygateway/internal/provider"
)

// capturingArchiveStore records every SaveArchive call for handler-level
// integration assertions. Thread-safe so it can be used with the async archiver.
type capturingArchiveStore struct {
	mu    sync.Mutex
	saved []archive.Archive
}

func (s *capturingArchiveStore) SaveArchive(a archive.Archive) error {
	s.mu.Lock()
	s.saved = append(s.saved, a)
	s.mu.Unlock()
	return nil
}
func (s *capturingArchiveStore) QueryArchives(int, int) ([]archive.Archive, int, error) {
	return nil, 0, nil
}
func (s *capturingArchiveStore) ExportArchives(int64, time.Time, int) ([]archive.Archive, error) {
	return nil, nil
}
func (s *capturingArchiveStore) DeleteArchivesBefore(time.Time) (int64, error) { return 0, nil }
func (s *capturingArchiveStore) DeleteArchive(int64) error                     { return nil }
func (s *capturingArchiveStore) Close() error                                  { return nil }

func (s *capturingArchiveStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

func (s *capturingArchiveStore) first() archive.Archive {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.saved) == 0 {
		return archive.Archive{}
	}
	return s.saved[0]
}

// ─── Messages ──────────────────────────────────────────────────────────────

func TestMessagesHandlerArchivesNonStreamWhenEnabled(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "coding",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "hello"}},
		},
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewMessageHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/messages", h.Handle)

	body := `{"model":"coding","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	// The archive worker is async; drain it.
	archiver.Close()

	if got := store.count(); got != 1 {
		t.Fatalf("expected 1 archived conversation, got %d", got)
	}
	ar := store.first()
	if ar.Protocol != archive.ProtocolMessages {
		t.Errorf("protocol = %s, want messages", ar.Protocol)
	}
	if ar.Status != archive.StatusCompleted {
		t.Errorf("status = %s, want completed", ar.Status)
	}
	if !strings.Contains(ar.RequestBody, `"model":"coding"`) {
		t.Errorf("request body not captured: %s", ar.RequestBody)
	}
	if !strings.Contains(ar.ResponseBody, "hello") {
		t.Errorf("response body not captured: %s", ar.ResponseBody)
	}
}

func TestMessagesHandlerArchivesStreamWhenEnabled(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name:       "stub",
		streamData: "event: message_start\ndata: {}\n\nevent: message_stop\ndata: {}\n\n",
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewMessageHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/messages", h.Handle)

	body := `{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	archiver.Close()

	if got := store.count(); got != 1 {
		t.Fatalf("expected 1 archived stream, got %d", got)
	}
	ar := store.first()
	if !ar.IsStream {
		t.Errorf("expected is_stream=true")
	}
	// message_stop marker should be detected → completed
	if ar.Status != archive.StatusCompleted {
		t.Errorf("status = %s, want completed (message_stop detected)", ar.Status)
	}
	// The tee'd response body should contain the streamed events
	if !strings.Contains(ar.ResponseBody, "message_stop") {
		t.Errorf("stream transcript not captured: %s", ar.ResponseBody)
	}
}

func TestMessagesHandlerDoesNotArchiveWhenDisabled(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: false}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "coding",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "hello"}},
		},
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewMessageHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/messages", h.Handle)

	body := `{"model":"coding","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if got := store.count(); got != 0 {
		t.Fatalf("expected 0 archives when disabled, got %d", got)
	}
}

func TestMessagesHandlerArchivesFailedRequestWithErrorStatus(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	failing := &failingProvider{name: "stub"}
	router.RegisterProvider("stub", failing)
	router.RegisterChain("coding", []string{"stub"})

	h := NewMessageHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/messages", h.Handle)

	body := `{"model":"coding","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	archiver.Close()

	if got := store.count(); got != 1 {
		t.Fatalf("expected 1 error archive, got %d", got)
	}
	ar := store.first()
	if ar.Status != archive.StatusError {
		t.Errorf("status = %s, want error", ar.Status)
	}
	if ar.ErrorReason == "" {
		t.Errorf("error reason should be populated")
	}
}

// ─── Chat Completions ──────────────────────────────────────────────────────

func TestChatCompletionsHandlerArchivesNonStreamWhenEnabled(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID: "chat_1", Type: "message", Role: "assistant", Model: "coding",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "hello"}},
		},
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewChatCompletionsHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/chat/completions", h.Handle)

	body := `{"model":"coding","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	archiver.Close()

	if got := store.count(); got != 1 {
		t.Fatalf("expected 1 archive, got %d", got)
	}
	ar := store.first()
	if ar.Protocol != archive.ProtocolChatCompletions {
		t.Errorf("protocol = %s, want chat_completions", ar.Protocol)
	}
	if ar.Status != archive.StatusCompleted {
		t.Errorf("status = %s, want completed", ar.Status)
	}
}

func TestChatCompletionsHandlerArchivesStreamWhenEnabled(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID: "chat_1", Type: "message", Role: "assistant", Model: "coding",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "hello"}},
		},
		streamData: "data: ok\n\n",
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewChatCompletionsHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/chat/completions", h.Handle)

	body := `{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	archiver.Close()

	if got := store.count(); got != 1 {
		t.Fatalf("expected 1 archived stream, got %d", got)
	}
	ar := store.first()
	if !ar.IsStream {
		t.Errorf("expected is_stream=true")
	}
	// The exact SSE output depends on the conversion path; terminal-state
	// detection is independently verified by parseStreamEndState unit tests.
	if ar.Protocol != archive.ProtocolChatCompletions {
		t.Errorf("protocol = %s, want chat_completions", ar.Protocol)
	}
}

// ─── Responses ─────────────────────────────────────────────────────────────

func TestResponsesHandlerArchivesNonStreamWhenEnabled(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID: "resp_1", Type: "message", Role: "assistant", Model: "coding",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "hello"}},
		},
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewResponsesHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/responses", h.Handle)

	body := `{"model":"coding","input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	archiver.Close()

	if got := store.count(); got != 1 {
		t.Fatalf("expected 1 archive, got %d", got)
	}
	ar := store.first()
	if ar.Protocol != archive.ProtocolResponses {
		t.Errorf("protocol = %s, want responses", ar.Protocol)
	}
	if ar.Status != archive.StatusCompleted {
		t.Errorf("status = %s, want completed", ar.Status)
	}
}

func TestResponsesHandlerArchivesStreamWhenEnabled(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID: "resp_1", Type: "message", Role: "assistant", Model: "coding",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "hi"}},
		},
		streamData: "data: ok\n\n",
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewResponsesHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/responses", h.Handle)

	body := `{"model":"coding","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	archiver.Close()

	if got := store.count(); got != 1 {
		t.Fatalf("expected 1 archived stream, got %d", got)
	}
	ar := store.first()
	if ar.Protocol != archive.ProtocolResponses {
		t.Errorf("protocol = %s, want responses", ar.Protocol)
	}
	// Verify the stream was archived — the conversion path output varies by
	// upstream format; we assert the structural metadata here. The terminal
	// state detection logic is covered by parseStreamEndState unit tests.
	if !ar.IsStream {
		t.Errorf("expected is_stream=true")
	}
}

func TestStreamingHandlersArchiveUnknownModelErrors(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		body     string
		protocol archive.Protocol
		register func(*gin.Engine, *provider.Router, *archive.Archiver)
	}{
		{
			name: "messages", path: "/v1/messages",
			body:     `{"model":"missing","stream":true,"messages":[{"role":"user","content":"hi"}],"max_tokens":10}`,
			protocol: archive.ProtocolMessages,
			register: func(engine *gin.Engine, router *provider.Router, archiver *archive.Archiver) {
				handler := NewMessageHandler(router, log.New(io.Discard, "", 0))
				handler.SetArchiver(archiver)
				engine.POST("/v1/messages", handler.Handle)
			},
		},
		{
			name: "chat_completions", path: "/v1/chat/completions",
			body:     `{"model":"missing","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			protocol: archive.ProtocolChatCompletions,
			register: func(engine *gin.Engine, router *provider.Router, archiver *archive.Archiver) {
				handler := NewChatCompletionsHandler(router, log.New(io.Discard, "", 0))
				handler.SetArchiver(archiver)
				engine.POST("/v1/chat/completions", handler.Handle)
			},
		},
		{
			name: "responses", path: "/v1/responses",
			body:     `{"model":"missing","stream":true,"input":"hi"}`,
			protocol: archive.ProtocolResponses,
			register: func(engine *gin.Engine, router *provider.Router, archiver *archive.Archiver) {
				handler := NewResponsesHandler(router, log.New(io.Discard, "", 0))
				handler.SetArchiver(archiver)
				engine.POST("/v1/responses", handler.Handle)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &capturingArchiveStore{}
			archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)
			router := provider.NewRouter(log.New(io.Discard, "", 0))
			engine := gin.New()
			tt.register(engine, router, archiver)

			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewBufferString(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			archiver.Close()

			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
			}
			if got := store.count(); got != 1 {
				t.Fatalf("expected 1 archived routing error, got %d", got)
			}
			ar := store.first()
			if ar.Protocol != tt.protocol || ar.Status != archive.StatusError || !ar.IsStream {
				t.Fatalf("archive = protocol %s status %s stream %v", ar.Protocol, ar.Status, ar.IsStream)
			}
		})
	}
}

// ─── Redaction in handler path ────────────────────────────────────────────

func TestMessagesHandlerArchivedRequestBodyIsRedacted(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID: "msg_1", Type: "message", Role: "assistant", Model: "coding",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "ok"}},
		},
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewMessageHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)

	engine := gin.New()
	engine.POST("/v1/messages", h.Handle)

	// Request carries a sensitive field that must NEVER be persisted
	body := `{"model":"coding","messages":[{"role":"user","content":"hi"}],"authorization":"Bearer secret-sk-123","max_tokens":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	archiver.Close()

	if store.count() != 1 {
		t.Fatalf("expected 1 archive, got %d", store.count())
	}
	ar := store.first()
	if strings.Contains(ar.RequestBody, "secret-sk-123") {
		t.Errorf("SENSITIVE DATA LEAKED into archive: %s", ar.RequestBody)
	}
	if !strings.Contains(ar.RequestBody, "[REDACTED]") {
		t.Errorf("authorization field not redacted: %s", ar.RequestBody)
	}
}

func TestMessagesHandlerArchivesStreamUsageMetadata(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		streamData: strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`,
			"",
			"event: message_delta",
			`data: {"type":"message_delta","usage":{"output_tokens":11}}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n"),
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewMessageHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)
	engine := gin.New()
	engine.POST("/v1/messages", h.Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	archiver.Close()

	ar := store.first()
	if ar.InputTokens != 7 || ar.OutputTokens != 11 {
		t.Fatalf("archived usage = input:%d output:%d, want input:7 output:11", ar.InputTokens, ar.OutputTokens)
	}
}

func TestChatCompletionsHandlerArchivesPreBodyStreamErrorAsError(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name:      "stub",
		streamErr: &provider.ProviderError{Provider: "stub", StatusCode: http.StatusUnauthorized, Message: "unauthorized"},
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewChatCompletionsHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)
	engine := gin.New()
	engine.POST("/v1/chat/completions", h.Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	archiver.Close()

	if got := store.first().Status; got != archive.StatusError {
		t.Fatalf("archive status = %s, want error (HTTP %d)", got, w.Code)
	}
}

func TestResponsesHandlerArchivesPreBodyStreamErrorAsError(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("stub", &stubChatProvider{
		name:      "stub",
		streamErr: &provider.ProviderError{Provider: "stub", StatusCode: http.StatusUnauthorized, Message: "unauthorized"},
	})
	router.RegisterChain("coding", []string{"stub"})

	h := NewResponsesHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)
	engine := gin.New()
	engine.POST("/v1/responses", h.Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"coding","stream":true,"input":"hi"}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	archiver.Close()

	if got := store.first().Status; got != archive.StatusError {
		t.Fatalf("archive status = %s, want error (HTTP %d)", got, w.Code)
	}
}

func TestChatCompletionsHandlerArchivesClientModelAlias(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("glm", &stubChatProvider{
		name: "glm",
		streamData: strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start","message":{"id":"msg_alias","model":"glm-5-turbo"}}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n"),
	})
	router.RegisterChain("glm-sonnet", []string{"glm"})

	h := NewChatCompletionsHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)
	engine := gin.New()
	engine.POST("/v1/chat/completions", h.Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"glm-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	archiver.Close()

	ar := store.first()
	if !strings.Contains(ar.RequestBody, `"model":"glm-sonnet"`) {
		t.Fatalf("archive request lost client model alias: %s", ar.RequestBody)
	}
	if strings.Contains(ar.RequestBody, `"model":"glm-5-turbo"`) {
		t.Fatalf("archive request contains provider model instead of client alias: %s", ar.RequestBody)
	}
}

func TestResponsesHandlerArchivesChatGPTPreBodyErrorAsError(t *testing.T) {
	store := &capturingArchiveStore{}
	archiver := archive.NewArchiver(store, archive.Config{Enabled: true, MaxBodyKB: 256}, nil)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("chatgpt", &stubChatGPTPassthroughProvider{
		name: "chatgpt",
		err:  &provider.ProviderError{Provider: "chatgpt", StatusCode: http.StatusUnauthorized, Message: "unauthorized"},
	})
	router.RegisterChain("gpt-5", []string{"chatgpt"})

	h := NewResponsesHandler(router, log.New(io.Discard, "", 0))
	h.SetArchiver(archiver)
	engine := gin.New()
	engine.POST("/v1/responses", h.Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5","stream":true,"input":"hi"}`))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	archiver.Close()

	if got := store.first().Status; got != archive.StatusError {
		t.Fatalf("archive status = %s, want error (HTTP %d)", got, w.Code)
	}
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// failingProvider always returns an error from ForwardRequest, for testing
// the error-archival path.
type failingProvider struct {
	name string
}

func (p *failingProvider) Name() string                     { return p.name }
func (p *failingProvider) URL() string                      { return "http://fail" }
func (p *failingProvider) APIKey() string                   { return "" }
func (p *failingProvider) UseBearer() bool                  { return true }
func (p *failingProvider) IsHealthy(_ context.Context) bool { return true }
func (p *failingProvider) ForwardRequest(_ context.Context, _ *provider.Request) (*provider.Response, error) {
	return nil, errors.New("upstream permanently down")
}
func (p *failingProvider) ForwardStream(_ context.Context, _ *provider.Request, _ io.Writer) error {
	return errors.New("upstream stream error")
}

// stubChatGPTPassthroughProvider exercises the native Responses passthrough
// path without making any network calls.
type stubChatGPTPassthroughProvider struct {
	name string
	err  error
}

func (p *stubChatGPTPassthroughProvider) Name() string                   { return p.name }
func (p *stubChatGPTPassthroughProvider) URL() string                    { return "http://chatgpt" }
func (p *stubChatGPTPassthroughProvider) APIKey() string                 { return "" }
func (p *stubChatGPTPassthroughProvider) UseBearer() bool                { return true }
func (p *stubChatGPTPassthroughProvider) IsHealthy(context.Context) bool { return true }
func (p *stubChatGPTPassthroughProvider) ForwardRequest(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, errors.New("not used")
}
func (p *stubChatGPTPassthroughProvider) ForwardRawResponsesStream(context.Context, json.RawMessage, io.Writer) error {
	return p.err
}
