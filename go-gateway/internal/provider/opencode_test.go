package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// opencode 免费通道的 provider 覆盖了「上游只提供流式端点」这一情形：非流式客户端
// 请求也要走流式端点，再把 SSE 累积回完整响应。这组测试同时钉住两件事——
// 累积结果正确，以及**真实发出的请求体带 stream:true**（否则上游直接 403）。

// openCodeCaptureServer 启动一个记录请求体、按序回放 SSE 帧的上游。
func openCodeCaptureServer(t *testing.T, frames ...string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = body
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = w.Write([]byte("data: " + f + "\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// TestOpenCodeForwardRequestAccumulatesStream 验证非流式路径把 SSE 累积成完整 Response：
// thinking / text / tool_use 三种块都在，工具参数拼成合法 JSON，用量与 stop_reason 正确。
func TestOpenCodeForwardRequestAccumulatesStream(t *testing.T) {
	srv, seen := openCodeCaptureServer(t,
		`{"id":"c1","object":"chat.completion.chunk","model":"paid-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"paid-model","choices":[{"index":0,"delta":{"reasoning_content":"想一想。"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"paid-model","choices":[{"index":0,"delta":{"content":"我来读文件。"},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"paid-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"paid-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"paid-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","object":"chat.completion.chunk","model":"paid-model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
		`[DONE]`,
	)

	p := NewOpenCodeProvider(&Config{Name: "paid-model", URL: srv.URL, Auth: NewStaticAuth("test-paid-key")})
	req := mustRequest(t, `{"model":"paid-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	resp, err := p.ForwardRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("ForwardRequest failed: %v", err)
	}

	// 非流式调用必须仍然以流式请求打到上游，否则免费通道 403。
	if stream, _ := (*seen)["stream"].(bool); !stream {
		t.Fatalf("非流式请求也必须带 stream:true，实际请求体: %v", *seen)
	}

	if resp.Role != "assistant" || resp.Type != "message" {
		t.Fatalf("响应外壳不正确: %+v", resp)
	}
	if resp.Model != "paid-model" {
		t.Fatalf("模型名应为绑定的上游模型 ID，得到 %q", resp.Model)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("finish_reason=tool_calls 应映射成 tool_use，得到 %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("用量不正确: %+v", resp.Usage)
	}

	var text, thinking, toolName, toolArgs string
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "thinking":
			thinking += block.Thinking
		case "tool_use":
			toolName = block.Name
			toolArgs = string(block.Input)
		}
	}
	if thinking != "想一想。" {
		t.Fatalf("thinking 块内容不正确: %q", thinking)
	}
	if text != "我来读文件。" {
		t.Fatalf("text 块内容不正确: %q", text)
	}
	if toolName != "read" {
		t.Fatalf("工具名应为 read，得到 %q", toolName)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(toolArgs), &args); err != nil {
		t.Fatalf("累积后的工具参数应是合法 JSON，得到 %q: %v", toolArgs, err)
	}
	if args["path"] != "a.go" {
		t.Fatalf("工具参数不正确: %v", args)
	}
}

// TestOpenCodeForwardStreamTranslatesToAnthropicSSE 验证流式路径仍走 OpenAIProvider
// 的翻译（Anthropic SSE 事件齐全），覆盖没有破坏继承来的行为。
func TestOpenCodeForwardStreamTranslatesToAnthropicSSE(t *testing.T) {
	srv, _ := openCodeCaptureServer(t,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	p := NewOpenCodeProvider(&Config{Name: "paid-model", URL: srv.URL, Auth: NewStaticAuth("test-paid-key")})
	req := mustRequest(t, `{"model":"paid-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	var sb strings.Builder
	if err := p.ForwardStream(context.Background(), req, &sb); err != nil {
		t.Fatalf("ForwardStream failed: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		`"type":"message_start"`, `"type":"content_block_start"`, `text_delta`,
		`"type":"content_block_stop"`, `"type":"message_delta"`, `"type":"message_stop"`,
		`"stop_reason":"end_turn"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Anthropic SSE 缺少 %s，实际输出:\n%s", want, out)
		}
	}
}

func TestOpenCodeFreeTierNeverAdvertisedAndPaidRequiresLogin(t *testing.T) {
	for _, id := range []string{"big-pickle", "mimo-v2.6-flash-free"} {
		p := NewOpenCodeProvider(&Config{Name: id, Auth: NewStaticAuth("test-paid-key"), URL: "http://example.invalid"})
		if p.Available() {
			t.Fatal("free tier advertised after login")
		}
		req := mustRequest(t, `{"model":"`+id+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		for _, stream := range []bool{true, false} {
			var err error
			if stream {
				err = p.ForwardStream(t.Context(), req, io.Discard)
			} else {
				_, err = p.ForwardRequest(t.Context(), req)
			}
			var pe *ProviderError
			if !errors.As(err, &pe) || pe.StatusCode != 403 || !strings.Contains(pe.Message, "登录不会解除限制") {
				t.Fatalf("wrong restriction error: %v", err)
			}
		}
	}
	for _, token := range []string{"", "public", "paid-key"} {
		p := NewOpenCodeProvider(&Config{Name: "paid-model", Auth: NewStaticAuth(token)})
		if p.Available() != (token == "paid-key") {
			t.Fatalf("bad login state %q", token)
		}
	}
}

func TestOpenCodeRestrictionSurvivesBoundModelRouting(t *testing.T) {
	r := NewRouter(log.New(io.Discard, "", 0))
	p := NewOpenCodeProvider(&Config{Name: "mimo-v2.6-flash-free", Auth: NewStaticAuth("paid-key")})
	r.RegisterProvider("bound", NewBoundModelProviderWrapper(p, "mimo-v2.6-flash-free", []string{"text"}))
	r.RegisterChain("free-alias", []string{"bound"})
	if r.ModelUsable("free-alias") {
		t.Fatal("free model advertised")
	}
	if _, err := r.Route("free-alias"); err == nil || !strings.Contains(err.Error(), "登录不会解除限制") {
		t.Fatalf("restriction lost: %v", err)
	}
}
