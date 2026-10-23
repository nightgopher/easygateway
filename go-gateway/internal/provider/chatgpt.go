package provider

// ChatGPT Codex Provider：使用 ChatGPT OAuth token 调用 chatgpt.com 的 Codex API。
// 不需要第三方中转，直接用你自己的 ChatGPT Plus/Pro 订阅。
// 请求和响应都是 OpenAI Responses API 格式，直接透传，不需要格式转换。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	chatgptCodexURL = "https://chatgpt.com/backend-api/codex/responses"
	chatgptTokenURL = "https://auth.openai.com/oauth/token"
	chatgptClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// ChatGPTModel describes a model exposed by the ChatGPT Codex subscription
// endpoint. Keep this fallback catalog aligned with the models currently
// available to the gateway's ChatGPT subscription account.
type ChatGPTModel struct {
	ID              string
	UpstreamID      string
	Name            string
	Capabilities    []string
	InputModalities []string
	MaxInputTokens  int
	MaxOutputTokens int
}

var chatGPTModelCatalog = []ChatGPTModel{
	{
		ID:              "gpt-6.1-sol",
		Name:            "GPT-6.1 Sol",
		Capabilities:    []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  272000,
		MaxOutputTokens: 128000,
	},
	// GPT-6 家族。Codex 订阅端点只接受 sol/luna/astra：gpt-6-terra 不存在，
	// gpt-6-pro 与各 -wm 变体返回 400 not supported。
	{
		ID:              "gpt-6-sol",
		Name:            "GPT-6 Sol",
		Capabilities:    []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  272000,
		MaxOutputTokens: 128000,
	},
	{
		ID:              "gpt-6-luna",
		Name:            "GPT-6 Luna",
		Capabilities:    []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  272000,
		MaxOutputTokens: 128000,
	},
	{
		ID:              "gpt-6-astra",
		Name:            "GPT-6 Astra",
		Capabilities:    []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  272000,
		MaxOutputTokens: 128000,
	},
	// GPT-5.6 家族仍在上游服务，保留以兼容既有客户端配置。
	{
		ID:              "gpt-5.6-sol",
		Name:            "GPT-5.6 Sol",
		Capabilities:    []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  272000,
		MaxOutputTokens: 128000,
	},
	{
		ID:              "gpt-5.6-terra",
		Name:            "GPT-5.6 Terra",
		Capabilities:    []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  272000,
		MaxOutputTokens: 128000,
	},
	{
		ID:              "gpt-5.6-luna",
		Name:            "GPT-5.6 Luna",
		Capabilities:    []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning},
		InputModalities: []string{"text", "image"},
		MaxInputTokens:  272000,
		MaxOutputTokens: 128000,
	},
}

// ChatGPTModelCatalog returns a defensive copy for router registration.
func ChatGPTModelCatalog() []ChatGPTModel {
	models := make([]ChatGPTModel, len(chatGPTModelCatalog))
	for i, model := range chatGPTModelCatalog {
		model.Capabilities = append([]string(nil), model.Capabilities...)
		model.InputModalities = append([]string(nil), model.InputModalities...)
		models[i] = model
	}
	return models
}

func chatGPTUpstreamModel(modelID string) string {
	for _, model := range chatGPTModelCatalog {
		if model.ID == modelID && model.UpstreamID != "" {
			return model.UpstreamID
		}
	}
	return modelID
}

// normalizeChatGPTInput adapts the shorthand Responses input accepted by the
// gateway to the list-shaped input required by the ChatGPT Codex endpoint.
// In particular, connection tests and lightweight clients commonly send
// `input: "hello"` instead of an input message array.
func normalizeChatGPTInput(reqMap map[string]json.RawMessage) error {
	rawInput, ok := reqMap["input"]
	if !ok {
		return nil
	}

	var text string
	if err := json.Unmarshal(rawInput, &text); err != nil {
		return nil
	}

	normalized := []map[string]any{
		{
			"type": "message",
			"role": "user",
			"content": []map[string]string{{
				"type": "input_text",
				"text": text,
			}},
		},
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return fmt.Errorf("normalize input: %w", err)
	}
	reqMap["input"] = encoded
	return nil
}

func normalizeChatGPTRequest(reqMap map[string]json.RawMessage) error {
	if err := normalizeChatGPTInput(reqMap); err != nil {
		return err
	}
	// The public OpenAI Responses API exposes these output-limit fields, but
	// the ChatGPT subscription Codex endpoint rejects them as unsupported.
	// The gateway still accepts them from compatible clients (including the
	// model connection probe) and lets the subscription endpoint choose its
	// own completion budget.
	delete(reqMap, "max_output_tokens")
	delete(reqMap, "max_completion_tokens")
	delete(reqMap, "max_tokens")
	return nil
}

// chatgptAuth stores the credentials needed by the Codex endpoint.
type chatgptAuth struct {
	AccessToken  string
	RefreshToken string
	AccountID    string
}

type chatgptAuthFormat string

const (
	chatgptAuthFormatCodex chatgptAuthFormat = "codex"
	chatgptAuthFormatPi    chatgptAuthFormat = "pi"
)

// These two JSON shapes are intentionally supported together:
//   - Codex CLI: {"tokens":{"access_token":...,"refresh_token":...}}
//   - Pi: {"openai-codex":{"type":"oauth","access":...,"refresh":...}}
type chatgptAuthFile struct {
	Tokens *struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	OpenAICodex *struct {
		AccessToken     string `json:"access_token"`
		Access          string `json:"access"`
		RefreshToken    string `json:"refresh_token"`
		Refresh         string `json:"refresh"`
		AccountID       string `json:"account_id"`
		AccountIDPascal string `json:"accountId"`
	} `json:"openai-codex"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// ChatGPTProvider uses a ChatGPT OAuth token to call the Codex Responses API.
type ChatGPTProvider struct {
	mu         sync.RWMutex
	auth       *chatgptAuth
	authPath   string
	authPaths  []string
	authFormat chatgptAuthFormat
	proxyURL   string
	client     *http.Client
}

// NewChatGPTProvider creates a provider using CHATGPT_AUTH_FILE when set, or
// the standard Codex CLI/Pi auth locations otherwise.
func NewChatGPTProvider(proxyURL string) *ChatGPTProvider {
	return NewChatGPTProviderWithAuthPath(proxyURL, os.Getenv("CHATGPT_AUTH_FILE"))
}

// NewChatGPTProviderWithAuthPath is useful for deployments and tests that keep
// OAuth credentials outside the default per-user locations.
func NewChatGPTProviderWithAuthPath(proxyURL, authPath string) *ChatGPTProvider {
	paths := chatGPTAuthPaths(authPath)
	p := &ChatGPTProvider{
		authPath:  firstPath(paths),
		authPaths: paths,
	}

	// Clone the default transport so direct connections still honor the normal
	// system proxy/TLS settings. An explicit provider proxy takes precedence.
	transport := &http.Transport{}
	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = defaultTransport.Clone()
	}
	if proxyURL != "" {
		if proxy, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(proxy)
		}
	}
	// 不用 http.Client.Timeout：它把整条 SSE 流计入总时长，长 Codex 生成会被
	// 砍断。改为 ResponseHeaderTimeout 限制首字节，流式停滞由看门狗中止
	transport.ResponseHeaderTimeout = 300 * time.Second
	p.client = &http.Client{Transport: transport}

	// Load credentials at startup so an unconfigured subscription does not
	// appear as a usable model in /v1/models.
	_ = p.loadAuth()

	return p
}

func chatGPTAuthPaths(explicit string) []string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return []string{expandPath(explicit)}
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".codex", "auth.json"),
		filepath.Join(home, ".pi", "agent", "auth.json"),
	}
}

func expandPath(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func firstPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func (p *ChatGPTProvider) Name() string    { return "chatgpt" }
func (p *ChatGPTProvider) URL() string     { return chatgptCodexURL }
func (p *ChatGPTProvider) APIKey() string  { return p.getAccessToken() }
func (p *ChatGPTProvider) UseBearer() bool { return true }
func (p *ChatGPTProvider) Capabilities() []string {
	return []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning}
}

func (p *ChatGPTProvider) getAccessToken() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.auth != nil {
		return p.auth.AccessToken
	}
	return ""
}

func (p *ChatGPTProvider) getAccountID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.auth != nil {
		return p.auth.AccountID
	}
	return ""
}

// AuthPath returns the selected credential file without exposing its contents.
func (p *ChatGPTProvider) AuthPath() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.authPath
}

// loadAuth loads either the Codex CLI or Pi credential format.
func (p *ChatGPTProvider) loadAuth() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadAuthLocked()
}

func (p *ChatGPTProvider) loadAuthLocked() error {
	if len(p.authPaths) == 0 {
		return fmt.Errorf("ChatGPT OAuth auth path is not configured")
	}

	var parseErrors []string
	for _, path := range p.authPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", path, err))
			}
			continue
		}

		auth, format, err := parseChatGPTAuth(data)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		p.auth = auth
		p.authPath = path
		p.authFormat = format
		return nil
	}

	if len(parseErrors) > 0 {
		return fmt.Errorf("no usable ChatGPT OAuth credentials: %s", strings.Join(parseErrors, "; "))
	}
	return fmt.Errorf("ChatGPT OAuth credentials not found (checked %s)", strings.Join(p.authPaths, ", "))
}

func parseChatGPTAuth(data []byte) (*chatgptAuth, chatgptAuthFormat, error) {
	var file chatgptAuthFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, "", fmt.Errorf("parse auth.json: %w", err)
	}

	if file.Tokens != nil && file.Tokens.AccessToken != "" {
		return makeChatGPTAuth(
			file.Tokens.AccessToken,
			file.Tokens.RefreshToken,
			file.Tokens.AccountID,
		), chatgptAuthFormatCodex, nil
	}
	if file.OpenAICodex != nil {
		access := file.OpenAICodex.Access
		if access == "" {
			access = file.OpenAICodex.AccessToken
		}
		refresh := file.OpenAICodex.Refresh
		if refresh == "" {
			refresh = file.OpenAICodex.RefreshToken
		}
		accountID := file.OpenAICodex.AccountID
		if accountID == "" {
			accountID = file.OpenAICodex.AccountIDPascal
		}
		if access != "" {
			return makeChatGPTAuth(access, refresh, accountID), chatgptAuthFormatPi, nil
		}
	}
	if file.AccessToken != "" {
		return makeChatGPTAuth(file.AccessToken, file.RefreshToken, file.AccountID), chatgptAuthFormatCodex, nil
	}
	return nil, "", fmt.Errorf("auth.json has no ChatGPT access token")
}

func makeChatGPTAuth(accessToken, refreshToken, accountID string) *chatgptAuth {
	if accountID == "" {
		accountID = extractAccountIDFromJWT(accessToken)
	}
	return &chatgptAuth{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		AccountID:    accountID,
	}
}

// refreshAuth refreshes an expired access token. staleAccessToken prevents
// concurrent 401 responses from rotating the refresh token more than once.
func (p *ChatGPTProvider) refreshAuth(ctx context.Context, staleAccessToken string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.auth == nil || p.auth.RefreshToken == "" {
		return fmt.Errorf("no ChatGPT refresh token available")
	}
	if staleAccessToken != "" && p.auth.AccessToken != staleAccessToken {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	values := url.Values{}
	values.Set("grant_type", "refresh_token")
	values.Set("refresh_token", p.auth.RefreshToken)
	values.Set("client_id", chatgptClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("send refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return NewHTTPError("chatgpt-auth", resp, body)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse refresh response: %w", err)
	}
	if result.AccessToken == "" {
		return fmt.Errorf("refresh response missing access_token")
	}

	accountID := extractAccountIDFromJWT(result.AccessToken)
	if accountID == "" {
		accountID = p.auth.AccountID
	}
	if accountID == "" {
		return fmt.Errorf("refresh response has no ChatGPT account id")
	}

	p.auth.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		p.auth.RefreshToken = result.RefreshToken
	}
	p.auth.AccountID = accountID

	// Refresh-token rotation must survive a gateway restart. A failure to write
	// the local cache does not invalidate the freshly refreshed in-memory token.
	_ = p.persistAuthLocked()
	return nil
}

func extractAccountIDFromJWT(tokenOrPayload string) string {
	payload := tokenOrPayload
	parts := strings.Split(tokenOrPayload, ".")
	if len(parts) == 3 {
		payload = parts[1]
	}

	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(payload)
	}
	if err != nil {
		return ""
	}

	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return ""
	}
	return claims.Auth.AccountID
}

func (p *ChatGPTProvider) persistAuthLocked() error {
	if p.auth == nil || p.authPath == "" || p.authFormat == "" {
		return nil
	}

	data, err := os.ReadFile(p.authPath)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	if root == nil {
		root = make(map[string]json.RawMessage)
	}

	setString := func(values map[string]json.RawMessage, key, value string) {
		encoded, _ := json.Marshal(value)
		values[key] = encoded
	}

	if p.authFormat == chatgptAuthFormatPi {
		values := make(map[string]json.RawMessage)
		if raw := root["openai-codex"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &values)
		}
		setString(values, "access", p.auth.AccessToken)
		setString(values, "refresh", p.auth.RefreshToken)
		setString(values, "accountId", p.auth.AccountID)
		root["openai-codex"], err = json.Marshal(values)
	} else {
		values := make(map[string]json.RawMessage)
		if raw := root["tokens"]; len(raw) > 0 {
			_ = json.Unmarshal(raw, &values)
		}
		setString(values, "access_token", p.auth.AccessToken)
		setString(values, "refresh_token", p.auth.RefreshToken)
		setString(values, "account_id", p.auth.AccountID)
		root["tokens"], err = json.Marshal(values)
	}
	if err != nil {
		return err
	}

	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p.authPath), "."+filepath.Base(p.authPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(updated); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, p.authPath)
}

// ForwardRequest is intentionally unsupported for this subscription API. The
// ChatGPT Codex backend exposes the native Responses streaming contract.
func (p *ChatGPTProvider) ForwardRequest(context.Context, *Request) (*Response, error) {
	return nil, fmt.Errorf("ChatGPT provider only supports streaming via Responses API passthrough")
}

// IsHealthy checks whether a usable OAuth credential is loaded. It does not
// verify that ChatGPT still accepts it; use Probe for upstream availability.
func (p *ChatGPTProvider) IsHealthy(context.Context) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.auth != nil && p.auth.AccessToken != "" && p.auth.AccountID != ""
}

// Probe sends a minimal Responses request and reports whether ChatGPT accepts
// the credential. A locally present token can still be revoked or rate limited,
// so only a real request distinguishes "configured" from "usable".
func (p *ChatGPTProvider) Probe(ctx context.Context) ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	accessToken := p.getAccessToken()
	accountID := p.getAccountID()
	if accessToken == "" || accountID == "" {
		if err := p.loadAuth(); err != nil {
			return ProbeResult{Status: ProbeOffline, Detail: "无可用的 ChatGPT OAuth 凭据"}
		}
		accessToken = p.getAccessToken()
		accountID = p.getAccountID()
	}
	if accessToken == "" || accountID == "" {
		return ProbeResult{Status: ProbeOffline, Detail: "ChatGPT 凭据缺少 account id"}
	}

	body, err := json.Marshal(map[string]any{
		"model":        chatgptProbeModel(),
		"instructions": "You are a helpful assistant.",
		"input":        []map[string]any{{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": probePrompt}}}},
		"stream":       true,
		"store":        false,
	})
	if err != nil {
		return ProbeResult{Status: ProbeUnknown, Detail: "无法构造探测请求"}
	}

	start := time.Now()
	resp, err := p.doResponsesRequest(ctx, body, accessToken, accountID)
	latency := time.Since(start)
	if err != nil {
		return ProbeResult{Status: ProbeDegraded, Detail: boundedErrorSummary(err), Latency: latency}
	}
	defer resp.Body.Close()
	// 只读状态码：探测不消费 SSE 正文，避免为一次健康检查生成完整回答。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return ProbeResult{Status: ProbeOnline, Detail: "上游返回 2xx", Latency: latency}
	}
	return ProbeResult{
		Status:     classifyProbeStatus(resp.StatusCode),
		StatusCode: resp.StatusCode,
		Detail:     fmt.Sprintf("ChatGPT 返回 HTTP %d", resp.StatusCode),
		Latency:    latency,
	}
}

// chatgptProbeModel 选择目录里第一个模型作为探测目标。
func chatgptProbeModel() string {
	for _, model := range chatGPTModelCatalog {
		if id := chatGPTUpstreamModel(model.ID); id != "" {
			return id
		}
	}
	return "gpt-5.6-sol"
}

// ForwardRawResponsesStream forwards a native Responses request to ChatGPT.
func (p *ChatGPTProvider) ForwardRawResponsesStream(ctx context.Context, reqBody json.RawMessage, w io.Writer) error {
	accessToken := p.getAccessToken()
	accountID := p.getAccountID()
	if accessToken == "" || accountID == "" {
		if err := p.loadAuth(); err != nil {
			return fmt.Errorf("no usable ChatGPT OAuth credential: %w", err)
		}
		accessToken = p.getAccessToken()
		accountID = p.getAccountID()
	}
	if accessToken == "" || accountID == "" {
		return fmt.Errorf("ChatGPT OAuth credential has no account id")
	}

	var reqMap map[string]json.RawMessage
	if err := json.Unmarshal(reqBody, &reqMap); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	// The Codex endpoint requires both fields even when the caller omitted them.
	reqMap["store"] = json.RawMessage(`false`)
	reqMap["stream"] = json.RawMessage(`true`)
	if _, ok := reqMap["instructions"]; !ok {
		reqMap["instructions"] = json.RawMessage(`"You are a helpful coding assistant."`)
	}
	if err := normalizeChatGPTRequest(reqMap); err != nil {
		return fmt.Errorf("normalize request input: %w", err)
	}
	if rawModel, ok := reqMap["model"]; ok {
		var modelID string
		if json.Unmarshal(rawModel, &modelID) == nil {
			upstreamModel := chatGPTUpstreamModel(modelID)
			if upstreamModel != modelID {
				encodedModel, _ := json.Marshal(upstreamModel)
				reqMap["model"] = encodedModel
			}
		}
	}

	finalBody, err := json.Marshal(reqMap)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	resp, err := p.doResponsesRequest(ctx, finalBody, accessToken, accountID)
	if err != nil {
		return fmt.Errorf("send request to ChatGPT: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if refreshErr := p.refreshAuth(ctx, accessToken); refreshErr == nil {
			accessToken = p.getAccessToken()
			accountID = p.getAccountID()
			resp, err = p.doResponsesRequest(ctx, finalBody, accessToken, accountID)
			if err != nil {
				return fmt.Errorf("retry after ChatGPT token refresh: %w", err)
			}
		} else {
			return NewHTTPError(p.Name(), &http.Response{StatusCode: http.StatusUnauthorized}, body)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return NewHTTPError(p.Name(), resp, body)
	}

	return copyValidatedStream(p.Name(), w, resp.Body)
}

func (p *ChatGPTProvider) doResponsesRequest(ctx context.Context, body []byte, accessToken, accountID string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", accountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	// Match Pi's subscription client marker so the backend sees the same
	// request shape as the supported openai-codex provider.
	req.Header.Set("originator", "pi")
	req.Header.Set("User-Agent", "go-llm-gateway/1.0")
	return p.client.Do(req)
}

// ChatGPTPassthroughMarker marks providers that natively speak Responses SSE.
type ChatGPTPassthroughMarker interface {
	ForwardRawResponsesStream(ctx context.Context, reqBody json.RawMessage, w io.Writer) error
}
