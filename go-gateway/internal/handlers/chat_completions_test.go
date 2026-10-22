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
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/weijian/go-llm-gateway/internal/metrics"
	"github.com/weijian/go-llm-gateway/internal/middleware"
	"github.com/weijian/go-llm-gateway/internal/provider"
)

type stubChatProvider struct {
	name        string
	resp        *provider.Response
	streamData  string
	streamErr   error
	streamCalls int
	lastReq     *provider.Request
}

func (p *stubChatProvider) Name() string                     { return p.name }
func (p *stubChatProvider) URL() string                      { return "http://example.com" }
func (p *stubChatProvider) APIKey() string                   { return "" }
func (p *stubChatProvider) UseBearer() bool                  { return true }
func (p *stubChatProvider) IsHealthy(_ context.Context) bool { return true }
func (p *stubChatProvider) ForwardRequest(_ context.Context, req *provider.Request) (*provider.Response, error) {
	p.lastReq = req
	return p.resp, nil
}
func (p *stubChatProvider) ForwardStream(_ context.Context, req *provider.Request, w io.Writer) error {
	p.streamCalls++
	p.lastReq = req
	if p.streamData != "" {
		if _, err := io.WriteString(w, p.streamData); err != nil {
			return err
		}
	}
	return p.streamErr
}

func TestChatCompletionsHandlerReturnsOpenAIResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	router.RegisterChain("coding", []string{"stub"})
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID:         "msg_123",
			Type:       "message",
			Role:       "assistant",
			Model:      "glm-5-turbo",
			StopReason: "end_turn",
			Content: []provider.ContentBlock{
				{Type: "text", Text: "hello from gateway"},
			},
		},
	})

	handler := NewChatCompletionsHandler(router, logger)
	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)

	body := `{"model":"coding","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp["object"] != "chat.completion" {
		t.Fatalf("expected object chat.completion, got %#v", resp["object"])
	}

	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %#v", resp["choices"])
	}

	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hello from gateway" {
		t.Fatalf("expected assistant content, got %#v", message["content"])
	}
	if _, ok := message["reasoning_content"]; ok {
		t.Fatalf("reasoning_content should be omitted when the provider returns no thinking, got %#v", message["reasoning_content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("expected finish_reason stop, got %#v", choice["finish_reason"])
	}
}

func TestChatCompletionsHandlerIncludesReasoningContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	router.RegisterChain("coding", []string{"stub"})
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID:         "msg_reasoning",
			Type:       "message",
			Role:       "assistant",
			Model:      "glm-5.3-flash",
			StopReason: "max_tokens",
			Content: []provider.ContentBlock{
				{Type: "thinking", Thinking: "checking the token budget"},
			},
		},
	})

	engine := gin.New()
	engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"reply briefly"}],"max_tokens":20}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("expected one choice, got %#v", got.Choices)
	}
	if got.Choices[0].Message.Content != "" {
		t.Fatalf("expected empty content for a reasoning-only response, got %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].Message.ReasoningContent != "checking the token budget" {
		t.Fatalf("expected reasoning_content to be preserved, got %q", got.Choices[0].Message.ReasoningContent)
	}
	if got.Choices[0].FinishReason != "length" {
		t.Fatalf("expected finish_reason length, got %q", got.Choices[0].FinishReason)
	}
}

func TestChatCompletionsRecordsRequestCorrelationAndProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	collector := metrics.NewCollector()
	router := provider.NewRouter(logger)
	router.RegisterProvider("stub", &stubChatProvider{
		name: "stub",
		resp: &provider.Response{ID: "msg_meta", Type: "message", Role: "assistant", Content: []provider.ContentBlock{{Type: "text", Text: "ok"}}},
	})
	router.RegisterChain("coding", []string{"stub"})

	engine := gin.New()
	engine.Use(middleware.RequestID())
	engine.Use(middleware.Logging(logger, collector))
	engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("X-Request-ID", "meta-123")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	records := collector.GetRecentLogs(1)
	if len(records) != 1 || records[0].RequestID != "meta-123" || records[0].Provider != "stub" || len(records[0].ProviderAttempts) != 1 {
		t.Fatalf("request record = %#v", records)
	}
}

func TestChatCompletionsHandlerConvertsSystemAndToolMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "stub",
		resp: &provider.Response{
			ID:         "msg_456",
			Type:       "message",
			Role:       "assistant",
			Model:      "glm-5-turbo",
			StopReason: "tool_use",
			Content: []provider.ContentBlock{
				{Type: "tool_use", ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Beijing"}`)},
			},
		},
	}
	router.RegisterProvider("stub", stub)
	router.RegisterChain("coding", []string{"stub"})

	handler := NewChatCompletionsHandler(router, logger)
	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)

	body := `{
		"model":"coding",
		"messages":[
			{"role":"system","content":"You are helpful"},
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"calling tool","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"sunny"}
		],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object"}}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if stub.lastReq == nil {
		t.Fatal("expected provider request to be captured")
	}
	if len(stub.lastReq.Messages) != 3 {
		t.Fatalf("expected 3 non-system messages, got %d", len(stub.lastReq.Messages))
	}
	if stub.lastReq.Messages[1].Role != "assistant" {
		t.Fatalf("expected assistant message, got %s", stub.lastReq.Messages[1].Role)
	}
	if len(stub.lastReq.Messages[1].Content.Blocks()) != 2 {
		t.Fatalf("expected assistant text + tool_use blocks, got %d", len(stub.lastReq.Messages[1].Content.Blocks()))
	}
	if stub.lastReq.Messages[2].Role != "user" {
		t.Fatalf("expected tool message to map to user, got %s", stub.lastReq.Messages[2].Role)
	}
	blocks := stub.lastReq.Messages[2].Content.Blocks()
	if len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "call_1" {
		t.Fatalf("expected tool_result block, got %#v", blocks)
	}
	if _, ok := stub.lastReq.RawField("system"); !ok {
		t.Fatal("expected system field to be preserved in raw payload")
	}
	if _, ok := stub.lastReq.RawField("tools"); !ok {
		t.Fatal("expected tools field to be preserved in raw payload")
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	choices := resp["choices"].([]any)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	toolCalls, ok := message["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("expected one tool_call, got %#v", message["tool_calls"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %#v", choice["finish_reason"])
	}
}

func TestChatCompletionsHandlerPreservesImageContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "vision-stub",
		resp: &provider.Response{
			ID:         "msg_vision",
			Type:       "message",
			Role:       "assistant",
			Model:      "glm-5.3-flash",
			StopReason: "end_turn",
			Content:    []provider.ContentBlock{{Type: "text", Text: "cat"}},
		},
	}
	router.RegisterProvider("vision", stub)
	router.RegisterChain("glm-5.3-flash", []string{"vision"})

	handler := NewChatCompletionsHandler(router, logger)
	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)
	body := `{"model":"glm-5.3-flash","messages":[{"role":"user","content":[{"type":"text","text":"What is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if stub.lastReq == nil || len(stub.lastReq.Messages) != 1 {
		t.Fatalf("expected one provider message, got %#v", stub.lastReq)
	}
	blocks := stub.lastReq.Messages[0].Content.Blocks()
	if len(blocks) != 2 || blocks[1].Type != "image_url" {
		t.Fatalf("expected text + image_url blocks, got %#v", blocks)
	}
	if len(blocks[1].ImageURL) == 0 || len(blocks[1].Raw) == 0 {
		t.Fatalf("expected image payload to be retained, got %#v", blocks[1])
	}
}

func TestChatCompletionsHandlerRejectsImageForTextOnlyModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "text-only",
		resp: &provider.Response{
			ID:         "must_not_be_called",
			Type:       "message",
			Role:       "assistant",
			StopReason: "end_turn",
		},
	}
	router.RegisterProvider("text", provider.NewBoundModelProviderWrapper(
		stub,
		"text-model",
		[]string{provider.CapabilityText, provider.CapabilityStreaming},
	))
	router.RegisterChain("glm-sonnet", []string{"text"})

	handler := NewChatCompletionsHandler(router, logger)
	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)
	body := `{"model":"glm-sonnet","messages":[{"role":"user","content":[{"type":"text","text":"What is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if stub.lastReq != nil {
		t.Fatal("text-only provider must not receive an image request")
	}
	if !strings.Contains(w.Body.String(), "vision") {
		t.Fatalf("expected unsupported vision detail, got %s", w.Body.String())
	}
}

func TestChatCompletionsHandlerStreamsOpenAISSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "stub",
		streamData: strings.Join([]string{
			"event: message_start",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_789\",\"model\":\"glm-5-turbo\"}}",
			"",
			"event: content_block_start",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
			"",
			"event: content_block_delta",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}",
			"",
			"event: content_block_stop",
			"data: {\"type\":\"content_block_stop\",\"index\":0}",
			"",
			"event: message_delta",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}",
			"",
			"event: message_stop",
			"data: {\"type\":\"message_stop\"}",
			"",
		}, "\n"),
	}
	router.RegisterProvider("stub", stub)
	router.RegisterChain("coding", []string{"stub"})

	handler := NewChatCompletionsHandler(router, logger)
	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)

	body := `{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE content-type, got %q", got)
	}
	bodyText := w.Body.String()
	if !strings.Contains(bodyText, `"object":"chat.completion.chunk"`) {
		t.Fatalf("expected OpenAI chunk object, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"content":"Hello"`) {
		t.Fatalf("expected streamed content chunk, got %s", bodyText)
	}
	if strings.Contains(bodyText, `"reasoning_content"`) {
		t.Fatalf("reasoning_content should be omitted when the provider emits no thinking, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"finish_reason":"stop"`) {
		t.Fatalf("expected stop finish_reason, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `data: [DONE]`) {
		t.Fatalf("expected DONE sentinel, got %s", bodyText)
	}
}

func TestChatCompletionsHandlerStreamsReasoningContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "stub",
		streamData: strings.Join([]string{
			"event: message_start",
			`data: {"type":"message_start","message":{"id":"msg_stream_reasoning","model":"glm-5.3-flash"}}`,
			"",
			"event: content_block_start",
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			"",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"checking the token budget"}}`,
			"",
			"event: content_block_stop",
			`data: {"type":"content_block_stop","index":0}`,
			"",
			"event: message_delta",
			`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":20}}`,
			"",
			"event: message_stop",
			`data: {"type":"message_stop"}`,
			"",
		}, "\n"),
	}
	router.RegisterProvider("stub", stub)
	router.RegisterChain("coding", []string{"stub"})

	engine := gin.New()
	engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"reply briefly"}],"max_tokens":20}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	bodyText := w.Body.String()
	if !strings.Contains(bodyText, `"reasoning_content":"checking the token budget"`) {
		t.Fatalf("expected reasoning delta to be preserved in OpenAI SSE, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"finish_reason":"length"`) {
		t.Fatalf("expected max_tokens to map to finish_reason length, got %s", bodyText)
	}
	if !strings.Contains(bodyText, "data: [DONE]") {
		t.Fatalf("expected DONE sentinel, got %s", bodyText)
	}
}

func TestChatCompletionsHandlerStreamsToolCalls(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "stub",
		streamData: strings.Join([]string{
			"event: message_start",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_999\",\"model\":\"glm-5-turbo\"}}",
			"",
			"event: content_block_start",
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_weather\",\"name\":\"get_weather\",\"input\":{}}}",
			"",
			"event: content_block_delta",
			"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Beijing\\\"}\"}}",
			"",
			"event: content_block_stop",
			"data: {\"type\":\"content_block_stop\",\"index\":1}",
			"",
			"event: message_delta",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}",
			"",
			"event: message_stop",
			"data: {\"type\":\"message_stop\"}",
			"",
		}, "\n"),
	}
	router.RegisterProvider("stub", stub)
	router.RegisterChain("coding", []string{"stub"})

	handler := NewChatCompletionsHandler(router, logger)
	engine := gin.New()
	engine.POST("/v1/chat/completions", handler.Handle)

	body := `{"model":"coding","stream":true,"messages":[{"role":"user","content":"what is weather"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	bodyText := w.Body.String()
	if !strings.Contains(bodyText, `"index":0`) {
		t.Fatalf("expected zero-based tool_call index 0, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"name":"get_weather"`) {
		t.Fatalf("expected tool name get_weather, got %s", bodyText)
	}
	if strings.Contains(bodyText, `"name":""`) {
		t.Fatalf("arguments-only tool chunks must omit the name field, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"arguments":"{\"city\":\"Beijing\"}"`) {
		t.Fatalf("expected arguments string, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected finish_reason tool_calls, got %s", bodyText)
	}
}

func TestChatCompletionsHandlerFallsBackBeforeStreamStarts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	first := &stubChatProvider{
		name:      "rate-limited",
		streamErr: &provider.ProviderError{Provider: "rate-limited", StatusCode: http.StatusTooManyRequests, Message: "quota exceeded"},
	}
	second := &stubChatProvider{
		name: "fallback",
		streamData: strings.Join([]string{
			"event: message_start",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fallback\",\"model\":\"fallback\"}}",
			"",
			"event: message_stop",
			"data: {\"type\":\"message_stop\"}",
			"",
		}, "\n"),
	}
	router.RegisterProvider(first.name, first)
	router.RegisterProvider(second.name, second)
	router.RegisterChain("coding", []string{first.name, second.name})

	engine := gin.New()
	engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected fallback 200, got %d: %s", w.Code, w.Body.String())
	}
	if first.streamCalls != 1 || second.streamCalls != 1 {
		t.Fatalf("expected both providers to be attempted once, got first=%d second=%d", first.streamCalls, second.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("expected fallback stream to complete, got %s", w.Body.String())
	}
}

func TestChatCompletionsHandlerPreservesProviderStatusWhenAllStreamsFail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	for _, name := range []string{"first", "second"} {
		stub := &stubChatProvider{
			name:      name,
			streamErr: &provider.ProviderError{Provider: name, StatusCode: http.StatusUnauthorized, Message: "invalid api key"},
		}
		router.RegisterProvider(name, stub)
	}
	router.RegisterChain("coding", []string{"first", "second"})

	engine := gin.New()
	engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected upstream 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestChatCompletionsHandlerDoesNotFallbackAfterStreamStarts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	first := &stubChatProvider{
		name:       "partial",
		streamData: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_partial\",\"model\":\"partial\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
		streamErr:  errors.New("upstream stream interrupted"),
	}
	second := &stubChatProvider{
		name:       "must-not-run",
		streamData: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_second\",\"model\":\"second\"}}\n\n",
	}
	router.RegisterProvider(first.name, first)
	router.RegisterProvider(second.name, second)
	router.RegisterChain("coding", []string{first.name, second.name})

	engine := gin.New()
	engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if second.streamCalls != 0 {
		t.Fatal("must not fallback after the first SSE bytes were sent")
	}
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("expected a stream error event, got %s", w.Body.String())
	}
}

func TestChatCompletionsHandlerFallsBackOnRegionalRestriction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	first := &stubChatProvider{
		name:      "region-blocked",
		streamErr: &provider.ProviderError{Provider: "region-blocked", StatusCode: http.StatusBadRequest, Message: "User location is not supported for the API use."},
	}
	second := &stubChatProvider{
		name: "fallback",
		streamData: strings.Join([]string{
			"event: message_start",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fallback\",\"model\":\"fallback\"}}",
			"",
			"event: message_stop",
			"data: {\"type\":\"message_stop\"}",
			"",
		}, "\n"),
	}
	router.RegisterProvider(first.name, first)
	router.RegisterProvider(second.name, second)
	router.RegisterChain("coding", []string{first.name, second.name})

	engine := gin.New()
	engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected fallback 200, got %d: %s", w.Code, w.Body.String())
	}
	if first.streamCalls != 1 || second.streamCalls != 1 {
		t.Fatalf("expected both providers to be attempted once, got first=%d second=%d", first.streamCalls, second.streamCalls)
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("expected fallback stream to complete, got %s", w.Body.String())
	}
}
