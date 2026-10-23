package provider

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactedErrorTextStripsCredentials(t *testing.T) {
	cases := []struct {
		name, in, mustNotContain string
	}{
		{"anthropic key", `anthropic HTTP 401: invalid x-api-key: sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890abcdef`, "sk-ant-"},
		{"openai key", `openai HTTP 401: incorrect api key provided sk-proj-0123456789abcdef`, "sk-proj-"},
		{"bearer", `upstream: Authorization Bearer eyJhbGciOi.example.token rejected`, "eyJhbGciOi"},
		{"kv secret", `provider failed: api_key=abcdef1234567890abcdef is invalid`, "abcdef1234567890"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := RedactedErrorText(errors.New(tc.in))
			if strings.Contains(out, tc.mustNotContain) {
				t.Errorf("redaction missed secret %q in %q", tc.mustNotContain, out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("expected [REDACTED] marker, got %q", out)
			}
		})
	}
	if RedactedErrorText(nil) != "" {
		t.Error("nil error should redact to empty string")
	}
	plain := RedactedErrorText(errors.New("connection refused"))
	if plain != "connection refused" {
		t.Errorf("secret-free message should pass through unchanged, got %q", plain)
	}
}
