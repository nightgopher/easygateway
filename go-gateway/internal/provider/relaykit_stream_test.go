package provider

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRelayKitStreamUsageAfterFinish(t *testing.T) {
	srv := openAIStreamTestServer(t, oaiChunk(`{"content":"hello"}`), oaiFinish("stop"), `{"id":"1","object":"chat.completion.chunk","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`, "[DONE]")
	p := NewOpenAIProvider(&Config{Name: "test", URL: srv.URL})
	var out bytes.Buffer
	if err := p.ForwardStream(context.Background(), &Request{Model: "m"}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{`"input_tokens":6`, `"output_tokens":2`, `"cache_read_input_tokens":4`, `event: message_stop`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in %s", want, text)
		}
	}
}

func TestRelayKitStreamErrorAfterFinishDoesNotSucceed(t *testing.T) {
	srv := openAIStreamTestServer(t, oaiChunk(`{"content":"partial"}`), oaiFinish("stop"), `{"error":{"message":"later failure","type":"server_error"}}`)
	p := NewOpenAIProvider(&Config{Name: "test", URL: srv.URL})
	var out bytes.Buffer
	if err := p.ForwardStream(context.Background(), &Request{Model: "m"}, &out); err == nil {
		t.Fatal("expected upstream error")
	}
	if strings.Contains(out.String(), "event: message_stop") || strings.Contains(out.String(), "event: message_delta") {
		t.Fatalf("reported success for failed stream: %s", out.String())
	}
}

func TestRelayKitStreamUsageBeforeContentDoesNotEndStream(t *testing.T) {
	srv := openAIStreamTestServer(t, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":0}}`, oaiChunk(`{"content":"hello"}`), oaiFinish("stop"), "[DONE]")
	p := NewOpenAIProvider(&Config{Name: "test", URL: srv.URL})
	var out bytes.Buffer
	if err := p.ForwardStream(context.Background(), &Request{Model: "m"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"text":"hello"`) || !strings.Contains(out.String(), `"input_tokens":10`) {
		t.Fatalf("lost content or usage: %s", out.String())
	}
}

func TestRelayKitStreamWithoutUsageIncludesZeroCounts(t *testing.T) {
	srv := openAIStreamTestServer(t, oaiChunk(`{"content":"hello"}`), oaiFinish("stop"), "[DONE]")
	p := NewOpenAIProvider(&Config{Name: "test", URL: srv.URL})
	var out bytes.Buffer
	if err := p.ForwardStream(context.Background(), &Request{Model: "m"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"type":"message_delta","usage"`) {
		t.Fatalf("missing zero usage: %s", out.String())
	}
}
