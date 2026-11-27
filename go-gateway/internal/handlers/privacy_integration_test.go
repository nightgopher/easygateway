package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/privacy"
	"github.com/nightgopher/easygateway/internal/provider"
)

type privacyProvider struct {
	t      *testing.T
	secret string
}

func (p *privacyProvider) Name() string                   { return "test" }
func (p *privacyProvider) URL() string                    { return "" }
func (p *privacyProvider) APIKey() string                 { return "" }
func (p *privacyProvider) UseBearer() bool                { return false }
func (p *privacyProvider) IsHealthy(context.Context) bool { return true }
func (p *privacyProvider) ForwardRequest(_ context.Context, r *provider.Request) (*provider.Response, error) {
	text := r.Messages[0].Content.String()
	if strings.Contains(text, p.secret) {
		p.t.Error("secret reached upstream")
	}
	return &provider.Response{ID: "msg", Type: "message", Role: "assistant", Model: "test", Content: []provider.ContentBlock{{Type: "text", Text: text}}, StopReason: "end_turn"}, nil
}
func (p *privacyProvider) ForwardStream(_ context.Context, r *provider.Request, w io.Writer) error {
	token := r.Messages[0].Content.String()
	if strings.Contains(token, p.secret) {
		p.t.Error("secret reached streaming upstream")
	}
	_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"model\":\"test\"}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	for _, part := range []string{token[:12], token[12:]} {
		b, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": part}})
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", b)
	}
	_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return nil
}

func TestPrivacyThroughRealChatAndMessagesHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "sk-" + strings.Repeat("q", 24)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	router.RegisterProvider("test", &privacyProvider{t: t, secret: secret})
	router.RegisterChain("test", []string{"test"})
	gate, _ := privacy.NewGate(0)
	gate.SetEnabled(true)
	e := gin.New()
	e.Use(gate.Middleware())
	e.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	e.POST("/v1/messages", NewMessageHandler(router, logger).Handle)
	e.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			body := `{"model":"test","messages":[{"role":"user","content":"` + secret + `"}],"max_tokens":256}`
			if path == "/v1/responses" {
				body = `{"model":"test","input":"` + secret + `"}`
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			if w.Code != 200 || !strings.Contains(w.Body.String(), secret) {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestPrivacyThroughRealStreamingHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "sk-" + strings.Repeat("q", 24)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	router.RegisterProvider("test", &privacyProvider{t: t, secret: secret})
	router.RegisterChain("test", []string{"test"})
	gate, _ := privacy.NewGate(0)
	gate.SetEnabled(true)
	e := gin.New()
	e.Use(gate.Middleware())
	e.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	e.POST("/v1/messages", NewMessageHandler(router, logger).Handle)
	e.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			body := `{"model":"test","messages":[{"role":"user","content":"` + secret + `"}],"stream":true,"max_tokens":256}`
			if path == "/v1/responses" {
				body = `{"model":"test","input":"` + secret + `","stream":true}`
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			if w.Code != 200 || !strings.Contains(w.Body.String(), secret) {
				t.Fatalf("stream got %d %s", w.Code, w.Body.String())
			}
		})
	}
}

type privacyFailProvider struct {
	t      *testing.T
	secret string
}

func (p *privacyFailProvider) Name() string                   { return "fail" }
func (p *privacyFailProvider) URL() string                    { return "" }
func (p *privacyFailProvider) APIKey() string                 { return "" }
func (p *privacyFailProvider) UseBearer() bool                { return false }
func (p *privacyFailProvider) IsHealthy(context.Context) bool { return true }
func (p *privacyFailProvider) ForwardRequest(_ context.Context, r *provider.Request) (*provider.Response, error) {
	if strings.Contains(r.Messages[0].Content.String(), p.secret) {
		p.t.Error("secret leaked to failing upstream")
	}
	return nil, &provider.ProviderError{Provider: "fail", StatusCode: 503}
}
func TestPrivacyAppliesBeforeFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "sk-" + strings.Repeat("q", 24)
	logger := log.New(io.Discard, "", 0)
	r := provider.NewRouter(logger)
	r.RegisterProvider("fail", &privacyFailProvider{t: t, secret: secret})
	r.RegisterProvider("test", &privacyProvider{t: t, secret: secret})
	r.RegisterChain("test", []string{"fail", "test"})
	g, _ := privacy.NewGate(0)
	g.SetEnabled(true)
	e := gin.New()
	e.Use(g.Middleware())
	e.POST("/v1/chat/completions", NewChatCompletionsHandler(r, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"`+secret+`"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), secret) {
		t.Fatalf("fallback: %d %s", w.Code, w.Body.String())
	}
}
