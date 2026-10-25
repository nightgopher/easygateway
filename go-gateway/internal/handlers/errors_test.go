package handlers

import (
	"errors"
	"net/http"
	"testing"

	"github.com/nightgopher/easygateway/internal/provider"
)

func TestRoutingErrorStatusPreservesProviderHTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{name: "bad request", status: http.StatusBadRequest},
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "not found", status: http.StatusNotFound},
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "upstream failure", status: http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &provider.ProviderError{StatusCode: tt.status, Message: "upstream error"}
			if got := routingErrorStatus(err); got != tt.status {
				t.Fatalf("routingErrorStatus() = %d, want %d", got, tt.status)
			}
		})
	}
}

func TestRoutingErrorStatusHandlesWrappedProviderError(t *testing.T) {
	err := errors.New("request failed")
	err = errors.Join(err, &provider.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "quota exceeded"})
	if got := routingErrorStatus(err); got != http.StatusTooManyRequests {
		t.Fatalf("routingErrorStatus() = %d, want 429", got)
	}
}

func TestRoutingErrorStatusHandlesUnknownModel(t *testing.T) {
	if got := routingErrorStatus(&provider.UnknownModelError{Model: "missing"}); got != http.StatusNotFound {
		t.Fatalf("routingErrorStatus() = %d, want 404", got)
	}
}
