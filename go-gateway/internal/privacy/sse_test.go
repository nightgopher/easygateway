package privacy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSSESplitAcrossEventsAndTransport(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	secret := "sk-" + strings.Repeat("x", 24)
	token, _ := s.Mask(secret)
	mk := func(text string) string {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
		return "data: " + string(b) + "\n\n"
	}
	input := mk("first "+token[:12]) + mk(token[12:]+" last") + "data: [DONE]\n\n"
	for size := 1; size < 16; size++ {
		d := s.NewSSEDecoder()
		var out string
		for i := 0; i < len(input); i += size {
			end := i + size
			if end > len(input) {
				end = len(input)
			}
			v, err := d.Push([]byte(input[i:end]))
			if err != nil {
				t.Fatal(err)
			}
			out += string(v)
		}
		v, err := d.Finish()
		if err != nil {
			t.Fatal(err)
		}
		out += string(v)
		if !strings.Contains(out, secret) || strings.Contains(out, token) {
			t.Fatalf("split %d: %s", size, out)
		}
	}
}
func TestSSEReportsIncompleteFrame(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	d := s.NewSSEDecoder()
	_, _ = d.Push([]byte("data: {"))
	if _, err := d.Finish(); err == nil {
		t.Fatal("expected error")
	}
}

func TestSSEParallelToolsRemainSeparate(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	one := "sk-" + strings.Repeat("a", 20)
	two := "sk-" + strings.Repeat("b", 20)
	a, _ := s.Mask(one)
	b, _ := s.Mask(two)
	frame := func(idx int, delta string) string {
		body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": idx, "function": map[string]string{"arguments": delta}}}}}}})
		return "data: " + string(body) + "\n\n"
	}
	input := frame(0, a[:10]) + frame(1, b[:10]) + frame(0, a[10:]) + frame(1, b[10:]) + "data: [DONE]\n\n"
	d := s.NewSSEDecoder()
	out, err := d.Push([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := d.Finish()
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, tail...)
	if !strings.Contains(string(out), one) || !strings.Contains(string(out), two) {
		t.Fatalf("missing restored tool values: %s", out)
	}
}
func TestSSEToolArgumentsEscapeSecrets(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	secret := "abc\"def\\ghi"
	token, _ := s.maskSecret(secret)
	inner := `{"api_key":"` + token + `"}`
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]string{"arguments": inner}}}}}}})
	d := s.NewSSEDecoder()
	out, err := d.Push([]byte("data: " + string(body) + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	line := strings.TrimSpace(strings.TrimPrefix(string(out), "data:"))
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatal(err)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(payload.Choices[0].Delta.ToolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["api_key"] != secret {
		t.Fatal("wrong credential")
	}
}
