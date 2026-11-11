package provider

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 探测必须把上游的确定性拒绝判成 offline，把暂时性受限判成 degraded。
func TestClassifyProbeStatus(t *testing.T) {
	cases := []struct {
		code int
		want ProbeStatus
	}{
		{http.StatusOK, ProbeOnline},
		{http.StatusUnauthorized, ProbeOffline},     // 凭据失效
		{http.StatusForbidden, ProbeOffline},        // 账号无该模型权限
		{http.StatusNotFound, ProbeOffline},         // 模型不存在
		{http.StatusPaymentRequired, ProbeOffline},  // 欠费
		{http.StatusTooManyRequests, ProbeDegraded}, // 限流，可能自愈
		{http.StatusInternalServerError, ProbeDegraded},
		{http.StatusBadGateway, ProbeDegraded},
	}
	for _, tc := range cases {
		if got := classifyProbeStatus(tc.code); got != tc.want {
			t.Errorf("classifyProbeStatus(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// 探测失败必须计入熔断器，避免「探测说在线」的反面：探测说挂了却不影响调度。
func TestCheckProviderHealthRecordsFailureIntoCircuit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Access to model denied"}}`)
	}))
	defer server.Close()

	router := NewRouterWithCircuitConfig(log.New(io.Discard, "", 0), CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Minute,
		SuccessThreshold: 1,
	})
	router.RegisterProvider("glm-4.7", NewOpenAIProvider(&Config{Name: "glm-4.7", URL: server.URL, APIKey: "k"}))
	router.RegisterChain("glm-4.7", []string{"glm-4.7"})

	result := router.CheckProviderHealth(context.Background(), "glm-4.7")
	if result.Status != ProbeOffline {
		t.Fatalf("probe status = %q, want offline", result.Status)
	}

	status, ok := router.ProviderStatus("glm-4.7")
	if !ok {
		t.Fatal("provider status missing")
	}
	if status.ProbeStatus != ProbeOffline || !status.HasProbe {
		t.Fatalf("status probe fields = %+v, want recorded offline probe", status)
	}
	if status.State != CircuitOpen {
		t.Fatalf("circuit state = %q, want open after failed probe", status.State)
	}
	if status.Status != "offline" {
		t.Fatalf("display status = %q, want offline", status.Status)
	}
}

// 支持探测的 provider 探测通过后应显示在线；不支持探测的返回 unknown，
// 不能再冒充在线，也不能因此改动熔断器。
func TestProbeUnknownDoesNotClaimOnline(t *testing.T) {
	router := NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("opaque", &opaqueProbeProvider{})
	router.RegisterChain("opaque", []string{"opaque"})

	result := router.CheckProviderHealth(context.Background(), "opaque")
	if result.Status != ProbeUnknown {
		t.Fatalf("probe status = %q, want unknown", result.Status)
	}
	status, _ := router.ProviderStatus("opaque")
	if status.Status == "online" {
		t.Fatalf("display status = %q, must not claim online without evidence", status.Status)
	}
	// 无结论的探测不能写进熔断器：否则一次「测不了」会被当成成功而清掉
	// 真实的连续失败计数。
	if status.HasResult || status.TotalSuccesses != 0 || status.TotalFailures != 0 {
		t.Fatalf("unknown probe must not touch the circuit: %+v", status)
	}
}

// 探测记录后，即使熔断器里还有历史成功，也必须以最近的探测结论为准。
func TestProbeResultOverridesStaleCircuitSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer server.Close()

	router := NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("glm-5.2", NewOpenAIProvider(&Config{Name: "glm-5.2", URL: server.URL, APIKey: "k"}))
	router.RegisterChain("glm-5.2", []string{"glm-5.2"})

	// 模拟一次历史成功，让熔断器认为该 provider 在线。
	router.RecordProviderSuccess("glm-5.2")
	before, _ := router.ProviderStatus("glm-5.2")
	if before.Status != "online" {
		t.Fatalf("precondition: status = %q, want online", before.Status)
	}

	router.CheckProviderHealth(context.Background(), "glm-5.2")
	after, _ := router.ProviderStatus("glm-5.2")
	if after.Status != "offline" || after.State != CircuitOpen || after.NextRetryAt.IsZero() {
		t.Fatalf("429 probe must cool immediately despite stale success: %+v", after)
	}
}

// 重置应同时清掉旧的探测结论，否则修好的 provider 会一直显示离线。
func TestResetProviderClearsProbeResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"denied"}}`)
	}))
	defer server.Close()

	router := NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("glm-4.7", NewOpenAIProvider(&Config{Name: "glm-4.7", URL: server.URL, APIKey: "k"}))
	router.RegisterChain("glm-4.7", []string{"glm-4.7"})

	router.CheckProviderHealth(context.Background(), "glm-4.7")
	if err := router.ResetProvider("glm-4.7"); err != nil {
		t.Fatalf("reset provider: %v", err)
	}
	status, _ := router.ProviderStatus("glm-4.7")
	if status.HasProbe {
		t.Fatalf("probe result survived reset: %+v", status)
	}
	if status.Status == "offline" {
		t.Fatalf("status after reset = %q, want not-offline", status.Status)
	}
}

// opaqueProbeProvider 模拟没有实现 Prober 的 provider。
type opaqueProbeProvider struct{}

func (p *opaqueProbeProvider) Name() string    { return "opaque" }
func (p *opaqueProbeProvider) URL() string     { return "" }
func (p *opaqueProbeProvider) APIKey() string  { return "" }
func (p *opaqueProbeProvider) UseBearer() bool { return false }
func (p *opaqueProbeProvider) IsHealthy(context.Context) bool {
	return true
}
func (p *opaqueProbeProvider) ForwardRequest(context.Context, *Request) (*Response, error) {
	return nil, errors.New("not implemented")
}
