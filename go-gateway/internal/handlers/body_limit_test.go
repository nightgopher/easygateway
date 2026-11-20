package handlers

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

// newBodyLimitRouter builds a router with one always-succeeding provider so a
// request that passes the body gate reaches a real response.
func newBodyLimitRouter(t *testing.T) *provider.Router {
	t.Helper()
	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterChain("coding", []string{"stub"})
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID:         "msg_body_limit",
			Type:       "message",
			Role:       "assistant",
			Model:      "stub-model",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "ok"}},
		},
	})
	return router
}

// tinyLimit is small enough that a normal payload trips it.
const tinyLimit = 256

func assertTooLarge(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %q)", response.Code, response.Body.String())
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("413 body is not JSON: %v (%q)", err, response.Body.String())
	}
	if payload.Error.Code != "request_too_large" {
		t.Fatalf("error code = %q, want request_too_large", payload.Error.Code)
	}
	if !strings.Contains(payload.Error.Message, "MiB") && !strings.Contains(payload.Error.Message, "bytes") {
		t.Fatalf("message does not state the limit: %q", payload.Error.Message)
	}
}

func oversizedBody() string {
	// A syntactically valid request whose padding pushes it past tinyLimit.
	return `{"model":"coding","max_tokens":128,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", tinyLimit*4) + `"}]}`
}

func TestChatCompletionsRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newBodyLimitRouter(t)
	handler := NewChatCompletionsHandler(router, log.New(io.Discard, "", 0))
	handler.SetMaxRequestBodyBytes(tinyLimit)

	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(oversizedBody()))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assertTooLarge(t, response)
}

func TestMessagesRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newBodyLimitRouter(t)
	handler := NewMessageHandler(router, log.New(io.Discard, "", 0))
	handler.SetMaxRequestBodyBytes(tinyLimit)

	engine := gin.New()
	engine.POST("/v1/messages", handler.Handle)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(oversizedBody()))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assertTooLarge(t, response)
}

func TestResponsesRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newBodyLimitRouter(t)
	handler := NewResponsesHandler(router, log.New(io.Discard, "", 0))
	handler.SetMaxRequestBodyBytes(tinyLimit)

	engine := gin.New()
	engine.POST("/v1/responses", handler.Handle)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(oversizedBody()))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assertTooLarge(t, response)
}

// The limit must not reject a normal payload: the body is rewound for the
// protocol decoder, so a request under the ceiling still parses.
func TestUnderLimitBodyStillReachesTheProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newBodyLimitRouter(t)
	handler := NewChatCompletionsHandler(router, log.New(io.Discard, "", 0))
	handler.SetMaxRequestBodyBytes(tinyLimit)

	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)

	body := `{"model":"coding","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "ok") {
		t.Fatalf("provider response missing from body: %q", response.Body.String())
	}
}

func TestDefaultLimitAppliesWhenUnset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := newBodyLimitRouter(t)
	// No SetMaxRequestBodyBytes call: the handler must fall back to the default
	// rather than reading the body unbounded.
	handler := NewChatCompletionsHandler(router, log.New(io.Discard, "", 0))

	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)

	body := `{"model":"coding","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 under the default ceiling (body %q)", response.Code, response.Body.String())
	}
}

func TestReadRequestBodyReportsUnreadableBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/probe", func(c *gin.Context) {
		if _, ok := readRequestBody(c, tinyLimit); !ok {
			return
		}
		c.Status(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodPost, "/probe", io.NopCloser(errReader{}))
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unreadable body", response.Code)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestByteSizeLabelRendersMiBAndBytes(t *testing.T) {
	if got := byteSizeLabel(64 << 20); got != "64 MiB" {
		t.Fatalf("byteSizeLabel(64MiB) = %q", got)
	}
	if got := byteSizeLabel(1000); got != "1000 bytes" {
		t.Fatalf("byteSizeLabel(1000) = %q", got)
	}
}
