package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// rotatingAuth 是实现了账号轮转协议的测试 AuthSource：tokenA 收到 429 后
// 切到 tokenB，模拟 channel.Accounts 的换号行为。
type rotatingAuth struct {
	mu        sync.Mutex
	token     string
	rotations int
	nextOK    bool // 下一次 OnRateLimited 的返回值
}

func (a *rotatingAuth) BearerToken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.token
}

func (a *rotatingAuth) Refresh(context.Context) bool { return false }

func (a *rotatingAuth) OnRateLimited(_ context.Context, accessToken, _ string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rotations++
	if !a.nextOK {
		return false
	}
	a.nextOK = false
	a.token = "token-b"
	_ = accessToken
	return true
}

// rateLimitTransport 依次返回预设的状态码，并记录每个请求的鉴权头。
type rateLimitTransport struct {
	mu     sync.Mutex
	status []int // 每次请求依次弹出；耗尽后重复最后一个
	tokens []string
	calls  int
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.tokens = append(t.tokens, req.Header.Get("Authorization"))
	status := t.status[len(t.status)-1]
	if call <= len(t.status) {
		status = t.status[call-1]
	}
	t.mu.Unlock()
	header := make(http.Header)
	if status == http.StatusTooManyRequests {
		header.Set("Retry-After", "120")
	}
	return &http.Response{
		StatusCode: status,
		Status:     "auto",
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("{}")),
	}, nil
}

func TestPostWithAuthRotatesAccountOn429(t *testing.T) {
	auth := &rotatingAuth{token: "token-a", nextOK: true}
	transport := &rateLimitTransport{status: []int{http.StatusTooManyRequests, http.StatusOK}}
	client := &http.Client{Transport: transport}
	cfg := &Config{Name: "cline", Auth: auth}

	resp, err := postWithAuth(context.Background(), client, "cline", "https://example.invalid/v1/chat/completions", cfg, func(req *http.Request) {
		cfg.applyAuth(req, true)
	}, []byte("{}"))
	if err != nil {
		t.Fatalf("429 后换号重试应成功: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(transport.tokens) != 2 || transport.tokens[0] != "Bearer token-a" || transport.tokens[1] != "Bearer token-b" {
		t.Fatalf("两次请求应分别用旧/新账号令牌, got %v", transport.tokens)
	}
	if auth.rotations != 1 {
		t.Fatalf("rotations = %d, want 1", auth.rotations)
	}
}

func TestPostWithAuthReturns429WhenNoAccountLeft(t *testing.T) {
	auth := &rotatingAuth{token: "token-a", nextOK: false}
	transport := &rateLimitTransport{status: []int{http.StatusTooManyRequests}}
	client := &http.Client{Transport: transport}
	cfg := &Config{Name: "cline", Auth: auth}

	resp, err := postWithAuth(context.Background(), client, "cline", "https://example.invalid/v1/chat/completions", cfg, func(req *http.Request) {
		cfg.applyAuth(req, true)
	}, []byte("{}"))
	if err != nil {
		t.Fatalf("轮转失败应原样返回 429 响应: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if transport.calls != 1 {
		t.Fatalf("calls = %d, want 1 (无账号可换时不重试)", transport.calls)
	}
}

// TestPostWithAuthRotatesOnlyOncePerRequest：换到的账号再次 429 时不再继续
// 换号，把 429 交还路由层走 provider 级熔断与链上降级。
func TestPostWithAuthRotatesOnlyOncePerRequest(t *testing.T) {
	auth := &rotatingAuth{token: "token-a", nextOK: true}
	transport := &rateLimitTransport{status: []int{http.StatusTooManyRequests, http.StatusTooManyRequests}}
	client := &http.Client{Transport: transport}
	cfg := &Config{Name: "cline", Auth: auth}

	resp, err := postWithAuth(context.Background(), client, "cline", "https://example.invalid/v1/chat/completions", cfg, func(req *http.Request) {
		cfg.applyAuth(req, true)
	}, []byte("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if transport.calls != 2 {
		t.Fatalf("calls = %d, want 2 (每个请求只轮转一次)", transport.calls)
	}
}

// staticAuthWithoutRotation 验证：不实现轮转协议的 AuthSource 在 429 时行为
// 与从前完全一致（原样返回，不重试）。
func TestPostWithAuthNoRotationForPlainAuthSource(t *testing.T) {
	auth := NewStaticAuth("sk-static")
	transport := &rateLimitTransport{status: []int{http.StatusTooManyRequests}}
	client := &http.Client{Transport: transport}
	cfg := &Config{Name: "glm", Auth: auth}

	resp, err := postWithAuth(context.Background(), client, "glm", "https://example.invalid/v1/chat/completions", cfg, func(req *http.Request) {
		cfg.applyAuth(req, true)
	}, []byte("{}"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || transport.calls != 1 {
		t.Fatalf("status = %d, calls = %d; want 429 without retry", resp.StatusCode, transport.calls)
	}
}
