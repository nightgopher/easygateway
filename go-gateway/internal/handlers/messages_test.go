package handlers

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestMessageHandlerPreservesProviderStatusWhenAllStreamsFail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	first := &stubChatProvider{
		name:      "first",
		streamErr: &provider.ProviderError{Provider: "first", StatusCode: http.StatusTooManyRequests, Message: "quota exceeded"},
	}
	second := &stubChatProvider{
		name:      "second",
		streamErr: errors.New("network unavailable"),
	}
	router.RegisterProvider(first.name, first)
	router.RegisterProvider(second.name, second)
	router.RegisterChain("coding", []string{first.name, second.name})

	engine := gin.New()
	engine.POST("/v1/messages", NewMessageHandler(router, logger).Handle)
	body := `{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for final network failure, got %d: %s", w.Code, w.Body.String())
	}
	if first.streamCalls != 1 || second.streamCalls != 1 {
		t.Fatalf("expected both providers to be attempted, got first=%d second=%d", first.streamCalls, second.streamCalls)
	}
}

func TestMessageHandlerSkipsOpenStreamProviderOnNextRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouterWithCircuitConfig(logger, provider.CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Hour,
		SuccessThreshold: 1,
	})
	first := &stubChatProvider{name: "first", streamErr: errors.New("provider unavailable")}
	second := &stubChatProvider{name: "second", streamData: "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"ok\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"}
	router.RegisterProvider(first.name, first)
	router.RegisterProvider(second.name, second)
	router.RegisterChain("coding", []string{first.name, second.name})

	engine := gin.New()
	engine.POST("/v1/messages", NewMessageHandler(router, logger).Handle)
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		return w
	}
	if w := request(); w.Code != http.StatusOK {
		t.Fatalf("first request status = %d: %s", w.Code, w.Body.String())
	}
	if w := request(); w.Code != http.StatusOK {
		t.Fatalf("second request status = %d: %s", w.Code, w.Body.String())
	}
	if first.streamCalls != 1 || second.streamCalls != 2 {
		t.Fatalf("stream calls = first:%d second:%d, open provider should be skipped", first.streamCalls, second.streamCalls)
	}
}
