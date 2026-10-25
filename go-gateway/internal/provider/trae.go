package provider

// Trae provider：对接字节 TRAE 的 SOLO 端点
// （POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat）。
//
// 该端点不是公开 API：请求体是 SOLO 专有形态（function / config_name / messages，
// assistant 的工具调用放在 tool_calls 并保留 function 对象），鉴权是
// `Authorization: Cloud-IDE-JWT <token>`（不是 Bearer，applyAuth 表达不了），响应是
// 私有 SSE（event: output / token_usage / done / error，帧以空行分隔）。
//
// 与 CloudCodeProvider 一样，上游协议细节全部收敛在这里：对外仍是标准的
// Anthropic 请求 / Anthropic SSE，handler 层无需感知。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	traeEndpoint = "https://trae-api-cn.mchost.guru"
	traeChatPath = "/api/agent/v3/llm_utils_chat"

	// traeModelPrefix 是对外模型名的命名空间前缀，只用于避开与其他供应商的模型 ID
	// 冲突（trae 的 glm-5.2 / kimi-k3 / minimax-m3 等与既有 provider 大量重名），
	// 发往上游前会被剥掉。
	traeModelPrefix = "trae/"

	// traeDefaultModel 是官方默认模型（TRAE_DEFAULT_MODEL）。
	traeDefaultModel = "glm-5.2"
	// traeDefaultFunction 是 SOLO 请求体的 function 通道（TRAE_FUNCTION）。
	traeDefaultFunction = "solo_work_lite"
	// traeMaxCompletionTokens 是 max_tokens 的硬上限（TRAE_DEFAULT_MAX_COMPLETION_TOKENS）。
	traeMaxCompletionTokens = 64000

	traeAgentHost = "https://trae-api-cn.mchost.guru"
	traeUserAgent = "Trae/0.1.52"

	traeIDEVersion     = "0.1.52"
	traeIDEVersionCode = "20260811"
	traeAppVersion     = "default"
	traeOSVersion      = "macOS 15.7.4"
	traeDeviceBrand    = "Apple"
)

// traeIdentitySource 是可选接口：AuthSource 可额外提供依赖账号凭据的身份头
// （Authorization / X-Cloudide-Token / X-Ide-Token / X-Uid / X-Machine-Id /
// X-Device-Id / X-App-Id）。provider 只认 AuthSource，拿不到 uid 等字段，故由
// channel 侧注入。未实现时只发静态头，上游会拒绝——这是未登录的自然结果。
type traeIdentitySource interface {
	TraeIdentityHeaders() map[string]string
}

// TraeProvider 绑定一个模型 ID，向上游发 SOLO 请求。
type TraeProvider struct {
	config *Config
	client *http.Client
	// endpoint 可覆盖，测试时指向本地假上游。
	endpoint string

	// mu 保护身份头的缓存（仅在 AuthSource 不提供 TraeIdentityHeaders 时用）。
	mu sync.RWMutex
}

// NewTraeProvider 构造 Trae provider。Config.URL 非空时作为端点基址，便于测试指向
// 本地假上游；生产留空即用生产端点。
func NewTraeProvider(config *Config) *TraeProvider {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = config.RequestTimeout
	if config.RequestTimeout <= 0 {
		transport.ResponseHeaderTimeout = defaultRequestTimeout
	}
	endpoint := traeEndpoint
	if config.URL != "" {
		endpoint = strings.TrimRight(config.URL, "/")
	}
	return &TraeProvider{
		config:   config,
		client:   &http.Client{Transport: transport},
		endpoint: endpoint,
	}
}

func (p *TraeProvider) Name() string   { return p.config.Name }
func (p *TraeProvider) URL() string    { return p.endpoint + traeChatPath }
func (p *TraeProvider) APIKey() string { return p.config.APIKey }

// UseBearer 这里没有实际意义：Trae 的鉴权头由 setHeaders 直接写入（Cloud-IDE-JWT），
// 不走 applyAuth 的 Bearer/x-api-key 二选一，返回 true 只为声明「非 x-api-key 方案」。
func (p *TraeProvider) UseBearer() bool { return true }

// Available 报告当前是否已有凭据；未登录时路由层直接跳过。
func (p *TraeProvider) Available() bool { return p.config.authAvailable() }

// IsHealthy 只反映本地配置：可用性判断交给 Available 与 Probe。
func (p *TraeProvider) IsHealthy(ctx context.Context) bool { return true }

// Probe 用绑定模型探测。
func (p *TraeProvider) Probe(ctx context.Context) ProbeResult {
	return p.ProbeModel(ctx, p.config.Name)
}

// ProbeModel 用指定模型发一个最小请求。
func (p *TraeProvider) ProbeModel(ctx context.Context, model string) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return probeVia(ctx, model, 0, p.ForwardRequest)
}

// ForwardRequest 发一次非流式请求：内部仍走流式端点，把私有 SSE 累积成一个完整响应。
func (p *TraeProvider) ForwardRequest(ctx context.Context, req *Request) (*Response, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := p.send(streamCtx, req, cancel)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var (
		blocks     []ContentBlock
		textBuf    strings.Builder
		thinkBuf   strings.Builder
		toolBlocks = map[int]int{} // wire index → blocks 下标
		toolOrder  []int
		stopReason string
		usageIn    int
		usageOut   int
	)

	flushText := func() {
		if textBuf.Len() > 0 {
			blocks = append(blocks, ContentBlock{Type: "text", Text: textBuf.String()})
			textBuf.Reset()
		}
	}
	flushThink := func() {
		if thinkBuf.Len() > 0 {
			blocks = append(blocks, ContentBlock{Type: "thinking", Thinking: thinkBuf.String()})
			thinkBuf.Reset()
		}
	}

	em := traeEmitter{
		onThinking: func(s string) { flushText(); thinkBuf.WriteString(s) },
		onText:     func(s string) { flushThink(); textBuf.WriteString(s) },
		onToolStart: func(wireIndex int, id, name string) {
			flushText()
			flushThink()
			if _, ok := toolBlocks[wireIndex]; ok {
				return
			}
			toolBlocks[wireIndex] = len(blocks)
			toolOrder = append(toolOrder, wireIndex)
			blocks = append(blocks, ContentBlock{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(`{}`)})
		},
		onToolDelta: func(wireIndex int, args string) {
			slot, ok := toolBlocks[wireIndex]
			if !ok || args == "" {
				return
			}
			blocks[slot].Input = appendTraeToolArgs(blocks[slot].Input, args)
		},
		onFinish: func(reason string) { stopReason = reason },
		onUsage:  func(input, output int) { usageIn, usageOut = input, output },
	}
	if err := consumeTraeSSE(resp.Body, em); err != nil {
		return nil, err
	}
	flushText()
	flushThink()

	if len(blocks) == 0 {
		blocks = []ContentBlock{{Type: "text", Text: ""}}
	}
	stopReason = traeStopReason(stopReason, len(toolBlocks) > 0)

	out := &Response{
		ID:         "msg_trae_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Type:       "message",
		Role:       "assistant",
		Model:      req.Model,
		StopReason: stopReason,
		Content:    blocks,
	}
	out.Usage.InputTokens = usageIn
	out.Usage.OutputTokens = usageOut
	return out, nil
}

// ForwardStream 把 Anthropic 请求翻成 SOLO 请求发出，把私有 SSE 翻回 Anthropic SSE 写入 w。
func (p *TraeProvider) ForwardStream(ctx context.Context, req *Request, w io.Writer) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := p.send(streamCtx, req, cancel)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	msgID := "msg_trae_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	writeSSE(w, "message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		msgID, req.Model,
	))
	writeSSE(w, "ping", `{"type":"ping"}`)

	nextIdx := 0
	thinkingIdx := -1
	textIdx := -1
	type toolState struct {
		idx     int
		id      string
		name    string
		started bool
	}
	toolStates := map[int]*toolState{}
	var toolOrder []int
	stopReason := ""
	usageIn, usageOut := 0, 0

	closeThinking := func() {
		if thinkingIdx >= 0 {
			writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, thinkingIdx))
			thinkingIdx = -1
		}
	}
	closeText := func() {
		if textIdx >= 0 {
			writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, textIdx))
			textIdx = -1
		}
	}

	em := traeEmitter{
		onThinking: func(s string) {
			closeText()
			if thinkingIdx < 0 {
				thinkingIdx = nextIdx
				nextIdx++
				writeSSE(w, "content_block_start", fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, thinkingIdx))
			}
			writeSSE(w, "content_block_delta", fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":%s}}`, thinkingIdx, jsonString(s)))
		},
		onText: func(s string) {
			closeThinking()
			if textIdx < 0 {
				textIdx = nextIdx
				nextIdx++
				writeSSE(w, "content_block_start", fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, textIdx))
			}
			writeSSE(w, "content_block_delta", fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, textIdx, jsonString(s)))
		},
		onToolStart: func(wireIndex int, id, name string) {
			if state, ok := toolStates[wireIndex]; ok {
				if id != "" {
					state.id = id
				}
				if name != "" {
					state.name = name
				}
			} else {
				toolStates[wireIndex] = &toolState{id: id, name: name, idx: -1}
				toolOrder = append(toolOrder, wireIndex)
			}
		},
		onToolDelta: func(wireIndex int, args string) {
			state := toolStates[wireIndex]
			if state == nil {
				state = &toolState{idx: -1}
				toolStates[wireIndex] = state
				toolOrder = append(toolOrder, wireIndex)
			}
			if !state.started {
				if state.name == "" {
					return // 还没拿到函数名，先丢弃（官方同样只在有可用名后才宣告）
				}
				closeText()
				closeThinking()
				state.started = true
				state.idx = nextIdx
				nextIdx++
				id := state.id
				if id == "" {
					id = "call_" + strconv.Itoa(wireIndex)
				}
				writeSSE(w, "content_block_start", fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`,
					state.idx, jsonString(id), jsonString(state.name)))
			}
			if args != "" {
				writeSSE(w, "content_block_delta", fmt.Sprintf(
					`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
					state.idx, jsonString(args)))
			}
		},
		onFinish: func(reason string) { stopReason = reason },
		onUsage:  func(input, output int) { usageIn, usageOut = input, output },
	}
	if err := consumeTraeSSE(resp.Body, em); err != nil {
		return err
	}

	closeThinking()
	closeText()
	for _, wireIndex := range toolOrder {
		state := toolStates[wireIndex]
		if state != nil && state.started {
			writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.idx))
		}
	}

	reason := traeStopReason(stopReason, len(toolStates) > 0)
	writeSSE(w, "message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		reason, usageIn, usageOut))
	writeSSE(w, "message_stop", `{"type":"message_stop"}`)
	return nil
}

// traeStopReason 把上游 finish_reason 映射成 Anthropic stop_reason；缺失时按是否有
// 工具调用推断（有工具调用 → tool_use）。
func traeStopReason(reason string, hasTools bool) string {
	if reason == "" {
		if hasTools {
			return "tool_use"
		}
		return "end_turn"
	}
	return mapTraeStopReason(reason)
}

// mapTraeStopReason 复刻官方的 finishReason 判定。
func mapTraeStopReason(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// appendTraeToolArgs 把一段增量参数拼到已有的 JSON 参数上。
func appendTraeToolArgs(existing json.RawMessage, delta string) json.RawMessage {
	trimmed := strings.TrimSpace(string(existing))
	if trimmed == "" || trimmed == "{}" {
		return json.RawMessage(delta)
	}
	return json.RawMessage(trimmed + delta)
}

// send 翻译请求、注入鉴权并发出，返回已确认 2xx 的响应。非 2xx 统一转成
// *ProviderError（复用网关既有的上游错误处理）。
func (p *TraeProvider) send(ctx context.Context, req *Request, cancel context.CancelFunc) (*http.Response, error) {
	// 未登录直接短路：路由层通常已按 Available 跳过，这里兜住直接调用 provider 的路径。
	if p.config.Auth != nil && !p.config.authAvailable() {
		return nil, &ProviderError{Provider: p.Name(), StatusCode: http.StatusUnauthorized, Message: "channel has no credential; login required"}
	}
	body, err := buildTraeBody(req)
	if err != nil {
		return nil, err
	}
	body, err = p.config.applyTransform(body)
	if err != nil {
		return nil, fmt.Errorf("transform trae request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL(), strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build trae request: %w", err)
	}
	usedToken := ""
	if p.config.Auth != nil {
		usedToken = p.config.Auth.BearerToken()
	}
	p.setHeaders(httpReq)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("trae request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// 401/403 时用账号池续期一次并重试，与 CloudCodeProvider 的语义一致。
		if p.config.refreshAuth(ctx, resp.StatusCode) {
			return p.sendOnce(ctx, body)
		}
		// 429 先做渠道内账号轮转（sendOnce 只重试一次，轮转失败则原 429
		// 返回给路由层走 provider 级配额熔断）。usedToken 是本次请求实际
		// 用的令牌，避免并发轮转后把冷却记到别的账号头上。
		if resp.StatusCode == http.StatusTooManyRequests &&
			p.config.rotateOnRateLimit(ctx, usedToken, resp.Header.Get("Retry-After")) {
			return p.sendOnce(ctx, body)
		}
		if resp.StatusCode == http.StatusUnauthorized && p.config.Auth != nil && !p.config.authAvailable() {
			return nil, &ProviderError{Provider: p.Name(), StatusCode: http.StatusUnauthorized, Message: "channel has no credential; login required"}
		}
		return nil, NewHTTPError(p.Name(), resp, b)
	}
	return resp, nil
}

// sendOnce 用已序列化的 body 重发一次（续期后的重试）。
func (p *TraeProvider) sendOnce(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL(), strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build trae retry request: %w", err)
	}
	p.setHeaders(httpReq)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("trae retry request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, NewHTTPError(p.Name(), resp, b)
	}
	return resp, nil
}

// setHeaders 复刻官方客户端的请求头。
//
// 鉴权头（Authorization: Cloud-IDE-JWT、X-Cloudide-Token、X-Uid…）来自渠道侧的
// traeIdentitySource：它们依赖当前账号的凭据字段，provider 拿不到。这里刻意不调用
// applyAuth——它只能产出 Bearer / x-api-key，表达不了 Cloud-IDE-JWT 方案。
func (p *TraeProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", traeUserAgent)
	req.Header.Set("X-App-Version", traeAppVersion)
	req.Header.Set("X-Ide-Version", traeIDEVersion)
	req.Header.Set("X-Ide-Version-Code", traeIDEVersionCode)
	req.Header.Set("X-App-Version-Code", traeIDEVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "macos")
	req.Header.Set("X-OS-Version", traeOSVersion)
	req.Header.Set("X-Device-Brand", traeDeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")

	if src, ok := p.config.Auth.(traeIdentitySource); ok {
		for name, value := range src.TraeIdentityHeaders() {
			if value != "" {
				req.Header.Set(name, value)
			}
		}
	}
}

// ─── 请求翻译 ────────────────────────────────────────────────────────────────

// buildTraeBody 把 Anthropic 请求翻译成 SOLO 请求体。
func buildTraeBody(req *Request) ([]byte, error) {
	messages, err := traeWireMessages(req)
	if err != nil {
		return nil, err
	}
	model := traeModelName(req.Model)
	body := map[string]any{
		"config_name": model,
		"model":       model,
		"stream":      true,
		"function":    traeDefaultFunction,
		"messages":    messages,
	}

	if req.MaxTokens > 0 {
		body["max_tokens"] = clampTraeMaxTokens(req.MaxTokens)
	}
	if raw, ok := req.raw["temperature"]; ok {
		var temp float64
		if json.Unmarshal(raw, &temp) == nil {
			body["temperature"] = temp
		}
	}
	if raw, ok := req.raw["stop_sequences"]; ok {
		var stop []string
		if json.Unmarshal(raw, &stop) == nil && len(stop) > 0 {
			body["stop"] = stop
		}
	}
	if tools := traeTools(req); len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := traeToolChoice(req); choice != nil {
		body["tool_choice"] = choice
	}
	return json.Marshal(body)
}

// traeModelName 剥掉 `trae/` 前缀并回退到默认模型。
func traeModelName(modelID string) string {
	name := strings.TrimPrefix(modelID, traeModelPrefix)
	// 上游的 config_name 不带厂商后缀（`model__xxx` 取前半段）。
	if idx := strings.Index(name, "__"); idx > 0 {
		name = name[:idx]
	}
	if name == "" {
		return traeDefaultModel
	}
	return name
}

// clampTraeMaxTokens 把 max_tokens 限制在官方硬上限内。
func clampTraeMaxTokens(value int) int {
	if value <= 0 {
		return value
	}
	if value > traeMaxCompletionTokens {
		return traeMaxCompletionTokens
	}
	return value
}

// traeWireMessages 把 Anthropic messages 转成 SOLO 的 messages 形态：
// system → {role:system,content:"..."}；assistant 的工具调用放进 tool_calls；
// tool_result 拆成独立的 {role:tool,tool_call_id,content}。
func traeWireMessages(req *Request) ([]any, error) {
	var out []any
	if sys := anthropicSystemText(req); sys != "" {
		out = append(out, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue
		}
		blocks := m.Content.Blocks()
		switch m.Role {
		case "assistant":
			text, toolCalls := traeAssistantParts(blocks)
			msg := map[string]any{"role": "assistant"}
			if text == "" && len(toolCalls) > 0 {
				msg["content"] = nil
			} else {
				msg["content"] = text
			}
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
			}
			if text == "" && len(toolCalls) == 0 {
				continue
			}
			out = append(out, msg)
		default:
			out = append(out, traeUserParts(blocks)...)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("trae: 请求没有可用的消息内容")
	}
	return out, nil
}

// traeAssistantParts 拆出 assistant 的文本与工具调用。
func traeAssistantParts(blocks []ContentBlock) (string, []any) {
	var text strings.Builder
	var toolCalls []any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(normalizeToolArgs(b.Input))
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		}
	}
	return text.String(), toolCalls
}

// traeUserParts 处理 user 消息：文本/图片组成一条 user 消息，tool_result 拆成
// 独立的 tool 消息。为了保持顺序，tool_result 之前的文本先单独成一条 user 消息。
func traeUserParts(blocks []ContentBlock) []any {
	var out []any
	var pending []any
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "user", "content": pending})
		pending = nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				pending = append(pending, map[string]any{"type": "text", "text": b.Text})
			}
		case "image":
			if part := traeImagePart(b); part != nil {
				pending = append(pending, part)
			}
		case "tool_result":
			flushPending()
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      traeToolResultText(b),
			})
		}
	}
	if len(pending) > 0 {
		// 单段文本用字符串形态（官方 transformSOLOMessage 同样两种都接受）。
		if len(pending) == 1 {
			if only, ok := pending[0].(map[string]any); ok && only["type"] == "text" {
				out = append(out, map[string]any{"role": "user", "content": only["text"]})
				return out
			}
		}
		out = append(out, map[string]any{"role": "user", "content": pending})
	}
	return out
}

// traeImagePart 把 Anthropic image 块转成 SOLO 的 image_url 部件。
func traeImagePart(b ContentBlock) map[string]any {
	if len(b.Source) == 0 {
		return nil
	}
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(b.Source, &source) != nil {
		return nil
	}
	url := source.URL
	if url == "" && source.Data != "" {
		mime := source.MediaType
		if mime == "" {
			mime = "image/png"
		}
		url = "data:" + mime + ";base64," + source.Data
	}
	if url == "" {
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// traeToolResultText 汇总 tool_result 的文本内容。
func traeToolResultText(b ContentBlock) string {
	if b.ContentStr != "" {
		return b.ContentStr
	}
	var parts []string
	for _, inner := range b.ContentBlocks {
		if inner.Type == "text" {
			parts = append(parts, inner.Text)
		}
	}
	text := strings.Join(parts, "")
	if text == "" {
		return "(no output)"
	}
	return text
}

// traeTools 把 Anthropic tools 转成 SOLO 的 functions 形态（parameters 序列化成字符串）。
func traeTools(req *Request) []any {
	raw, ok := req.raw["tools"]
	if !ok {
		return nil
	}
	var tools []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	var out []any
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.InputSchema) > 0 {
			// SOLO 要求 parameters 是 JSON 字符串而不是对象。
			fn["parameters"] = string(t.InputSchema)
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// traeToolChoice 把 Anthropic tool_choice 归一化成 SOLO 接受的形态：
// auto/required 直接透传，指定工具时压成函数名字符串，none 表示不带工具。
func traeToolChoice(req *Request) any {
	raw, ok := req.raw["tool_choice"]
	if !ok {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(choice.Type)) {
	case "auto":
		return "auto"
	case "any", "required":
		return "required"
	case "none":
		return "none"
	case "tool":
		name := strings.TrimSpace(choice.Name)
		if name == "" {
			return "auto"
		}
		return name
	default:
		return nil
	}
}

// ─── SSE 消费 ───────────────────────────────────────────────────────────────

// traeEmitter 是私有 SSE 事件的回调集合。wireIndex 是上游 tool_calls 里的 index。
type traeEmitter struct {
	onThinking  func(string)
	onText      func(string)
	onToolStart func(wireIndex int, id, name string)
	onToolDelta func(wireIndex int, args string)
	onFinish    func(string)
	onUsage     func(input, output int)
}

// consumeTraeSSE 逐帧读取 Trae 私有 SSE。
//
// 帧格式与 OpenAI 不同：每帧由 `event:` 与可多行的 `data:` 组成，以空行结束；
// `data:` 是**追加**（不是覆盖）。事件类型：output（文本/思考/工具调用增量）、
// token_usage（用量）、done（finish_reason）、error（业务错误）。
func consumeTraeSSE(body io.Reader, em traeEmitter) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)

	var (
		currentEvent string
		currentData  strings.Builder
		sawEvent     bool
		terminal     bool
		emitErr      error
	)
	dispatch := func() error {
		event := strings.TrimSpace(currentEvent)
		data := strings.TrimSpace(currentData.String())
		currentEvent = ""
		currentData.Reset()
		if data == "" {
			return nil
		}
		sawEvent = true
		var frame map[string]any
		if json.Unmarshal([]byte(data), &frame) != nil {
			return nil // 解析失败按空帧处理（与官方一致）
		}
		terminal = terminal || event == "done"
		return applyTraeFrame(event, frame, em)
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if err := dispatch(); err != nil {
				emitErr = err
				break
			}
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			currentEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			currentData.WriteString(strings.TrimPrefix(trimmed, "data:"))
		}
	}
	if emitErr != nil {
		return emitErr
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := dispatch(); err != nil {
		return err
	}
	if !sawEvent {
		return fmt.Errorf("trae: upstream returned no events (empty response before first model event)")
	}
	if !terminal {
		return incompleteStreamError("trae")
	}
	return nil
}

// applyTraeFrame 按事件类型分发一帧。
func applyTraeFrame(event string, frame map[string]any, em traeEmitter) error {
	switch event {
	case "output":
		if text, ok := frame["response"].(string); ok && text != "" {
			em.onText(text)
		}
		if reasoning, ok := frame["reasoning_content"].(string); ok && reasoning != "" {
			em.onThinking(reasoning)
		}
		if calls, ok := frame["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				if call == nil {
					continue
				}
				wireIndex := 0
				if idx, ok := call["index"].(float64); ok {
					wireIndex = int(idx)
				}
				id, _ := call["id"].(string)
				fn, _ := call["function"].(map[string]any)
				name := ""
				args := ""
				if fn != nil {
					name, _ = fn["name"].(string)
					switch typed := fn["arguments"].(type) {
					case string:
						args = typed
					case map[string]any:
						if encoded, err := json.Marshal(typed); err == nil {
							args = string(encoded)
						}
					}
				}
				if id != "" || name != "" {
					em.onToolStart(wireIndex, id, name)
				}
				if args != "" {
					em.onToolDelta(wireIndex, args)
				}
			}
		}
	case "token_usage":
		prompt := int(numberValue(frame, "prompt_tokens"))
		completion := int(numberValue(frame, "completion_tokens"))
		em.onUsage(prompt, completion)
	case "done":
		if reason, ok := frame["finish_reason"].(string); ok {
			em.onFinish(reason)
		}
	case "error":
		code := numberValue(frame, "code")
		message, _ := frame["message"].(string)
		if message == "" {
			message = "upstream error"
		}
		return traeStreamError(int(code), message)
	}
	return nil
}

// traeStreamError 把上游的 error 事件转成 ProviderError。
//
// code 4001 是「模型不被接受」：上游认可的模型列表里存在但不可调用的自定义模型，
// 需要引导用户换模型，故把说明拼进 message。
func traeStreamError(code int, message string) *ProviderError {
	text := fmt.Sprintf("trae: %s (code=%d)", message, code)
	if code == 4001 {
		text += "—— 模型不被上游接受：它通常是「仅可见但不可调用」的自定义模型（需先在 TRAE IDE 内自行配置供应商），请改用其它模型"
	}
	return &ProviderError{Provider: "trae", StatusCode: http.StatusBadGateway, Message: text}
}
