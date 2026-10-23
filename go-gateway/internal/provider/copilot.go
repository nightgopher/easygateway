package provider

// Copilot provider：GitHub Copilot API（OpenAI 兼容格式）
// 使用特殊 headers 认证，token 需要定期刷新

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CopilotProvider 实现 OpenAI 兼容的 GitHub Copilot API
type CopilotProvider struct {
	config      *Config
	client      *http.Client
	mu          sync.RWMutex // 保护 token / apiURL 的并发读写
	apiURL      string       // 从 token 解析的 API 地址（受 mu 保护）
	token       string       // Copilot token（含 proxy-ep 等信息，受 mu 保护）
	githubToken string       // GitHub OAuth token（用于刷新）
}

func NewCopilotProvider(config *Config, githubToken string) *CopilotProvider {
	// 不用 http.Client.Timeout：它把整条 SSE 流计入总时长，长生成会在 120s 被
	// 砍断。改为 ResponseHeaderTimeout 限制首字节，流式停滞由看门狗中止
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 120 * time.Second
	p := &CopilotProvider{
		config:      config,
		client:      &http.Client{Transport: transport},
		token:       config.APIKey,
		githubToken: githubToken,
	}
	// 从 token 解析 API 地址
	p.apiURL = p.extractAPIURL(config.APIKey)
	return p
}

// extractAPIURL 从 Copilot token 中提取 API 地址
// token 格式：tid=...;exp=...;proxy-ep=proxy.individual.githubcopilot.com;...
func (p *CopilotProvider) extractAPIURL(token string) string {
	// 默认地址
	defaultURL := "https://api.individual.githubcopilot.com/chat/completions"

	if token == "" {
		return defaultURL
	}

	// 解析 proxy-ep
	parts := strings.Split(token, ";")
	for _, part := range parts {
		if strings.HasPrefix(part, "proxy-ep=") {
			proxyHost := strings.TrimPrefix(part, "proxy-ep=")
			// proxy.xxx -> api.xxx
			apiHost := strings.Replace(proxyHost, "proxy.", "api.", 1)
			return fmt.Sprintf("https://%s/chat/completions", apiHost)
		}
	}

	return defaultURL
}

func (p *CopilotProvider) Name() string { return p.config.Name }
func (p *CopilotProvider) URL() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.apiURL
}
func (p *CopilotProvider) APIKey() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.token
}
func (p *CopilotProvider) UseBearer() bool { return true }

// mapModel 把请求模型名映射到 Copilot 支持的模型。
// 别名已移除，非 Copilot 自己的模型名一律回落到 Copilot 默认模型，
// 因为它只服务 GPT 系列。
func (p *CopilotProvider) mapModel(reqModel string) string {
	switch reqModel {
	case "gpt-4o-mini":
		return "gpt-4o-mini"
	default:
		return "gpt-4o"
	}
}

// sendRequest 发一次非流式 copilot 请求，返回响应和 body（body 已读完、resp.Body 已关闭）。
func (p *CopilotProvider) sendRequest(ctx context.Context, req *Request) (*http.Response, []byte, error) {
	oaiReq := toOpenAIRequest(req)
	oaiReq.Model = p.mapModel(req.Model)

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal copilot request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("send request: %w", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}
	return resp, respBody, nil
}

// ForwardRequest 将 Anthropic 格式请求转为 OpenAI 格式，发出后将响应转回 Anthropic 格式。
// 收到 401 时自动用 GitHub token 刷新 Copilot token 并重试一次。
func (p *CopilotProvider) ForwardRequest(ctx context.Context, req *Request) (*Response, error) {
	resp, respBody, err := p.sendRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && p.refreshToken() {
		resp, respBody, err = p.sendRequest(ctx, req)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, NewHTTPError(p.Name(), resp, respBody)
	}

	var oaiResp openAIResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("parse copilot response: %w", err)
	}

	return fromOpenAIResponse(&oaiResp), nil
}

// sendStreamRequest 发一次流式 copilot 请求，401 时刷新 token 重试一次。成功返回已就绪的 resp。
func (p *CopilotProvider) sendStreamRequest(ctx context.Context, req *Request) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		streamReq := toOpenAIRequest(req)
		streamReq.Model = p.mapModel(req.Model)
		streamReq.Stream = true

		body, err := json.Marshal(streamReq)
		if err != nil {
			return nil, fmt.Errorf("marshal copilot stream request: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create stream request: %w", err)
		}
		p.setHeaders(httpReq)

		resp, err := p.client.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("send stream request: %w", err)
		}

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && p.refreshToken() {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, NewHTTPError(p.Name(), resp, b)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("copilot stream: token refresh failed")
}

// ForwardStream 将 Anthropic 格式请求转为 OpenAI 流式请求，把 OpenAI SSE 转为 Anthropic SSE 写入 w。
// 收到 401 时自动刷新 Copilot token 并重试一次。
func (p *CopilotProvider) ForwardStream(ctx context.Context, req *Request, w io.Writer) error {
	resp, err := p.sendStreamRequest(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	msgID := "msg_copilot_" + time.Now().Format("20060102150405")

	// 用于拼装流式 tool_calls（key = index）
	type toolCallAccum struct {
		id        string
		name      string
		arguments strings.Builder
	}
	toolCalls := map[int]*toolCallAccum{}

	// 文本内容 block 是否已开始
	textBlockStarted := false

	writeSSE(w, "message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		msgID, req.Model,
	))
	writeSSE(w, "ping", `{"type":"ping"}`)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	finishReason := ""
	terminal := false
	var streamUsage *openAIStreamUsage

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			terminal = true
			break
		}
		if err := streamPayloadError("", []byte(payload)); err != nil {
			return err
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		delta := choice.Delta

		// --- 文本内容 ---
		if delta.Content != "" {
			if !textBlockStarted {
				textBlockStarted = true
				writeSSE(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			}
			writeSSE(w, "content_block_delta", fmt.Sprintf(
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`,
				jsonString(delta.Content),
			))
		}

		// --- 工具调用（增量拼装）---
		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			if _, ok := toolCalls[idx]; !ok {
				toolCalls[idx] = &toolCallAccum{}
			}
			acc := toolCalls[idx]
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.arguments.WriteString(tc.Function.Arguments)
		}

		// --- finish_reason ---
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finishReason = *choice.FinishReason
			terminal = true
		}
		// --- usage（最后一条 chunk 携带） ---
		if chunk.Usage != nil {
			streamUsage = chunk.Usage
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	if !terminal {
		return incompleteStreamError(p.Name())
	}

	// 关闭文本 block
	if textBlockStarted {
		writeSSE(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
	}

	// 输出拼装完整的 tool_use blocks（按 index 顺序）
	for i := 0; i < len(toolCalls); i++ {
		acc, ok := toolCalls[i]
		if !ok {
			continue
		}
		blockIdx := 1 + i
		if !textBlockStarted {
			blockIdx = i
		}

		var inputRaw json.RawMessage
		if err := json.Unmarshal([]byte(acc.arguments.String()), &inputRaw); err != nil {
			inputRaw = json.RawMessage(`{}`)
		}
		inputJSON, _ := json.Marshal(inputRaw)

		writeSSE(w, "content_block_start", fmt.Sprintf(
			`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%s,"name":%s,"input":{}}}`,
			blockIdx, jsonString(acc.id), jsonString(acc.name),
		))
		writeSSE(w, "content_block_delta", fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}`,
			blockIdx, jsonString(string(inputJSON)),
		))
		writeSSE(w, "content_block_stop", fmt.Sprintf(
			`{"type":"content_block_stop","index":%d}`, blockIdx,
		))
	}

	stopReason := mapFinishReason(finishReason)
	// 用上游真实 usage 构造 message_delta（Anthropic SSE 格式）
	inputTokens := 0
	outputTokens := 0
	cacheReadTokens := 0
	cacheUsageKnown := false
	if streamUsage != nil {
		inputTokens = streamUsage.PromptTokens
		outputTokens = streamUsage.CompletionTokens
		if streamUsage.PromptTokensDetails != nil && streamUsage.PromptTokensDetails.cacheTokensKnown {
			cacheReadTokens = streamUsage.PromptTokensDetails.CachedTokens
			cacheUsageKnown = true
		}
	}
	if cacheUsageKnown {
		inputTokens -= cacheReadTokens
		if inputTokens < 0 {
			inputTokens = 0
		}
		writeSSE(w, "message_delta", fmt.Sprintf(
			`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d}}`,
			stopReason, inputTokens, outputTokens, cacheReadTokens,
		))
	} else {
		writeSSE(w, "message_delta", fmt.Sprintf(
			`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
			stopReason, inputTokens, outputTokens,
		))
	}
	writeSSE(w, "message_stop", `{"type":"message_stop"}`)

	return nil
}

// IsHealthy 只反映本地 token 是否非空，不验证上游可用性；上游可用性请用 Probe。
func (p *CopilotProvider) IsHealthy(ctx context.Context) bool {
	return p.token != ""
}

// Probe 发一个最小请求验证 token 与账号配额。Copilot 的 token 过期很快，
// 本地非空并不代表上游仍接受，必须真实调用一次。
func (p *CopilotProvider) Probe(ctx context.Context) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// 走非流式转发路径：ForwardRequest 内部已处理 401 后的 token 刷新。
	return probeVia(ctx, p.mapModel(""), 0, p.ForwardRequest)
}

func (p *CopilotProvider) setHeaders(req *http.Request) {
	p.mu.RLock()
	token := p.token
	p.mu.RUnlock()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.35.0")
	req.Header.Set("Editor-Version", "vscode/1.107.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.35.0")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
}

// refreshToken 用 GitHub token 刷新 Copilot token，成功后更新 token 和 apiURL。
// 并发安全；无 githubToken 或刷新失败时返回 false。供 401 重试使用。
func (p *CopilotProvider) refreshToken() bool {
	if p.githubToken == "" {
		return false
	}
	newToken, _, err := RefreshCopilotToken(p.githubToken)
	if err != nil || newToken == "" {
		return false
	}
	p.mu.Lock()
	p.token = newToken
	p.apiURL = p.extractAPIURL(newToken)
	p.mu.Unlock()
	return true
}

// RefreshCopilotToken 使用 GitHub token 刷新 Copilot token
// 返回新的 Copilot token 和过期时间
func RefreshCopilotToken(githubToken string) (string, int64, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", "https://api.github.com/copilot_internal/v2/token", nil)
	if err != nil {
		return "", 0, err
	}

	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.35.0")
	req.Header.Set("Editor-Version", "vscode/1.107.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.35.0")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, err
	}

	if resp.StatusCode != http.StatusOK {
		return "", 0, NewHTTPError("copilot-auth", resp, body)
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", 0, err
	}

	return result.Token, result.ExpiresAt, nil
}
