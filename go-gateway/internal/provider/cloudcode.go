package provider

// CloudCode provider：对接 Google Antigravity / CloudCode 的私有端点
// （v1internal:streamGenerateContent）。这类端点不是公开 API：请求体是
// 一个 CloudCode 专有的 {model, project, request:{contents,...}} 信封，响应是
// CloudCode 形态的 SSE（不是 OpenAI 也不是 Anthropic）。
//
// 该 provider 把 Anthropic 格式请求翻译成这个信封发出，再把 CloudCode SSE 翻译回
// **Anthropic SSE**——与 OpenAIProvider 一样，上游协议细节全部在这里收敛，handler
// 层无需感知。Gemini 无法复用 OpenAIProvider：信封结构与 SSE 形状都不同。

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cloudCodeEndpointDaily      = "https://daily-cloudcode-pa.googleapis.com"
	cloudCodeEndpointSandbox    = "https://daily-cloudcode-pa.sandbox.googleapis.com"
	cloudCodeStreamPath         = "/v1internal:streamGenerateContent?alt=sse"
	cloudCodeLoadCodeAssistURL  = "/v1internal:loadCodeAssist"
	cloudCodeLoadCodeAssistBody = `{"metadata":{"ideType":"ANTIGRAVITY"}}`
	cloudCodeDefaultProject     = "aicode-consumers"
	cloudCodeDefaultModel       = "gemini-3.8-flash"
	cloudCodeLane               = "infer"
	// cloudCodeModelPrefix 是对外模型名的可选命名空间前缀，只用于避开与其他
	// 供应商的模型 ID 冲突，发往上游前会被剥掉。
	cloudCodeModelPrefix = "cloudcode/"
)

// cloudCodeIdentityHeaders 复刻 Antigravity 客户端的身份头。上游按这些头识别
// 官方客户端，缺失会被拒。
var cloudCodeIdentityHeaders = map[string]string{
	"User-Agent":         "antigravity/4.3.0 (cmdc-pak)",
	"x-client-name":      "antigravity",
	"x-client-version":   "4.3.0",
	"x-machine-id":       "cmdc-pak",
	"x-vscode-sessionid": "proxy",
}

// cloudCodeThinkingBudgets 把思考档位映射成 thinkingBudget；tiered 用 -1 表示
// 「不给预算、由上游自行决定」（序列化时省略该字段）。
var cloudCodeThinkingBudgets = map[string]int{
	"low":    1000,
	"medium": 4000,
	"high":   10000,
	"tiered": -1,
}

// cloudCodeProjectSource 是可选接口：AuthSource 可额外提供 CloudCode 项目 ID 的
// 读写。provider 借此把 loadCodeAssist 的结果缓存到账号池上，避免每个模型实例
// 各查一次。未实现时 provider 退化为内存缓存 + 默认项目。
type cloudCodeProjectSource interface {
	CloudCodeProject() string
	SetCloudCodeProject(string)
}

// CloudCodeProvider 绑定一个模型 ID，向上游发 CloudCode 信封请求。
type CloudCodeProvider struct {
	config *Config
	client *http.Client
	// endpoint 可覆盖，测试时指向本地假上游。
	endpoint string
	// customEndpoint 表示 endpoint 被 Config.URL 覆盖（测试）；此时 loadCodeAssist
	// 也只打这一个地址，不去访问真实的沙箱/生产主机。
	customEndpoint bool

	mu      sync.RWMutex
	project string
}

// NewCloudCodeProvider 构造 CloudCode provider。Config.URL 非空时作为端点基址，
// 便于测试指向本地假上游；生产留空即用生产端点。
func NewCloudCodeProvider(config *Config) *CloudCodeProvider {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = config.RequestTimeout
	if config.RequestTimeout <= 0 {
		transport.ResponseHeaderTimeout = defaultRequestTimeout
	}
	endpoint := cloudCodeEndpointDaily
	custom := false
	if config.URL != "" {
		endpoint = strings.TrimRight(config.URL, "/")
		custom = true
	}
	return &CloudCodeProvider{
		config:         config,
		client:         &http.Client{Transport: transport},
		endpoint:       endpoint,
		customEndpoint: custom,
	}
}

func (p *CloudCodeProvider) Name() string { return p.config.Name }
func (p *CloudCodeProvider) URL() string {
	return p.endpoint + cloudCodeStreamPath
}
func (p *CloudCodeProvider) APIKey() string  { return p.config.APIKey }
func (p *CloudCodeProvider) UseBearer() bool { return true }

// Available 报告当前是否已有凭据；未登录时路由层直接跳过。
func (p *CloudCodeProvider) Available() bool { return p.config.authAvailable() }

// IsHealthy 只反映本地配置：可用性判断交给 Available 与 Probe。
func (p *CloudCodeProvider) IsHealthy(ctx context.Context) bool { return true }

// Probe 用绑定模型探测。
func (p *CloudCodeProvider) Probe(ctx context.Context) ProbeResult {
	return p.ProbeModel(ctx, p.config.Name)
}

// ProbeModel 用指定模型发一个最小请求。
func (p *CloudCodeProvider) ProbeModel(ctx context.Context, model string) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return probeVia(ctx, model, 0, p.ForwardRequest)
}

// ForwardRequest 发一次非流式请求：内部仍走流式端点，把 SSE 累积成一个完整响应。
func (p *CloudCodeProvider) ForwardRequest(ctx context.Context, req *Request) (*Response, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := p.send(streamCtx, req, cancel)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var (
		blocks      []ContentBlock
		textBuf     strings.Builder
		thinkBuf    strings.Builder
		toolByID    = map[string]int{}
		stopReason  = "end_turn"
		usageInput  int
		usageOutput int
		usageCache  int
		usageKnown  bool
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

	em := cloudCodeEmitter{
		onThinking: func(s string) { flushText(); thinkBuf.WriteString(s) },
		onText:     func(s string) { flushThink(); textBuf.WriteString(s) },
		onToolCall: func(id, name string, args json.RawMessage, _ string) {
			flushText()
			flushThink()
			if _, ok := toolByID[id]; ok {
				return
			}
			toolByID[id] = len(blocks)
			blocks = append(blocks, ContentBlock{Type: "tool_use", ID: id, Name: name, Input: normalizeToolArgs(args)})
		},
		onFinish: func(reason string) { stopReason = reason },
		onUsage: func(input, output, cache, total int) {
			usageInput, usageOutput, usageCache, usageKnown = input, output, cache, true
		},
	}
	if err := consumeCloudCodeSSE(resp.Body, em); err != nil {
		return nil, err
	}
	flushText()
	flushThink()

	if len(blocks) == 0 {
		blocks = []ContentBlock{{Type: "text", Text: ""}}
	}

	out := &Response{
		ID:         "msg_gemini_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Type:       "message",
		Role:       "assistant",
		Model:      req.Model,
		StopReason: stopReason,
		Content:    blocks,
	}
	out.Usage.InputTokens = usageInput
	out.Usage.OutputTokens = usageOutput
	if usageKnown && usageCache > 0 {
		out.CacheReadInputTokens = usageCache
		out.CacheUsageKnown = true
	}
	return out, nil
}

// ForwardStream 把 Anthropic 请求翻成 CloudCode 信封发出，把 CloudCode SSE 翻回
// Anthropic SSE 写入 w。
func (p *CloudCodeProvider) ForwardStream(ctx context.Context, req *Request, w io.Writer) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	resp, err := p.send(streamCtx, req, cancel)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	msgID := "msg_gemini_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	writeSSE(w, "message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		msgID, req.Model,
	))
	writeSSE(w, "ping", `{"type":"ping"}`)

	type blockState struct{ idx int }
	nextIdx := 0
	thinkingIdx := -1
	textIdx := -1
	toolStates := map[string]*blockState{}
	var toolOrder []string
	stopReason := "end_turn"
	usageInput, usageOutput, usageCache := 0, 0, 0

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

	em := cloudCodeEmitter{
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
		onToolCall: func(id, name string, args json.RawMessage, _ string) {
			closeText()
			closeThinking()
			state, ok := toolStates[id]
			if !ok {
				state = &blockState{idx: nextIdx}
				nextIdx++
				toolStates[id] = state
				toolOrder = append(toolOrder, id)
				writeSSE(w, "content_block_start", fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`,
					state.idx, jsonString(id), jsonString(name)))
			}
			if len(args) > 0 && string(args) != "{}" {
				writeSSE(w, "content_block_delta", fmt.Sprintf(
					`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
					state.idx, jsonString(string(args))))
			}
		},
		onFinish: func(reason string) { stopReason = reason },
		onUsage: func(input, output, cache, total int) {
			usageInput, usageOutput, usageCache = input, output, cache
		},
	}
	if err := consumeCloudCodeSSE(resp.Body, em); err != nil {
		return err
	}

	closeThinking()
	closeText()
	for _, id := range toolOrder {
		writeSSE(w, "content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, toolStates[id].idx))
	}

	if usageCache > 0 {
		writeSSE(w, "message_delta", fmt.Sprintf(
			`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d}}`,
			stopReason, usageInput, usageOutput, usageCache))
	} else {
		writeSSE(w, "message_delta", fmt.Sprintf(
			`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
			stopReason, usageInput, usageOutput))
	}
	writeSSE(w, "message_stop", `{"type":"message_stop"}`)
	return nil
}

// send 翻译请求、注入鉴权并发出，返回已确认 2xx 的响应。非 2xx 统一转成
// *ProviderError（复用网关既有的上游错误处理）。
func (p *CloudCodeProvider) send(ctx context.Context, req *Request, cancel context.CancelFunc) (*http.Response, error) {
	// 未登录直接短路（与 postWithAuth 语义一致）：路由层通常已按 Available 跳过，
	// 这里兜住直接调用 provider 的路径，避免发一次注定 401 的请求。
	if p.config.Auth != nil && !p.config.authAvailable() {
		return nil, &ProviderError{Provider: p.Name(), StatusCode: http.StatusUnauthorized, Message: "channel has no credential; login required"}
	}
	project := p.resolveProject(ctx)
	envelope, err := buildCloudCodeEnvelope(req, project)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("marshal cloudcode request: %w", err)
	}
	body, err = p.config.applyTransform(body)
	if err != nil {
		return nil, fmt.Errorf("transform cloudcode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL(), strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build cloudcode request: %w", err)
	}
	usedToken := ""
	if p.config.Auth != nil {
		usedToken = p.config.Auth.BearerToken()
	}
	p.setHeaders(httpReq)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cloudcode request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// 401/403 时用账号池续期一次并重试，与 postWithAuth 的语义一致。
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
			return nil, &ProviderError{Provider: p.Name(), StatusCode: 401, Message: "channel has no credential; login required"}
		}
		return nil, NewHTTPError(p.Name(), resp, b)
	}
	return resp, nil
}

// sendOnce 用已序列化的 body 重发一次（续期后的重试）。
func (p *CloudCodeProvider) sendOnce(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL(), strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build cloudcode retry request: %w", err)
	}
	p.setHeaders(httpReq)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cloudcode retry request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, NewHTTPError(p.Name(), resp, b)
	}
	return resp, nil
}

func (p *CloudCodeProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	for name, value := range cloudCodeIdentityHeaders {
		req.Header.Set(name, value)
	}
	// 流式请求刻意不带 Accept（与官方客户端抓包一致）。
	p.config.applyAuth(req, true)
}

// resolveProject 返回 CloudCode 项目 ID：优先账号池缓存，其次 loadCodeAssist，
// 最后退回默认项目。
func (p *CloudCodeProvider) resolveProject(ctx context.Context) string {
	src, _ := p.config.Auth.(cloudCodeProjectSource)
	if src != nil {
		if v := src.CloudCodeProject(); v != "" {
			return v
		}
	}
	p.mu.RLock()
	cached := p.project
	p.mu.RUnlock()
	if cached != "" {
		return cached
	}

	project := p.loadCodeAssist(ctx)
	if project == "" {
		project = cloudCodeDefaultProject
	}
	p.mu.Lock()
	p.project = project
	p.mu.Unlock()
	if src != nil {
		src.SetCloudCodeProject(project)
	}
	return project
}

// loadCodeAssist 查询账号的 CloudCode 项目。失败返回空串（调用方退回默认）。
func (p *CloudCodeProvider) loadCodeAssist(ctx context.Context) string {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hosts := []string{cloudCodeEndpointSandbox, cloudCodeEndpointDaily}
	if p.customEndpoint {
		hosts = []string{p.endpoint}
	}
	for _, host := range hosts {
		req, err := http.NewRequestWithContext(callCtx, http.MethodPost, host+cloudCodeLoadCodeAssistURL, strings.NewReader(cloudCodeLoadCodeAssistBody))
		if err != nil {
			continue
		}
		p.setHeaders(req)
		resp, err := p.client.Do(req)
		if err != nil {
			continue
		}
		var body struct {
			CloudaicompanionProject json.RawMessage `json:"cloudaicompanionProject"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			continue
		}
		if project := parseCloudCodeProject(body.CloudaicompanionProject); project != "" {
			return project
		}
	}
	return ""
}

// parseCloudCodeProject 兼容 project 为字符串或 {id:...} 对象两种形态。
func parseCloudCodeProject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.ID
	}
	return ""
}

// ─── 请求翻译 ────────────────────────────────────────────────────────────────

// buildCloudCodeEnvelope 把 Anthropic 请求翻译成 CloudCode 信封。
func buildCloudCodeEnvelope(req *Request, project string) (map[string]any, error) {
	upstream, effort := cloudCodeModelSpec(req.Model)
	contents, err := buildCloudCodeContents(req)
	if err != nil {
		return nil, err
	}

	requestBody := map[string]any{
		"contents":         contents,
		"generationConfig": buildCloudCodeGenerationConfig(req, effort),
	}

	if session := deriveCloudCodeSessionID(project, firstUserText(req), cloudCodeLane); session != "" {
		requestBody["sessionId"] = session
	}
	if sys := anthropicSystemText(req); sys != "" {
		requestBody["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": sys}},
		}
	}
	if tools := buildCloudCodeTools(req); len(tools) > 0 {
		requestBody["tools"] = []any{map[string]any{"functionDeclarations": tools}}
		if tc := buildCloudCodeToolConfig(req); tc != nil {
			requestBody["toolConfig"] = tc
		}
	}

	return map[string]any{
		"model":     upstream,
		"project":   project,
		"request":   requestBody,
		"requestId": "agent/" + strconv.FormatInt(time.Now().UnixMilli(), 10) + "/" + mustRandomHex(4),
		"userAgent": "antigravity",
	}, nil
}

// cloudCodeModelSpec 把对外模型名拆成上游真名与思考档。例如
// gemini-3.8-flash-high → upstream=gemini-3.8-flash-high, effort=high。
//
// 对外名可以带 `cloudcode/` 前缀：现有配置里 CLIProxyAPI 的 antigravity 供应商
// 已占用 `gemini-3.8-flash-high` 这个名字，渠道模型加前缀后才能与它并存（同名会
// 互相覆盖注册）。前缀属于网关侧命名，不会出现在发往上游的 model 字段里。
func cloudCodeModelSpec(modelID string) (string, string) {
	modelID = strings.TrimPrefix(modelID, cloudCodeModelPrefix)
	base := modelID
	effort := "medium"
	for _, tier := range []string{"low", "medium", "high", "tiered"} {
		if strings.HasSuffix(modelID, "-"+tier) {
			base = strings.TrimSuffix(modelID, "-"+tier)
			effort = tier
			break
		}
	}
	if base == "" {
		base = cloudCodeDefaultModel
	}
	return base + "-" + effort, effort
}

// buildCloudCodeGenerationConfig 组装 generationConfig，含思考档位。
func buildCloudCodeGenerationConfig(req *Request, effort string) map[string]any {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000
	}
	thinking := map[string]any{"includeThoughts": true}
	if budget, ok := cloudCodeThinkingBudgets[effort]; ok && budget >= 0 {
		thinking["thinkingBudget"] = budget
	}
	config := map[string]any{
		"maxOutputTokens": maxTokens,
		"thinkingConfig":  thinking,
	}
	if raw, ok := req.raw["temperature"]; ok {
		var temp float64
		if json.Unmarshal(raw, &temp) == nil {
			config["temperature"] = temp
		}
	}
	return config
}

// anthropicSystemText 取出 system 字段的纯文本（数组时拼接各 text 块）。
func anthropicSystemText(req *Request) string {
	raw, ok := req.raw["system"]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// firstUserText 返回第一条 user 消息的首段文本，用于派生稳定的 sessionId。
func firstUserText(req *Request) string {
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content.Blocks() {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// deriveCloudCodeSessionID 用 FNV-1a 64 位哈希派生会话 ID（同一项目/首条用户
// 输入/lane 恒定，保证上游的会话亲和性）。空输入返回空串。
func deriveCloudCodeSessionID(project, firstText, lane string) string {
	if project == "" && firstText == "" {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(project))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(lane))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(firstText))
	return strconv.FormatInt(int64(h.Sum64()), 10)
}

// buildCloudCodeContents 把 Anthropic messages 翻成 contents。system 消息跳过
// （单独进 systemInstruction），thinking 块丢弃，assistant → model。
func buildCloudCodeContents(req *Request) ([]any, error) {
	toolNames := collectToolUseNames(req)
	var contents []any
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		var parts []any
		for _, b := range m.Content.Blocks() {
			switch b.Type {
			case "text":
				if b.Text != "" {
					parts = append(parts, map[string]any{"text": b.Text})
				}
			case "image":
				if inline := cloudCodeInlineData(b); inline != nil {
					parts = append(parts, inline)
				}
			case "tool_use":
				fc := map[string]any{
					"name": b.Name,
					"args": cloudCodeToolArgs(b.Input),
				}
				if sig := cloudCodeThoughtSignature(b); sig != "" {
					fc = map[string]any{"functionCall": map[string]any{"name": b.Name, "args": cloudCodeToolArgs(b.Input)}, "thoughtSignature": sig}
				} else {
					fc = map[string]any{"functionCall": fc}
				}
				parts = append(parts, fc)
			case "tool_result":
				name := toolNames[b.ToolUseID]
				if name == "" {
					name = "tool"
				}
				response := map[string]any{"content": cloudCodeToolResultText(b)}
				if b.IsError {
					response["error"] = true
				}
				parts = append(parts, map[string]any{
					"functionResponse": map[string]any{"name": name, "response": response},
				})
			}
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	if len(contents) == 0 {
		return nil, fmt.Errorf("cloudcode: 请求没有可用的消息内容")
	}
	return contents, nil
}

// collectToolUseNames 建立 tool_use id → name 的映射，供 tool_result 回填工具名。
func collectToolUseNames(req *Request) map[string]string {
	names := map[string]string{}
	for _, m := range req.Messages {
		for _, b := range m.Content.Blocks() {
			if b.Type == "tool_use" && b.ID != "" {
				names[b.ID] = b.Name
			}
		}
	}
	return names
}

// cloudCodeInlineData 把 Anthropic image 块转成 inlineData。
func cloudCodeInlineData(b ContentBlock) map[string]any {
	if len(b.Source) == 0 {
		return nil
	}
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if json.Unmarshal(b.Source, &source) != nil || source.Data == "" {
		return nil
	}
	mime := source.MediaType
	if mime == "" {
		mime = "image/*"
	}
	return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": source.Data}}
}

// cloudCodeToolArgs 归一化工具参数：nil → {}。
func cloudCodeToolArgs(raw json.RawMessage) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return map[string]any{}
	}
	return v
}

// cloudCodeThoughtSignature 从 tool_use 原始块里取 thoughtSignature（存在时需回传）。
func cloudCodeThoughtSignature(b ContentBlock) string {
	if len(b.Raw) == 0 {
		return ""
	}
	var raw struct {
		ThoughtSignature string `json:"thought_signature"`
		Signature        string `json:"signature"`
	}
	if json.Unmarshal(b.Raw, &raw) != nil {
		return ""
	}
	return firstNonEmptyString(raw.ThoughtSignature, raw.Signature)
}

// cloudCodeToolResultText 汇总 tool_result 的文本内容。
func cloudCodeToolResultText(b ContentBlock) string {
	if b.ContentStr != "" {
		return b.ContentStr
	}
	var parts []string
	for _, inner := range b.ContentBlocks {
		if inner.Type == "text" {
			parts = append(parts, inner.Text)
		}
	}
	return strings.Join(parts, "")
}

// normalizeToolArgs 保证 tool_use 的 input 是可序列化的 JSON。
func normalizeToolArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// buildCloudCodeTools 把 Anthropic tools 转成 functionDeclarations，并清洗 schema。
func buildCloudCodeTools(req *Request) []any {
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
		decl := map[string]any{"name": t.Name}
		if t.Description != "" {
			decl["description"] = t.Description
		}
		if len(t.InputSchema) > 0 {
			var schema any
			if json.Unmarshal(t.InputSchema, &schema) == nil {
				decl["parameters"] = sanitizeCloudCodeSchema(schema)
			}
		}
		out = append(out, decl)
	}
	return out
}

// cloudCodeSchemaKeys 是 CloudCode 接受的 JSON Schema 键白名单，其余一律剔除
// （上游对未知键报错）。
var cloudCodeSchemaKeys = map[string]bool{
	"type": true, "format": true, "description": true, "nullable": true,
	"enum": true, "items": true, "minItems": true, "maxItems": true,
	"properties": true, "required": true, "minProperties": true, "maxProperties": true,
	"minLength": true, "maxLength": true, "pattern": true, "anyOf": true,
	"propertyOrdering": true, "minimum": true, "maximum": true,
}

// sanitizeCloudCodeSchema 深度清洗 schema：只留白名单键，递归 properties/items/anyOf。
func sanitizeCloudCodeSchema(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for key, value := range obj {
		if !cloudCodeSchemaKeys[key] {
			continue
		}
		switch key {
		case "properties":
			if props, ok := value.(map[string]any); ok {
				cleaned := map[string]any{}
				for name, sub := range props {
					cleaned[name] = sanitizeCloudCodeSchema(sub)
				}
				out[key] = cleaned
			}
		case "items":
			out[key] = sanitizeCloudCodeSchema(value)
		case "anyOf":
			if list, ok := value.([]any); ok {
				cleaned := make([]any, 0, len(list))
				for _, sub := range list {
					cleaned = append(cleaned, sanitizeCloudCodeSchema(sub))
				}
				out[key] = cleaned
			}
		case "enum":
			// 只接受字符串枚举。
			if list, ok := value.([]any); ok {
				cleaned := make([]any, 0, len(list))
				for _, item := range list {
					if _, ok := item.(string); ok {
						cleaned = append(cleaned, item)
					}
				}
				if len(cleaned) > 0 {
					out[key] = cleaned
				}
			}
		case "type":
			out[key], out["nullable"] = normalizeCloudCodeType(value, obj["nullable"])
		default:
			out[key] = value
		}
	}
	return out
}

// normalizeCloudCodeType 处理 type 为数组的情况（取首个非 null，并置 nullable）。
func normalizeCloudCodeType(value, nullable any) (any, any) {
	list, ok := value.([]any)
	if !ok {
		return value, nullable
	}
	pick := ""
	isNullable := nullable == true
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			continue
		}
		if s == "null" {
			isNullable = true
			continue
		}
		if pick == "" {
			pick = s
		}
	}
	if pick == "" {
		pick = "string"
	}
	return pick, isNullable
}

// buildCloudCodeToolConfig 依据 tool_choice 生成 toolConfig。
func buildCloudCodeToolConfig(req *Request) map[string]any {
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
	mode := "AUTO"
	switch choice.Type {
	case "none":
		mode = "NONE"
	case "any":
		mode = "ANY"
	case "tool":
		mode = "ANY"
	case "auto":
		mode = "AUTO"
	}
	config := map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
	if mode == "ANY" && choice.Name != "" {
		config["functionCallingConfig"].(map[string]any)["allowedFunctionNames"] = []any{choice.Name}
	}
	return config
}

// ─── SSE 消费 ───────────────────────────────────────────────────────────────

// cloudCodeEmitter 是 SSE 事件的回调集合。
type cloudCodeEmitter struct {
	onThinking func(string)
	onText     func(string)
	onToolCall func(id, name string, args json.RawMessage, thoughtSignature string)
	onFinish   func(string)
	onUsage    func(input, output, cache, total int)
}

// consumeCloudCodeSSE 逐帧读取 CloudCode SSE，按块类型回调。usage 取
// totalTokenCount 最大的一帧（上游逐块累加，最后一帧最全）。
func consumeCloudCodeSSE(body io.Reader, em cloudCodeEmitter) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)

	bestTotal := -1
	toolIndex := 0
	terminal := false
	onFinish := em.onFinish
	em.onFinish = func(reason string) { terminal = true; onFinish(reason) }
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			terminal = true
			continue
		}
		if payload == "" {
			continue
		}
		if err := streamPayloadError("", []byte(payload)); err != nil {
			return err
		}
		var frame map[string]any
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		inner, _ := frame["response"].(map[string]any)
		if inner == nil {
			inner = frame
		}
		consumeCloudCodeFrame(inner, em, &toolIndex)

		if meta, ok := inner["usageMetadata"].(map[string]any); ok {
			total := int(numberValue(meta, "totalTokenCount"))
			if total >= bestTotal {
				bestTotal = total
				prompt := int(numberValue(meta, "promptTokenCount"))
				cached := int(numberValue(meta, "cachedContentTokenCount"))
				if prompt-cached < 0 {
					prompt = cached
				}
				em.onUsage(prompt-cached, int(numberValue(meta, "candidatesTokenCount")), cached, total)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !terminal {
		return incompleteStreamError("cloudcode")
	}
	return nil
}

func consumeCloudCodeFrame(inner map[string]any, em cloudCodeEmitter, toolIndex *int) {
	candidates, _ := inner["candidates"].([]any)
	if len(candidates) == 0 {
		return
	}
	cand, _ := candidates[0].(map[string]any)
	if cand == nil {
		return
	}
	content, _ := cand["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		if part == nil {
			continue
		}
		if fc, ok := part["functionCall"].(map[string]any); ok {
			args, _ := json.Marshal(fc["args"])
			name, _ := fc["name"].(string)
			sig, _ := part["thoughtSignature"].(string)
			em.onToolCall(newCloudCodeToolCallID(*toolIndex), name, normalizeToolArgs(args), sig)
			*toolIndex++
			continue
		}
		if text, ok := part["text"].(string); ok && text != "" {
			if thought, _ := part["thought"].(bool); thought {
				em.onThinking(text)
			} else {
				em.onText(text)
			}
		}
	}
	if reason, ok := cand["finishReason"].(string); ok && reason != "" {
		em.onFinish(mapCloudCodeFinish(reason))
	}
}

// mapCloudCodeFinish 把 CloudCode finishReason 映射成 Anthropic stop_reason。
func mapCloudCodeFinish(reason string) string {
	switch reason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "STOP", "STOP_SEQUENCE", "FINISH_REASON_UNSPECIFIED", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func newCloudCodeToolCallID(index int) string {
	return "gemini_tool_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + "_" + strconv.Itoa(index)
}

// numberValue 读取 map 里的数值字段（float64 / json.Number / 数字字符串）。
func numberValue(obj map[string]any, key string) float64 {
	switch v := obj[key].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// mustRandomHex 生成 n 字节随机数的十六进制串；失败时退化为时间戳（不致命）。
func mustRandomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}
