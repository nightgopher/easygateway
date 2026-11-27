package privacy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/gwkeys"
)

// Gate enables privacy for supported data-plane calls. Disabled by default.
type Gate struct {
	engine  *Engine
	enabled atomic.Bool
	limit   int64
}

func NewGate(limit int64) (*Gate, error) {
	e, err := NewEngine()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 64 << 20
	}
	return &Gate{engine: e, limit: limit}, nil
}
func (g *Gate) Enabled() bool     { return g.enabled.Load() }
func (g *Gate) SetEnabled(v bool) { g.enabled.Store(v) }
func isProtectedPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/chat/completions", "/v1/messages", "/messages", "/v1/responses", "/responses", "/v1/systemone", "/systemone", "/v1/embeddings", "/embeddings", "/v1/audio/transcriptions", "/audio/transcriptions":
		return true
	}
	return false
}

// Middleware is applied only after authentication. It fails closed on an
// unsupported request or unparseable response while privacy is enabled.
func (g *Gate) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !g.Enabled() || c.Request.Method != http.MethodPost || !isProtectedPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		if encoding := c.GetHeader("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "privacy: encoded request not supported"})
			return
		}
		if !strings.HasPrefix(strings.ToLower(c.GetHeader("Content-Type")), "application/json") {
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "privacy: this content type is not supported"})
			return
		}
		limited := io.LimitReader(c.Request.Body, g.limit+1)
		body, err := io.ReadAll(limited)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "privacy: cannot read request"})
			return
		}
		if int64(len(body)) > g.limit {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "privacy: request too large"})
			return
		}
		scope := c.GetString(gwkeys.ContextKey)
		if scope == "" {
			scope = "master"
		}
		session := g.engine.NewSession(scope)
		body, err = session.MaskJSON(body)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "privacy: request cannot be processed safely"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		old := c.Writer
		w := &responseWriter{ResponseWriter: old, session: session}
		c.Writer = w
		c.Next()
		err = w.finish()
		c.Writer = old
		if err != nil && !old.Written() {
			old.Header().Del("Content-Length")
			c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "privacy: response could not be restored"})
		}
	}
}

type responseWriter struct {
	gin.ResponseWriter
	session *Session
	stream  *SSEDecoder
	buf     bytes.Buffer
	failed  error
	mode    int
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if w.mode == 0 {
		if strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream") {
			w.mode = 1
			w.stream = w.session.NewSSEDecoder()
			w.Header().Del("Content-Length")
		} else {
			w.mode = 2
		}
	}
	if w.mode == 2 {
		if w.buf.Len()+len(p) > 64<<20 {
			w.failed = errors.New("privacy: response too large")
			return 0, w.failed
		}
		return w.buf.Write(p)
	}
	data, err := w.stream.Push(p)
	if err == nil && len(data) > 0 {
		_, err = w.ResponseWriter.Write(data)
	}
	if err != nil {
		w.failed = err
		return 0, err
	}
	return len(p), nil
}
func (w *responseWriter) WriteString(v string) (int, error) { return w.Write([]byte(v)) }
func (w *responseWriter) Flush() {
	if w.mode == 1 {
		w.ResponseWriter.Flush()
	}
}
func (w *responseWriter) finish() error {
	if w.failed != nil {
		return w.failed
	}
	if w.mode == 1 {
		tail, err := w.stream.Finish()
		if err != nil {
			return err
		}
		if len(tail) > 0 {
			_, err = w.ResponseWriter.Write(tail)
		}
		return err
	}
	if w.mode == 2 {
		data, err := w.session.RestoreJSON(w.buf.Bytes())
		if err != nil {
			return err
		}
		w.Header().Del("Content-Length")
		_, err = w.ResponseWriter.Write(data)
		return err
	}
	return nil
}
