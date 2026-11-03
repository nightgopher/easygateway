package provider

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// MessageContent 支持两种格式：
//   - 字符串：{"role":"user","content":"hello"}
//   - 数组：{"role":"user","content":[{"type":"text","text":"hello"}]}
//
// Claude Code 发出的是数组格式，部分测试工具发字符串格式，两种都要支持。
type MessageContent struct {
	str    string
	blocks []ContentBlock
	isStr  bool
}

func (c MessageContent) MarshalJSON() ([]byte, error) {
	if c.isStr {
		return json.Marshal(c.str)
	}
	return json.Marshal(c.blocks)
}

func (c *MessageContent) UnmarshalJSON(data []byte) error {
	// 尝试字符串
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.str = s
		c.isStr = true
		return nil
	}
	// 尝试数组
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	c.blocks = blocks
	c.isStr = false
	return nil
}

// String 返回纯文本内容（合并所有 text block）
func (c MessageContent) String() string {
	if c.isStr {
		return c.str
	}
	var result string
	for _, b := range c.blocks {
		if b.Type == "text" {
			result += b.Text
		}
	}
	return result
}

func NewStringContent(s string) MessageContent {
	return MessageContent{str: s, isStr: true}
}

func NewBlocksContent(blocks []ContentBlock) MessageContent {
	return MessageContent{blocks: blocks}
}

func (c MessageContent) Blocks() []ContentBlock {
	if c.isStr {
		if c.str == "" {
			return nil
		}
		return []ContentBlock{{Type: "text", Text: c.str}}
	}
	return c.blocks
}

// Message 表示一条消息，content 兼容字符串和数组两种格式
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

// Request 是 Anthropic API 请求体。
// 所有字段保留为原始 JSON（raw map），仅解出 model/messages/stream/max_tokens
// 用于路由和分发，其余字段（system数组、thinking、tools、context_management 等）原样透传。
type Request struct {
	Model     string                     // 路由用，可能被改写
	Messages  []Message                  // 路由用
	MaxTokens int                        // 路由用
	Stream    bool                       // 路由用
	raw       map[string]json.RawMessage // 完整原始字段
}

func (r *Request) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.raw = raw
	if v, ok := raw["model"]; ok {
		_ = json.Unmarshal(v, &r.Model)
	}
	if v, ok := raw["max_tokens"]; ok {
		_ = json.Unmarshal(v, &r.MaxTokens)
	}
	if v, ok := raw["stream"]; ok {
		_ = json.Unmarshal(v, &r.Stream)
	}
	if v, ok := raw["messages"]; ok {
		_ = json.Unmarshal(v, &r.Messages)
	}
	return nil
}

// MarshalJSON 透传所有原始字段，但用改写后的 model 覆盖原值
func (r *Request) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(r.raw))
	for k, v := range r.raw {
		out[k] = v
	}
	if b, err := json.Marshal(r.Model); err == nil {
		out["model"] = b
	}
	return json.Marshal(out)
}

func (r *Request) SetRawField(key string, value any) error {
	if r.raw == nil {
		r.raw = make(map[string]json.RawMessage)
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	r.raw[key] = b
	return nil
}

func (r *Request) RawField(key string) (json.RawMessage, bool) {
	if r.raw == nil {
		return nil, false
	}
	v, ok := r.raw[key]
	return v, ok
}

// ContentBlock 是网关内部的内容块。
//
// 这个结构保留了 OpenAI/Anthropic 常见的多模态字段。以前这里只保留
// text/tool_use/tool_result，导致 OpenAI 的 image_url 在入口转换时被静默丢弃。
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// OpenAI 多模态块（image_url、video_url、file、input_audio 等）。
	// 使用 RawMessage 是为了不限制不同兼容服务对字段内部结构的扩展。
	ImageURL   json.RawMessage `json:"image_url,omitempty"`
	VideoURL   json.RawMessage `json:"video_url,omitempty"`
	File       json.RawMessage `json:"file,omitempty"`
	InputAudio json.RawMessage `json:"input_audio,omitempty"`
	Source     json.RawMessage `json:"source,omitempty"` // Anthropic image source

	// tool_use 字段
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result 字段
	ToolUseID     string         `json:"tool_use_id,omitempty"`
	ContentStr    string         `json:"-"` // 当 content 是字符串时
	ContentBlocks []ContentBlock `json:"-"` // 当 content 是数组时
	IsError       bool           `json:"is_error,omitempty"`
	// thinking 字段（MiMo 思维链）
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// Raw 保存入口收到的原始块，供同协议 provider 尽量无损地重新编码。
	// 它不参与默认 JSON 输出，避免把内部元数据泄漏给上游。
	Raw json.RawMessage `json:"-"`
}

func (c ContentBlock) MarshalJSON() ([]byte, error) {
	if len(c.Raw) > 0 {
		return c.Raw, nil
	}
	var content any
	if len(c.ContentBlocks) > 0 {
		content = c.ContentBlocks
	} else if c.ContentStr != "" {
		content = c.ContentStr
	}
	type serializedContentBlock struct {
		Type       string          `json:"type"`
		Text       string          `json:"text,omitempty"`
		ImageURL   json.RawMessage `json:"image_url,omitempty"`
		VideoURL   json.RawMessage `json:"video_url,omitempty"`
		File       json.RawMessage `json:"file,omitempty"`
		InputAudio json.RawMessage `json:"input_audio,omitempty"`
		Source     json.RawMessage `json:"source,omitempty"`
		ID         string          `json:"id,omitempty"`
		Name       string          `json:"name,omitempty"`
		Input      json.RawMessage `json:"input,omitempty"`
		ToolUseID  string          `json:"tool_use_id,omitempty"`
		Content    any             `json:"content,omitempty"`
		IsError    bool            `json:"is_error,omitempty"`
		Thinking   string          `json:"thinking,omitempty"`
		Signature  string          `json:"signature,omitempty"`
	}
	return json.Marshal(serializedContentBlock{
		Type:       c.Type,
		Text:       c.Text,
		ImageURL:   c.ImageURL,
		VideoURL:   c.VideoURL,
		File:       c.File,
		InputAudio: c.InputAudio,
		Source:     c.Source,
		ID:         c.ID,
		Name:       c.Name,
		Input:      c.Input,
		ToolUseID:  c.ToolUseID,
		Content:    content,
		IsError:    c.IsError,
		Thinking:   c.Thinking,
		Signature:  c.Signature,
	})
}

func (c *ContentBlock) UnmarshalJSON(data []byte) error {
	// 用 alias 类型避免递归
	type Alias struct {
		Type       string          `json:"type"`
		Text       string          `json:"text"`
		ImageURL   json.RawMessage `json:"image_url"`
		VideoURL   json.RawMessage `json:"video_url"`
		File       json.RawMessage `json:"file"`
		InputAudio json.RawMessage `json:"input_audio"`
		Source     json.RawMessage `json:"source"`
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		Input      json.RawMessage `json:"input"`
		ToolUseID  string          `json:"tool_use_id"`
		IsError    bool            `json:"is_error"`
		Content    json.RawMessage `json:"content"`
		Thinking   string          `json:"thinking"`
		Signature  string          `json:"signature"`
	}
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	c.Type = a.Type
	c.Text = a.Text
	c.ImageURL = a.ImageURL
	c.VideoURL = a.VideoURL
	c.File = a.File
	c.InputAudio = a.InputAudio
	c.Source = a.Source
	c.ID = a.ID
	c.Name = a.Name
	c.Input = a.Input
	c.ToolUseID = a.ToolUseID
	c.IsError = a.IsError
	c.Thinking = a.Thinking
	c.Signature = a.Signature
	c.Raw = append(c.Raw[:0], data...)

	// 处理 tool_result 的 content 字段（字符串或数组）
	if a.Content != nil {
		var s string
		if err := json.Unmarshal(a.Content, &s); err == nil {
			c.ContentStr = s
		} else {
			var blocks []ContentBlock
			if err := json.Unmarshal(a.Content, &blocks); err == nil {
				c.ContentBlocks = blocks
			}
		}
	}
	return nil
}

// Response 是 Anthropic API 响应体（对齐真实 Anthropic 格式）
type Response struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"` // 数组，非对象
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	// Cache usage remains internal metadata except when serializing the
	// Anthropic-compatible response, where the protocol exposes it in usage.
	CacheReadInputTokens     int  `json:"-"`
	CacheCreationInputTokens int  `json:"-"`
	CacheInputTokens         int  `json:"-"`
	CacheUsageKnown          bool `json:"-"`
}

func (r *Response) UnmarshalJSON(data []byte) error {
	type responseAlias Response
	var decoded responseAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = Response(decoded)

	var usage struct {
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	}
	var payload struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if len(payload.Usage) > 0 {
		if err := json.Unmarshal(payload.Usage, &usage); err != nil {
			return err
		}
	}
	if usage.CacheReadInputTokens != nil {
		r.CacheReadInputTokens = *usage.CacheReadInputTokens
		r.CacheUsageKnown = true
	}
	if usage.CacheCreationInputTokens != nil {
		r.CacheCreationInputTokens = *usage.CacheCreationInputTokens
		r.CacheUsageKnown = true
	}
	if r.CacheUsageKnown {
		r.CacheInputTokens = r.Usage.InputTokens + r.CacheReadInputTokens + r.CacheCreationInputTokens
	}
	return nil
}

func (r Response) MarshalJSON() ([]byte, error) {
	type responseAlias Response
	encoded, err := json.Marshal(responseAlias(r))
	if err != nil {
		return nil, err
	}
	if !r.CacheUsageKnown && r.CacheReadInputTokens == 0 && r.CacheCreationInputTokens == 0 {
		return encoded, nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return nil, err
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(payload["usage"], &usage); err != nil {
		return nil, err
	}
	cacheRead, err := json.Marshal(r.CacheReadInputTokens)
	if err != nil {
		return nil, err
	}
	cacheCreation, err := json.Marshal(r.CacheCreationInputTokens)
	if err != nil {
		return nil, err
	}
	usage["cache_read_input_tokens"] = cacheRead
	usage["cache_creation_input_tokens"] = cacheCreation
	payload["usage"], err = json.Marshal(usage)
	if err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}

// ErrorResponse 错误响应结构
type ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// Provider 定义提供商的接口
type Provider interface {
	Name() string
	URL() string
	APIKey() string
	UseBearer() bool
	ForwardRequest(ctx context.Context, req *Request) (*Response, error)
	IsHealthy(ctx context.Context) bool
}

// StreamProvider 是可选接口：提供商自行实现流式逻辑（例如需要格式转换的 OpenAI 兼容接口）
// 若提供商未实现此接口，handler 将直接透传上游 SSE 流。
type StreamProvider interface {
	Provider
	ForwardStream(ctx context.Context, req *Request, w io.Writer) error
}

// BoundModelProvider 是可选接口，实现该接口的 Provider 表示它固定处理特定的模型 ID。
// 用于需要动态绑定模型名的场景（如 Copilot 等）。
type BoundModelProvider interface {
	Provider
	BoundModel() string
}

// FirstOutputOverrideProvider 是可选接口：provider 借它给出 providers.yaml 里
// 的 per-provider 首字超时覆盖（0 表示该档沿用全局路由策略）。路由层优先取
// 覆盖值，并在覆盖生效时把本请求的共享等待预算抬升到至少该值，避免被默认
// fallback 预算钳回。
type FirstOutputOverrideProvider interface {
	Provider
	FirstOutputTimeoutOverride() (plain, reasoning time.Duration)
}

// Config 提供商配置
type Config struct {
	Name           string
	URL            string
	APIKey         string
	UseBearer      bool
	RequestTimeout time.Duration

	// FirstOutputTimeout / ReasoningFirstOutputTimeout 是 per-provider 首字超时
	// 覆盖，来自 providers.yaml（0 表示沿用全局路由策略）。思考请求走 Reasoning
	// 档，其余走 FirstOutput 档，语义与 ROUTING_*_FIRST_OUTPUT_SECONDS 一致。
	FirstOutputTimeout          time.Duration
	ReasoningFirstOutputTimeout time.Duration

	// Auth 为非空时取代 APIKey：每次请求从它取动态令牌，401/403 时调用其
	// Refresh 续期并重试一次。账号渠道（internal/channel）用它注入 OAuth token。
	Auth AuthSource
	// Some account services use 403 for authorization rather than expiry.
	RefreshUnauthorizedOnly bool
	// ExtraHeaders 是渠道特有的固定请求头（例如复刻官方客户端的 X-Title）。
	// 在每个请求上设置，值非空才写入。
	ExtraHeaders map[string]string
	// DynamicHeaders 是按请求动态求值的请求头，与 ExtraHeaders 互补：ExtraHeaders
	// 在构造 Config 时就固定，而这里每次请求都重新调用（例如把当前账号的令牌再写
	// 进一个上游要求的自定义头）。返回 nil 表示不写；同名头在 ExtraHeaders 之后
	// 应用，因此可覆盖它。实现必须并发安全。
	DynamicHeaders func() map[string]string
	// Transform 在请求体序列化后、发送前做最后一次改写（例如按模型补 thinking）。
	// 为 nil 时原样发送。
	Transform RequestTransform
}
