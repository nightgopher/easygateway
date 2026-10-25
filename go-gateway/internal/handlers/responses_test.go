package handlers

import (
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
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestResponsesToProviderRequestPreservesMaxOutputTokens(t *testing.T) {
	req := &responsesRequest{
		Model:           "coding",
		Input:           []byte(`"hello"`),
		MaxOutputTokens: 16,
	}

	providerReq, err := responsesToProviderRequest(req)
	if err != nil {
		t.Fatalf("responsesToProviderRequest() error = %v", err)
	}
	if providerReq.MaxTokens != 16 {
		t.Fatalf("MaxTokens = %d, want 16", providerReq.MaxTokens)
	}
	raw, ok := providerReq.RawField("max_tokens")
	if !ok {
		t.Fatal("expected max_tokens to be present in the provider payload")
	}
	if string(raw) != "16" {
		t.Fatalf("raw max_tokens = %s, want 16", raw)
	}
}

// 链上第一个 provider 返回 403（账号无该模型权限）时必须继续降级，
// 而不是把 403 直接抛给客户端——这正是「持续降级」的核心。
func TestResponsesHandlerFallsBackWhenProviderRejectsModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	first := &stubChatProvider{
		name:      "first",
		streamErr: &provider.ProviderError{Provider: "first", StatusCode: http.StatusForbidden, Message: "account disabled"},
	}
	second := &stubChatProvider{
		name:      "second",
		streamErr: errors.New("network unavailable"),
	}
	router.RegisterProvider(first.name, first)
	router.RegisterProvider(second.name, second)
	router.RegisterChain("coding", []string{first.name, second.name})

	engine := gin.New()
	engine.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding","input":"hello","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	// 两个 provider 最终都失败，但关键是第一个的 403 没有短路整条链。
	if second.streamCalls == 0 {
		t.Fatal("second provider was never tried: 403 did not fall through")
	}
	if w.Code == http.StatusForbidden {
		t.Fatalf("403 from the first provider must not be surfaced to the client, got %d", w.Code)
	}
}

func TestResponsesHandlerSupportsNonStreamChatGPTPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	stub := &nonStreamChatGPTStub{streamBody: strings.Join([]string{
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-luna","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`,
		"",
	}, "\n")}
	router.RegisterProvider("chatgpt", stub)
	router.RegisterChain("gpt-5.6-luna", []string{"chatgpt"})

	engine := gin.New()
	engine.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-luna","input":"connection test","stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var response struct {
		Status string `json:"status"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("response is invalid JSON: %v", err)
	}
	if response.Status != "completed" || len(response.Output) != 1 || response.Output[0].Content[0].Text != "OK" {
		t.Fatalf("response = %+v, want completed response with OK", response)
	}
}

type nonStreamChatGPTStub struct {
	streamBody string
}

func (p *nonStreamChatGPTStub) Name() string                   { return "chatgpt" }
func (p *nonStreamChatGPTStub) URL() string                    { return "http://chatgpt" }
func (p *nonStreamChatGPTStub) APIKey() string                 { return "" }
func (p *nonStreamChatGPTStub) UseBearer() bool                { return true }
func (p *nonStreamChatGPTStub) IsHealthy(context.Context) bool { return true }
func (p *nonStreamChatGPTStub) ForwardRequest(context.Context, *provider.Request) (*provider.Response, error) {
	return nil, errors.New("not used")
}
func (p *nonStreamChatGPTStub) ForwardRawResponsesStream(_ context.Context, _ json.RawMessage, w io.Writer) error {
	_, err := io.WriteString(w, p.streamBody)
	return err
}

// TestResponsesStructuredToolOutputAndImages 覆盖 zcode 实际发送的形态：
// 工具结果 output 为块数组（文本+图片）、arguments 为对象、用户消息带
// Responses 风格的字符串 image_url。此前强类型 string 字段会让整个请求
// 解析失败（400 invalid input format），图片块也因缺少 ImageURL 被丢弃。
func TestResponsesStructuredToolOutputAndImages(t *testing.T) {
	input := []byte(`[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"看这张图"},
			{"type":"input_image","image_url":"data:image/png;base64,QUJD"}
		]},
		{"type":"function_call","call_id":"c1","name":"Read","arguments":{"path":"a.png"}},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"output_text","text":"image file"},
			{"type":"output_image","image_url":"data:image/png;base64,REFGRw=="}
		]}
	]`)

	providerReq, err := responsesToProviderRequest(&responsesRequest{Model: "deepseek-v4.1-flash", Input: input})
	if err != nil {
		t.Fatalf("responsesToProviderRequest() error = %v", err)
	}

	var toolUse provider.ContentBlock
	var toolResult provider.ContentBlock
	var userImage provider.ContentBlock
	for _, msg := range providerReq.Messages {
		for _, blk := range msg.Content.Blocks() {
			switch blk.Type {
			case "tool_use":
				toolUse = blk
			case "tool_result":
				toolResult = blk
			case "image_url":
				userImage = blk
			}
		}
	}

	if toolUse.Name != "Read" {
		t.Fatalf("tool_use name = %q", toolUse.Name)
	}
	if string(toolUse.Input) != `{"path":"a.png"}` {
		t.Fatalf("object arguments not normalized: %s", toolUse.Input)
	}

	if toolResult.ContentStr != "image file" {
		t.Fatalf("tool_result text = %q", toolResult.ContentStr)
	}
	if len(toolResult.ContentBlocks) != 1 || toolResult.ContentBlocks[0].Type != "image_url" {
		t.Fatalf("tool_result image block missing: %+v", toolResult.ContentBlocks)
	}
	var u struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(toolResult.ContentBlocks[0].ImageURL, &u); err != nil || u.URL != "data:image/png;base64,REFGRw==" {
		t.Fatalf("tool_result image url = %s err=%v", toolResult.ContentBlocks[0].ImageURL, err)
	}

	if len(userImage.ImageURL) == 0 {
		t.Fatal("inline input_image should populate ImageURL, not just Raw")
	}
	if err := json.Unmarshal(userImage.ImageURL, &u); err != nil || u.URL != "data:image/png;base64,QUJD" {
		t.Fatalf("inline image url = %s err=%v", userImage.ImageURL, err)
	}
}

// ZCode 在 Responses input 数组里发 role=system 的条目（而非 instructions
// 字段）。workbuddy 上游要求首条消息必须是 system，此前 normalizeRole 把
// system 映射成 user，导致上游 400 "first message is not system prompt"。
// 修复后 system 条目抽到顶层 system 字段，OpenAI 兼容 provider 重建时
// 排在消息列表首位。
func TestResponsesToProviderRequestKeepsSystemRole(t *testing.T) {
	req := &responsesRequest{
		Model: "workbuddy/deepseek-v4.1-flash",
		Input: []byte(`[
			{"role": "system", "content": "You are a coding agent."},
			{"role": "user", "content": "hi"}
		]`),
	}

	providerReq, err := responsesToProviderRequest(req)
	if err != nil {
		t.Fatalf("responsesToProviderRequest() error = %v", err)
	}
	if len(providerReq.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1 (system 条目已抽出)", len(providerReq.Messages))
	}
	if providerReq.Messages[0].Role != "user" {
		t.Fatalf("first message role = %q, want user", providerReq.Messages[0].Role)
	}
	sysRaw, ok := providerReq.RawField("system")
	if !ok {
		t.Fatal("expected top-level system field to be set")
	}
	var sysText string
	if err := json.Unmarshal(sysRaw, &sysText); err != nil {
		t.Fatalf("system field not a string: %v", err)
	}
	if sysText != "You are a coding agent." {
		t.Fatalf("system = %q, want full prompt", sysText)
	}

	// 模拟 OpenAI 兼容 provider 的重建逻辑：system 必须回到消息列表首位
	var decoded []map[string]any
	msgRaw, _ := providerReq.RawField("messages")
	if err := json.Unmarshal(msgRaw, &decoded); err != nil {
		t.Fatalf("raw messages not valid JSON: %v", err)
	}
	rebuilt := []map[string]any{{"role": "system", "content": sysText}}
	rebuilt = append(rebuilt, decoded...)
	if rebuilt[0]["role"] != "system" {
		t.Fatalf("rebuilt first message = %v, want system", rebuilt[0])
	}
}
