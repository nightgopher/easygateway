package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeepVToolReasoningPlaceholder(t *testing.T) {
	const tool = `{"functionCall":{"id":"call_a","name":"lookup","args":{"q":"x"}}}`
	tests := []struct {
		name, model string
		parts       []string
		want        string
	}{
		{"standalone sentinel before tool", "glm-5.3-flash", []string{`{"reasoning":"."}`, tool}, ""},
		{"spaced sentinel before tool", "glm-5.3-flash", []string{`{"reasoning":" "}`, `{"reasoning":"."}`, `{"reasoning":" "}`, tool}, ""},
		{"real reasoning split after dot", "glm-5.3-flash", []string{`{"reasoning":"."}`, `{"reasoning":"分析"}`, tool}, ".分析"},
		{"multi punctuation preserved", "glm-5.3-flash", []string{`{"reasoning":"."}`, `{"reasoning":"."}`, tool}, ".."},
		{"normal reasoning preserved", "glm-5.3-flash", []string{`{"reasoning":"查询"}`, tool}, "查询"},
		{"dot before text preserved", "glm-5.3-flash", []string{`{"reasoning":"."}`, `{"text":"answer"}`}, "."},
		{"dot without a tool preserved", "glm-5.3-flash", []string{`{"reasoning":"."}`}, "."},
		{"other model untouched", "deepseek-flash", []string{`{"reasoning":"."}`, tool}, "."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var src strings.Builder
			for _, part := range tc.parts {
				src.WriteString(`data: {"candidates":[{"content":{"parts":[` + part + `]}}]}` + "\n\n")
			}
			src.WriteString("data: [DONE]\n\n")
			p := NewDeepVProvider(&Config{Name: "deepv"}, "", tc.model)
			var output strings.Builder
			if err := p.convertStream(strings.NewReader(src.String()), &output, tc.model); err != nil {
				t.Fatal(err)
			}
			var got strings.Builder
			for _, line := range strings.Split(output.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var e struct {
					Delta struct {
						Type     string `json:"type"`
						Thinking string `json:"thinking"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
					t.Fatal(err)
				}
				if e.Delta.Type == "thinking_delta" {
					got.WriteString(e.Delta.Thinking)
				}
			}
			if got.String() != tc.want {
				t.Fatalf("thinking = %q; want %q", got.String(), tc.want)
			}
			if strings.Contains(strings.Join(tc.parts, ""), "functionCall") && !strings.Contains(output.String(), `"stop_reason":"tool_use"`) {
				t.Fatal("tool call was not preserved")
			}
		})
	}
}

func TestDeepVNonStreamingToolReasoningPlaceholder(t *testing.T) {
	const input = `{"candidates":[{"content":{"parts":[{"reasoning":"."},{"functionCall":{"id":"call_1","name":"lookup","args":{"q":"x"}}}]}}]}`
	for _, tc := range []struct {
		model        string
		wantThinking bool
	}{{"glm-5.3-flash", false}, {"deepseek-flash", true}} {
		p := NewDeepVProvider(&Config{Name: "deepv"}, "", tc.model)
		got, err := p.parseResponse([]byte(input), tc.model)
		if err != nil {
			t.Fatal(err)
		}
		thought, tool := false, false
		for _, b := range got.Content {
			if b.Type == "thinking" {
				thought = true
			}
			if b.Type == "tool_use" {
				tool = true
			}
		}
		if thought != tc.wantThinking || !tool {
			t.Fatalf("model=%s thinking=%v tool=%v", tc.model, thought, tool)
		}
	}
}

func TestDeepVFilteredToolReasoningReplayedAsRequired(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv"}, "", "glm-5.3-flash")
	raw := []byte(`{"candidates":[{"content":{"parts":[{"reasoning":"."},{"functionCall":{"id":"call_a","name":"lookup","args":{"q":"x"}}}]}}]}`)
	response, err := p.parseResponse(raw, "glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	history, err := json.Marshal(map[string]any{"model": "glm-5.3-flash", "messages": []any{map[string]any{"role": "assistant", "content": response.Content}}})
	if err != nil {
		t.Fatal(err)
	}
	var req Request
	if err = json.Unmarshal(history, &req); err != nil {
		t.Fatal(err)
	}
	converted, err := p.convertRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Contents) != 1 || len(converted.Contents[0].Parts) != 2 {
		t.Fatalf("unexpected replay: %+v", converted.Contents)
	}
	parts := converted.Contents[0].Parts
	if parts[0].Reasoning != placeholderReasoning || parts[1].FunctionCall == nil || parts[1].FunctionCall.Args["q"] != "x" {
		t.Fatalf("placeholder or tool call missing: %+v", parts)
	}
}
