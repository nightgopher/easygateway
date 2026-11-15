package provider

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProviderConfigAppliesRequestTimeout(t *testing.T) {
	t.Setenv("GLM_API_KEY", "glm-test-key")

	configured, err := NewOpenAIProviderFromConfig(&ProviderConfig{
		Name:                  "glm",
		Type:                  "openai",
		URL:                   "https://example.invalid/v1/chat/completions",
		APIKeyEnv:             "GLM_API_KEY",
		RequestTimeoutSeconds: 900,
	})
	if err != nil {
		t.Fatalf("expected provider to configure: %v", err)
	}

	openAI, ok := configured.(*OpenAIProvider)
	if !ok {
		t.Fatalf("expected *OpenAIProvider, got %T", configured)
	}
	// 超时语义：限制首字节时间 + 流式空闲看门狗，不再是 client 总时长
	if got := openAI.requestTimeout(); got != 900*time.Second {
		t.Fatalf("request timeout = %s, want 15m", got)
	}
	transport, ok := openAI.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", openAI.client.Transport)
	}
	if transport.ResponseHeaderTimeout != 900*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %s, want 15m", transport.ResponseHeaderTimeout)
	}
	if openAI.client.Timeout != 0 {
		t.Fatalf("client.Timeout 应保持未设置（流式总时长不受限），实际 %s", openAI.client.Timeout)
	}
}

// 上游模型 ID 是对外主名，历史别名同时可用（旧客户端不失效）。
func TestSetupProvidersExposesUpstreamNameAndLegacyAlias(t *testing.T) {
	router := NewRouter(log.New(io.Discard, "", 0))
	path := writeTempProvidersConfig(t, `
providers:
  - name: glm
    type: openai
    url: https://example.invalid/v1/chat/completions
    api_key_env: GLM_TEST_KEY
    models:
      - id: glm-4.7
        aliases: [glm-haiku]
        capabilities: [text]
`)
	t.Setenv("GLM_TEST_KEY", "test-key")

	if _, err := SetupProvidersFromConfig(router, path, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("setup providers: %v", err)
	}

	for _, modelName := range []string{"glm-4.7", "glm-haiku"} {
		providers, err := router.Route(modelName)
		if err != nil {
			t.Fatalf("route %s: %v", modelName, err)
		}
		if len(providers) != 1 {
			t.Fatalf("route %s returned %d providers, want 1", modelName, len(providers))
		}
		// 包装器把请求模型改写为绑定的上游名，两条入口最终都发往 glm-4.7。
		bound, ok := providers[0].(BoundModelProvider)
		if !ok {
			t.Fatalf("route %s provider does not implement BoundModelProvider", modelName)
		}
		if got := bound.BoundModel(); got != "glm-4.7" {
			t.Fatalf("route %s bound model = %q, want %q", modelName, got, "glm-4.7")
		}
	}
}

func writeTempProvidersConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write providers.yaml: %v", err)
	}
	return path
}

// 账号渠道（type: channel）在 providers.yaml 里声明，但实例必须由注入的工厂构造。
// 这条路径曾经失效：channel 不是 registry 认识的类型，若先走 registry.Create 会
// 报 "unknown provider type: channel" 并 continue，导致渠道模型一个都注册不上。
func TestSetupProvidersRegistersChannelModels(t *testing.T) {
	previous := channelProviderFactory
	SetChannelProviderFactory(func(channelName, modelID string) (Provider, bool) {
		if channelName != "minimax" {
			return nil, false
		}
		return &stubChatProvider{name: "channel-" + modelID}, true
	})
	t.Cleanup(func() { channelProviderFactory = previous })

	router := NewRouter(log.New(io.Discard, "", 0))
	path := writeTempProvidersConfig(t, `
providers:
  - name: minimax
    type: channel
    models:
      - id: MiniMax-M3
        aliases: [minimax-m3-alias]
        capabilities: [text]
`)

	if _, err := SetupProvidersFromConfig(router, path, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("setup providers: %v", err)
	}

	for _, modelName := range []string{"MiniMax-M3", "minimax-m3-alias"} {
		providers, err := router.Route(modelName)
		if err != nil {
			t.Fatalf("route %s: %v (渠道模型未注册)", modelName, err)
		}
		if len(providers) != 1 {
			t.Fatalf("route %s returned %d providers, want 1", modelName, len(providers))
		}
		bound, ok := providers[0].(BoundModelProvider)
		if !ok {
			t.Fatalf("route %s provider does not implement BoundModelProvider", modelName)
		}
		if got := bound.BoundModel(); got != "MiniMax-M3" {
			t.Fatalf("route %s bound model = %q, want MiniMax-M3", modelName, got)
		}
	}
}

// stubChatProvider 是测试用的最小 Provider，仅用于断言注册/路由结果。
type stubChatProvider struct{ name string }

func (p *stubChatProvider) Name() string                   { return p.name }
func (p *stubChatProvider) URL() string                    { return "" }
func (p *stubChatProvider) APIKey() string                 { return "" }
func (p *stubChatProvider) UseBearer() bool                { return false }
func (p *stubChatProvider) IsHealthy(context.Context) bool { return true }
func (p *stubChatProvider) ForwardRequest(context.Context, *Request) (*Response, error) {
	return nil, errors.New("stub")
}
