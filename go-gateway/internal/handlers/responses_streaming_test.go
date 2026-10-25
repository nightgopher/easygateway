package handlers

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

// ZCode（Vercel AI SDK openai-responses 解析器）用严格 Zod schema 校验每条
// 流式事件：response.output_text.delta / response.function_call_arguments.delta
// 的 item_id、function_call item 的 arguments 均为必填，缺失时整条事件被
// 静默丢弃，表现为"有 usage 无正文/工具调用"。以下测试锁定这些字段。

func TestResponsesStreamTextDeltaCarriesItemID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "stub",
		streamData: strings.Join([]string{
			"event: message_start",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"deepseek-v4-flash\"}}",
			"",
			"event: content_block_start",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
			"",
			"event: content_block_delta",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}",
			"",
			"event: content_block_stop",
			"data: {\"type\":\"content_block_stop\",\"index\":0}",
			"",
			"event: message_delta",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}",
			"",
			"event: message_stop",
			"data: {\"type\":\"message_stop\"}",
			"",
		}, "\n"),
	}
	router.RegisterProvider("stub", stub)
	router.RegisterChain("coding", []string{"stub"})

	engine := gin.New()
	engine.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	body := `{"model":"coding","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	bodyText := w.Body.String()
	if !strings.Contains(bodyText, `"type":"response.output_text.delta"`) {
		t.Fatalf("expected output_text.delta event, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"item_id":"msg_0"`) {
		t.Fatalf("expected output_text.delta to carry item_id msg_0 (required by ZCode/AI SDK schema), got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"delta":"你好"`) {
		t.Fatalf("expected text delta payload, got %s", bodyText)
	}
}

func TestResponsesStreamToolCallFromWholeBlockInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "stub",
		// deepv 等上游在 content_block_start 就携带完整 input，不发 input_json_delta
		streamData: strings.Join([]string{
			"event: message_start",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"model\":\"deepseek-v4-flash\"}}",
			"",
			"event: content_block_start",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_weather\",\"name\":\"get_weather\",\"input\":{\"city\":\"上海\"}}}",
			"",
			"event: content_block_stop",
			"data: {\"type\":\"content_block_stop\",\"index\":0}",
			"",
			"event: message_delta",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}",
			"",
			"event: message_stop",
			"data: {\"type\":\"message_stop\"}",
			"",
		}, "\n"),
	}
	router.RegisterProvider("stub", stub)
	router.RegisterChain("coding", []string{"stub"})

	engine := gin.New()
	engine.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	body := `{"model":"coding","input":"weather?","stream":true,"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	bodyText := w.Body.String()
	if !strings.Contains(bodyText, `"arguments":""`) {
		t.Fatalf("expected function_call item in output_item.added to carry arguments (required by ZCode/AI SDK schema), got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"type":"response.function_call_arguments.delta"`) {
		t.Fatalf("expected function_call_arguments.delta event for whole-block input, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"item_id":"fc_call_weather"`) {
		t.Fatalf("expected function_call_arguments.delta to carry item_id fc_call_weather, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"delta":"{\"city\":\"上海\"}"`) {
		t.Fatalf("expected whole-block input to be forwarded as arguments delta, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"arguments":"{\"city\":\"上海\"}"`) {
		t.Fatalf("expected output_item.done to carry full arguments, got %s", bodyText)
	}
}

func TestResponsesStreamToolCallArgumentsDeltaCarriesItemID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "stub",
		// anthropic 风格：input 占位为空对象，参数走 input_json_delta
		streamData: strings.Join([]string{
			"event: message_start",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_3\",\"model\":\"glm-5.3\"}}",
			"",
			"event: content_block_start",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_1\",\"name\":\"get_weather\",\"input\":{}}}",
			"",
			"event: content_block_delta",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Beijing\\\"}\"}}",
			"",
			"event: content_block_stop",
			"data: {\"type\":\"content_block_stop\",\"index\":0}",
			"",
			"event: message_delta",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}",
			"",
			"event: message_stop",
			"data: {\"type\":\"message_stop\"}",
			"",
		}, "\n"),
	}
	router.RegisterProvider("stub", stub)
	router.RegisterChain("coding", []string{"stub"})

	engine := gin.New()
	engine.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	body := `{"model":"coding","input":"weather?","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	bodyText := w.Body.String()
	if !strings.Contains(bodyText, `"type":"response.function_call_arguments.delta"`) {
		t.Fatalf("expected function_call_arguments.delta event, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"item_id":"fc_call_1"`) {
		t.Fatalf("expected function_call_arguments.delta to carry item_id fc_call_1, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"arguments":"{\"city\":\"Beijing\"}"`) {
		t.Fatalf("expected output_item.done to carry full arguments, got %s", bodyText)
	}
}

func TestChatCompletionsStreamSeedsWholeBlockToolArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &stubChatProvider{
		name: "stub",
		// deepv 等上游在 content_block_start 就携带完整 input，不发 input_json_delta；
		// 此前 chat 链路完全不发参数 chunk，客户端收到空参数工具调用
		streamData: strings.Join([]string{
			"event: message_start",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_4\",\"model\":\"deepseek-v4-flash\"}}",
			"",
			"event: content_block_start",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_weather\",\"name\":\"get_weather\",\"input\":{\"city\":\"上海\"}}}",
			"",
			"event: content_block_stop",
			"data: {\"type\":\"content_block_stop\",\"index\":0}",
			"",
			"event: message_delta",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}",
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

	body := `{"model":"coding","stream":true,"messages":[{"role":"user","content":"weather?"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	bodyText := w.Body.String()
	if !strings.Contains(bodyText, `"arguments":"{\"city\":\"上海\"}"`) {
		t.Fatalf("expected whole-block tool input to reach the client as arguments, got %s", bodyText)
	}
	if !strings.Contains(bodyText, `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected finish_reason tool_calls, got %s", bodyText)
	}
}
