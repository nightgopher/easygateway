package middleware

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRecoveryReturnsJSONErrorAndKeepsTheProcessAlive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := &lockedBuffer{}
	engine := gin.New()
	engine.Use(RequestID())
	engine.Use(Recovery(log.New(sink, "", 0)))
	engine.GET("/v1/models", func(*gin.Context) { panic("handler exploded") })

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("X-Request-ID", "trace-recovery")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	var body struct {
		Error struct {
			Message   string `json:"message"`
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, response.Body.String())
	}
	if body.Error.Message == "" || body.Error.Type != "server_error" {
		t.Fatalf("unexpected error payload: %#v", body.Error)
	}
	if body.Error.RequestID != "trace-recovery" {
		t.Fatalf("request_id = %q, want the correlation ID", body.Error.RequestID)
	}

	logged := sink.String()
	if !strings.Contains(logged, "panic recovered") || !strings.Contains(logged, "trace-recovery") {
		t.Fatalf("log line does not correlate the panic: %q", logged)
	}
	if !strings.Contains(logged, "handler exploded") {
		t.Fatalf("log line does not carry the panic value: %q", logged)
	}
}

// A stream that already wrote a 200 must not get a second status line: the
// payload would be corrupted. Recovery may only record the failure.
func TestRecoveryDoesNotRewriteAnAlreadyCommittedResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sink := &lockedBuffer{}
	engine := gin.New()
	engine.Use(Recovery(log.New(sink, "", 0)))
	engine.GET("/v1/messages", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.WriteString("data: {\"type\":\"message_start\"}\n\n")
		c.Writer.Flush()
		panic("mid-stream failure")
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/messages", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the original 200 to survive", response.Code)
	}
	if got := response.Body.String(); strings.Contains(got, "internal server error") {
		t.Fatalf("recovery appended a JSON error to the SSE body: %q", got)
	}
	if !strings.Contains(sink.String(), "mid-stream failure") {
		t.Fatalf("panic was not logged: %q", sink.String())
	}
}

func TestRecoveryIsTransparentWithoutAPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(Recovery(log.New(&lockedBuffer{}, "", 0)))
	engine.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok"`) {
		t.Fatalf("unexpected passthrough response: %d %q", response.Code, response.Body.String())
	}
}
