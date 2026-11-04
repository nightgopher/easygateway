package provider

import (
	"context"
	"log"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestRouterSkipsUnavailableFallbackProviders(t *testing.T) {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	router := NewRouter(logger)
	router.RegisterProvider("glm", NewAnthropicProvider(&Config{
		Name: "glm", URL: "https://api.example.com", APIKey: "test-key",
	}))
	router.RegisterChain("coding", []string{"glm", "ali"})

	providers, err := router.Route("coding")
	if err != nil {
		t.Fatalf("Route() failed when only an optional fallback was unavailable: %v", err)
	}
	if len(providers) != 1 || providers[0].Name() != "glm" {
		t.Fatalf("expected the available primary provider, got %#v", providers)
	}
}

func TestRouterCircuitBreakerSkipsOpenProviderAndFallsBack(t *testing.T) {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	first := &errorPolicyProvider{
		name: "first",
		err:  &ProviderError{Provider: "first", StatusCode: http.StatusServiceUnavailable, Message: "down"},
	}
	second := &errorPolicyProvider{name: "second", resp: &Response{Model: "fallback"}}
	router := NewRouterWithCircuitConfig(logger, CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Hour,
		SuccessThreshold: 1,
	})
	router.RegisterProvider(first.name, first)
	router.RegisterProvider(second.name, second)
	router.RegisterChain("coding", []string{first.name, second.name})

	if _, err := router.Forward(context.Background(), "coding", &Request{Model: "coding"}); err != nil {
		t.Fatalf("first request should fallback: %v", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("first request calls = first:%d second:%d", first.calls, second.calls)
	}

	if _, err := router.Forward(context.Background(), "coding", &Request{Model: "coding"}); err != nil {
		t.Fatalf("second request should use healthy fallback: %v", err)
	}
	if first.calls != 1 {
		t.Fatalf("open provider should be skipped, calls = %d", first.calls)
	}
	status, ok := router.ProviderStatus("first")
	if !ok || status.State != CircuitOpen || status.Status != "offline" {
		t.Fatalf("unexpected first provider status: %#v", status)
	}
}
