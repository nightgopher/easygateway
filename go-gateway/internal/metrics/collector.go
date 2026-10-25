package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nightgopher/easygateway/internal/requestmeta"
)

// RequestRecord 单条请求记录
type RequestRecord struct {
	RequestID                string                        `json:"request_id,omitempty"`
	ProviderAttempts         []requestmeta.ProviderAttempt `json:"provider_attempts,omitempty"`
	Timestamp                time.Time                     `json:"timestamp"`
	Method                   string                        `json:"method"`
	Path                     string                        `json:"path"`
	Model                    string                        `json:"model"`
	Provider                 string                        `json:"provider"`
	StatusCode               int                           `json:"status_code"`
	Latency                  float64                       `json:"latency_ms"` // 毫秒
	InputTokens              int                           `json:"input_tokens"`
	OutputTokens             int                           `json:"output_tokens"`
	CacheReadInputTokens     int                           `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int                           `json:"cache_creation_input_tokens"`
	CacheInputTokens         int                           `json:"cache_input_tokens"`
	CacheUsageKnown          bool                          `json:"cache_usage_known"`
	IsStream                 bool                          `json:"is_stream"`
	Error                    string                        `json:"error,omitempty"`
}

// ModelStats 模型维度统计
type ModelStats struct {
	Model                    string   `json:"model"`
	Requests                 int      `json:"requests"`
	Successes                int      `json:"successes"`
	Errors                   int      `json:"errors"`
	TotalTokens              int      `json:"total_tokens"`
	AvgLatency               float64  `json:"avg_latency_ms"`
	Provider                 string   `json:"provider"`
	CacheReadInputTokens     int      `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens"`
	CacheInputTokens         int      `json:"cache_input_tokens"`
	CacheUsageRequests       int      `json:"cache_usage_requests"`
	CacheHitRate             *float64 `json:"cache_hit_rate"`
}

// ProviderStats 提供商维度统计
type ProviderStats struct {
	Provider   string    `json:"provider"`
	Requests   int       `json:"requests"`
	Successes  int       `json:"successes"`
	Errors     int       `json:"errors"`
	AvgLatency float64   `json:"avg_latency_ms"`
	Status     string    `json:"status"` // "online", "degraded", "offline"
	LastCheck  time.Time `json:"last_check"`
}

// DashboardSummary 仪表盘概览
type DashboardSummary struct {
	TodayRequests            int      `json:"today_requests"`
	TodayTokens              int64    `json:"today_tokens"`
	SuccessRate              float64  `json:"success_rate"`
	ActiveModels             int      `json:"active_models"`
	AvgLatency               float64  `json:"avg_latency_ms"`
	Uptime                   string   `json:"uptime"`
	CacheReadInputTokens     int      `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int      `json:"cache_creation_input_tokens"`
	CacheInputTokens         int      `json:"cache_input_tokens"`
	CacheUsageRequests       int      `json:"cache_usage_requests"`
	CacheHitRate             *float64 `json:"cache_hit_rate"`
}

// CacheStats contains only provider-reported cache usage. A request whose
// provider omitted cache usage is intentionally excluded from the hit-rate
// denominator and sample count.
type CacheStats struct {
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	CacheInputTokens         int
	CacheUsageRequests       int
}

// Store 持久化存储接口
type Store interface {
	SaveRecord(r RequestRecord) error
	GetRecentLogs(limit int) ([]RequestRecord, error)
}

// IsBusinessRequest reports whether a request is one of the model API entry
// points that should be included in Dashboard metrics and request logs.
// Browser UI traffic, health checks, model discovery, admin endpoints, and
// static assets are intentionally excluded from business metrics.
func IsBusinessRequest(method, path string) bool {
	if method != http.MethodPost {
		return false
	}

	switch path {
	case "/v1/messages", "/messages",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses", "/v1/systemone", "/systemone":
		return true
	default:
		return false
	}
}

// SSE 已提交 HTTP 200 后仍可能中断；最终请求错误比响应头更能反映终态。
func RequestSucceeded(r RequestRecord) bool {
	return r.StatusCode >= 200 && r.StatusCode < 400 && r.Error == ""
}

// Collector 内存指标收集器
type Collector struct {
	mu            sync.RWMutex
	records       []RequestRecord // 环形缓冲
	maxRecords    int
	startTime     time.Time
	todayStart    time.Time
	store         Store // 可选的持久化存储
	persistCtx    context.Context
	cancelWrites  context.CancelFunc
	pendingWrites int
	writesDone    chan struct{}
	persistError  error
	closed        bool

	// 按天重置的计数器
	todayTotal      int
	todayTokens     int64
	todaySuccess    int
	todayErrors     int
	todayLatencySum float64
	todayCacheStats CacheStats

	// 按模型聚合
	modelStats map[string]*ModelStats

	// 按提供商聚合
	providerStats map[string]*ProviderStats

	// 活跃模型集合（今日有请求的）
	activeModels map[string]bool
}

// NewCollector 创建新的指标收集器
func NewCollector() *Collector {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	persistCtx, cancelWrites := context.WithCancel(context.Background())
	writesDone := make(chan struct{})
	close(writesDone)
	return &Collector{
		records:       make([]RequestRecord, 0, 2000),
		maxRecords:    2000,
		startTime:     now,
		todayStart:    todayStart,
		modelStats:    make(map[string]*ModelStats),
		providerStats: make(map[string]*ProviderStats),
		activeModels:  make(map[string]bool),
		persistCtx:    persistCtx,
		cancelWrites:  cancelWrites,
		writesDone:    writesDone,
	}
}

// SetStore 设置持久化存储
func (c *Collector) SetStore(store Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = store
	if store == nil || len(c.records) > 0 {
		return
	}
	// Restore the recent lightweight log window after a restart. The Dashboard
	// KPIs and per-model/provider aggregates are "today"-scoped, so replay only
	// today's persisted records into the aggregates; older records are kept in
	// the ring buffer for the request log view.
	persisted, err := store.GetRecentLogs(c.maxRecords)
	if err != nil {
		return
	}
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	c.todayStart = todayStart
	for i := len(persisted) - 1; i >= 0; i-- {
		r := persisted[i]
		if !IsBusinessRequest(r.Method, r.Path) {
			continue
		}
		c.records = append(c.records, r)
		if !r.Timestamp.Before(todayStart) {
			c.applyToAggregates(r)
		}
	}

	// SQLite keeps unbounded daily aggregates separately from the 2,000-entry
	// request-log window. Prefer those totals after restart when the store
	// supports them, so the cache rate does not silently shrink to the log ring.
	if dailyStore, ok := store.(interface {
		GetDailyStats(date string) ([]ModelStats, error)
	}); ok {
		stats, err := dailyStore.GetDailyStats(todayStart.Format("2006-01-02"))
		if err == nil && len(stats) > 0 {
			c.modelStats = make(map[string]*ModelStats, len(stats))
			c.activeModels = make(map[string]bool, len(stats))
			c.todayTotal = 0
			c.todaySuccess = 0
			c.todayErrors = 0
			c.todayLatencySum = 0
			c.todayTokens = 0
			c.todayCacheStats = CacheStats{}
			for i := range stats {
				ms := &stats[i]
				if ms.Model != "" {
					c.modelStats[ms.Model] = ms
					c.activeModels[ms.Model] = true
				}
				c.todayTotal += ms.Requests
				c.todaySuccess += ms.Successes
				c.todayErrors += ms.Errors
				// 日汇总不限于日志窗口，重启后的今日 Token 必须从同一基线恢复。
				c.todayTokens += int64(ms.TotalTokens)
				c.todayLatencySum += ms.AvgLatency * float64(ms.Requests)
				c.todayCacheStats.CacheReadInputTokens += ms.CacheReadInputTokens
				c.todayCacheStats.CacheCreationInputTokens += ms.CacheCreationInputTokens
				c.todayCacheStats.CacheInputTokens += ms.CacheInputTokens
				c.todayCacheStats.CacheUsageRequests += ms.CacheUsageRequests
			}
		}
	}
}

// Record 记录一条请求
func (c *Collector) Record(r RequestRecord) {
	if !IsBusinessRequest(r.Method, r.Path) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 检查是否需要重置每日计数器
	now := r.Timestamp
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if todayStart.After(c.todayStart) {
		c.resetDaily(todayStart)
	}

	// 追加到环形缓冲
	if len(c.records) >= c.maxRecords {
		// 移除最旧的记录
		copy(c.records, c.records[1:])
		c.records[len(c.records)-1] = r
	} else {
		c.records = append(c.records, r)
	}

	// 持久化到 SQLite
	if c.store != nil && !c.closed {
		if c.pendingWrites == 0 {
			c.writesDone = make(chan struct{})
		}
		c.pendingWrites++
		store := c.store
		go c.saveRecord(store, r)
	}

	c.applyToAggregates(r)
}

func (c *Collector) saveRecord(store Store, r RequestRecord) {
	var err error
	if contextual, ok := store.(interface {
		SaveRecordContext(context.Context, RequestRecord) error
	}); ok {
		err = contextual.SaveRecordContext(c.persistCtx, r)
	} else {
		err = store.SaveRecord(r)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.persistError == nil {
		c.persistError = err
	}
	c.pendingWrites--
	if c.pendingWrites == 0 {
		close(c.writesDone)
	}
}

// Close 必须在 HTTP 请求排空后调用；接收过的异步指标先落盘，再关闭数据库。
// 共用退出 deadline，写入挂起时取消支持 context 的存储，不无限等待。
func (c *Collector) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	done := c.writesDone
	c.mu.Unlock()
	select {
	case <-done:
		c.cancelWrites()
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.persistError
	case <-ctx.Done():
		c.cancelWrites()
		return ctx.Err()
	}
}

// applyToAggregates 把一条业务请求计入今日计数器与模型/提供商聚合。
// Record 与重启恢复（SetStore）共用；调用方需持锁并保证时间顺序。
func (c *Collector) applyToAggregates(r RequestRecord) {
	// 更新每日计数
	c.todayTotal++
	if RequestSucceeded(r) {
		c.todaySuccess++
	} else {
		c.todayErrors++
	}
	c.todayLatencySum += r.Latency
	c.todayTokens += int64(totalTokens(r))

	// 更新模型统计
	if r.Model != "" {
		c.activeModels[r.Model] = true
		ms, ok := c.modelStats[r.Model]
		if !ok {
			ms = &ModelStats{Model: r.Model, Provider: r.Provider}
			c.modelStats[r.Model] = ms
		}
		ms.Requests++
		ms.TotalTokens += totalTokens(r)
		if RequestSucceeded(r) {
			ms.Successes++
		} else {
			ms.Errors++
		}
		// 增量平均延迟
		ms.AvgLatency = (ms.AvgLatency*float64(ms.Requests-1) + r.Latency) / float64(ms.Requests)
		applyModelCacheStats(ms, r)
	}
	applyCacheStats(&c.todayCacheStats, r)

	// 更新提供商统计
	if r.Provider != "" {
		ps, ok := c.providerStats[r.Provider]
		if !ok {
			ps = &ProviderStats{Provider: r.Provider, Status: "online"}
			c.providerStats[r.Provider] = ps
		}
		ps.Requests++
		ps.LastCheck = r.Timestamp
		if RequestSucceeded(r) {
			ps.Successes++
		} else {
			ps.Errors++
			// 连续错误超过阈值则标记为 degraded
			if ps.Errors > 0 && float64(ps.Errors)/float64(ps.Requests) > 0.3 {
				ps.Status = "degraded"
			}
		}
		ps.AvgLatency = (ps.AvgLatency*float64(ps.Requests-1) + r.Latency) / float64(ps.Requests)
	}
}

// resetDaily 重置每日计数器
func (c *Collector) resetDaily(todayStart time.Time) {
	c.todayStart = todayStart
	c.todayTotal = 0
	c.todaySuccess = 0
	c.todayErrors = 0
	c.todayLatencySum = 0
	c.todayCacheStats = CacheStats{}
	c.activeModels = make(map[string]bool)
	// modelStats/providerStats 同样按「今日」语义清理，与面板标题一致
	c.modelStats = make(map[string]*ModelStats)
	c.providerStats = make(map[string]*ProviderStats)
}

// GetDashboard 获取仪表盘概览
func (c *Collector) GetDashboard() DashboardSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var successRate float64
	if c.todayTotal > 0 {
		successRate = float64(c.todaySuccess) / float64(c.todayTotal) * 100
	}

	var avgLatency float64
	if c.todayTotal > 0 {
		avgLatency = c.todayLatencySum / float64(c.todayTotal)
	}

	uptime := time.Since(c.startTime)
	uptimeStr := formatDuration(uptime)

	return DashboardSummary{
		TodayRequests:            c.todayTotal,
		TodayTokens:              c.todayTokens,
		SuccessRate:              successRate,
		ActiveModels:             len(c.activeModels),
		AvgLatency:               avgLatency,
		Uptime:                   uptimeStr,
		CacheReadInputTokens:     c.todayCacheStats.CacheReadInputTokens,
		CacheCreationInputTokens: c.todayCacheStats.CacheCreationInputTokens,
		CacheInputTokens:         c.todayCacheStats.CacheInputTokens,
		CacheUsageRequests:       c.todayCacheStats.CacheUsageRequests,
		CacheHitRate:             cacheHitRate(c.todayCacheStats),
	}
}

// GetModelStats 获取所有模型统计
func (c *Collector) GetModelStats() []ModelStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]ModelStats, 0, len(c.modelStats))
	for _, ms := range c.modelStats {
		copy := *ms
		copy.CacheHitRate = cacheHitRate(CacheStats{
			CacheReadInputTokens:     copy.CacheReadInputTokens,
			CacheCreationInputTokens: copy.CacheCreationInputTokens,
			CacheInputTokens:         copy.CacheInputTokens,
			CacheUsageRequests:       copy.CacheUsageRequests,
		})
		result = append(result, copy)
	}
	return result
}

func applyCacheStats(total *CacheStats, r RequestRecord) {
	if !r.CacheUsageKnown {
		return
	}
	total.CacheReadInputTokens += r.CacheReadInputTokens
	total.CacheCreationInputTokens += r.CacheCreationInputTokens
	total.CacheInputTokens += r.CacheInputTokens
	total.CacheUsageRequests++
}

func applyModelCacheStats(total *ModelStats, r RequestRecord) {
	if !r.CacheUsageKnown {
		return
	}
	total.CacheReadInputTokens += r.CacheReadInputTokens
	total.CacheCreationInputTokens += r.CacheCreationInputTokens
	total.CacheInputTokens += r.CacheInputTokens
	total.CacheUsageRequests++
}

func cacheHitRate(stats CacheStats) *float64 {
	if stats.CacheInputTokens <= 0 {
		return nil
	}
	rate := float64(stats.CacheReadInputTokens) / float64(stats.CacheInputTokens) * 100
	return &rate
}

func totalTokens(r RequestRecord) int {
	totalInput := r.InputTokens
	if r.CacheUsageKnown && r.CacheInputTokens > totalInput {
		// Anthropic reports uncached input separately from cache reads/writes;
		// OpenAI and Gemini report cached input as part of input_tokens.
		totalInput = r.CacheInputTokens
	}
	return totalInput + r.OutputTokens
}

// GetProviderStats 获取所有提供商统计
func (c *Collector) GetProviderStats() []ProviderStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]ProviderStats, 0, len(c.providerStats))
	for _, ps := range c.providerStats {
		result = append(result, *ps)
	}
	return result
}

// GetRecentLogs 获取最近的请求日志
func (c *Collector) GetRecentLogs(limit int) []RequestRecord {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if limit <= 0 || limit > len(c.records) {
		limit = len(c.records)
	}

	// 返回最新的 limit 条
	start := len(c.records) - limit
	if start < 0 {
		start = 0
	}
	result := make([]RequestRecord, limit)
	copy(result, c.records[start:])

	// 反转，最新的在前
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// GetUptime 返回服务运行时长
func (c *Collector) GetUptime() time.Duration {
	return time.Since(c.startTime)
}

// formatDuration 格式化时长为人类可读字符串
func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
