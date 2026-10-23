package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/weijian/go-llm-gateway/internal/archive"
	"github.com/weijian/go-llm-gateway/internal/provider"
)

// ─── Responses API 数据结构 ──────────────────────────────────────────────────

// responsesRequest 是 OpenAI Responses API 请求格式
type responsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions,omitempty"`
	Input           json.RawMessage `json:"input"`
	Tools           json.RawMessage `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Reasoning       json.RawMessage `json:"reasoning,omitempty"`
	// 透传其他字段
	raw map[string]json.RawMessage
}

func (r *responsesRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.raw = raw
	if v, ok := raw["model"]; ok {
		_ = json.Unmarshal(v, &r.Model)
	}
	if v, ok := raw["instructions"]; ok {
		_ = json.Unmarshal(v, &r.Instructions)
	}
	if v, ok := raw["input"]; ok {
		r.Input = v
	}
	if v, ok := raw["tools"]; ok {
		r.Tools = v
	}
	if v, ok := raw["tool_choice"]; ok {
		r.ToolChoice = v
	}
	if v, ok := raw["max_output_tokens"]; ok {
		_ = json.Unmarshal(v, &r.MaxOutputTokens)
	}
	if v, ok := raw["temperature"]; ok {
		var temp float64
		if err := json.Unmarshal(v, &temp); err == nil {
			r.Temperature = &temp
		}
	}
	if v, ok := raw["stream"]; ok {
		_ = json.Unmarshal(v, &r.Stream)
	}
	if v, ok := raw["reasoning"]; ok {
		r.Reasoning = v
	}
	return nil
}

// inputItem 表示 input 数组中的一个元素
type inputItem struct {
	Type    string          `json:"type,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	// function_call 字段
	CallID    string         `json:"call_id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Arguments flexJSONString `json:"arguments,omitempty"`
	// function_call_output 字段。部分客户端（zcode）的工具结果带图时会把
	// output 发成块数组而非字符串，必须兼容，否则整个请求解析失败。
	Output flexOutput `json:"output,omitempty"`
	// custom_tool_call 字段
	ID    string         `json:"id,omitempty"`
	Input flexJSONString `json:"input,omitempty"` // custom tool 的 input（字符串或结构化）

	// reasoning item / 思考模式字段。思考模式下带工具调用的历史轮次必须原样
	// 回传思维链，否则上游（DeepV/easyrouterio）直接拒绝整个请求。
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary,omitempty"`
	ReasoningText    string `json:"reasoning_text,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// flexJSONString 接受字符串或结构化 JSON（对象/数组/数字等）。部分客户端
// 会把 arguments/input 发成对象，直接用 string 反序列化会让整个 input
// 数组解析失败；结构化内容重新序列化为紧凑 JSON 字符串。
type flexJSONString string

func (f *flexJSONString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexJSONString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return err
	}
	*f = flexJSONString(compact)
	return nil
}

// flexOutput 兼容 function_call_output.output 的字符串与结构化形态。
// zcode 等客户端的工具结果带图时是块数组（output_text / output_image 等），
// 文本块合并为字符串，图片块保留为 ContentBlock 供多模态 provider 转发。
type flexOutput struct {
	Text   string
	Blocks []provider.ContentBlock
}

func (f *flexOutput) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		f.Text = s
		return nil
	}
	if string(b) == "null" {
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(b, &parts); err == nil {
		f.appendBlocks(parts)
		return nil
	}
	// 对象形态：优先取 content 块数组，其次 text 字段。
	var obj struct {
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		f.Text = obj.Text
		if len(obj.Content) > 0 {
			if err := json.Unmarshal(obj.Content, &parts); err == nil {
				f.appendBlocks(parts)
			} else if err := json.Unmarshal(obj.Content, &s); err == nil {
				f.appendText(s)
			}
		}
		return nil
	}
	return fmt.Errorf("unsupported function_call_output.output format")
}

func (f *flexOutput) appendText(t string) {
	if t == "" {
		return
	}
	if f.Text != "" {
		f.Text += "\n"
	}
	f.Text += t
}

func (f *flexOutput) appendBlocks(parts []json.RawMessage) {
	for _, part := range parts {
		var meta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(part, &meta); err != nil {
			continue
		}
		switch meta.Type {
		case "output_text", "input_text", "text":
			f.appendText(meta.Text)
		case "output_image", "input_image", "image_url", "image":
			if blk := imageBlockFromPart(part); blk != nil {
				f.Blocks = append(f.Blocks, *blk)
			}
		}
	}
}

// imageBlockFromPart 把图片块归一化为 provider.ContentBlock。image_url 在
// Responses 协议里是纯字符串（data: URL 或链接），在 chat.completions 协议
// 里是 {"url": ...} 对象，统一成 ImageURL 对象供 convertImagePart 消费。
func imageBlockFromPart(part json.RawMessage) *provider.ContentBlock {
	var probe struct {
		ImageURL json.RawMessage `json:"image_url"`
		Source   json.RawMessage `json:"source"`
		URL      string          `json:"url"`
	}
	if err := json.Unmarshal(part, &probe); err != nil {
		return nil
	}
	blk := &provider.ContentBlock{Type: "image_url", Raw: append([]byte(nil), part...)}
	switch {
	case len(probe.ImageURL) > 0:
		var u string
		if json.Unmarshal(probe.ImageURL, &u) == nil && u != "" {
			blk.ImageURL = json.RawMessage(`{"url":` + quoteJSONString(u) + `}`)
		} else {
			blk.ImageURL = probe.ImageURL
		}
	case len(probe.Source) > 0:
		blk.Source = probe.Source
	case probe.URL != "":
		blk.ImageURL = json.RawMessage(`{"url":` + quoteJSONString(probe.URL) + `}`)
	default:
		return nil
	}
	return blk
}

func quoteJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// responsesTool 表示 Responses API 中的工具定义
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// responsesResponse 是 OpenAI Responses API 响应格式
type responsesResponse struct {
	ID        string         `json:"id"`
	Object    string         `json:"object"` // "response"
	CreatedAt int64          `json:"created_at"`
	Status    string         `json:"status"` // "completed", "incomplete", "failed"
	Model     string         `json:"model"`
	Output    []outputItem   `json:"output"`
	Usage     responsesUsage `json:"usage"`
}

type responsesUsage struct {
	InputTokens        int                          `json:"input_tokens"`
	OutputTokens       int                          `json:"output_tokens"`
	TotalTokens        int                          `json:"total_tokens"`
	InputTokensDetails *responsesInputTokensDetails `json:"input_tokens_details,omitempty"`
}

type responsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// outputItem 是 Responses API 响应中的输出项
type outputItem struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"` // "message", "function_call", "reasoning"
	Status  string          `json:"status,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content []outputContent `json:"content,omitempty"`
	// function_call 字段
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type outputContent struct {
	Type string `json:"type"` // "output_text"
	Text string `json:"text,omitempty"`
}

// ─── ResponsesHandler ──────────────────────────────────────────────────────

type responsesHandler struct {
	router   *provider.Router
	logger   *log.Logger
	archiver *archive.Archiver
}

func NewResponsesHandler(router *provider.Router, logger *log.Logger) *responsesHandler {
	return &responsesHandler{router: router, logger: logger}
}

// SetArchiver injects the conversation archiver.
func (h *responsesHandler) SetArchiver(a *archive.Archiver) {
	h.archiver = a
}

// Handle 处理 /v1/responses 端点
func (h *responsesHandler) Handle(c *gin.Context) {
	c.Request = c.Request.WithContext(h.router.BeginRouting(c.Request.Context()))
	rawBody, _ := io.ReadAll(c.Request.Body)
	logRequestSummary(h.logger, c, "responses", len(rawBody))
	c.Request.Body = io.NopCloser(strings.NewReader(string(rawBody)))

	var req responsesRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": fmt.Sprintf("Invalid request body: %v", err),
			"type":    "invalid_request_error",
		}})
		return
	}
	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": "Model is required",
			"type":    "invalid_request_error",
		}})
		return
	}
	setRequestMetadata(c, req.Model, req.Stream)

	// 检查是否走 ChatGPT 透传路径（Responses API 原生格式，不需要 Anthropic 转换）
	if h.tryChatGPTPassthrough(c, &req, rawBody) {
		return
	}

	if req.Stream {
		h.handleStream(c, &req, rawBody)
	} else {
		h.handleNonStream(c, &req, rawBody)
	}
}

// handleNonStream 处理非流式请求
func (h *responsesHandler) handleNonStream(c *gin.Context, req *responsesRequest, rawBody []byte) {
	providerReq, err := responsesToProviderRequest(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
		}})
		return
	}

	resp, finalProvider, attempts, err := h.router.ForwardWithDetails(c.Request.Context(), providerReq.Model, providerReq)
	if err != nil {
		err = nativeFallbackError(c, err)
	}
	setForwardMetadata(c, finalProvider, append(providerAttempts(c), attempts...), err)
	if err != nil {
		h.logger.Printf("Responses forward failed: %s", provider.RedactedErrorText(err))
		setProviderErrorHeaders(c, err)
		if h.archiver != nil && h.archiver.Enabled() {
			// Sanitize the error message for BOTH response_body and error_reason
			// to prevent credential leakage (e.g. "invalid api key: sk-secret").
			sanitized := archiveErrorReason(err)
			submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody,
				[]byte(fmt.Sprintf(`{"error":%q}`, sanitized)),
				archive.StatusError, routingErrorStatus(err), archiveErrorReason(err))
		}
		c.JSON(routingErrorStatus(err), gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "server_error",
		}})
		return
	}
	setUsageMetadata(c, totalPromptTokens(resp), resp.Usage.OutputTokens)
	setCacheUsageMetadata(c, resp.CacheReadInputTokens, resp.CacheCreationInputTokens, resp.CacheInputTokens, resp.CacheUsageKnown)

	responseObj := providerToResponsesResponse(resp, req.Model)
	if h.archiver != nil && h.archiver.Enabled() {
		submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody,
			[]byte(marshalCompact(responseObj)),
			archive.StatusCompleted, http.StatusOK, "")
	}

	c.JSON(http.StatusOK, responseObj)
}

// ─── Responses → Anthropic 格式转换 ──────────────────────────────────────────

func responsesToProviderRequest(req *responsesRequest) (*provider.Request, error) {
	out := &provider.Request{
		Model: req.Model,
	}

	out.Stream = req.Stream

	// 解析 input（可能是数组或字符串）
	var messages []provider.Message
	// input 数组里 role=system/developer 的条目统一抽到顶层 system 字段。
	// zcode 会在 input 里发 system 消息而不是 instructions；此前 normalizeRole
	// 把它映射成 user，workbuddy 上游要求首条消息必须是 system，直接 400
	// ("first message is not system prompt")。抽出后 OpenAI 兼容 provider
	// 会把 raw["system"] 重建为第一条 system 消息，Anthropic provider 走顶层
	// system 字段，两边都合法。
	var systemTexts []string

	// 先尝试作为字符串
	var inputStr string
	if err := json.Unmarshal(req.Input, &inputStr); err == nil {
		messages = append(messages, provider.Message{
			Role:    "user",
			Content: provider.NewStringContent(inputStr),
		})
	} else {
		// 作为数组解析
		var items []inputItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("invalid input format: expected string or array")
		}

		for _, item := range items {
			switch {
			case item.Type == "":
				// 可能是简单 message（有 role 无 type）
				if item.Role != "" {
					content := parseContentField(item.Content)
					if text := systemTextFromItem(item.Role, content); text != "" {
						systemTexts = append(systemTexts, text)
						continue
					}
					role := normalizeRole(item.Role)
					if r := firstNonEmpty(item.ReasoningContent, item.ReasoningText); r != "" && role == "assistant" {
						content = prependThinkingBlock(content, r)
					}
					messages = append(messages, provider.Message{
						Role:    role,
						Content: content,
					})
				}
			case item.Type == "message":
				content := parseContentField(item.Content)
				if text := systemTextFromItem(item.Role, content); text != "" {
					systemTexts = append(systemTexts, text)
					continue
				}
				role := normalizeRole(item.Role)
				if r := firstNonEmpty(item.ReasoningContent, item.ReasoningText); r != "" && role == "assistant" {
					content = prependThinkingBlock(content, r)
				}
				messages = append(messages, provider.Message{
					Role:    role,
					Content: content,
				})
			case item.Type == "function_call":
				// function_call → assistant message with tool_use
				var inputJSON json.RawMessage
				if item.Arguments != "" {
					inputJSON = json.RawMessage(string(item.Arguments))
				} else {
					inputJSON = json.RawMessage(`{}`)
				}
				blocks := make([]provider.ContentBlock, 0, 2)
				if r := firstNonEmpty(item.ReasoningContent, item.ReasoningText); r != "" {
					blocks = append(blocks, provider.ContentBlock{Type: "thinking", Thinking: r})
				}
				blocks = append(blocks, provider.ContentBlock{
					Type:  "tool_use",
					ID:    item.CallID,
					Name:  item.Name,
					Input: inputJSON,
				})
				messages = append(messages, provider.Message{
					Role:    "assistant",
					Content: provider.NewBlocksContent(blocks),
				})
			case item.Type == "function_call_output":
				// function_call_output → user message with tool_result。
				// 结构化 output 里的文本并入 ContentStr，图片块挂在
				// ContentBlocks 上，由支持视觉的 provider 转成 inlineData。
				toolResult := provider.ContentBlock{
					Type:       "tool_result",
					ToolUseID:  item.CallID,
					ContentStr: item.Output.Text,
				}
				if len(item.Output.Blocks) > 0 {
					toolResult.ContentBlocks = item.Output.Blocks
				}
				messages = append(messages, provider.Message{
					Role: "user",
					Content: provider.NewBlocksContent([]provider.ContentBlock{
						toolResult,
					}),
				})
			case item.Type == "reasoning":
				if r := reasoningTextFromItem(item); r != "" {
					messages = append(messages, provider.Message{
						Role: "assistant",
						Content: provider.NewBlocksContent([]provider.ContentBlock{
							{Type: "thinking", Thinking: r},
						}),
					})
				}
			default:
				// 其他类型暂跳过
			}
		}
	}

	// 合并连续的 assistant 消息（多个 function_call 可能产生连续 assistant 消息）
	messages = mergeAssistantMessages(messages)

	out.Messages = messages

	// 构建 raw 字段
	out.SetRawField("messages", out.Messages)
	// system：instructions 在前，input 数组里的 system 条目按出现顺序合并在后
	if req.Instructions != "" {
		systemTexts = append([]string{req.Instructions}, systemTexts...)
	}
	if len(systemTexts) > 0 {
		out.SetRawField("system", strings.Join(systemTexts, "\n\n"))
	}
	// Responses API 的 max_output_tokens 对应内部/Anthropic 兼容请求的
	// max_tokens。之前为了规避部分 GLM 在极小预算下的空响应而直接丢弃，
	// 导致调用方的输出上限失效；兼容性应由具体 provider 处理，而不是在
	// 协议转换层静默删除。
	if req.MaxOutputTokens > 0 {
		out.MaxTokens = req.MaxOutputTokens
		out.SetRawField("max_tokens", req.MaxOutputTokens)
	}
	if out.Stream {
		out.SetRawField("stream", true)
	}
	if req.Temperature != nil {
		out.SetRawField("temperature", *req.Temperature)
	}

	// 转换 tools
	if req.Tools != nil {
		var respTools []responsesTool
		if err := json.Unmarshal(req.Tools, &respTools); err == nil {
			anthropicTools := make([]map[string]any, 0, len(respTools))
			for _, t := range respTools {
				if t.Type == "function" {
					anthropicTools = append(anthropicTools, map[string]any{
						"name":         t.Name,
						"description":  t.Description,
						"input_schema": t.Parameters,
					})
				}
			}
			if len(anthropicTools) > 0 {
				out.SetRawField("tools", anthropicTools)
			}
		}
	}

	return out, nil
}

// parseContentField 解析 Responses API 的 content 字段。
// 文本块和多模态块都保留到内部结构，避免 Responses → Chat/Anthropic
// 转换时把 input_image、input_file 等内容压成空文本。
func parseContentField(content json.RawMessage) provider.MessageContent {
	if content == nil {
		return provider.NewStringContent("")
	}

	// 尝试字符串
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return provider.NewStringContent(s)
	}

	// 尝试数组（input_text / input_image / input_file 等格式）。
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err == nil {
		blocks := make([]provider.ContentBlock, 0, len(parts))
		for _, part := range parts {
			var meta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(part, &meta); err != nil {
				continue
			}
			switch meta.Type {
			case "input_text", "output_text", "text":
				blocks = append(blocks, provider.ContentBlock{Type: "text", Text: meta.Text})
			case "input_image", "image_url":
				// 归一化 image_url 的字符串/对象两种形态并填充 ImageURL/Source，
				// 否则只带 Raw 的块会被 DeepV 的 convertImagePart 静默丢弃。
				if blk := imageBlockFromPart(part); blk != nil {
					blocks = append(blocks, *blk)
				} else {
					blocks = append(blocks, provider.ContentBlock{Type: "image_url", Raw: append([]byte(nil), part...)})
				}
			case "input_file", "file", "file_url":
				blocks = append(blocks, provider.ContentBlock{Type: "file", Raw: append([]byte(nil), part...)})
			case "input_audio", "audio":
				blocks = append(blocks, provider.ContentBlock{Type: "input_audio", Raw: append([]byte(nil), part...)})
			default:
				// Unknown blocks remain visible to capability validation and can be
				// handled by a provider that understands the original shape.
				blocks = append(blocks, provider.ContentBlock{Type: meta.Type, Raw: append([]byte(nil), part...)})
			}
		}
		if len(blocks) > 0 {
			return provider.NewBlocksContent(blocks)
		}
	}

	return provider.NewStringContent(string(content))
}

// systemTextFromItem 判断 Responses input 条目是否是 system/developer 角色的
// 纯文本消息；是则返回其文本内容（调用方合并进顶层 system 字段），否则返回空串。
// 仅接受纯文本：system 消息携带图片/工具块等非文本内容时仍按普通消息走原路径，
// 避免把多模态内容压扁丢失。
func systemTextFromItem(role string, content provider.MessageContent) string {
	if role != "system" && role != "developer" {
		return ""
	}
	if blocks := content.Blocks(); len(blocks) > 0 {
		for _, b := range blocks {
			if b.Type != "text" {
				return ""
			}
		}
	}
	return content.String()
}

// normalizeRole 将 Responses API 的 role 映射到 Anthropic 格式
func normalizeRole(role string) string {
	switch role {
	case "developer", "system":
		return "user" // Anthropic 没有 system role，用 system 字段代替
	case "assistant":
		return "assistant"
	case "user", "tool":
		return "user"
	default:
		return role
	}
}

// mergeAssistantMessages 合并连续的 assistant 消息（多个 function_call 场景）
// prependThinkingBlock 把思维链作为第一个 content block 前置，保证
// reasoning 排在同一轮其他 block 之前（上游按顺序校验）。
func prependThinkingBlock(content provider.MessageContent, reasoning string) provider.MessageContent {
	blocks := make([]provider.ContentBlock, 0, len(content.Blocks())+1)
	blocks = append(blocks, provider.ContentBlock{Type: "thinking", Thinking: reasoning})
	blocks = append(blocks, content.Blocks()...)
	return provider.NewBlocksContent(blocks)
}

// reasoningTextFromItem 从 Responses reasoning item 中取思维链文本。
// 字段名各家不一：标准是 summary[].text，DeepSeek 用 reasoning_text，
// 部分兼容实现直接把文本放在 content 里。
func reasoningTextFromItem(item inputItem) string {
	var parts []string
	for _, s := range item.Summary {
		if s.Text != "" {
			parts = append(parts, s.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	if item.ReasoningText != "" {
		return item.ReasoningText
	}
	if item.ReasoningContent != "" {
		return item.ReasoningContent
	}
	if len(item.Content) > 0 {
		var asString string
		if err := json.Unmarshal(item.Content, &asString); err == nil && asString != "" {
			return asString
		}
		var blocks []provider.ContentBlock
		if err := json.Unmarshal(item.Content, &blocks); err == nil {
			for _, b := range blocks {
				if (b.Type == "reasoning_text" || b.Type == "text" || b.Type == "summary_text") && b.Text != "" {
					parts = append(parts, b.Text)
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func mergeAssistantMessages(msgs []provider.Message) []provider.Message {
	if len(msgs) <= 1 {
		return msgs
	}
	var result []provider.Message
	i := 0
	for i < len(msgs) {
		if msgs[i].Role == "assistant" && i+1 < len(msgs) && msgs[i+1].Role == "assistant" {
			// 合并：收集连续的 assistant 消息的所有 blocks
			var allBlocks []provider.ContentBlock
			j := i
			for j < len(msgs) && msgs[j].Role == "assistant" {
				allBlocks = append(allBlocks, msgs[j].Content.Blocks()...)
				j++
			}
			result = append(result, provider.Message{
				Role:    "assistant",
				Content: provider.NewBlocksContent(allBlocks),
			})
			i = j
		} else {
			result = append(result, msgs[i])
			i++
		}
	}
	return result
}

// ─── Anthropic → Responses 格式转换 ──────────────────────────────────────────

func providerToResponsesResponse(resp *provider.Response, model string) *responsesResponse {
	now := time.Now().Unix()
	output := make([]outputItem, 0)
	outputIndex := 0

	// 收集文本内容块
	var textBlocks []provider.ContentBlock
	var toolBlocks []provider.ContentBlock

	for _, block := range resp.Content {
		if block.Type == "text" {
			textBlocks = append(textBlocks, block)
		} else if block.Type == "tool_use" {
			toolBlocks = append(toolBlocks, block)
		}
	}

	// 如果有文本内容，生成一个 message output item
	if len(textBlocks) > 0 {
		var text string
		for _, b := range textBlocks {
			text += b.Text
		}
		output = append(output, outputItem{
			ID:     fmt.Sprintf("resp_msg_%d", outputIndex),
			Type:   "message",
			Status: "completed",
			Role:   "assistant",
			Content: []outputContent{
				{
					Type: "output_text",
					Text: text,
				},
			},
		})
		outputIndex++
	}

	// 每个 tool_use 生成一个 function_call output item
	for _, block := range toolBlocks {
		arguments := "{}"
		if len(block.Input) > 0 {
			arguments = string(block.Input)
		}
		output = append(output, outputItem{
			ID:        fmt.Sprintf("fc_%s", block.ID),
			Type:      "function_call",
			Status:    "completed",
			CallID:    block.ID,
			Name:      block.Name,
			Arguments: arguments,
		})
		outputIndex++
	}

	// 如果没有任何输出，返回空 message
	if len(output) == 0 {
		output = append(output, outputItem{
			ID:     "resp_msg_0",
			Type:   "message",
			Status: "completed",
			Role:   "assistant",
			Content: []outputContent{
				{Type: "output_text", Text: ""},
			},
		})
	}

	status := "completed"
	if resp.StopReason == "max_tokens" {
		status = "incomplete"
	}

	promptTokens := totalPromptTokens(resp)
	usage := responsesUsage{
		InputTokens:  promptTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  promptTokens + resp.Usage.OutputTokens,
	}
	if resp.CacheUsageKnown {
		usage.InputTokensDetails = &responsesInputTokensDetails{CachedTokens: resp.CacheReadInputTokens}
	}
	return &responsesResponse{
		ID:        resp.ID,
		Object:    "response",
		CreatedAt: now,
		Status:    status,
		Model:     model,
		Output:    output,
		Usage:     usage,
	}
}

// ─── ChatGPT 透传路径 ──────────────────────────────────────────────────────

// tryChatGPTPassthrough 检查请求是否应该走 ChatGPT 透传路径
// ChatGPT 的 Codex API 直接返回 Responses API SSE 格式，不需要 Anthropic 转换
func (h *responsesHandler) tryChatGPTPassthrough(c *gin.Context, req *responsesRequest, rawBody []byte) bool {
	if !req.Stream {
		return h.tryChatGPTNonStreamPassthrough(c, req, rawBody)
	}
	providerReq, err := responsesToProviderRequest(req)
	if err != nil {
		return false
	}
	providerChain, err := h.router.RouteForStreamRequest(req.Model, providerReq)
	if err != nil {
		return false
	}

	var sink *archiveSink
	if h.archiver != nil && h.archiver.Enabled() {
		sink = newArchiveSink(h.archiver.MaxBodyBytes())
	}

	for _, p := range providerChain {
		if chatgptPassthrough, ok := p.(provider.ChatGPTPassthroughMarker); ok {
			if h.router.ProviderAttempted(c.Request.Context(), req.Model, p) || !h.router.AllowProviderRequestFor(req.Model, p) {
				continue
			}
			started := time.Now()
			h.logger.Printf("[RESPONSES] Using ChatGPT passthrough for model %s", req.Model)
			streamWriterBase := newDeferredStreamWriter(c)
			var streamWriter io.Writer = streamWriterBase
			if sink != nil {
				streamWriter = newTeeStreamWriter(streamWriterBase, sink)
			}
			usageWriter := newUsageTrackingWriter(streamWriter, c)

			// 直接透传原始请求体到 ChatGPT
			if err := runGuardedStream(c, h.router, providerReq, p, usageWriter, func(ctx context.Context, _ *provider.Request, w io.Writer) error {
				return chatgptPassthrough.ForwardRawResponsesStream(ctx, json.RawMessage(rawBody), w)
			}); err != nil {
				usageWriter.Finish()
				recordProviderAttempt(c, p.Name(), started, err)
				h.router.RecordProviderFailureFor(req.Model, p, err)
				h.router.MarkProviderAttempted(c.Request.Context(), req.Model, p)
				c.Set("native_passthrough_error", err)
				h.logger.Printf("[RESPONSES] ChatGPT passthrough failed: %s", provider.RedactedErrorText(err))
				if !c.Writer.Written() && provider.ShouldFallback(err) {
					continue
				}
				if c.Writer.Written() {
					_ = writeResponsesStreamError(c.Writer, err)
				} else {
					setProviderErrorHeaders(c, err)
					c.JSON(routingErrorStatus(err), gin.H{"error": gin.H{
						"message": err.Error(),
						"type":    "server_error",
					}})
				}
				if sink != nil {
					sinkBody := sink.Bytes()
					if len(sinkBody) == 0 {
						submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody, nil,
							archive.StatusError, routingErrorStatus(err), archiveErrorReason(err))
					} else {
						status, reason := parseStreamEndState(sinkBody, err)
						submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody, sinkBody,
							status, c.Writer.Status(), reason)
					}
				}
				return true
			}
			usageWriter.Finish()
			recordProviderAttempt(c, p.Name(), started, nil)
			h.router.RecordProviderSuccessFor(req.Model, p)
			streamWriterBase.Commit()
			if sink != nil {
				status, reason := parseStreamEndState(sink.Bytes(), nil)
				submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody, sink.Bytes(),
					status, c.Writer.Status(), reason)
			}
			return true
		}
		return false
	}

	return false
}

// tryChatGPTNonStreamPassthrough supports clients that use the standard
// non-streaming Responses request, including provider connection tests. The
// ChatGPT Codex endpoint is stream-only, so collect its native SSE response
// before returning a normal JSON Responses object.
func (h *responsesHandler) tryChatGPTNonStreamPassthrough(c *gin.Context, req *responsesRequest, rawBody []byte) bool {
	providerReq, err := responsesToProviderRequest(req)
	if err != nil {
		return false
	}
	providerChain, err := h.router.RouteForRequest(req.Model, providerReq)
	if err != nil {
		return false
	}

	for _, p := range providerChain {
		chatgptPassthrough, ok := p.(provider.ChatGPTPassthroughMarker)
		if !ok {
			return false
		}
		if h.router.ProviderAttempted(c.Request.Context(), req.Model, p) || !h.router.AllowProviderRequestFor(req.Model, p) {
			continue
		}

		started := time.Now()
		var upstream bytes.Buffer
		var attemptTiming provider.StreamTiming
		err := h.router.RunNonStreamAttempt(c.Request.Context(), p.Name(), func(ctx context.Context) error {
			var err error
			attemptTiming, err = runGuardedStreamTimed(ctx, h.router, providerReq, p, &upstream, func(attemptCtx context.Context, _ *provider.Request, w io.Writer) error {
				return chatgptPassthrough.ForwardRawResponsesStream(attemptCtx, json.RawMessage(rawBody), w)
			})
			return err
		})
		c.Set("attempt_stream_timing", attemptTiming)
		if err == nil {
			var responseBody []byte
			var inputTokens, outputTokens int
			responseBody, inputTokens, outputTokens, err = collectNativeResponsesResponse(upstream.Bytes(), req.Model)
			if err == nil {
				recordProviderAttempt(c, p.Name(), started, nil)
				h.router.RecordProviderSuccessFor(req.Model, p)
				setUsageMetadata(c, inputTokens, outputTokens)
				var decoded any
				if json.Unmarshal(responseBody, &decoded) == nil {
					if usage, found := findUsageDetails(decoded); found && usage.CacheUsageKnown {
						setCacheUsageMetadata(c, usage.CacheReadInputTokens, usage.CacheCreationInputTokens, usage.CacheInputTokens, true)
					}
				}
				if h.archiver != nil && h.archiver.Enabled() {
					submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody,
						responseBody, archive.StatusCompleted, http.StatusOK, "")
				}
				c.Data(http.StatusOK, "application/json", responseBody)
				return true
			}
		}

		recordProviderAttempt(c, p.Name(), started, err)
		h.router.RecordProviderFailureFor(req.Model, p, err)
		h.router.MarkProviderAttempted(c.Request.Context(), req.Model, p)
		c.Set("native_passthrough_error", err)
		h.logger.Printf("[RESPONSES] ChatGPT non-stream passthrough failed: %s", provider.RedactedErrorText(err))
		if provider.ShouldFallback(err) {
			continue
		}
		setProviderErrorHeaders(c, err)
		if h.archiver != nil && h.archiver.Enabled() {
			submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody, nil,
				archive.StatusError, routingErrorStatus(err), archiveErrorReason(err))
		}
		c.JSON(routingErrorStatus(err), gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "server_error",
		}})
		return true
	}

	return false
}
