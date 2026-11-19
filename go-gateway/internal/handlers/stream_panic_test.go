package handlers

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/middleware"
	"github.com/nightgopher/easygateway/internal/provider"
)

// panickingStreamProvider fails the way a malformed upstream response would:
// inside the forward call, on the goroutine that pumps the SSE pipe.
type panickingStreamProvider struct {
	name string
}

func (p *panickingStreamProvider) Name() string                   { return p.name }
func (p *panickingStreamProvider) URL() string                    { return "http://example.com" }
func (p *panickingStreamProvider) APIKey() string                 { return "" }
func (p *panickingStreamProvider) UseBearer() bool                { return true }
func (p *panickingStreamProvider) IsHealthy(context.Context) bool { return true }
func (p *panickingStreamProvider) ForwardRequest(context.Context, *provider.Request) (*provider.Response, error) {
	panic("upstream response was malformed")
}
func (p *panickingStreamProvider) ForwardStream(context.Context, *provider.Request, io.Writer) error {
	panic("upstream stream was malformed")
}

type captureLogger struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *captureLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *captureLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Both stream pumps hand the pipe writer to a background goroutine while the
// request goroutine drains the reader. If a panic there produced no value on
// the result channel, the request would block forever — so these tests fail on
// timeout rather than hanging the suite.
func TestStreamPanicDoesNotHangChatCompletions(t *testing.T) {
	runStreamPanicCase(t, "/v1/chat/completions",
		`{"model":"coding","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		func(router *provider.Router, logger *log.Logger) http.Handler {
			return wrapHandler(logger, NewChatCompletionsHandler(router, logger).Handle)
		})
}

func TestStreamPanicDoesNotHangResponses(t *testing.T) {
	runStreamPanicCase(t, "/v1/responses",
		`{"model":"coding","input":"hi","stream":true}`,
		func(router *provider.Router, logger *log.Logger) http.Handler {
			return wrapHandler(logger, NewResponsesHandler(router, logger).Handle)
		})
}

func TestStreamPanicDoesNotHangMessages(t *testing.T) {
	runStreamPanicCase(t, "/v1/messages",
		`{"model":"coding","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		func(router *provider.Router, logger *log.Logger) http.Handler {
			return wrapHandler(logger, NewMessageHandler(router, logger).Handle)
		})
}

// wrapHandler mirrors main.go's composition: gin.New() plus the recovery
// middleware. Without Recovery the messages path (which forwards
// synchronously) would surface the panic to the test process instead of the
// client, and the test would be measuring the wrong thing.
func wrapHandler(logger *log.Logger, handle gin.HandlerFunc) http.Handler {
	engine := gin.New()
	engine.Use(middleware.Recovery(logger))
	engine.POST("/v1/chat/completions", handle)
	engine.POST("/v1/responses", handle)
	engine.POST("/v1/messages", handle)
	return engine
}

func runStreamPanicCase(t *testing.T, path, body string, build func(*provider.Router, *log.Logger) http.Handler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	sink := &captureLogger{}
	logger := log.New(sink, "", 0)

	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterChain("coding", []string{"boom"})
	router.RegisterProvider("boom", &panickingStreamProvider{name: "boom"})

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	served := make(chan struct{})
	go func() {
		defer close(served)
		build(router, logger).ServeHTTP(response, request)
	}()

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never returned: the stream pump panic left the request waiting on its result channel", path)
	}

	// Two containment points are legitimate: the stream pump's own goroutine
	// (safego) and the HTTP handler (Recovery). Whichever applies, the panic
	// must be reported rather than swallowed.
	logged := sink.String()
	if !strings.Contains(logged, "panic contained in") && !strings.Contains(logged, "panic recovered") {
		t.Fatalf("%s: the panic was not reported: %q", path, logged)
	}
}
