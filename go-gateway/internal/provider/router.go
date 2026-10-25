package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nightgopher/easygateway/internal/requestmeta"
)

// Router 管理提供商路由和 fallback
type Router struct {
	providers      map[string]Provider
	chains         map[string][]string // 模型名 -> 提供商名列表
	models         map[string]ModelInfo
	runtime        map[string]*providerRuntime
	modelOverrides map[string]modelCapabilityOverride
	circuitConfig  CircuitBreakerConfig
	policy         RoutingPolicy
	rateGroups     map[string]*CircuitBreaker
	healthTTL      time.Duration
	doctorMu       sync.Mutex
	mu             sync.RWMutex
	logger         *log.Logger
}

type providerRuntime struct {
	channel string
	enabled bool
	breaker *CircuitBreaker
	quota   *CircuitBreaker
	// lastProbe 保存最近一次上游探测的结论。它与熔断器分开：熔断只反映连续
	// 请求失败，探测结论是「人工验证过上游可用性」的证据，面板据此区分
	// 「在线」和「只是没失败过」。
	lastProbe      ProbeResult
	lastProbeAt    time.Time
	hasProbe       bool
	anonymousProbe bool
}

type modelCapabilityOverride struct {
	capabilities    []string
	inputModalities []string
}

// RuntimeProviderStatus is the redacted provider state returned by the admin
// API. It never includes URL, API key, or any other credential material.
type RuntimeProviderStatus struct {
	Name                string
	Enabled             bool
	State               CircuitState
	Status              string
	ConsecutiveFailures int
	TotalFailures       int
	TotalSuccesses      int
	HasResult           bool
	LastFailureAt       time.Time
	LastSuccessAt       time.Time
	OpenedAt            time.Time
	NextRetryAt         time.Time
	// 最近一次上游探测的结论。HasProbe 为 false 表示从未探测过，此时 Status
	// 只反映熔断器，不代表上游可用性。
	HasProbe    bool
	ProbeStatus ProbeStatus
	ProbeDetail string
	LastProbeAt time.Time
}

type RouteProviderStatus struct {
	Name    string       `json:"name"`
	Enabled bool         `json:"enabled"`
	State   CircuitState `json:"state"`
	Status  string       `json:"status"`
}

type RouteStatus struct {
	Model     string                `json:"model"`
	Providers []RouteProviderStatus `json:"providers"`
}

var (
	ErrUnknownProvider = errors.New("unknown provider")
	ErrUnknownChain    = errors.New("unknown model route")
)

// NewRouter 创建新的路由器
func NewRouter(logger *log.Logger) *Router {
	return NewRouterWithCircuitConfig(logger, defaultCircuitBreakerConfig)
}

// NewRouterWithCircuitConfig creates a router with explicit circuit settings.
// The default constructor remains compatible with existing embedders.
func NewRouterWithCircuitConfig(logger *log.Logger, circuitConfig CircuitBreakerConfig) *Router {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Router{
		providers:      make(map[string]Provider),
		chains:         make(map[string][]string),
		models:         make(map[string]ModelInfo),
		runtime:        make(map[string]*providerRuntime),
		modelOverrides: make(map[string]modelCapabilityOverride),
		circuitConfig:  circuitConfig,
		policy:         DefaultRoutingPolicy(),
		rateGroups:     make(map[string]*CircuitBreaker),
		healthTTL:      15 * time.Minute,
		logger:         logger,
	}
}

// RegisterProvider 注册提供商
func (r *Router) RegisterProvider(name string, p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = p
	if existing, ok := r.runtime[name]; ok {
		// Preserve an administrator's enabled/disabled choice across a provider
		// refresh while replacing the circuit for the new client instance.
		existing.breaker = NewCircuitBreaker(r.circuitConfig)
	} else {
		r.runtime[name] = &providerRuntime{
			enabled: true,
			breaker: NewCircuitBreaker(r.circuitConfig),
		}
	}
	r.logger.Printf("Registered provider: %s", name)
}

// RegisterChain 注册 fallback 链
func (r *Router) RegisterChain(modelName string, providers []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chains[modelName] = append([]string(nil), providers...)
	if _, exists := r.models[modelName]; !exists {
		r.models[modelName] = r.inferChainModelInfoLocked(modelName, providers)
	}
	r.logger.Printf("Registered chain: %s -> %v", modelName, providers)
}

func (r *Router) inferChainModelInfoLocked(modelName string, providerNames []string) ModelInfo {
	capabilities := make([]string, 0)
	seen := make(map[string]bool)
	for _, providerName := range providerNames {
		configured, exists := r.providers[providerName]
		if !exists {
			continue
		}
		declared := []string{CapabilityText}
		if capable, ok := configured.(CapabilityProvider); ok {
			declared = capable.Capabilities()
		}
		for _, capability := range declared {
			if capability == "" || seen[capability] {
				continue
			}
			seen[capability] = true
			capabilities = append(capabilities, capability)
		}
	}
	if len(capabilities) == 0 {
		capabilities = []string{CapabilityText}
	}
	return ModelInfo{
		ID:              modelName,
		Provider:        "router",
		Capabilities:    capabilities,
		InputModalities: modelInputModalities(capabilities, nil),
	}
}

// RegisterModel records public metadata for a model or alias. It is separate
// from RegisterChain so routing remains compatible with manually registered
// providers while /v1/models can expose capabilities and limits.
func (r *Router) RegisterModel(info ModelInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if info.ID == "" {
		return
	}
	if len(info.Capabilities) == 0 {
		info.Capabilities = []string{CapabilityText}
	}
	info.Capabilities = append([]string(nil), info.Capabilities...)
	info.InputModalities = append([]string(nil), info.InputModalities...)
	r.models[info.ID] = info
}

// Route 返回模型对应的提供商链
func (r *Router) Route(modelName string) ([]Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerNames, ok := r.chains[modelName]
	if !ok {
		return nil, &UnknownModelError{Model: modelName}
	}

	var result []Provider
	var unavailable []string
	var reasons []string
	for _, name := range providerNames {
		p, ok := r.providers[name]
		if !ok {
			unavailable = append(unavailable, name)
			continue
		}
		if runtime := r.runtime[name]; runtime != nil {
			if !runtime.enabled || !runtime.breaker.CanAttempt() || (runtime.quota != nil && !runtime.quota.CanAttempt()) {
				unavailable = append(unavailable, name)
				continue
			}
		}
		// 账号渠道未登录时直接跳过：否则每次请求都要先发一次必然 401 的上游调用。
		if !providerAvailable(p) {
			if detail, ok := p.(interface{ UnavailableReason() string }); ok && detail.UnavailableReason() != "" {
				reasons = append(reasons, detail.UnavailableReason())
			}
			unavailable = append(unavailable, name)
			continue
		}
		result = append(result, p)
	}
	if len(result) == 0 {
		return nil, &NoAvailableProvidersError{Model: modelName, Configured: append([]string(nil), providerNames...), Reason: strings.Join(append([]string{"disabled, unavailable, or circuit open"}, reasons...), "; ")}
	}
	if len(unavailable) > 0 {
		r.logger.Printf("Skipping unavailable providers for model %s: %v", modelName, unavailable)
	}
	return result, nil
}

// ModelUsable reports whether a model currently has at least one provider
// worth recommending: the chain exists and some provider in it is enabled
// with usable credentials (a channel is logged in). Circuit-breaker and rate
// limit state are deliberately ignored — they are transient, while the
// admin surfaces that list this model for clients and the assistant should
// stay stable. Unlike Route it never logs and reserves no attempts.
func (r *Router) ModelUsable(modelName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	providerNames, ok := r.chains[modelName]
	if !ok {
		return false
	}
	for _, name := range providerNames {
		p, ok := r.providers[name]
		if !ok {
			continue
		}
		if runtime := r.runtime[name]; runtime != nil && !runtime.enabled {
			continue
		}
		if !providerAvailable(p) {
			continue
		}
		return true
	}
	return false
}

// RouteForRequest applies capability filtering before a request reaches a
// provider. If no configured provider supports the request, returning a client
// error is safer than dropping an image and retrying a text-only provider.
func (r *Router) RouteForRequest(modelName string, req *Request) ([]Provider, error) {
	r.mu.RLock()
	info := r.models[modelName]
	r.mu.RUnlock()
	if req != nil && info.Protocol == "systemone" {
		return nil, &UnsupportedCapabilityError{Model: modelName, Required: req.RequiredCapabilities(), Endpoint: "/v1/systemone"}
	}
	providers, err := r.Route(modelName)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return providers, nil
	}
	required := req.RequiredCapabilities()
	r.mu.RLock()
	override, hasOverride := r.modelOverrides[modelName]
	modelInfo, hasModelInfo := r.models[modelName]
	r.mu.RUnlock()
	if hasOverride && !capabilitiesSupportRequired(override.capabilities, required) {
		return nil, &UnsupportedCapabilityError{Model: modelName, Required: required}
	}
	// RegisterChain creates a compatibility-only "router" entry when a model
	// has no explicit metadata. Keep that legacy path permissive; only an
	// explicit RegisterModel entry should enforce per-model capabilities.
	if !hasOverride && hasModelInfo && modelInfo.Provider != "router" && !capabilitiesSupportRequired(modelInfo.Capabilities, required) {
		return nil, &UnsupportedCapabilityError{Model: modelName, Required: required}
	}
	filtered := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if supportsCapabilities(p, required) {
			filtered = append(filtered, p)
			continue
		}
		r.logger.Printf("Skipping provider %s for model %s: unsupported capabilities %v", p.Name(), modelName, required)
	}
	if len(filtered) == 0 {
		return nil, &UnsupportedCapabilityError{Model: modelName, Required: required}
	}
	return filtered, nil
}

func capabilitiesSupportRequired(declared, required []string) bool {
	set := make(map[string]bool, len(declared))
	for _, capability := range declared {
		set[capability] = true
	}
	for _, capability := range required {
		if !set[capability] {
			return false
		}
	}
	return true
}

// RouteForStream 返回流式请求的提供商链（功能同 Route，供 handler 使用）
func (r *Router) RouteForStream(modelName string) ([]Provider, error) {
	return r.Route(modelName)
}

// RouteForStreamRequest is the capability-aware variant used by streaming
// handlers. RouteForStream is retained for callers that only know the model.
func (r *Router) RouteForStreamRequest(modelName string, req *Request) ([]Provider, error) {
	return r.RouteForRequest(modelName, req)
}

// Forward 转发请求，带 fallback 逻辑（非流式）
func (r *Router) Forward(ctx context.Context, modelName string, req *Request) (*Response, error) {
	resp, _, _, err := r.ForwardWithDetails(ctx, modelName, req)
	return resp, err
}

// ForwardWithDetails forwards a non-streaming request and returns the final
// provider together with one bounded result for every provider attempt. The
// existing Forward method remains as a compatibility wrapper for embedders.
func (r *Router) ForwardWithDetails(ctx context.Context, modelName string, req *Request) (*Response, string, []requestmeta.ProviderAttempt, error) {
	ctx = r.BeginRouting(ctx)
	providers, err := r.RouteForRequest(modelName, req)
	if err != nil {
		return nil, "", nil, err
	}

	originalModel := req.Model
	var lastErr error
	var finalProvider string
	attempts := make([]requestmeta.ProviderAttempt, 0, len(providers))
	attempted := false
	for i, p := range providers {
		if r.ProviderAttempted(ctx, modelName, p) || !r.AllowProviderRequestFor(modelName, p) {
			continue
		}
		attempted = true
		started := time.Now()
		r.logger.Printf("Attempting provider %d/%d: %s", i+1, len(providers), p.Name())
		if bmp, ok := p.(BoundModelProvider); ok {
			req.Model = bmp.BoundModel()
		} else {
			req.Model = r.mapModelName(modelName, p.Name())
		}

		var resp *Response
		err := r.RunNonStreamAttempt(ctx, p.Name(), func(attemptCtx context.Context) error {
			var attemptErr error
			resp, attemptErr = p.ForwardRequest(attemptCtx, req)
			return attemptErr
		})
		req.Model = originalModel

		if err != nil {
			r.RecordProviderFailureFor(modelName, p, err)
			r.logger.Printf("Provider %s failed: %s", p.Name(), RedactedErrorText(err))
			lastErr = err
			attempt := requestmeta.ProviderAttempt{
				Provider:  p.Name(),
				Status:    "error",
				LatencyMs: float64(time.Since(started).Microseconds()) / 1000,
				Error:     boundedErrorSummary(err),
			}
			var providerErr *ProviderError
			if errors.As(err, &providerErr) {
				attempt.StatusCode = providerErr.StatusCode
			}
			attempts = append(attempts, attempt)
			finalProvider = p.Name()
			if ctx.Err() != nil || !ShouldFallback(err) {
				return nil, finalProvider, attempts, err
			}
			continue
		}
		r.RecordProviderSuccessFor(modelName, p)
		r.logger.Printf("Provider %s succeeded", p.Name())
		attempts = append(attempts, requestmeta.ProviderAttempt{
			Provider:  p.Name(),
			Status:    "success",
			LatencyMs: float64(time.Since(started).Microseconds()) / 1000,
		})
		return resp, p.Name(), attempts, nil
	}
	if !attempted && lastErr == nil {
		return nil, finalProvider, attempts, &NoAvailableProvidersError{Model: modelName, Configured: r.chainProviders(modelName), Reason: "disabled, unavailable, or circuit open"}
	}
	return nil, finalProvider, attempts, fmt.Errorf("all providers failed, last error: %w", lastErr)
}

func boundedErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	const maxBytes = 512
	value := err.Error()
	if len(value) > maxBytes {
		return value[:maxBytes] + "..."
	}
	return value
}

// mapModelName 把对外模型名映射到某个 provider 实际接受的上游模型名。
//
// 对外只暴露上游真名（providers.yaml 不再设别名），且模型绑定由
// BoundModelProvider 完成，因此这里恒等返回请求里的模型名。保留该函数是为了
// 现有调用点（handler 侧改写 req.Model）不变。
func (r *Router) mapModelName(modelName, providerName string) string {
	return modelName
}

// MapModel 是 mapModelName 的公开版本，供 handler 使用
func (r *Router) MapModel(modelName, providerName string) string {
	return r.mapModelName(modelName, providerName)
}

// ListProviders 列出所有提供商名称
func (r *Router) ListProviders() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListChains 列出所有已注册的 chain 名称（即可用模型名）
func (r *Router) ListChains() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.chains))
	for name := range r.chains {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListModelInfos returns a stable snapshot for /v1/models and admin tooling.
func (r *Router) ListModelInfos() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]ModelInfo, 0, len(r.models))
	for _, info := range r.models {
		info.Capabilities = append([]string(nil), info.Capabilities...)
		info.InputModalities = append([]string(nil), info.InputModalities...)
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// AllowProviderRequest reserves permission for one actual provider attempt.
// Route snapshots are intentionally non-reserving so failed stream attempts
// can safely fall back without leaking a half-open probe.
func (r *Router) AllowProviderRequest(name string) bool {
	r.mu.RLock()
	runtime, ok := r.runtime[name]
	if !ok || !runtime.enabled {
		r.mu.RUnlock()
		return false
	}
	allowed := runtime.quota == nil || runtime.quota.Allow()
	if allowed {
		allowed = runtime.breaker.Allow()
		if !allowed && runtime.quota != nil {
			runtime.quota.ReleaseProbe()
		}
	}
	r.mu.RUnlock()
	return allowed
}

// AllowProviderRequestFor resolves the configured chain key instead of using
// Provider.Name(). A model-bound wrapper may intentionally expose the base
// provider name, while its runtime circuit belongs to the chain key.
func (r *Router) AllowProviderRequestFor(modelName string, p Provider) bool {
	name := r.providerRuntimeName(modelName, p)
	if name == "" {
		return false
	}
	return r.AllowProviderRequest(name)
}

// RecordProviderSuccess records a completed request or stream.
func (r *Router) RecordProviderSuccess(name string) {
	r.mu.RLock()
	runtime := r.runtime[name]
	if runtime != nil {
		// runtime 的指针字段也会被渠道刷新替换，必须在 Router 锁内取快照。
		snapshot := *runtime
		runtime = &snapshot
	}
	r.mu.RUnlock()
	if runtime != nil {
		runtime.breaker.RecordSuccess()
		if runtime.quota != nil {
			runtime.quota.RecordSuccess()
		}
	}
}

func (r *Router) RecordProviderSuccessFor(modelName string, p Provider) {
	name := r.providerRuntimeName(modelName, p)
	if name != "" {
		r.RecordProviderSuccess(name)
	}
}

// RecordProviderFailure updates the circuit only for provider-level failures.
// Client validation/auth errors must not take a healthy provider offline.
func (r *Router) RecordProviderFailure(name string, err error) {
	if err == nil {
		return
	}
	r.mu.RLock()
	runtime := r.runtime[name]
	if runtime != nil {
		snapshot := *runtime
		runtime = &snapshot
	}
	r.mu.RUnlock()
	if runtime != nil {
		if isClientCancellation(err) || errors.Is(err, ErrRoutingBudgetExceeded) || !ShouldFallback(err) {
			runtime.breaker.ReleaseProbe()
			if runtime.quota != nil {
				runtime.quota.ReleaseProbe()
			}
			return
		}
		var httpErr *ProviderError
		if errors.As(err, &httpErr) && httpErr.StatusCode == 429 {
			runtime.breaker.RecordRateLimit(httpErr.RetryAfter)
			if runtime.quota != nil {
				runtime.quota.RecordRateLimit(httpErr.RetryAfter)
			}
		} else {
			runtime.breaker.RecordFailure()
			if runtime.quota != nil {
				runtime.quota.ReleaseProbe()
			}
		}
	}
}

func (r *Router) RecordProviderFailureFor(modelName string, p Provider, err error) {
	name := r.providerRuntimeName(modelName, p)
	if name != "" {
		r.RecordProviderFailure(name, err)
	}
}

func (r *Router) providerRuntimeName(modelName string, p Provider) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, name := range r.chains[modelName] {
		candidate := r.providers[name]
		if providersEqual(candidate, p) {
			return name
		}
	}
	if _, ok := r.runtime[p.Name()]; ok {
		return p.Name()
	}
	return ""
}

func providersEqual(left, right Provider) bool {
	if left == nil || right == nil {
		return false
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() || !leftValue.Type().Comparable() {
		return false
	}
	return leftValue.Interface() == rightValue.Interface()
}

// SetProviderEnabled enables or disables a provider for all chains.
func (r *Router) SetProviderEnabled(name string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	runtime, ok := r.runtime[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
	runtime.enabled = enabled
	return nil
}

// ResetProvider closes a provider circuit without changing its enabled state.
// 同时清掉上一次探测结论：重置的语义是「忘掉历史判断」，留着一个旧的
// offline 结论会让面板继续显示已修好的 provider 为离线。
func (r *Router) ResetProvider(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	runtime, ok := r.runtime[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
	runtime.breaker.Reset()
	if runtime.quota != nil {
		runtime.quota.Reset()
	}
	runtime.hasProbe = false
	runtime.lastProbe = ProbeResult{}
	runtime.lastProbeAt = time.Time{}
	return nil
}

// CheckProviderHealth runs a real upstream probe and records the outcome in the
// same circuit used by normal requests.
//
// 探测走真实请求，所以「探测通过」才真正代表上游可用。只有明确的上游结论
// （online/degraded/offline）才会改动熔断器；unknown（provider 不支持探测）
// 不写结果，避免用「没有结论」冒充成功而清掉真实的失败计数。
func (r *Router) CheckProviderHealth(ctx context.Context, name string) ProbeResult {
	r.mu.RLock()
	p, ok := r.providers[name]
	runtime := r.runtime[name]
	if runtime != nil {
		snapshot := *runtime
		runtime = &snapshot
	}
	r.mu.RUnlock()
	if !ok || runtime == nil {
		return ProbeResult{Status: ProbeUnknown, Detail: fmt.Sprintf("%v: %s", ErrUnknownProvider, name)}
	}
	if !runtime.enabled {
		return ProbeResult{Status: ProbeOffline, Detail: fmt.Sprintf("provider %s is disabled", name)}
	}
	if !providerAvailable(p) {
		detail := "provider credentials unavailable"
		if reason, ok := p.(interface{ UnavailableReason() string }); ok && reason.UnavailableReason() != "" {
			detail = reason.UnavailableReason()
		}
		return ProbeResult{Status: ProbeOffline, Detail: detail}
	}

	result := probeProvider(ctx, p)
	r.mu.Lock()
	// 旧实例的在途探测不能覆盖渠道刷新后的新实例状态。
	current := r.runtime[name]
	if current == nil || !providersEqual(r.providers[name], p) {
		r.mu.Unlock()
		return result
	}
	current.lastProbe = result
	current.lastProbeAt = time.Now()
	current.hasProbe = true
	snapshot := *current
	runtime = &snapshot
	r.mu.Unlock()
	switch result.Status {
	case ProbeOnline:
		runtime.breaker.RecordSuccess()
		if runtime.quota != nil {
			runtime.quota.RecordSuccess()
		}
	case ProbeDegraded, ProbeOffline:
		if result.StatusCode == http.StatusTooManyRequests {
			runtime.breaker.RecordRateLimit(result.RetryAfter)
			if runtime.quota != nil {
				runtime.quota.RecordRateLimit(result.RetryAfter)
			}
		} else {
			runtime.breaker.RecordFailure()
		}
	case ProbeUnknown:
		// 无结论：保持熔断器现状。
	}
	r.logger.Printf("Probe %s: %s (status=%d, latency=%v) %s", name, result.Status, result.StatusCode, result.Latency, result.Detail)
	return result
}

// probeProvider 用 Prober 接口探测；未实现该接口的 provider 返回 unknown
// 而不是冒充在线。
func probeProvider(ctx context.Context, p Provider) ProbeResult {
	if prober, ok := p.(Prober); ok {
		return prober.Probe(ctx)
	}
	return ProbeResult{Status: ProbeUnknown, Detail: "provider 不支持上游探测，健康状态未知"}
}

func (r *Router) ProviderStatus(name string) (RuntimeProviderStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	runtime, ok := r.runtime[name]
	if !ok || runtime == nil {
		return RuntimeProviderStatus{}, false
	}
	return runtimeStatus(name, runtime), true
}

func (r *Router) ListProviderStatuses() []RuntimeProviderStatus {
	names := r.ListProviders()
	result := make([]RuntimeProviderStatus, 0, len(names))
	for _, name := range names {
		if status, ok := r.ProviderStatus(name); ok {
			result = append(result, status)
		}
	}
	return result
}

func runtimeStatus(name string, runtime *providerRuntime) RuntimeProviderStatus {
	snapshot := runtime.breaker.Snapshot()
	if runtime.quota != nil {
		quota := runtime.quota.Snapshot()
		if quota.State != CircuitClosed {
			if snapshot.State != CircuitOpen {
				snapshot.State = quota.State
			}
			if quota.NextRetryAt.After(snapshot.NextRetryAt) {
				snapshot.NextRetryAt = quota.NextRetryAt
			}
		}
	}
	status := "unknown"
	if !runtime.enabled || snapshot.State == CircuitOpen {
		status = "offline"
	} else if runtime.hasProbe {
		// 探测是最新的、针对上游可用性的直接证据，优先于「历史请求成功过」。
		// 否则一个已被上游限流的 provider 会因为早先成功过而长期显示在线。
		switch runtime.lastProbe.Status {
		case ProbeOnline:
			status = "online"
		case ProbeDegraded:
			status = "degraded"
		case ProbeOffline:
			status = "offline"
		default:
			status = "unknown"
		}
	} else if snapshot.State == CircuitHalfOpen || (snapshot.HasResult && snapshot.ConsecutiveFailures > 0) {
		status = "degraded"
	} else if snapshot.State == CircuitClosed && snapshot.HasResult {
		status = "online"
	}
	return RuntimeProviderStatus{
		Name:                name,
		Enabled:             runtime.enabled,
		State:               snapshot.State,
		Status:              status,
		ConsecutiveFailures: snapshot.ConsecutiveFailures,
		TotalFailures:       snapshot.TotalFailures,
		TotalSuccesses:      snapshot.TotalSuccesses,
		HasResult:           snapshot.HasResult,
		LastFailureAt:       snapshot.LastFailureAt,
		LastSuccessAt:       snapshot.LastSuccessAt,
		OpenedAt:            snapshot.OpenedAt,
		NextRetryAt:         snapshot.NextRetryAt,
		HasProbe:            runtime.hasProbe,
		ProbeStatus:         runtime.lastProbe.Status,
		ProbeDetail:         runtime.lastProbe.Detail,
		LastProbeAt:         runtime.lastProbeAt,
	}
}

// SetChainOrder changes only the order of providers already present in a
// route. This prevents an admin typo from injecting an unregistered provider.
func (r *Router) SetChainOrder(modelName string, providers []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.chains[modelName]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownChain, modelName)
	}
	if len(providers) != len(current) {
		return fmt.Errorf("route %s must contain exactly %d providers", modelName, len(current))
	}
	wanted := make(map[string]bool, len(current))
	for _, name := range current {
		wanted[name] = true
	}
	seen := make(map[string]bool, len(providers))
	for _, name := range providers {
		if !wanted[name] {
			return fmt.Errorf("provider %s is not configured for route %s", name, modelName)
		}
		if seen[name] {
			return fmt.Errorf("provider %s is duplicated in route %s", name, modelName)
		}
		seen[name] = true
	}
	r.chains[modelName] = append([]string(nil), providers...)
	return nil
}

func (r *Router) chainProviders(modelName string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.chains[modelName]...)
}

// ListRouteStatuses returns ordered chains with redacted provider runtime
// state. The order is the actual failover priority used by the router.
func (r *Router) ListRouteStatuses() []RouteStatus {
	r.mu.RLock()
	models := make([]string, 0, len(r.chains))
	for model := range r.chains {
		models = append(models, model)
	}
	sort.Strings(models)
	result := make([]RouteStatus, 0, len(models))
	for _, model := range models {
		status := RouteStatus{Model: model}
		for _, name := range r.chains[model] {
			runtime := r.runtime[name]
			if runtime == nil {
				status.Providers = append(status.Providers, RouteProviderStatus{Name: name, State: CircuitOpen, Status: "offline"})
				continue
			}
			providerStatus := runtimeStatus(name, runtime)
			status.Providers = append(status.Providers, RouteProviderStatus{
				Name: providerStatus.Name, Enabled: providerStatus.Enabled, State: providerStatus.State, Status: providerStatus.Status,
			})
		}
		result = append(result, status)
	}
	r.mu.RUnlock()
	return result
}

// SetModelCapabilities updates the public model registry and applies an
// override used by capability-aware routing for subsequent requests.
func (r *Router) SetModelCapabilities(modelName string, capabilities, modalities []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.models[modelName]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownChain, modelName)
	}
	capabilities = normalizeModelCapabilities(capabilities)
	if len(modalities) == 0 {
		modalities = modelInputModalities(capabilities, nil)
	}
	info.Capabilities = append([]string(nil), capabilities...)
	info.InputModalities = append([]string(nil), modalities...)
	r.models[modelName] = info
	r.modelOverrides[modelName] = modelCapabilityOverride{
		capabilities: append([]string(nil), capabilities...), inputModalities: append([]string(nil), modalities...),
	}
	return nil
}

// SetProviderRateLimitGroup shares only quota cooldown, not ordinary failures.
// Configure this when an upstream applies one account/channel quota to models.
func (r *Router) SetProviderRateLimitGroup(name, group string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	runtime := r.runtime[name]
	if runtime == nil || group == "" {
		return
	}
	quota := r.rateGroups[group]
	if quota == nil {
		quota = NewCircuitBreaker(r.circuitConfig)
		r.rateGroups[group] = quota
	}
	runtime.quota = quota
}

// ModelChannel identifies a channel only when the model's entire chain belongs
// to one channel; mixed fallback chains have no single channel label.
func (r *Router) ModelChannel(model string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	channel := ""
	for _, name := range r.chains[model] {
		runtime := r.runtime[name]
		if runtime == nil || runtime.channel == "" {
			return ""
		}
		if channel != "" && channel != runtime.channel {
			return ""
		}
		channel = runtime.channel
	}
	return channel
}

// SetProviderChannel associates configured model instances with their login channel.
func (r *Router) SetProviderChannel(name, channel string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if runtime := r.runtime[name]; runtime != nil {
		runtime.channel = channel
	}
}
func (r *Router) ChannelProviderNames(channel string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for name, runtime := range r.runtime {
		if runtime.channel == channel && channel != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ResetChannelProviders clears stale missing-credential probes after successful login.
func (r *Router) ResetChannelProviders(channel string) {
	for _, name := range r.ChannelProviderNames(channel) {
		_ = r.ResetProvider(name)
	}
}
