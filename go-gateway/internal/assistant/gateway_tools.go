package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	piagent "github.com/nightgopher/easyagent/sdk/agent"
)

// 网关运行状态工具：让助理能回答"哪些 Provider 熔断了""今天流量如何"
// 这类运维问题，而不是凭记忆猜测。实现走 loopback Admin API——与助理
// 的 LLM 出口同一套 BaseURL/APIKey（默认 MasterKey），只读、不落库。

const gatewayHTTPTimeout = 15 * time.Second

type gatewayStatusTool struct {
	baseURL, apiKey string
}

func newGatewayStatusTool(baseURL, apiKey string) piagent.Tool {
	return &gatewayStatusTool{baseURL: baseURL, apiKey: apiKey}
}
func (t *gatewayStatusTool) Name() string { return "gateway_status" }
func (t *gatewayStatusTool) Description() string {
	return "查询网关实时运行状态：各 Provider 的健康/熔断状态、请求数与延迟，以及今日请求总量、成功率、平均延迟和缓存命中率。回答任何关于网关当前运行状况的问题前必须先调用本工具。"
}
func (t *gatewayStatusTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *gatewayStatusTool) Validate(params json.RawMessage) (json.RawMessage, error) {
	return params, nil
}
func (t *gatewayStatusTool) Execute(ctx context.Context, _ json.RawMessage, _ func(piagent.PartialResult)) (piagent.ToolResult, error) {
	var payload struct {
		Summary struct {
			TodayRequests int      `json:"today_requests"`
			SuccessRate   float64  `json:"success_rate"`
			AvgLatency    float64  `json:"avg_latency_ms"`
			CacheHitRate  *float64 `json:"cache_hit_rate"`
			Uptime        string   `json:"uptime"`
		} `json:"summary"`
		Providers []struct {
			Name       string  `json:"name"`
			Status     string  `json:"status"`
			State      string  `json:"state"`
			Requests   int     `json:"requests"`
			Errors     int     `json:"errors"`
			AvgLatency float64 `json:"avg_latency"`
		} `json:"providers"`
	}
	if err := t.fetch(ctx, "/admin/dashboard", &payload); err != nil {
		return piagent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "今日请求 %d · 成功率 %.1f%% · 平均延迟 %.0f ms · 运行 %s",
		payload.Summary.TodayRequests, payload.Summary.SuccessRate, payload.Summary.AvgLatency, payload.Summary.Uptime)
	if payload.Summary.CacheHitRate != nil {
		fmt.Fprintf(&b, " · 缓存命中 %.1f%%", *payload.Summary.CacheHitRate)
	}
	b.WriteString("\nProvider 状态：")
	for _, p := range payload.Providers {
		fmt.Fprintf(&b, "\n- %s：健康=%s 熔断器=%s 请求=%d 错误=%d 平均延迟=%.0f ms",
			p.Name, p.Status, p.State, p.Requests, p.Errors, p.AvgLatency)
	}
	if len(payload.Providers) == 0 {
		b.WriteString("（无）")
	}
	return piagent.ToolResult{Content: b.String()}, nil
}

type gatewayLogsTool struct {
	baseURL, apiKey string
}

func newGatewayLogsTool(baseURL, apiKey string) piagent.Tool {
	return &gatewayLogsTool{baseURL: baseURL, apiKey: apiKey}
}
func (t *gatewayLogsTool) Name() string { return "gateway_logs" }
func (t *gatewayLogsTool) Description() string {
	return "查询网关最近的请求日志（时间、模型、Provider、状态码、延迟、错误）。默认最近 10 条；failures_only=true 时只看失败的请求，用于排查熔断和上游错误。"
}
func (t *gatewayLogsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"limit":         map[string]any{"type": "integer", "description": "返回条数，默认 10，上限 50"},
			"failures_only": map[string]any{"type": "boolean", "description": "只返回失败的请求"},
		},
	}
}
func (t *gatewayLogsTool) Validate(params json.RawMessage) (json.RawMessage, error) {
	return params, nil
}
func (t *gatewayLogsTool) Execute(ctx context.Context, params json.RawMessage, _ func(piagent.PartialResult)) (piagent.ToolResult, error) {
	limit, failuresOnly := 10, false
	var args struct {
		Limit        *int `json:"limit"`
		FailuresOnly bool `json:"failures_only"`
	}
	if len(params) > 0 && json.Unmarshal(params, &args) == nil {
		if args.Limit != nil {
			limit = *args.Limit
		}
		failuresOnly = args.FailuresOnly
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	var payload struct {
		Logs []struct {
			Timestamp string  `json:"timestamp"`
			Model     string  `json:"model"`
			Provider  string  `json:"provider"`
			Code      int     `json:"status_code"`
			Latency   float64 `json:"latency_ms"`
			Error     string  `json:"error"`
		} `json:"logs"`
	}
	if err := t.fetch(ctx, "/admin/logs?limit="+fmt.Sprint(limit), &payload); err != nil {
		return piagent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	var b strings.Builder
	count := 0
	for _, l := range payload.Logs {
		if failuresOnly && l.Code < 400 && l.Error == "" {
			continue
		}
		count++
		line := fmt.Sprintf("- %s %s → %s HTTP %d %.0f ms", l.Timestamp, l.Model, l.Provider, l.Code, l.Latency)
		if l.Error != "" {
			line += " 错误：" + l.Error
		}
		b.WriteString(line + "\n")
	}
	if count == 0 {
		if failuresOnly {
			return piagent.ToolResult{Content: "最近请求中没有失败记录。"}, nil
		}
		return piagent.ToolResult{Content: "暂无请求日志。"}, nil
	}
	return piagent.ToolResult{Content: b.String()}, nil
}

func (t *gatewayStatusTool) fetch(ctx context.Context, path string, out any) error {
	return fetchGatewayJSON(ctx, t.baseURL, t.apiKey, path, out)
}
func (t *gatewayLogsTool) fetch(ctx context.Context, path string, out any) error {
	return fetchGatewayJSON(ctx, t.baseURL, t.apiKey, path, out)
}

func fetchGatewayJSON(ctx context.Context, baseURL, apiKey, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, gatewayHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("构造网关查询失败：%v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("查询网关状态失败：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("网关查询返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析网关响应失败：%v", err)
	}
	return nil
}
