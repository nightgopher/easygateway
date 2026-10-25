package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

const guardedSuccessSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ok\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"good\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestRoutingGuardFallsBackAndCancelsHTTPPreludeAcrossProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		path, body string
		handler    func(*provider.Router, *log.Logger) gin.HandlerFunc
	}{
		{"/v1/messages", `{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`, func(r *provider.Router, l *log.Logger) gin.HandlerFunc { return NewMessageHandler(r, l).Handle }},
		{"/v1/chat/completions", `{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`, func(r *provider.Router, l *log.Logger) gin.HandlerFunc { return NewChatCompletionsHandler(r, l).Handle }},
		{"/v1/responses", `{"model":"coding","stream":true,"input":"hi"}`, func(r *provider.Router, l *log.Logger) gin.HandlerFunc { return NewResponsesHandler(r, l).Handle }},
	} {
		t.Run(tt.path, func(t *testing.T) {
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"discard_me\"}}\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()
			logger := log.New(io.Discard, "", 0)
			router := provider.NewRouter(logger)
			router.SetRoutingPolicy(provider.RoutingPolicy{FirstOutputTimeout: 80 * time.Millisecond, FallbackWaitBudget: time.Second})
			router.RegisterProvider("slow", provider.NewAnthropicProvider(&provider.Config{Name: "slow", URL: upstream.URL, APIKey: "test-key"}))
			good := &stubChatProvider{name: "good", streamData: guardedSuccessSSE}
			router.RegisterProvider("good", good)
			router.RegisterChain("coding", []string{"slow", "good"})
			var attempts []requestmeta.ProviderAttempt
			engine := gin.New()
			engine.Use(func(c *gin.Context) { c.Next(); attempts = providerAttempts(c) })
			engine.POST(tt.path, tt.handler(router, logger))
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest("POST", tt.path, strings.NewReader(tt.body)))
			if w.Code != 200 || strings.Contains(w.Body.String(), "discard_me") || !strings.Contains(w.Body.String(), "OK") || good.streamCalls != 1 {
				t.Fatalf("status=%d body=%s goodcalls=%d", w.Code, w.Body.String(), good.streamCalls)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("upstream HTTP not canceled")
			}
			if len(attempts) != 2 || attempts[0].StatusCode != 504 || attempts[1].FirstOutputMs == nil {
				t.Fatalf("attempts=%+v", attempts)
			}
			raw, _ := json.Marshal(attempts)
			if !strings.Contains(string(raw), "first_output_ms") {
				t.Fatal("timing absent from persisted JSON")
			}
		})
	}
}

type failingNative struct {
	stubChatProvider
	rawCalls int
}

func (p *failingNative) ForwardRawResponsesStream(ctx context.Context, raw json.RawMessage, w io.Writer) error {
	p.rawCalls++
	return errors.New("native unavailable")
}
func TestNativeFailureIsNotRetriedThroughConvertedPath(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	first := &failingNative{stubChatProvider: stubChatProvider{name: "native"}}
	good := &stubChatProvider{name: "good", streamData: guardedSuccessSSE}
	router.RegisterProvider("native", first)
	router.RegisterProvider("good", good)
	router.RegisterChain("coding", []string{"native", "good"})
	engine := gin.New()
	engine.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"coding","input":"hi","stream":true}`)))
	if w.Code != 200 || first.rawCalls != 1 || first.streamCalls != 0 || good.streamCalls != 1 {
		t.Fatalf("status=%d native=%d duplicate=%d good=%d body=%s", w.Code, first.rawCalls, first.streamCalls, good.streamCalls, w.Body.String())
	}
}

type rateLimitedNative struct {
	stubChatProvider
	rawCalls int
}

func (p *rateLimitedNative) ForwardRawResponsesStream(context.Context, json.RawMessage, io.Writer) error {
	p.rawCalls++
	return &provider.ProviderError{Provider: p.name, StatusCode: 429, RetryAfter: "60", Message: "limited"}
}
func TestNativeRateLimitPreservesStatusWhenNoFallbackIsAvailable(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	p := &rateLimitedNative{stubChatProvider: stubChatProvider{name: "native"}}
	router.RegisterProvider("native", p)
	router.RegisterChain("coding", []string{"native"})
	engine := gin.New()
	engine.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	for _, stream := range []bool{true, false} {
		router.ResetProvider("native")
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":"coding","input":"hi","stream":%t}`, stream)
		engine.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
		if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
			t.Fatalf("status=%d retry=%s body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	}
	if p.rawCalls != 2 || p.streamCalls != 0 {
		t.Fatalf("calls=%d duplicate=%d", p.rawCalls, p.streamCalls)
	}
}
