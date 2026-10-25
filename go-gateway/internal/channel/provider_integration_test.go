package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/internal/provider"
)

// 这组测试把「渠道账号池 → provider 实例 → 上游请求」整条链接起来跑，是单测里
// 唯一能覆盖鉴权头形态的地方：channel 包自身的测试只到登录/解析为止，看不出
// provider 到底发的是 x-api-key 还是 Authorization: Bearer。
//
// 为什么放 channel 内部包：NewAccounts 需要未导出的 *oauthClient 与 refreshFn，
// 外部测试包构造不出来；provider 不 import channel（靠 AuthSource 结构化对接），
// 反向依赖不会成环，所以这里可以同时引用两边。

// channelAccounts 造一个已登录的账号池（存储里直接塞一条有效凭据）。
func channelAccounts(t *testing.T, name, token string) *Accounts {
	t.Helper()
	store := NewStore(t.TempDir())
	if err := store.Save(name, []Credential{{
		AccessToken:  token,
		RefreshToken: "refresh-not-used",
		ExpiresAt:    time.Now().Add(2 * time.Hour),
	}}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	return NewAccounts(name, store, newOAuthClient(), func(context.Context, Credential) (Credential, error) {
		return Credential{}, errors.New("test stub: refresh not configured")
	})
}

// minimaxProvider 复刻 channels.go 里的装配，用来断言真实装配产生的请求形态。
// 与 main 里的工厂保持一致：这是刻意的重复，一旦 channels.go 改错，这里会失败。
func minimaxProvider(accounts *Accounts, url string) *provider.AnthropicProvider {
	return provider.NewAnthropicProvider(&provider.Config{
		Name:      "MiniMax-M3",
		URL:       url,
		Auth:      accounts,
		UseBearer: true,
		Transform: MiniMaxTransformRequest,
	})
}

func clineProvider(accounts *Accounts, url string) *provider.OpenAIProvider {
	return provider.NewOpenAIProvider(&provider.Config{
		Name:         "cline-free/mimo-v2.6-flash",
		URL:          url,
		Auth:         accounts,
		ExtraHeaders: ClineClientHeaders(),
	})
}

// TestMiniMaxProviderSendsBearerAuth 是回归测试：minimax 上游要求
// Authorization: Bearer <token>，绝不能发 Anthropic 官方的 x-api-key。
// 之前 channels.go 漏设 UseBearer 时会发 x-api-key，对真实上游必然 401。
func TestMiniMaxProviderSendsBearerAuth(t *testing.T) {
	var gotAuth, gotAPIKey, gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M3","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	accounts := channelAccounts(t, "minimax", "mini-token")
	p := minimaxProvider(accounts, server.URL)

	req := mustRequest(t, `{"model":"MiniMax-M3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("ForwardRequest failed: %v", err)
	}

	if gotAuth != "Bearer mini-token" {
		t.Fatalf("minimax 必须用 Authorization: Bearer，得到 %q", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("minimax 不应发送 x-api-key，得到 %q", gotAPIKey)
	}
	// Anthropic Messages 协议版本头仍需保留。
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version 头缺失: %q", gotVersion)
	}
}

// TestMiniMaxProviderAppliesAdaptiveTransform 验证 M3.1 的请求体改写确实作用在
// 发往上游的 body 上（单测只覆盖了纯函数，这里覆盖真实发送路径）。
func TestMiniMaxProviderAppliesAdaptiveTransform(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"MiniMax-M3.1-Flash-Preview","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`))
	}))
	defer server.Close()

	accounts := channelAccounts(t, "minimax", "tok")
	p := minimaxProvider(accounts, server.URL)

	req := mustRequest(t, `{"model":"MiniMax-M3.1-Flash-Preview","max_tokens":16,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("ForwardRequest failed: %v", err)
	}

	thinking, _ := gotBody["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "adaptive" {
		t.Fatalf("发往上游的 thinking 应被改写为 adaptive，实际 %v", gotBody["thinking"])
	}
}

// TestClineProviderSendsWorkOSBearerAndHeaders 验证 cline 的鉴权与复刻头：
// Authorization 必须是 `Bearer workos:<token>`，四个客户端头必须齐全。
func TestClineProviderSendsWorkOSBearerAndHeaders(t *testing.T) {
	var gotAuth string
	gotHeaders := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		for name := range ClineClientHeaders() {
			gotHeaders[name] = r.Header.Get(name)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"cline-free/mimo-v2.6-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	accounts := channelAccounts(t, "cline", "workos:cline-token")
	p := clineProvider(accounts, server.URL)

	req := mustRequest(t, `{"model":"cline-free/mimo-v2.6-flash","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("ForwardRequest failed: %v", err)
	}

	if gotAuth != "Bearer workos:cline-token" {
		t.Fatalf("cline Authorization 应为 Bearer workos:<token>，得到 %q", gotAuth)
	}
	for name, want := range ClineClientHeaders() {
		if gotHeaders[name] != want {
			t.Fatalf("cline 客户端头 %s 应为 %q，得到 %q", name, want, gotHeaders[name])
		}
	}
}

// TestClineProviderPrefixesBareToken 验证账号池里存的是裸 token 时，渠道层补上
// workos: 前缀（parseClineToken 负责），provider 只做 Bearer 拼接。
func TestClineProviderPrefixesBareToken(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	// 模拟真实登录产物：凭据里已经是带前缀的值。
	accounts := channelAccounts(t, "cline", "workos:abc")
	p := clineProvider(accounts, server.URL)
	req := mustRequest(t, `{"model":"cline-free/mimo-v2.6-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("ForwardRequest failed: %v", err)
	}
	if gotAuth != "Bearer workos:abc" {
		t.Fatalf("前缀应保持单份，得到 %q", gotAuth)
	}
}

// TestChannelProviderUnavailableWhenLoggedOut 验证未登录时 provider 报告不可用，
// 且不发请求——路由层据此跳过，不必先撞一次注定失败的请求。
func TestChannelProviderUnavailableWhenLoggedOut(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	empty := NewAccounts("minimax", NewStore(t.TempDir()), newOAuthClient(), nil)
	p := minimaxProvider(empty, server.URL)

	if p.Available() {
		t.Fatalf("未登录时 Available 应为 false")
	}
	req := mustRequest(t, `{"model":"MiniMax-M3","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err == nil {
		t.Fatalf("未登录时应直接报错，不发请求")
	}
	if hits != 0 {
		t.Fatalf("未登录时不应触达上游，实际 %d 次", hits)
	}
}

// TestChannelProviderRefreshOn401 验证上游返回 401 后 provider 会用账号池的
// refresh 续期并重试一次；续期成功则第二次带新令牌。
func TestChannelProviderRefreshOn401(t *testing.T) {
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if len(auths) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"MiniMax-M3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	store := NewStore(t.TempDir())
	// 凭据已过期（保证 401 后 refresh 会被触发），并在假上游上验证新令牌生效。
	if err := store.Save("minimax", []Credential{{
		AccessToken:  "old-token",
		RefreshToken: "r",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 用可注入的续期函数把令牌换成 new-token（真实续期需网络，这里只测调度）。
	accounts := NewAccounts("minimax", store, newOAuthClient(), func(_ context.Context, cred Credential) (Credential, error) {
		next := cred
		next.AccessToken = "new-token"
		next.ExpiresAt = time.Now().Add(2 * time.Hour)
		return next, nil
	})

	p := minimaxProvider(accounts, server.URL)
	req := mustRequest(t, `{"model":"MiniMax-M3","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("续期后应成功，得到 %v", err)
	}

	if len(auths) != 2 {
		t.Fatalf("应重试一次（共 2 次请求），实际 %d 次: %v", len(auths), auths)
	}
	if auths[0] != "Bearer old-token" || auths[1] != "Bearer new-token" {
		t.Fatalf("续期前后令牌不正确: %v", auths)
	}
}

func mustRequest(t *testing.T, body string) *provider.Request {
	t.Helper()
	var req provider.Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return &req
}

// lobsterProvider 复刻 channels.go 里 lobster 的装配：OpenAI 协议 + 客户端复刻头。
func lobsterProvider(accounts *Accounts, url string, headers map[string]string) *provider.OpenAIProvider {
	return provider.NewOpenAIProvider(&provider.Config{
		Name:         "kimi-k2.7-code",
		URL:          url,
		Auth:         accounts,
		ExtraHeaders: headers,
	})
}

// TestLobsterProviderSendsBearerAndClientHeaders 验证 lobster 上游要求
// Authorization: Bearer 且三个客户端复刻头齐全（缺版本头会被当非官方客户端拒）。
func TestLobsterProviderSendsBearerAndClientHeaders(t *testing.T) {
	var gotAuth string
	gotHeaders := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		for name := range lobsterClientHeadersForTest() {
			gotHeaders[name] = r.Header.Get(name)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"kimi-k2.7-code","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	accounts := channelAccounts(t, "lobster", "lobster-token")
	headers := lobsterClientHeadersForTest()
	p := lobsterProvider(accounts, server.URL, headers)

	req := mustRequest(t, `{"model":"kimi-k2.7-code","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("ForwardRequest failed: %v", err)
	}
	if gotAuth != "Bearer lobster-token" {
		t.Fatalf("lobster 必须用 Authorization: Bearer，得到 %q", gotAuth)
	}
	for name, want := range headers {
		if gotHeaders[name] != want {
			t.Fatalf("lobster 客户端头 %s 应为 %q，得到 %q", name, want, gotHeaders[name])
		}
	}
}

// lobsterClientHeadersForTest 返回与生产一致的复刻头（版本用固定值，避免测试触网）。
func lobsterClientHeadersForTest() map[string]string {
	return map[string]string{
		"User-Agent":                      "LobsterAI/0.1.0",
		"X-LobsterAI-Client-Capabilities": "kimi-k3-agentic-v1,thinking-level-control-v1",
		"X-LobsterAI-Client-Version":      "2026.9.4",
	}
}

// TestChannelProviderRefreshOn401Lobster 验证 lobster 同样受益于账号池续期：
// 401 后 provider 用注入的 refresh 换新令牌并重试一次。
func TestChannelProviderRefreshOn401Lobster(t *testing.T) {
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if len(auths) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	store := NewStore(t.TempDir())
	writeCreds(t, store, "lobster", Credential{
		AccessToken:  "old-token",
		RefreshToken: "r",
		ExpiresAt:    time.Now().Add(-time.Hour),
	})
	accounts := NewAccounts("lobster", store, newOAuthClient(), func(_ context.Context, cred Credential) (Credential, error) {
		next := cred
		next.AccessToken = "new-token"
		next.ExpiresAt = time.Now().Add(2 * time.Hour)
		return next, nil
	})

	p := lobsterProvider(accounts, server.URL, nil)
	req := mustRequest(t, `{"model":"kimi-k2.7-code","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("续期后应成功，得到 %v", err)
	}
	if len(auths) != 2 || auths[0] != "Bearer old-token" || auths[1] != "Bearer new-token" {
		t.Fatalf("续期前后令牌不正确: %v", auths)
	}
}

// buddyProvider 复刻 channels.go 里 codebuddy/workbuddy 的装配：OpenAI 协议 +
// 按模型解析的复刻头 + 剥离命名空间前缀的请求体改写。
func buddyProvider(channel *BuddyChannel, accounts *Accounts, url, model string) *provider.OpenAIProvider {
	return provider.NewOpenAIProvider(&provider.Config{
		Name:         model,
		URL:          url,
		Auth:         accounts,
		ExtraHeaders: channel.ClientHeaders(model),
		Transform:    channel.TransformRequest,
	})
}

// TestBuddyProviderSendsBearerAndClientHeaders 验证 buddy 推理请求的鉴权与复刻头。
// 这些头是上游识别官方客户端的唯一依据，缺任意一项都会被拒。
func TestBuddyProviderSendsBearerAndClientHeaders(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	gotHeaders := map[string]string{}

	// 渠道实例只用来取头与做改写，端点指向假上游。
	channel := newBuddyForTest(t, "http://127.0.0.1:1", buddyChannelWorkBuddy)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		for name := range channel.ClientHeaders("workbuddy/gpt-5.6-sol") {
			gotHeaders[name] = r.Header.Get(name)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	accounts := channelAccounts(t, "workbuddy", "buddy-token")
	p := buddyProvider(channel, accounts, server.URL, "workbuddy/gpt-5.6-sol")

	// 请求体用对外模型名（带前缀）：上游收到的必须是剥掉前缀的真名。
	req := mustRequest(t, `{"model":"workbuddy/gpt-5.6-sol","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("ForwardRequest failed: %v", err)
	}

	if gotAuth != "Bearer buddy-token" {
		t.Fatalf("buddy 必须用 Authorization: Bearer，得到 %q", gotAuth)
	}
	if gotBody["model"] != "gpt-5.6-sol" {
		t.Fatalf("发往上游的模型名应剥掉渠道前缀，得到 %v", gotBody["model"])
	}
	for name, want := range channel.ClientHeaders("workbuddy/gpt-5.6-sol") {
		if got := gotHeaders[name]; got != want {
			t.Fatalf("buddy 复刻头 %s 应为 %q，得到 %q", name, want, got)
		}
	}
	// User-Agent 必须被渠道覆盖掉网关默认 UA，否则上游按非官方客户端处理。
	if got := gotHeaders["User-Agent"]; got != buddyWorkBuddyUAIntl {
		t.Fatalf("User-Agent 应被渠道覆盖为 WorkBuddy 国际版形态，得到 %q", got)
	}
}

// TestBuddyProviderUnavailableWhenLoggedOut 验证未登录时 provider 报告不可用且不触达上游。
func TestBuddyProviderUnavailableWhenLoggedOut(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	channel := newBuddyForTest(t, "http://127.0.0.1:1", buddyChannelCodeBuddy)
	empty := NewAccounts("codebuddy", NewStore(t.TempDir()), newOAuthClient(), nil)
	p := buddyProvider(channel, empty, server.URL, "codebuddy/glm-5.3")

	if p.Available() {
		t.Fatalf("未登录时 Available 应为 false")
	}
	req := mustRequest(t, `{"model":"codebuddy/glm-5.3","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err == nil {
		t.Fatalf("未登录时应直接报错，不发请求")
	}
	if hits != 0 {
		t.Fatalf("未登录时不应触达上游，实际 %d 次", hits)
	}
}

// TestBuddyProviderRefreshOn401 验证 401 后账号池续期并重试一次；buddy 的续期
// 走 refreshCredential（真实请求），这里注入一个假账号池来只测调度。
func TestBuddyProviderRefreshOn401(t *testing.T) {
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if len(auths) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"expired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	store := NewStore(t.TempDir())
	writeCreds(t, store, "codebuddy", Credential{
		AccessToken:  "old-token",
		RefreshToken: "r",
		ExpiresAt:    time.Now().Add(-time.Hour),
	})
	accounts := NewAccounts("codebuddy", store, newOAuthClient(), func(_ context.Context, cred Credential) (Credential, error) {
		next := cred
		next.AccessToken = "new-token"
		next.ExpiresAt = time.Now().Add(2 * time.Hour)
		return next, nil
	})

	channel := newBuddyForTest(t, "http://127.0.0.1:1", buddyChannelCodeBuddy)
	p := buddyProvider(channel, accounts, server.URL, "codebuddy/glm-5.3")
	req := mustRequest(t, `{"model":"codebuddy/glm-5.3","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := p.ForwardRequest(context.Background(), req); err != nil {
		t.Fatalf("续期后应成功，得到 %v", err)
	}
	if len(auths) != 2 || auths[0] != "Bearer old-token" || auths[1] != "Bearer new-token" {
		t.Fatalf("续期前后令牌不正确: %v", auths)
	}
}
