package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDeepVProviderCapabilities(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv-deepseek-flash", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-flash")
	for _, c := range []string{CapabilityText, CapabilityVision, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning} {
		if !hasCapability(p.Capabilities(), c) {
			t.Errorf("missing capability %q", c)
		}
	}
}

func hasCapability(list []string, want string) bool {
	for _, c := range list {
		if c == want {
			return true
		}
	}
	return false
}

func TestDeepVConvertRequest(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")

	raw := `{
		"model": "deepseek-v4.1-flash",
		"max_tokens": 1024,
		"temperature": 0.7,
		"system": [{"type":"text","text":"be brief"}],
		"tools": [{"name":"lookup","description":"d","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"what is this?"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
			]},
			{"role":"assistant","content":[
				{"type":"text","text":"let me check"},
				{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"found"}
			]}
		]
	}`

	var req Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	out, err := p.convertRequest(&req)
	if err != nil {
		t.Fatalf("convertRequest: %v", err)
	}

	if out.Model != "deepseek-v4.1-flash" {
		t.Errorf("model = %q", out.Model)
	}
	if out.SystemInstruction == nil || len(out.SystemInstruction.Parts) != 1 {
		t.Fatal("system instruction missing")
	}
	if out.Config.MaxOutputTokens != 1024 {
		t.Errorf("maxOutputTokens = %d", out.Config.MaxOutputTokens)
	}
	if out.Config.Temperature != 0.7 {
		t.Errorf("temperature = %v", out.Config.Temperature)
	}
	if len(out.Config.Tools) != 1 || len(out.Config.Tools[0].FunctionDeclarations) != 1 {
		t.Fatal("tools not converted")
	}

	if len(out.Contents) != 3 {
		t.Fatalf("contents len = %d, want 3", len(out.Contents))
	}

	user := out.Contents[0]
	if user.Role != "user" || len(user.Parts) != 2 {
		t.Fatalf("first user content wrong: role=%q parts=%d", user.Role, len(user.Parts))
	}
	if user.Parts[1].InlineData == nil {
		t.Fatal("image block should become inlineData")
	}
	if user.Parts[1].InlineData.MimeType != "image/png" || user.Parts[1].InlineData.Data != "aGVsbG8=" {
		t.Errorf("inlineData = %+v", user.Parts[1].InlineData)
	}

	model := out.Contents[1]
	// 缺思维链的工具调用轮会被注入占位 reasoning part，排在其他 part 之前。
	if model.Role != "model" || model.Parts[0].Reasoning != placeholderReasoning {
		t.Fatalf("placeholder reasoning not injected: %+v", model)
	}
	if model.Parts[2].FunctionCall == nil {
		t.Fatalf("assistant tool_use not converted: %+v", model)
	}

	toolResult := out.Contents[2]
	if toolResult.Parts[0].FunctionResponse == nil {
		t.Fatal("tool_result not converted")
	}
	if toolResult.Parts[0].FunctionResponse.Name != "lookup" {
		t.Errorf("function response name = %q", toolResult.Parts[0].FunctionResponse.Name)
	}
}

// TestDeepVToolCallTurnKeepsRealReasoning 验证：带真实思维链的工具调用轮
// 原样回传，不被占位文本覆盖；缺思维链的轮次（含纯文本 model 轮）注入占位，
// 上游思考模式要求所有历史 model 轮必须带 reasoning。
func TestDeepVToolCallTurnKeepsRealReasoning(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")

	raw := `{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"go"}]},
			{"role":"assistant","content":[
				{"type":"text","text":"plain answer, no tools"}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"real chain"},
				{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":"found"}
			]}
		]
	}`

	var req Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	out, err := p.convertRequest(&req)
	if err != nil {
		t.Fatalf("convertRequest: %v", err)
	}
	if len(out.Contents) != 4 {
		t.Fatalf("contents len = %d, want 4", len(out.Contents))
	}

	plain := out.Contents[1]
	if plain.Parts[0].Reasoning != placeholderReasoning || plain.Parts[1].Text != "plain answer, no tools" {
		t.Errorf("plain text turn should get placeholder reasoning: %+v", plain.Parts)
	}

	withTools := out.Contents[2]
	if withTools.Parts[0].Reasoning != "real chain" {
		t.Errorf("real reasoning should be preserved, got %q", withTools.Parts[0].Reasoning)
	}
}

func TestDeepVConvertImageURL(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")

	raw := `{
		"model": "deepseek-v4.1-flash",
		"messages": [{"role":"user","content":[
			{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}},
			{"type":"image_url","image_url":{"url":"https://cdn.example.com/pic.png"}}
		]}]
	}`
	var req Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := p.convertRequest(&req)
	if err != nil {
		t.Fatalf("convertRequest: %v", err)
	}
	parts := out.Contents[0].Parts
	if len(parts) != 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	if parts[0].InlineData == nil || parts[0].InlineData.MimeType != "image/jpeg" || parts[0].InlineData.Data != "QUJD" {
		t.Errorf("data URI part = %+v", parts[0])
	}
	if parts[1].FileData == nil || parts[1].FileData.FileURI != "https://cdn.example.com/pic.png" || parts[1].FileData.MimeType != "image/png" {
		t.Errorf("remote URL part = %+v", parts[1])
	}
}

func TestDeepVParseResponse(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")

	body := `{
		"candidates": [{
			"content": {"parts": [
				{"text": "hello "},
				{"text": "world"},
				{"functionCall": {"id":"fc_1","name":"lookup","args":{"q":"1"}}}
			]},
			"finishReason": "TOOL_CALL"
		}],
		"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 7}
	}`

	resp, err := p.parseResponse([]byte(body), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("content len = %d", len(resp.Content))
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "world" {
		t.Errorf("content[1] = %+v", resp.Content[1])
	}
	if resp.Content[2].Type != "tool_use" || resp.Content[2].Name != "lookup" {
		t.Errorf("content[2] = %+v", resp.Content[2])
	}
	if !strings.Contains(string(resp.Content[2].Input), `"q":"1"`) {
		t.Errorf("tool input = %s", resp.Content[2].Input)
	}
}

func TestDeepVParseResponseImage(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")
	body := `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"QUJD"}}]},"finishReason":"STOP"}]}`
	resp, err := p.parseResponse([]byte(body), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "image" {
		t.Fatalf("content = %+v", resp.Content)
	}
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if err := json.Unmarshal(resp.Content[0].Source, &source); err != nil {
		t.Fatalf("source unmarshal: %v", err)
	}
	if source.Type != "base64" || source.MediaType != "image/png" || source.Data != "QUJD" {
		t.Errorf("source = %+v", source)
	}
}

func TestDeepVIsHealthyWithoutToken(t *testing.T) {
	// 默认 HOME 下有文件时可能误判；这里只验证不 panic 且返回 bool。
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")
	_ = p.IsHealthy(context.Background())
}

// TestDeepVToolResultImageForwarding 验证工具结果里的图片块会转成
// inlineData part 随 functionResponse 一起上送，供多模态模型读图。
func TestDeepVToolResultImageForwarding(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")

	raw := `{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"read it"}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"need the file"},
				{"type":"tool_use","id":"tu_1","name":"Read","input":{"path":"a.png"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":[
					{"type":"text","text":"png file"},
					{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}
				]}
			]}
		]
	}`

	var req Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	out, err := p.convertRequest(&req)
	if err != nil {
		t.Fatalf("convertRequest: %v", err)
	}

	toolTurn := out.Contents[2]
	if toolTurn.Role != "user" {
		t.Fatalf("tool result turn role = %q", toolTurn.Role)
	}
	assistantTurn := out.Contents[1]
	if assistantTurn.Parts[0].Reasoning != "need the file" {
		t.Errorf("reasoning before tool_use missing: %+v", assistantTurn.Parts[0])
	}
	fr := toolTurn.Parts[0].FunctionResponse
	if fr == nil || fr.Name != "Read" || fr.Response["result"] != "png file" {
		t.Fatalf("functionResponse wrong: %+v", fr)
	}
	if len(toolTurn.Parts) < 2 || toolTurn.Parts[1].InlineData == nil {
		t.Fatalf("tool result image not forwarded as inlineData: %+v", toolTurn.Parts)
	}
	if toolTurn.Parts[1].InlineData.MimeType != "image/png" || toolTurn.Parts[1].InlineData.Data != "QUJD" {
		t.Errorf("inlineData = %+v", toolTurn.Parts[1].InlineData)
	}
}

// TestDeepVNormalizeToolResultTurns 验证并行工具调用的多个 tool_result 轮会被
// 合并成一个 user 轮。上游 GenAI 格式要求同一 model 轮的工具结果同轮返回，
// 否则报 "Duplicate tool output for call_id" 并拒绝整个请求。
func TestDeepVNormalizeToolResultTurns(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")

	raw := `{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"run two commands"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_A","name":"Bash","input":{"command":"echo A"}},
				{"type":"tool_use","id":"call_B","name":"Bash","input":{"command":"echo B"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_A","content":"A"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_B","content":"B"}]},
			{"role":"user","content":[{"type":"text","text":"now summarize"}]}
		]
	}`

	var req Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	out, err := p.convertRequest(&req)
	if err != nil {
		t.Fatalf("convertRequest: %v", err)
	}

	// 两个并行 functionCall 仍在同一个 model 轮。
	modelTurn := out.Contents[1]
	if modelTurn.Role != "model" {
		t.Fatalf("turn 1 role = %q", modelTurn.Role)
	}
	calls := 0
	for _, part := range modelTurn.Parts {
		if part.FunctionCall != nil {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("parallel function calls = %d, want 2", calls)
	}

	// 两个 functionResponse 必须落在同一个 user 轮。
	toolTurn := out.Contents[2]
	if toolTurn.Role != "user" {
		t.Fatalf("tool result turn role = %q", toolTurn.Role)
	}
	responses := 0
	for _, part := range toolTurn.Parts {
		if part.FunctionResponse != nil {
			responses++
		}
	}
	if responses != 2 {
		t.Fatalf("function responses in one turn = %d, want 2", responses)
	}
	if toolTurn.Parts[0].FunctionResponse.ID != "call_A" || toolTurn.Parts[1].FunctionResponse.ID != "call_B" {
		t.Fatalf("function response order wrong: %+v", toolTurn.Parts)
	}

	// 后续普通 user 轮不能被吞掉或并进工具结果轮。
	if len(out.Contents) != 4 {
		t.Fatalf("contents = %d, want 4", len(out.Contents))
	}
	last := out.Contents[3]
	if last.Role != "user" || len(last.Parts) != 1 || last.Parts[0].Text != "now summarize" {
		t.Fatalf("trailing user turn lost: %+v", last)
	}
}

// TestDeepVParseResponseParallelToolCallIDs 验证同一轮并行工具调用返回给客户端
// 的 tool_use id 互不相同。上游 functionCall 自带唯一 id，必须原样保留；早期实现
// 用"工具名+秒级时间戳"重新生成，同轮并行调用会撞成同一个 id。
func TestDeepVParseResponseParallelToolCallIDs(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-flash")

	body := `{
		"candidates": [{
			"content": {
				"parts": [
					{"functionCall": {"id": "call_00_abc", "name": "get_time", "args": {"city": "Beijing"}}},
					{"functionCall": {"id": "call_01_def", "name": "get_time", "args": {"city": "Tokyo"}}}
				],
				"role": "model"
			},
			"finishReason": "FUNCTION_CALL"
		}]
	}`

	resp, err := p.parseResponse([]byte(body), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}

	var ids []string
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			ids = append(ids, block.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("tool_use blocks = %d, want 2", len(ids))
	}
	if ids[0] == ids[1] {
		t.Fatalf("parallel tool_use ids collide: %q", ids[0])
	}
	if ids[0] != "call_00_abc" || ids[1] != "call_01_def" {
		t.Fatalf("upstream ids not preserved: %v", ids)
	}
}

// TestDeepVParseResponseMissingToolCallIDs 验证上游没给 id 时本地生成的 id 也不重号。
func TestDeepVParseResponseMissingToolCallIDs(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-flash")

	body := `{
		"candidates": [{
			"content": {
				"parts": [
					{"functionCall": {"name": "Bash", "args": {"command": "echo A"}}},
					{"functionCall": {"name": "Bash", "args": {"command": "echo B"}}}
				],
				"role": "model"
			},
			"finishReason": "FUNCTION_CALL"
		}]
	}`

	resp, err := p.parseResponse([]byte(body), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}

	var ids []string
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			ids = append(ids, block.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("tool_use blocks = %d, want 2", len(ids))
	}
	if ids[0] == ids[1] {
		t.Fatalf("generated tool_use ids collide: %q", ids[0])
	}
}

// TestDeepVRewriteQuotaError 验证"单请求 token 总量超限"的 402 被改写成 400
// 并附处置建议；真实欠费等其他 402 与非 402 错误保持原样。
func TestDeepVRewriteQuotaError(t *testing.T) {
	quota := &ProviderError{
		Provider:   "deepv-deepseek-flash",
		StatusCode: http.StatusPaymentRequired,
		Message:    "Quota limit exceeded: Request exceeds maximum tokens per request limit (200000)",
		RequestID:  "req-1",
	}
	got := rewriteDeepVQuotaError(quota)
	if got.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got.StatusCode)
	}
	if got.Message == quota.Message {
		t.Fatalf("message not rewritten: %q", got.Message)
	}
	if !strings.Contains(got.Message, "max_output_tokens") {
		t.Fatalf("message missing guidance: %q", got.Message)
	}
	if got.Provider != "deepv-deepseek-flash" || got.RequestID != "req-1" {
		t.Fatalf("provider metadata lost: %+v", got)
	}

	billing := &ProviderError{Provider: "deepv", StatusCode: http.StatusPaymentRequired, Message: "Insufficient balance"}
	if got := rewriteDeepVQuotaError(billing); got.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("billing 402 rewritten to %d", got.StatusCode)
	}

	serverErr := &ProviderError{Provider: "deepv", StatusCode: http.StatusInternalServerError, Message: "maximum tokens per request limit (200000)"}
	if got := rewriteDeepVQuotaError(serverErr); got.StatusCode != http.StatusInternalServerError {
		t.Fatalf("500 rewritten to %d", got.StatusCode)
	}

	if got := rewriteDeepVQuotaError(nil); got != nil {
		t.Fatalf("nil error rewritten: %+v", got)
	}
}

// DeepV requires args to be an object, including for empty tool inputs.
func TestDeepVEmptyToolArgsPreserved(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv", URL: "https://example.com/v1/chat/messages"}, "", "deepseek-v4.1-flash")

	raw := `{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_e","name":"bash","input":{}}
			]}
		]
	}`

	var req Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	out, err := p.convertRequest(&req)
	if err != nil {
		t.Fatalf("convertRequest: %v", err)
	}
	data, err := json.Marshal(out.Contents[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"args":{}`) {
		t.Fatalf("empty args must remain an object, got %s", data)
	}
}

func TestDeepVHaikuOmitsTemperature(t *testing.T) {
	for _, model := range []string{"claude-haiku-5-5", "mimo-v2.6-pro", "deepseek-flash"} {
		t.Run(model, func(t *testing.T) {
			p := NewDeepVProvider(&Config{Name: model}, "", model)
			req := &Request{}
			if err := json.Unmarshal([]byte(`{"model":"coding","max_tokens":256,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`), req); err != nil {
				t.Fatal(err)
			}
			converted, err := p.convertRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(converted)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Config map[string]json.RawMessage `json:"config"`
			}
			json.Unmarshal(body, &wire)
			_, present := wire.Config["temperature"]
			if present != (model != "claude-haiku-5-5") {
				t.Fatalf("%s temperature presence=%v: %s", model, present, body)
			}
			raw, ok := req.RawField("temperature")
			if !ok || string(raw) != "0.7" {
				t.Fatal("client request mutated")
			}
		})
	}
}

// Exercise the wire events clients consume, then return their tool inputs as history.
func TestDeepVToolStreamRoundTrip(t *testing.T) {
	cases := []struct {
		name, parts string
		count       int
	}{
		{"string fragments", `[{"functionCall":{"id":"call_a","name":"edit"}},{"functionCall":{"id":"call_a","args":"{\"path\":\"/tmp/"}},{"functionCall":{"id":"call_a","args":"probe.txt\"}"}}]`, 1},
		{"repeated object frames", `[{"functionCall":{"id":"call_a","name":"edit","args":{"path":"/tmp/probe.txt"}}},{"functionCall":{"id":"call_a","name":"edit","args":{"path":"/tmp/probe.txt"}}}]`, 1},
		{"parallel same tool", `[{"functionCall":{"id":"call_a","name":"edit","args":{"path":"/tmp/probe.txt"}}},{"functionCall":{"id":"call_b","name":"edit","args":{"path":"/tmp/probe.txt"}}}]`, 2},
		{"missing ids", `[{"functionCall":{"name":"edit","args":{"path":"/tmp/probe.txt"}}},{"functionCall":{"name":"edit","args":{"path":"/tmp/probe.txt"}}}]`, 2},
		{"omitted continuation id", `[{"functionCall":{"id":"call_a","name":"edit"}},{"functionCall":{"args":"{\"path\":\"/tmp/probe.txt\"}"}}]`, 1},
		{"late id", `[{"functionCall":{"name":"edit"}},{"functionCall":{"id":"call_a","args":"{\"path\":\"/tmp/probe.txt\"}"}}]`, 1},
		{"missing id continuation", `[{"functionCall":{"name":"edit"}},{"functionCall":{"args":"{\"path\":\"/tmp/probe.txt\"}"}}]`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parts []json.RawMessage
			if err := json.Unmarshal([]byte(tc.parts), &parts); err != nil {
				t.Fatal(err)
			}
			var source strings.Builder
			for _, part := range parts {
				source.WriteString(`data: {"candidates":[{"content":{"parts":[` + string(part) + `]}}]}` + "\n\n")
			}
			source.WriteString("data: [DONE]\n\n")
			p := NewDeepVProvider(&Config{Name: "deepv"}, "", "glm-5.3-flash")
			var output strings.Builder
			if err := p.convertStream(strings.NewReader(source.String()), &output, "deepv-glm-5.3-flash"); err != nil {
				t.Fatal(err)
			}
			// 按规范客户端的方式重组：start 只带空 input 占位，参数从
			// input_json_delta 累积（严格客户端如 DeepSeek harness 只认这条路径）。
			var blocks []ContentBlock
			ids := map[string]bool{}
			started := map[int]ContentBlock{}
			args := map[int]*strings.Builder{}
			for _, line := range strings.Split(output.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event struct {
					Type         string       `json:"type"`
					Index        int          `json:"index"`
					ContentBlock ContentBlock `json:"content_block"`
					Delta        struct {
						Type        string `json:"type"`
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				switch {
				case event.Type == "content_block_start" && event.ContentBlock.Type == "tool_use":
					b := event.ContentBlock
					if b.ID == "" || ids[b.ID] {
						t.Fatalf("missing or repeated id %q", b.ID)
					}
					ids[b.ID] = true
					started[event.Index] = b
				case event.Type == "content_block_delta" && event.Delta.Type == "input_json_delta":
					if _, ok := started[event.Index]; !ok {
						t.Fatalf("input_json_delta for unknown index %d", event.Index)
					}
					if args[event.Index] == nil {
						args[event.Index] = &strings.Builder{}
					}
					args[event.Index].WriteString(event.Delta.PartialJSON)
				case event.Type == "content_block_stop":
					b, ok := started[event.Index]
					if !ok {
						continue
					}
					accumulated := ""
					if args[event.Index] != nil {
						accumulated = args[event.Index].String()
					}
					var input map[string]interface{}
					if err := json.Unmarshal([]byte(accumulated), &input); err != nil || input["path"] != "/tmp/probe.txt" {
						t.Fatalf("tool input lost: %s (%v)", accumulated, err)
					}
					// UnmarshalJSON 会保留原始字节到 Raw 且 MarshalJSON 优先输出
					// Raw，重组后的参数必须同时清掉 Raw 才会生效。
					b.Raw = nil
					b.Input = json.RawMessage(accumulated)
					blocks = append(blocks, b)
				}
			}
			if len(blocks) != tc.count {
				t.Fatalf("calls=%d want=%d", len(blocks), tc.count)
			}
			if !strings.Contains(output.String(), `"stop_reason":"tool_use"`) {
				t.Fatal("missing tool_use finish reason")
			}
			history, _ := json.Marshal(map[string]interface{}{"model": "deepv-glm-5.3-flash", "messages": []interface{}{map[string]interface{}{"role": "assistant", "content": blocks}}})
			var req Request
			if err := json.Unmarshal(history, &req); err != nil {
				t.Fatal(err)
			}
			converted, err := p.convertRequest(&req)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range converted.Contents[0].Parts {
				if part.FunctionCall != nil && part.FunctionCall.Args["path"] != "/tmp/probe.txt" {
					t.Fatal("history lost path")
				}
			}
		})
	}
}

func TestDeepVStreamRejectsBrokenToolArguments(t *testing.T) {
	for _, args := range []string{`"{\"path\":"`, `"[]"`, `[]`} {
		t.Run(args, func(t *testing.T) {
			source := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","name":"edit","args":` + args + `}}]}}]}` + "\n\ndata: [DONE]\n\n"
			var output strings.Builder
			p := NewDeepVProvider(&Config{Name: "deepv"}, "", "glm-5.3-flash")
			if err := p.convertStream(strings.NewReader(source), &output, "deepv"); err == nil {
				t.Fatal("broken args accepted")
			}
			if strings.Contains(output.String(), "event: message_stop") {
				t.Fatal("error reported as success")
			}
		})
	}
}

func TestDeepVParseResponseStringAndDuplicateCalls(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv"}, "", "glm-5.3-flash")
	body := `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","name":"edit","args":"{\"path\":\"/tmp/probe.txt\"}"}},{"functionCall":{"id":"call_a","name":"edit","args":{"path":"/tmp/probe.txt"}}}]}}]}`
	response, err := p.parseResponse([]byte(body), "deepv")
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 1 || response.Content[0].ID != "call_a" {
		t.Fatalf("duplicate calls: %+v", response.Content)
	}
	if string(response.Content[0].Input) != `{"path":"/tmp/probe.txt"}` {
		t.Fatalf("input=%s", response.Content[0].Input)
	}
}

func TestDeepVStreamRejectsErrors(t *testing.T) {
	for _, source := range []string{
		"data: broken JSON\n\n",
		"data: {\"error\":\"upstream failed\"}\n\n",
		"data: " + strings.Repeat("x", 1024*1024) + "\n\n",
	} {
		var output strings.Builder
		p := NewDeepVProvider(&Config{Name: "deepv"}, "", "glm-5.3-flash")
		if err := p.convertStream(strings.NewReader(source), &output, "deepv"); err == nil {
			t.Fatal("stream failure accepted")
		}
		if strings.Contains(output.String(), "event: message_stop") {
			t.Fatal("error reported as success")
		}
	}
}

func TestDeepVStreamPreservesImageAndText(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv"}, "", "glm-5.3-flash")
	source := `data: {"candidates":[{"content":{"parts":[{"text":"generated"},{"inlineData":{"mimeType":"image/png","data":"cGl4ZWxz"}}]}}]}` + "\n\ndata: [DONE]\n\n"
	var output strings.Builder
	if err := p.convertStream(strings.NewReader(source), &output, "deepv"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"text_delta"`, `"text":"generated"`, `"type":"image"`, `"media_type":"image/png"`, `"data":"cGl4ZWxz"`, `event: message_stop`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestDeepVGeneratedCallIDsAcrossTurns(t *testing.T) {
	p := NewDeepVProvider(&Config{Name: "deepv"}, "", "glm-5.3-flash")
	body := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"inspect_file","args":{}}}]}}]}`)
	first, err := p.parseResponse(body, "deepv")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.parseResponse(body, "deepv")
	if err != nil {
		t.Fatal(err)
	}
	if first.Content[0].ID == second.Content[0].ID {
		t.Fatal("generated IDs reused across turns")
	}
}
