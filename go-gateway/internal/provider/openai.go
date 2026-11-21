package provider

// OpenAI provider：把 Anthropic 格式请求转成 OpenAI 格式发出，再把响应转回 Anthropic 格式。
// 用于对接 EasyClaw 等只支持 /v1/chat/completions 的中转服务。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
)

// ─── OpenAI 请求/响应结构 ──────────────────────────────────────────────────────

type openAIToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"` // for tool result messages
	// 推理内容（deepseek 系兼容上游在非流式响应中返回）
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// chatTemplateKwargs 用于 skyclaw 等模型的 chat_template_kwargs 字段
type chatTemplateKwargs struct {
	EnableThinking *bool `json:"enable_thinking,omitempty"`
}

type openAIRequest struct {
	Model               string                     `json:"model"`
	Messages            []openAIMessage            `json:"messages"`
	MaxTokens           int                        `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                        `json:"max_completion_tokens,omitempty"`
	Temperature         float64                    `json:"temperature,omitempty"`
	TopP                float64                    `json:"top_p,omitempty"`
	TopK                int                        `json:"top_k,omitempty"`
	Stream              bool                       `json:"stream,omitempty"`
	Tools               []openAITool               `json:"tools,omitempty"`
	ToolChoice          json.RawMessage            `json:"tool_choice,omitempty"`
	Thinking            json.RawMessage            `json:"thinking,omitempty"`
	ReasoningEffort     string                     `json:"reasoning_effort,omitempty"`
	ExtraBody           json.RawMessage            `json:"extra_body,omitempty"`
	ResponseFormat      json.RawMessage            `json:"response_format,omitempty"`
	StreamOptions       json.RawMessage            `json:"stream_options,omitempty"`
	ChatTemplateKwargs  *chatTemplateKwargs        `json:"chat_template_kwargs,omitempty"`
	ExtraFields         map[string]json.RawMessage `json:"-"`
}

func (r openAIRequest) MarshalJSON() ([]byte, error) {
	type alias openAIRequest
	knownJSON, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(knownJSON, &known); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(r.ExtraFields)+len(known))
	for key, value := range r.ExtraFields {
		out[key] = append(json.RawMessage(nil), value...)
	}
	for key, value := range known {
		out[key] = value
	}
	return json.Marshal(out)
}

// openAITool 是 OpenAI 格式的工具定义
type openAITool struct {
	Type     string           `json:"type"` // "function"
	Function openAIToolSchema `json:"function"`
}

type openAIToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

// ─── OpenAI 流式 SSE 结构 ──────────────────────────────────────────────────────

// openAIStreamToolCall 是流式 tool_calls 中的单个增量片段
type openAIStreamToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIStreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// 推理内容：deepseek 系用 reasoning_content，OpenRouter 用 reasoning
	ReasoningContent string                 `json:"reasoning_content,omitempty"`
	Reasoning        string                 `json:"reasoning,omitempty"`
	ToolCalls        []openAIStreamToolCall `json:"tool_calls,omitempty"`
}

type openAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type openAIStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	Usage   *openAIStreamUsage   `json:"usage,omitempty"`
}

// openAIStreamUsage 流式 chunk 中的 usage（最后一条 chunk 携带）
type openAIStreamUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	cacheTokensKnown bool
}

func (d *promptTokensDetails) UnmarshalJSON(data []byte) error {
	var decoded struct {
		CachedTokens *int `json:"cached_tokens"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	d.CachedTokens = 0
	d.cacheTokensKnown = decoded.CachedTokens != nil
	if decoded.CachedTokens != nil {
		d.CachedTokens = *decoded.CachedTokens
	}
	return nil
}

// ─── OpenAIProvider ──────────────────────────────────────────────────────────

type OpenAIProvider struct {
	config *Config
	client *http.Client
}

const defaultRequestTimeout = 120 * time.Second

// requestTimeout 返回该 provider 的超时配置（RequestTimeout 未配置时用默认值）。
// 语义：限制响应首字节时间（TTFB，含上游排队；非流式请求等价于总时长）；
// 流式 body 的总时长不受限，由 ForwardStream 的空闲看门狗兜底。
func (p *OpenAIProvider) requestTimeout() time.Duration {
	if p.config.RequestTimeout > 0 {
		return p.config.RequestTimeout
	}
	return defaultRequestTimeout
}

func NewOpenAIProvider(config *Config) *OpenAIProvider {
	// 不用 http.Client.Timeout：它把整个流式 body 读取计入总时长，长生成会被
	// 整体砍断。改为 ResponseHeaderTimeout 限制首字节，流式停滞由看门狗中止
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = config.RequestTimeout
	if config.RequestTimeout <= 0 {
		transport.ResponseHeaderTimeout = defaultRequestTimeout
	}
	return &OpenAIProvider{
		config: config,
		client: &http.Client{Transport: transport},
	}
}

func (p *OpenAIProvider) Name() string    { return p.config.Name }
func (p *OpenAIProvider) URL() string     { return p.config.URL }
func (p *OpenAIProvider) APIKey() string  { return p.config.APIKey }
func (p *OpenAIProvider) UseBearer() bool { return true }

// FirstOutputTimeoutOverride 暴露 providers.yaml 的 per-provider 首字超时覆盖；
// 放在基础 provider 上，BoundModelProviderWrapper 通过接口提升把它暴露给路由层。
func (p *OpenAIProvider) FirstOutputTimeoutOverride() (time.Duration, time.Duration) {
	return p.config.FirstOutputTimeout, p.config.ReasoningFirstOutputTimeout
}

// ForwardRequest 将 Anthropic 格式请求转为 OpenAI 格式，发出后将响应转回 Anthropic 格式
func (p *OpenAIProvider) ForwardRequest(ctx context.Context, req *Request) (*Response, error) {
	oaiReq := toOpenAIRequest(req)

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}
	body, err = p.config.applyTransform(body)
	if err != nil {
		return nil, fmt.Errorf("transform openai request: %w", err)
	}

	resp, err := postWithAuth(ctx, p.client, p.Name(), p.config.URL, p.config, p.setHeaders, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, NewHTTPError(p.Name(), resp, respBody)
	}

	var oaiResp openAIResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}

	return fromOpenAIResponse(&oaiResp), nil
}

// ForwardStream 将 Anthropic 格式请求转为 OpenAI 流式请求，把 OpenAI SSE 转为 Anthropic SSE 写入 w
func (p *OpenAIProvider) ForwardStream(ctx context.Context, req *Request, w io.Writer) error {
	streamReq := toOpenAIRequest(req)
	streamReq.Stream = true

	body, err := json.Marshal(streamReq)
	if err != nil {
		return fmt.Errorf("marshal openai stream request: %w", err)
	}
	body, err = p.config.applyTransform(body)
	if err != nil {
		return fmt.Errorf("transform openai stream request: %w", err)
	}

	// 派生可取消 context：空闲看门狗超时触发 cancel，中止停滞的上游流
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := postWithAuth(streamCtx, p.client, p.Name(), p.config.URL, p.config, p.setHeaders, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return NewHTTPError(p.Name(), resp, b)
	}

	return p.forwardRelayKitStream(streamCtx, cancel, resp.Body, req.Model, w)

}

// IsHealthy 只反映本地配置：OpenAI 兼容 provider 无法在不发请求的情况下判断
// 上游可用性，因此恒为 true。上游可用性请用 Probe。
func (p *OpenAIProvider) IsHealthy(ctx context.Context) bool {
	return true
}

// Probe 发一个最小请求验证鉴权与模型可用性。
// max_tokens 留空：上游默认值最安全，避免过小的上限让模型报参数错误。
func (p *OpenAIProvider) Probe(ctx context.Context) ProbeResult {
	return p.ProbeModel(ctx, p.config.Name)
}

// ProbeModel 用指定模型探测。绑定模型名的包装器借此传入真实上游模型名。
func (p *OpenAIProvider) ProbeModel(ctx context.Context, model string) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return probeVia(ctx, model, 0, p.ForwardRequest)
}

func (p *OpenAIProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "go-llm-gateway/1.0")
	// applyAuth 处理静态 api_key 与渠道动态令牌二选一，并补渠道附加头。
	p.config.applyAuth(req, true)
	// 动态头最后应用：账号类渠道可要求把当前账号令牌再写进自定义头。
	applyDynamicHeaders(req, p.config.DynamicHeaders)
}

// ─── 始终思考模型处理 ──────────────────────────────────────────────────────────

// alwaysThinkingModels 列出上游强制思考、不接受 thinking:disabled 的模型。
// 请求带 disabled 转发到这些模型会被上游直接拒绝。
var alwaysThinkingModels = map[string]bool{
	"glm-5.3": true,
}

// thinkingIsDisabled 判断 thinking 字段是否为 {"type":"disabled"}。
func thinkingIsDisabled(raw json.RawMessage) bool {
	var t struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &t) == nil && t.Type == "disabled"
}

// ─── 格式转换辅助函数 ──────────────────────────────────────────────────────────

func toOpenAIRequest(req *Request) *openAIRequest {
	legacy := toOpenAIRequestLegacy(req)
	if converted, ok := relayKitRequest(req, legacy); ok {
		return converted
	}
	return legacy
}

func toOpenAIRequestLegacy(req *Request) *openAIRequest {
	var msgs []openAIMessage

	// 重建 system 消息（toProviderRequest 提取到了 raw["system"]，不在 Messages 里）
	if sysRaw, ok := req.raw["system"]; ok {
		var sysContent any
		if err := json.Unmarshal(sysRaw, &sysContent); err == nil && sysContent != nil {
			msgs = append(msgs, openAIMessage{
				Role:    "system",
				Content: sysContent,
			})
		}
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			blocks := m.Content.Blocks()
			if len(blocks) == 0 {
				msgs = append(msgs, openAIMessage{Role: "user", Content: m.Content.String()})
				continue
			}

			var contentParts []json.RawMessage
			flushUserContent := func() {
				if len(contentParts) == 0 {
					return
				}
				content := any(nil)
				allText := true
				var textParts []string
				for _, part := range contentParts {
					var textBlock struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if err := json.Unmarshal(part, &textBlock); err != nil || textBlock.Type != "text" {
						allText = false
						break
					}
					textParts = append(textParts, textBlock.Text)
				}
				if allText {
					content = strings.Join(textParts, "")
				} else {
					content = contentParts
				}
				msgs = append(msgs, openAIMessage{
					Role:    "user",
					Content: content,
				})
				contentParts = nil
			}

			for _, b := range blocks {
				switch b.Type {
				case "text":
					if part := openAIContentPart(b); len(part) > 0 {
						contentParts = append(contentParts, part)
					}
				case "tool_result":
					flushUserContent()

					var resultParts []string
					for _, inner := range b.ContentBlocks {
						if inner.Type == "text" {
							resultParts = append(resultParts, inner.Text)
						}
					}
					if b.ContentStr != "" {
						resultParts = []string{b.ContentStr}
					}
					resultContent := strings.Join(resultParts, "")
					if resultContent == "" {
						resultContent = "(tool result)"
					}

					msgs = append(msgs, openAIMessage{
						Role:       "tool",
						Content:    resultContent,
						ToolCallID: b.ToolUseID,
					})
				default:
					if part := openAIContentPart(b); len(part) > 0 {
						contentParts = append(contentParts, part)
					}
				}
			}

			flushUserContent()
		case "assistant":
			// assistant 消息：可能含 text 和 tool_use block
			// OpenAI 格式：tool_use 写进 ToolCalls，text 写进 Content
			var content string
			var toolCalls []openAIToolCall
			for _, b := range m.Content.Blocks() {
				switch b.Type {
				case "text":
					content += b.Text
				case "tool_use":
					args := "{}"
					if b.Input != nil {
						args = string(b.Input)
					}
					toolCalls = append(toolCalls, openAIToolCall{
						ID:   b.ID,
						Type: "function",
						Function: openAIToolFunction{
							Name:      b.Name,
							Arguments: args,
						},
					})
				}
			}
			if len(m.Content.Blocks()) == 0 {
				content = m.Content.String()
			}
			msgs = append(msgs, openAIMessage{
				Role:      "assistant",
				Content:   content,
				ToolCalls: toolCalls,
			})
		default:
			msgs = append(msgs, openAIMessage{Role: m.Role, Content: m.Content.String()})
		}
	}

	oaiReq := &openAIRequest{
		Model:       req.Model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		ExtraFields: make(map[string]json.RawMessage),
	}
	for key, value := range req.raw {
		switch key {
		case "messages", "system", "tools":
			continue
		}
		oaiReq.ExtraFields[key] = append(json.RawMessage(nil), value...)
	}

	// 透传额外字段（例如 skyclaw 的 top_k、chat_template_kwargs 等）
	if v, ok := req.raw["top_k"]; ok {
		_ = json.Unmarshal(v, &oaiReq.TopK)
	}
	if v, ok := req.raw["chat_template_kwargs"]; ok {
		_ = json.Unmarshal(v, &oaiReq.ChatTemplateKwargs)
	}
	if v, ok := req.raw["temperature"]; ok {
		_ = json.Unmarshal(v, &oaiReq.Temperature)
	}
	if v, ok := req.raw["top_p"]; ok {
		_ = json.Unmarshal(v, &oaiReq.TopP)
	}
	if v, ok := req.raw["tool_choice"]; ok {
		oaiReq.ToolChoice = v
	}
	if v, ok := req.raw["thinking"]; ok {
		// 始终思考模型（如 glm-5.3）不接受 thinking:disabled——上游报错
		// 「该模型始终思考，不支持关闭思考」。丢弃 disabled 让其走默认思考档。
		if !alwaysThinkingModels[req.Model] || !thinkingIsDisabled(v) {
			oaiReq.Thinking = append(json.RawMessage(nil), v...)
		}
	}
	if v, ok := req.raw["reasoning_effort"]; ok {
		_ = json.Unmarshal(v, &oaiReq.ReasoningEffort)
	}
	if v, ok := req.raw["extra_body"]; ok {
		oaiReq.ExtraBody = append(json.RawMessage(nil), v...)
	}
	if v, ok := req.raw["response_format"]; ok {
		oaiReq.ResponseFormat = append(json.RawMessage(nil), v...)
	}
	if v, ok := req.raw["stream_options"]; ok {
		oaiReq.StreamOptions = append(json.RawMessage(nil), v...)
	}
	if v, ok := req.raw["max_completion_tokens"]; ok {
		_ = json.Unmarshal(v, &oaiReq.MaxCompletionTokens)
	}

	// 转换 tools：Anthropic input_schema → OpenAI parameters
	if toolsRaw, ok := req.raw["tools"]; ok {
		var anthropicTools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(toolsRaw, &anthropicTools); err == nil {
			for _, t := range anthropicTools {
				oaiReq.Tools = append(oaiReq.Tools, openAITool{
					Type: "function",
					Function: openAIToolSchema{
						Name:        t.Name,
						Description: t.Description,
						Parameters:  t.InputSchema,
					},
				})
			}
		}
	}

	return oaiReq
}

// openAIContentPart converts one internal non-text block without reducing it
// to text. Raw blocks from an OpenAI request are emitted unchanged so fields
// added by a compatible provider are not lost in the round trip.
func openAIContentPart(block ContentBlock) json.RawMessage {
	if block.Type == "image" && len(block.Source) > 0 {
		var source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		}
		if err := json.Unmarshal(block.Source, &source); err == nil {
			url := source.URL
			if url == "" && source.Data != "" {
				mediaType := source.MediaType
				if mediaType == "" {
					mediaType = "image/*"
				}
				url = "data:" + mediaType + ";base64," + source.Data
			}
			if url != "" {
				data, _ := json.Marshal(map[string]any{
					"type":      "image_url",
					"image_url": map[string]string{"url": url},
				})
				return data
			}
		}
	}
	if len(block.Raw) > 0 {
		var responseBlock struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			ImageURL json.RawMessage `json:"image_url"`
			Detail   string          `json:"detail"`
		}
		if json.Unmarshal(block.Raw, &responseBlock) == nil {
			switch responseBlock.Type {
			case "input_text", "output_text":
				data, _ := json.Marshal(map[string]any{
					"type": "text",
					"text": responseBlock.Text,
				})
				return data
			case "input_image":
				var imageURL any
				var url string
				if json.Unmarshal(responseBlock.ImageURL, &url) == nil {
					image := map[string]any{"url": url}
					if responseBlock.Detail != "" {
						image["detail"] = responseBlock.Detail
					}
					imageURL = image
				} else {
					var image map[string]any
					if json.Unmarshal(responseBlock.ImageURL, &image) == nil {
						if responseBlock.Detail != "" && image["detail"] == nil {
							image["detail"] = responseBlock.Detail
						}
						imageURL = image
					}
				}
				if imageURL != nil {
					data, _ := json.Marshal(map[string]any{
						"type":      "image_url",
						"image_url": imageURL,
					})
					return data
				}
			}
		}
		return append(json.RawMessage(nil), block.Raw...)
	}
	var value any
	switch block.Type {
	case "text":
		value = map[string]any{"type": block.Type, "text": block.Text}
	case "image_url":
		value = map[string]any{"type": block.Type, "image_url": json.RawMessage(block.ImageURL)}
	case "video_url":
		value = map[string]any{"type": block.Type, "video_url": json.RawMessage(block.VideoURL)}
	case "file", "file_url":
		value = map[string]any{"type": block.Type, "file": json.RawMessage(block.File)}
	case "input_audio", "audio":
		value = map[string]any{"type": block.Type, "input_audio": json.RawMessage(block.InputAudio)}
	default:
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func fromOpenAIResponse(oai *openAIResponse) *Response {
	if converted, ok := relayKitOpenAIResponse(oai); ok {
		return converted
	}
	return fromOpenAIResponseLegacy(oai)
}

// RelayKit converts the common response shape. Keep the existing converter for
// inputs whose semantics RelayKit would otherwise change (multiple choices,
// non-string content or non-object tool arguments).
func relayKitOpenAIResponse(oai *openAIResponse) (*Response, bool) {
	if len(oai.Choices) != 1 {
		return nil, false
	}
	choice := oai.Choices[0]
	if choice.Message.Content != nil {
		if _, ok := choice.Message.Content.(string); !ok {
			return nil, false
		}
	}
	for _, call := range choice.Message.ToolCalls {
		var input map[string]json.RawMessage
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil || input == nil {
			return nil, false
		}
	}
	data, err := json.Marshal(oai)
	if err != nil {
		return nil, false
	}
	var source dto.OpenAITextResponse
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, false
	}
	converted := relayconvert.ResponseOpenAI2Claude(&source, nil)
	data, err = json.Marshal(converted)
	if err != nil {
		return nil, false
	}
	var result Response
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, false
	}
	for i := range result.Content {
		result.Content[i].Raw = nil
	}
	// Preserve the gateway's accounting contract, including explicitly zero
	// cached-token counts, independent of RelayKit's usage representation.
	result.Usage.InputTokens = oai.Usage.PromptTokens
	result.Usage.OutputTokens = oai.Usage.CompletionTokens
	if details := oai.Usage.PromptTokensDetails; details != nil && details.cacheTokensKnown {
		result.CacheReadInputTokens = details.CachedTokens
		result.CacheInputTokens = oai.Usage.PromptTokens
		result.CacheUsageKnown = true
		result.Usage.InputTokens -= details.CachedTokens
		if result.Usage.InputTokens < 0 {
			result.Usage.InputTokens = 0
		}
	} else {
		result.CacheReadInputTokens, result.CacheCreationInputTokens, result.CacheInputTokens = 0, 0, 0
		result.CacheUsageKnown = false
	}
	// Unknown finish reasons retain the gateway's established fallback.
	result.StopReason = mapFinishReason(choice.FinishReason)
	return &result, true
}

func fromOpenAIResponseLegacy(oai *openAIResponse) *Response {
	var blocks []ContentBlock
	var stopReason string

	if len(oai.Choices) > 0 {
		choice := oai.Choices[0]
		stopReason = mapFinishReason(choice.FinishReason)

		// 推理内容（deepseek 系）→ Anthropic thinking 块，置于 text 之前
		if reasoning := choice.Message.ReasoningContent; reasoning != "" {
			blocks = append(blocks, ContentBlock{Type: "thinking", Thinking: reasoning})
		}

		// 文本内容
		if content := openAIResponseContentText(choice.Message.Content); content != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: content})
		}

		// 工具调用：OpenAI tool_calls → Anthropic tool_use
		for _, tc := range choice.Message.ToolCalls {
			var input json.RawMessage
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				// arguments 不是合法 JSON，包成字符串
				input, _ = json.Marshal(tc.Function.Arguments)
			}
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
	}

	if len(blocks) == 0 {
		blocks = []ContentBlock{{Type: "text", Text: ""}}
	}

	resp := &Response{
		ID:         oai.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      oai.Model,
		StopReason: stopReason,
		Content:    blocks,
	}
	resp.Usage.InputTokens = oai.Usage.PromptTokens
	resp.Usage.OutputTokens = oai.Usage.CompletionTokens
	if details := oai.Usage.PromptTokensDetails; details != nil && details.cacheTokensKnown {
		resp.CacheReadInputTokens = details.CachedTokens
		resp.CacheInputTokens = oai.Usage.PromptTokens
		resp.CacheUsageKnown = true
		resp.Usage.InputTokens -= details.CachedTokens
		if resp.Usage.InputTokens < 0 {
			resp.Usage.InputTokens = 0
		}
	}
	return resp
}

func openAIResponseContentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var text strings.Builder
		for _, item := range value {
			if block, ok := item.(map[string]any); ok {
				if value, ok := block["text"].(string); ok {
					text.WriteString(value)
				}
			}
		}
		return text.String()
	default:
		return ""
	}
}

// mapFinishReason 把 OpenAI finish_reason 转成 Anthropic stop_reason
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// writeSSE 写一条 SSE 事件
func writeSSE(w io.Writer, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// jsonString 把字符串转为 JSON 字符串字面量（含引号和转义）
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
