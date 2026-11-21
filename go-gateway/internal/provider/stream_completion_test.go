package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func TestProvidersRejectStreamEOFWithoutTerminal(t *testing.T) {
	srv := openAIStreamTestServer(t, oaiChunk(`{"content":"partial"}`))
	copilot := NewCopilotProvider(&Config{Name: "copilot", APIKey: "fixture"}, "")
	copilot.apiURL = srv.URL
	for _, p := range []StreamProvider{
		NewOpenAIProvider(&Config{Name: "openai", URL: srv.URL}), copilot,
	} {
		t.Run(p.(Provider).Name(), func(t *testing.T) {
			var out strings.Builder
			if err := p.ForwardStream(context.Background(), &Request{Model: "mock"}, &out); err == nil {
				t.Fatal("partial stream was accepted as complete")
			}
			if strings.Contains(out.String(), `"type":"message_stop"`) {
				t.Fatal("truncated stream synthesized message_stop")
			}
		})
	}
	t.Run("deepv", func(t *testing.T) {
		p := &DeepVProvider{config: &Config{Name: "deepv"}}
		var out strings.Builder
		err := p.convertStream(strings.NewReader(`data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`+"\n\n"), &out, "mock")
		if err == nil || strings.Contains(out.String(), `"type":"message_stop"`) {
			t.Fatalf("err=%v, output=%s", err, out.String())
		}
	})
}

type streamFixtureTransport struct{ body string }

func (s streamFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(s.body)), Request: req}, nil
}

func TestNativePassthroughRejectsStreamEOFWithoutTerminal(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		forward    func(string, io.Writer) error
	}{
		{"anthropic", progressFrame, func(body string, dst io.Writer) error {
			p := NewAnthropicProvider(&Config{Name: "anthropic", URL: "https://fixture.invalid/messages"})
			p.client = &http.Client{Transport: streamFixtureTransport{body}}
			return p.ForwardStream(context.Background(), &Request{Model: "mock"}, dst)
		}},
		{"chatgpt", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", func(body string, dst io.Writer) error {
			p := &ChatGPTProvider{auth: &chatgptAuth{AccessToken: "fixture", AccountID: "fixture"}, client: &http.Client{Transport: streamFixtureTransport{body}}}
			return p.ForwardRawResponsesStream(context.Background(), []byte(`{"model":"gpt-6-sol","input":"hi"}`), dst)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			if err := tt.forward(tt.body, &out); err == nil || out.Len() == 0 {
				t.Fatalf("err=%v, output=%s", err, out.String())
			}
		})
	}
}

func TestValidatedStreamPreservesSplitTerminalAndIncompleteStatus(t *testing.T) {
	for _, body := range []string{progressFrame + terminalFrame, "data: {\"type\":\"response.completed\"}\n\n", "data: {\"type\":\"response.incomplete\"}\n\n"} {
		var out strings.Builder
		if err := copyValidatedStream("mock", &out, iotest.OneByteReader(strings.NewReader(body))); err != nil || out.String() != body {
			t.Fatalf("err=%v, output=%s", err, out.String())
		}
	}
}

func TestValidatedStreamRejectsFailureBeforeTerminal(t *testing.T) {
	for _, body := range []string{
		progressFrame + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"server_error\"}}\n\n" + terminalFrame,
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\"}}}\n\ndata: {\"type\":\"response.completed\"}\n\n",
	} {
		var out strings.Builder
		if err := copyValidatedStream("mock", &out, iotest.OneByteReader(strings.NewReader(body))); err == nil {
			t.Fatal("upstream failure was accepted as complete")
		}
	}
}

func TestStreamGuardRequiresTerminalAfterOutput(t *testing.T) {
	for _, frame := range []string{
		progressFrame,
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
	} {
		var dst strings.Builder
		_, err := NewRouter(nil).RunStreamAttempt(context.Background(), &Request{}, &errorPolicyProvider{name: "mock"}, &dst,
			func(_ context.Context, w io.Writer) error { _, err := io.WriteString(w, frame); return err })
		if err == nil || dst.Len() == 0 {
			t.Fatalf("missing terminal accepted: err=%v, output=%s", err, dst.String())
		}
	}
}

func TestConvertersRejectUpstreamErrorBeforeDone(t *testing.T) {
	errorFrame := "data: {\"error\":{\"message\":\"fixture upstream failed\",\"type\":\"server_error\"}}\n\n"
	srv := openAIStreamTestServer(t, oaiChunk(`{"content":"partial"}`), `{"error":{"message":"fixture upstream failed","type":"server_error"}}`, "[DONE]")
	copilot := NewCopilotProvider(&Config{Name: "copilot", APIKey: "fixture"}, "")
	copilot.apiURL = srv.URL
	for _, p := range []StreamProvider{NewOpenAIProvider(&Config{Name: "openai", URL: srv.URL}), copilot} {
		t.Run(p.(Provider).Name(), func(t *testing.T) {
			var out strings.Builder
			_, err := NewRouter(nil).RunStreamAttempt(context.Background(), &Request{Model: "mock"}, p.(Provider), &out,
				func(ctx context.Context, w io.Writer) error { return p.ForwardStream(ctx, &Request{Model: "mock"}, w) })
			if err == nil || !strings.Contains(out.String(), "partial") || strings.Contains(out.String(), `"type":"message_stop"`) {
				t.Fatalf("err=%v, output=%s", err, out.String())
			}
		})
	}
	t.Run("deepv", func(t *testing.T) {
		p := &DeepVProvider{config: &Config{Name: "deepv"}}
		var out strings.Builder
		body := `data: {"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}` + "\n\n" + errorFrame + "data: [DONE]\n\n"
		if err := p.convertStream(strings.NewReader(body), &out, "mock"); err == nil || strings.Contains(out.String(), `"type":"message_stop"`) {
			t.Fatalf("err=%v, output=%s", err, out.String())
		}
	})
}
