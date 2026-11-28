package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// ChatGPT Codex 后端提供账号维度的模型清单端点（/codex/models），返回当前
// 登录账号实际可用的模型——包括尚未写入静态回退目录的新模型。请求需要
// 声明一个 codex CLI 版本号供服务端做能力协商。
var chatgptClientVersion = "0.55.0"

// chatgptModelsEndpoint is a var so tests can point it at a stub server.
var chatgptModelsEndpoint = chatgptCodexBase + "/codex/models?client_version=" + chatgptClientVersion

var (
	chatGPTCatalogMu      sync.RWMutex
	chatGPTCatalogDynamic []ChatGPTModel
)

// ChatGPTModelCatalog returns the live catalog fetched from the Codex backend
// when one is available, falling back to the curated static catalog.
func ChatGPTModelCatalog() []ChatGPTModel {
	chatGPTCatalogMu.RLock()
	live := chatGPTCatalogDynamic
	chatGPTCatalogMu.RUnlock()
	return cloneChatGPTCatalog(live)
}

func setChatGPTModelCatalog(models []ChatGPTModel) {
	chatGPTCatalogMu.Lock()
	chatGPTCatalogDynamic = cloneChatGPTCatalog(models)
	chatGPTCatalogMu.Unlock()
}

func cloneChatGPTCatalog(models []ChatGPTModel) []ChatGPTModel {
	out := make([]ChatGPTModel, len(models))
	for i, model := range models {
		model.Capabilities = append([]string(nil), model.Capabilities...)
		model.InputModalities = append([]string(nil), model.InputModalities...)
		out[i] = model
	}
	return out
}

// RefreshModelCatalog fetches the account-scoped model list from the ChatGPT
// Codex backend and replaces the in-memory catalog. Slugs already present in
// the curated fallback keep their curated metadata (capabilities, context
// sizes); unknown slugs get generic metadata so newly released upstream
// models surface without waiting for a release.
func (p *ChatGPTProvider) RefreshModelCatalog(ctx context.Context) ([]ChatGPTModel, error) {
	slugs, err := p.fetchCodexModels(ctx)
	if err != nil {
		return nil, err
	}
	merged := mergeChatGPTCatalog(slugs)
	setChatGPTModelCatalog(merged)
	return cloneChatGPTCatalog(merged), nil
}

func (p *ChatGPTProvider) fetchCodexModels(ctx context.Context) ([]string, error) {
	token := p.getAccessToken()
	body, status, err := p.requestCodexModels(ctx, token)
	if status == http.StatusUnauthorized {
		// 与 /responses 相同：令牌可能过期，刷新一次再试。
		if refreshErr := p.refreshAuth(ctx, token); refreshErr == nil {
			body, status, err = p.requestCodexModels(ctx, p.getAccessToken())
		} else {
			err = refreshErr
		}
	}
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("codex models endpoint returned %d", status)
	}
	return parseCodexModels(body)
}

func (p *ChatGPTProvider) requestCodexModels(ctx context.Context, accessToken string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatgptModelsEndpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("chatgpt-account-id", p.getAccountID())
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "pi")
	req.Header.Set("User-Agent", "go-llm-gateway/1.0")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	return body, resp.StatusCode, nil
}

// parseCodexModels 兼容多种返回形状：顶层数组、{"models": [...]}、
// {"data": [...]}，条目可为字符串或对象（id 依次尝试 id/model/slug/name）。
func parseCodexModels(body []byte) ([]string, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode codex models response: %w", err)
	}
	entries := codexModelsArray(root)
	var slugs []string
	for _, entry := range entries {
		switch e := entry.(type) {
		case string:
			if e != "" {
				slugs = append(slugs, e)
			}
		case map[string]any:
			id := codexString(e, "id", "model", "slug", "name")
			if id != "" {
				slugs = append(slugs, id)
			}
		}
	}
	if len(slugs) == 0 {
		return nil, fmt.Errorf("codex models response contained no models")
	}
	return slugs, nil
}

func codexModelsArray(root any) []any {
	switch v := root.(type) {
	case []any:
		return v
	case map[string]any:
		for _, key := range []string{"models", "data"} {
			if arr, ok := v[key].([]any); ok {
				return arr
			}
		}
		for _, child := range v {
			if m, ok := child.(map[string]any); ok {
				for _, key := range []string{"models", "data"} {
					if arr, ok := m[key].([]any); ok {
						return arr
					}
				}
			}
		}
	}
	return nil
}

func codexString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// mergeChatGPTCatalog 以账号实际可用清单为准；静态回退目录里有 curated
// 元数据（能力、上下文窗口）的条目沿用 curated 版本，新 slug 用通用元数据。
func mergeChatGPTCatalog(slugs []string) []ChatGPTModel {
	curated := make(map[string]ChatGPTModel, len(chatGPTModelCatalog))
	for _, m := range chatGPTModelCatalog {
		curated[m.ID] = m
	}
	out := make([]ChatGPTModel, 0, len(slugs))
	seen := make(map[string]bool, len(slugs))
	for _, slug := range slugs {
		if seen[slug] {
			continue
		}
		seen[slug] = true
		if m, ok := curated[slug]; ok {
			out = append(out, m)
			continue
		}
		out = append(out, ChatGPTModel{
			ID:              slug,
			Name:            slug,
			Capabilities:    []string{CapabilityText, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning},
			InputModalities: []string{"text"},
			MaxInputTokens:  272000,
			MaxOutputTokens: 128000,
		})
	}
	return out
}

// normalizeChatGPTSlugList 保留给未来的元数据扩展使用；当前条目元数据
// 以静态目录为准。
func normalizeChatGPTSlugList(raw json.RawMessage) []string {
	slugs, _ := parseCodexModels(raw)
	return slugs
}
