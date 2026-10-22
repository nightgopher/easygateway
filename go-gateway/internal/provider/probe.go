package provider

// 上游可用性探测。
//
// 背景：管理面板的「探测」以前调用 Provider.IsHealthy，而绝大多数实现只检查
// 本地配置（OpenAI 直接 return true、DeepV/Copilot/ChatGPT 只检查 token 字段
// 非空）。结果是探测恒为「在线」，即使上游已经把账号限流（429）、拒绝模型访问
// （403）也照报在线，而且探测成功还会把熔断器的连续失败计数清零，等于用假信号
// 掩盖真实故障。
//
// 这里的 Probe 走一遍真实转发路径，只发一个最小请求，然后把上游的 HTTP 结论
// 归类成在线/降级/离线；无法探测的 provider 明确报 unknown，不再冒充在线。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// ProbeStatus 是探测结论的分类。
//
// 区分 degraded 与 offline 是为了给出可执行的下一步：offline 表示需要人改配置
// （凭据失效、账号无该模型权限），degraded 表示上游暂时受限（限流、上游 5xx、
// 网络抖动），通常等待即可恢复。
type ProbeStatus string

const (
	ProbeOnline   ProbeStatus = "online"
	ProbeDegraded ProbeStatus = "degraded"
	ProbeOffline  ProbeStatus = "offline"
	ProbeUnknown  ProbeStatus = "unknown"
)

// probeTimeout 限制单次探测的总时长：探测必须比正常请求更早放弃，避免面板卡住。
const probeTimeout = 20 * time.Second

// probePrompt 是探测请求的正文。取最短的普通文本，避免触发长生成。
const probePrompt = "ping"

// ProbeResult 是一次上游探测的结论。
type ProbeResult struct {
	Status     ProbeStatus
	StatusCode int
	RetryAfter string
	Detail     string
	Latency    time.Duration
}

// Prober 是可选接口：provider 若能发一个最小请求来验证「鉴权 + 模型可用」，
// 就实现它。Router 的探测入口优先使用该接口；未实现的 provider 退回
// IsHealthy（只反映本地配置，不代表上游可用），并标记为 unknown。
type Prober interface {
	Probe(ctx context.Context) ProbeResult
}

// modelProber 用于探测需要显式模型名的 provider。绑定模型名的包装器
// （BoundModelProviderWrapper）会把绑定模型传给底层实现。
type modelProber interface {
	ProbeModel(ctx context.Context, model string) ProbeResult
}

// classifyProbeStatus 把上游 HTTP 状态码映射为探测结论。
//
// 401/402/403/404 归为 offline：这些是「改了配置才能恢复」的确定性拒绝，
// 包括凭据失效、账号无模型权限、模型不存在、欠费。
// 其余（429、5xx 等）归为 degraded：上游可达但当前受限，等待可能自愈。
func classifyProbeStatus(statusCode int) ProbeStatus {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return ProbeOnline
	case statusCode == http.StatusUnauthorized,
		statusCode == http.StatusPaymentRequired,
		statusCode == http.StatusForbidden,
		statusCode == http.StatusNotFound:
		return ProbeOffline
	default:
		return ProbeDegraded
	}
}

// probeVia 用一个最小请求走真实转发路径，把结果归类为探测结论。
// forward 传 provider 自己的 ForwardRequest，因此探测和线上请求走同一套
// 鉴权、格式转换和错误处理，不会出现「探测通过但真实请求失败」的偏差。
func probeVia(ctx context.Context, model string, maxTokens int, forward func(context.Context, *Request) (*Response, error)) ProbeResult {
	req := newProbeRequest(model, probePrompt)
	if req == nil {
		return ProbeResult{Status: ProbeUnknown, Detail: "无法构造探测请求"}
	}
	if maxTokens > 0 {
		// Anthropic 协议要求 max_tokens 必填；OpenAI 兼容协议留空让上游用默认值，
		// 避免过小的上限让「始终思考」类模型报参数错误而误判为离线。
		_ = req.SetRawField("max_tokens", maxTokens)
	}

	start := time.Now()
	_, err := forward(ctx, req)
	latency := time.Since(start)
	if err == nil {
		return ProbeResult{Status: ProbeOnline, Detail: "上游返回 2xx", Latency: latency}
	}

	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		status := classifyProbeStatus(providerErr.StatusCode)
		if providerErr.StatusCode == http.StatusBadRequest && strings.Contains(providerErr.Message, "ModelProtocolUnsupported") {
			status = ProbeOffline
		}
		return ProbeResult{
			Status:     status,
			StatusCode: providerErr.StatusCode,
			RetryAfter: providerErr.RetryAfter,
			Detail:     providerErr.Message,
			Latency:    latency,
		}
	}
	// 网络层错误（超时、连接被拒等）通常可自愈，归为 degraded 而不是离线。
	return ProbeResult{Status: ProbeDegraded, Detail: boundedErrorSummary(err), Latency: latency}
}

func newProbeRequest(model, prompt string) *Request {
	payload, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   false,
	})
	if err != nil {
		return nil
	}
	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil
	}
	return &req
}
