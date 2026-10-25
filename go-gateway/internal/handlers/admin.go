package handlers

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/provider"
)

// DailyStatsSource 读取持久化的按天聚合行，历史统计端点的数据来源。
type DailyStatsSource interface {
	GetDailyStatRows(from, to string) ([]metrics.DailyStatRow, error)
}

// AdminHandler 处理管理面板 API
type AdminHandler struct {
	router     *provider.Router
	collector  *metrics.Collector
	logger     *log.Logger
	dailyStats DailyStatsSource // 可为 nil：无持久化时历史端点返回 503
}

// NewAdminHandler 创建管理面板 handler
func NewAdminHandler(router *provider.Router, collector *metrics.Collector, logger *log.Logger, dailyStats DailyStatsSource) *AdminHandler {
	return &AdminHandler{
		router:     router,
		collector:  collector,
		logger:     logger,
		dailyStats: dailyStats,
	}
}

// HandleDashboard GET /admin/dashboard
// 返回仪表盘 KPI 概览
func (h *AdminHandler) HandleDashboard(c *gin.Context) {
	summary := h.collector.GetDashboard()

	providers := h.providerPayloads()

	// 补充模型排行
	modelStats := h.collector.GetModelStats()
	models := make([]gin.H, 0, len(modelStats))
	for _, ms := range modelStats {
		models = append(models, gin.H{
			"model":                       ms.Model,
			"provider":                    ms.Provider,
			"requests":                    ms.Requests,
			"total_tokens":                ms.TotalTokens,
			"avg_latency":                 ms.AvgLatency,
			"successes":                   ms.Successes,
			"errors":                      ms.Errors,
			"cache_read_input_tokens":     ms.CacheReadInputTokens,
			"cache_creation_input_tokens": ms.CacheCreationInputTokens,
			"cache_input_tokens":          ms.CacheInputTokens,
			"cache_usage_requests":        ms.CacheUsageRequests,
			"cache_hit_rate":              ms.CacheHitRate,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"summary": gin.H{
			"today_requests":              summary.TodayRequests,
			"today_tokens":                summary.TodayTokens,
			"success_rate":                summary.SuccessRate,
			"active_models":               summary.ActiveModels,
			"avg_latency_ms":              summary.AvgLatency,
			"uptime":                      summary.Uptime,
			"cache_read_input_tokens":     summary.CacheReadInputTokens,
			"cache_creation_input_tokens": summary.CacheCreationInputTokens,
			"cache_input_tokens":          summary.CacheInputTokens,
			"cache_usage_requests":        summary.CacheUsageRequests,
			"cache_hit_rate":              summary.CacheHitRate,
		},
		"providers": providers,
		"models":    models,
	})
}

// HandleProviders GET /admin/providers
// 返回所有提供商状态
func (h *AdminHandler) HandleProviders(c *gin.Context) {
	result := h.providerPayloads()

	c.JSON(http.StatusOK, gin.H{
		"providers": result,
		"total":     len(result),
	})
}

// HandleModels GET /admin/models
// 返回所有模型统计
func (h *AdminHandler) HandleModels(c *gin.Context) {
	modelStats := h.collector.GetModelStats()
	statsByModel := make(map[string]metrics.ModelStats, len(modelStats))
	for _, stat := range modelStats {
		statsByModel[stat.Model] = stat
	}
	routes := h.router.ListRouteStatuses()
	routeByModel := make(map[string]provider.RouteStatus, len(routes))
	for _, route := range routes {
		routeByModel[route.Model] = route
	}

	result := make([]gin.H, 0)
	seen := make(map[string]bool)
	for _, item := range h.router.ModelCatalog() {
		info := item.Info
		seen[info.ID] = true
		stat := statsByModel[info.ID]
		entry := modelPayload(info, stat, routeByModel[info.ID])
		health := item.Health
		entry["health"] = health
		switch health.Status {
		case "available":
			entry["status"] = "online"
		case "unavailable":
			entry["status"] = "offline"
		default:
			entry["status"] = health.Status
		}
		result = append(result, entry)
	}
	// Preserve historical stats if a model was removed from the current config
	// during a live reload.
	for _, stat := range modelStats {
		if seen[stat.Model] {
			continue
		}
		result = append(result, gin.H{
			"model": stat.Model, "provider": stat.Provider, "status": "removed",
			"requests": stat.Requests, "total_tokens": stat.TotalTokens,
			"avg_latency": stat.AvgLatency, "successes": stat.Successes, "errors": stat.Errors,
			"cache_read_input_tokens":     stat.CacheReadInputTokens,
			"cache_creation_input_tokens": stat.CacheCreationInputTokens,
			"cache_input_tokens":          stat.CacheInputTokens,
			"cache_usage_requests":        stat.CacheUsageRequests,
			"cache_hit_rate":              stat.CacheHitRate,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"models": result,
		"total":  len(result),
	})
}

// HandleLogs GET /admin/logs
// 返回最近的请求日志
func (h *AdminHandler) HandleLogs(c *gin.Context) {
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	logs := h.collector.GetRecentLogs(limit)
	result := make([]gin.H, 0, len(logs))
	for _, r := range logs {
		entry := gin.H{
			"timestamp":                   r.Timestamp,
			"request_id":                  r.RequestID,
			"method":                      r.Method,
			"path":                        r.Path,
			"model":                       r.Model,
			"provider":                    r.Provider,
			"provider_attempts":           r.ProviderAttempts,
			"status_code":                 r.StatusCode,
			"latency_ms":                  r.Latency,
			"input_tokens":                r.InputTokens,
			"output_tokens":               r.OutputTokens,
			"cache_read_input_tokens":     r.CacheReadInputTokens,
			"cache_creation_input_tokens": r.CacheCreationInputTokens,
			"cache_input_tokens":          r.CacheInputTokens,
			"cache_usage_known":           r.CacheUsageKnown,
			"is_stream":                   r.IsStream,
		}
		if r.CacheUsageKnown && r.CacheInputTokens > 0 {
			rate := float64(r.CacheReadInputTokens) / float64(r.CacheInputTokens) * 100
			entry["cache_hit_rate"] = rate
		} else {
			entry["cache_hit_rate"] = nil
		}
		if r.Error != "" {
			entry["error"] = r.Error
		}
		result = append(result, entry)
	}

	c.JSON(http.StatusOK, gin.H{
		"logs":  result,
		"total": len(result),
	})
}

// HandleHealth GET /admin/health
// 返回详细的系统健康状态
func (h *AdminHandler) HandleHealth(c *gin.Context) {
	dashboard := h.collector.GetDashboard()
	providerStatuses := make([]gin.H, 0)
	onlineCount := 0
	knownProviderCount := 0
	degradedProvider := false
	for _, status := range h.router.ListProviderStatuses() {
		switch status.Status {
		case "online":
			onlineCount++
			knownProviderCount++
		case "degraded", "offline":
			knownProviderCount++
			degradedProvider = true
		default:
			// A provider with no request or probe result is configured but
			// unverified. It must not make the whole gateway look degraded.
			// The Dashboard displays this state as "unknown" separately.
		}
		providerStatuses = append(providerStatuses, gin.H{
			"provider": status.Name,
			"status":   status.Status,
			"state":    status.State,
			"enabled":  status.Enabled,
		})
	}

	overallStatus := "ok"
	if dashboard.SuccessRate < 95 && dashboard.TodayRequests > 10 {
		overallStatus = "degraded"
	}
	if knownProviderCount > 0 && onlineCount == 0 {
		overallStatus = "degraded"
	} else if degradedProvider {
		overallStatus = "degraded"
	}

	c.JSON(http.StatusOK, gin.H{
		"status":           overallStatus,
		"uptime":           dashboard.Uptime,
		"today_requests":   dashboard.TodayRequests,
		"success_rate":     dashboard.SuccessRate,
		"active_models":    dashboard.ActiveModels,
		"avg_latency_ms":   dashboard.AvgLatency,
		"providers_online": onlineCount,
		"providers_total":  len(providerStatuses),
		"providers":        providerStatuses,
	})
}

// HandleConfig GET /admin/config
// 返回当前配置信息
func (h *AdminHandler) HandleConfig(c *gin.Context) {
	providers := h.router.ListProviders()
	chains := h.router.ListChains()

	c.JSON(http.StatusOK, gin.H{
		"providers":       providers,
		"chains":          chains,
		"routes":          h.router.ListRouteStatuses(),
		"models":          h.router.ListModelInfos(),
		"total_providers": len(providers),
		"total_chains":    len(chains),
	})
}

// HandleRoutes GET /admin/routes returns the ordered failover chains.
func (h *AdminHandler) HandleRoutes(c *gin.Context) {
	routes := h.router.ListRouteStatuses()
	c.JSON(http.StatusOK, gin.H{"routes": routes, "total": len(routes)})
}

type providerUpdateRequest struct {
	Enabled *bool `json:"enabled"`
}

// HandleUpdateProvider PATCH /admin/providers/:name updates runtime controls.
// Only enablement is mutable here; credentials remain environment-backed.
func (h *AdminHandler) HandleUpdateProvider(c *gin.Context) {
	var request providerUpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled must be a boolean"})
		return
	}
	if err := h.router.SetProviderEnabled(c.Param("name"), *request.Enabled); err != nil {
		h.writeControlError(c, err)
		return
	}
	status, _ := h.router.ProviderStatus(c.Param("name"))
	c.JSON(http.StatusOK, providerStatusPayload(status))
}

// HandleResetProvider POST /admin/providers/:name/reset manually closes a
// circuit after an operator has fixed credentials or an upstream incident.
func (h *AdminHandler) HandleResetProvider(c *gin.Context) {
	if err := h.router.ResetProvider(c.Param("name")); err != nil {
		h.writeControlError(c, err)
		return
	}
	status, _ := h.router.ProviderStatus(c.Param("name"))
	c.JSON(http.StatusOK, providerStatusPayload(status))
}

// HandleCheckProvider POST /admin/providers/:name/health-check runs a real
// upstream probe on demand. It reports the classified outcome (online /
// degraded / offline / unknown) together with the upstream evidence.
func (h *AdminHandler) HandleCheckProvider(c *gin.Context) {
	result := h.router.CheckProviderHealth(c.Request.Context(), c.Param("name"))
	payload := gin.H{
		"healthy":      result.Status == provider.ProbeOnline,
		"probe_status": string(result.Status),
		"detail":       result.Detail,
	}
	if result.StatusCode > 0 {
		payload["status_code"] = result.StatusCode
	}
	if result.Latency > 0 {
		payload["latency_ms"] = float64(result.Latency.Microseconds()) / 1000
	}
	status, _ := h.router.ProviderStatus(c.Param("name"))
	payload["provider"] = providerStatusPayload(status)
	c.JSON(http.StatusOK, payload)
}

type routeUpdateRequest struct {
	Providers []string `json:"providers"`
}

// HandleUpdateRoute PUT /admin/routes/:model changes failover priority while
// requiring the same provider set as the current route.
func (h *AdminHandler) HandleUpdateRoute(c *gin.Context) {
	var request routeUpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil || len(request.Providers) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "providers must be a non-empty array"})
		return
	}
	if err := h.router.SetChainOrder(c.Param("model"), request.Providers); err != nil {
		h.writeControlError(c, err)
		return
	}
	for _, route := range h.router.ListRouteStatuses() {
		if route.Model == c.Param("model") {
			c.JSON(http.StatusOK, route)
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
}

type modelUpdateRequest struct {
	Capabilities    *[]string `json:"capabilities"`
	InputModalities *[]string `json:"input_modalities"`
}

// HandleUpdateModel PUT /admin/models/:model updates public capability
// metadata and the capability gate used for later requests.
func (h *AdminHandler) HandleUpdateModel(c *gin.Context) {
	var request modelUpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil || (request.Capabilities == nil && request.InputModalities == nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "capabilities or input_modalities is required"})
		return
	}
	var current provider.ModelInfo
	found := false
	for _, info := range h.router.ListModelInfos() {
		if info.ID == c.Param("model") {
			current, found = info, true
			break
		}
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
		return
	}
	capabilities := current.Capabilities
	var modalities []string
	if request.Capabilities != nil {
		capabilities = *request.Capabilities
	}
	if request.InputModalities != nil {
		modalities = *request.InputModalities
	} else if request.Capabilities == nil {
		modalities = current.InputModalities
	}
	if err := h.router.SetModelCapabilities(c.Param("model"), capabilities, modalities); err != nil {
		h.writeControlError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model": c.Param("model")})
}

// HandleStats GET /admin/stats
// 返回详细统计信息
func (h *AdminHandler) HandleStats(c *gin.Context) {
	dashboard := h.collector.GetDashboard()
	modelStats := h.collector.GetModelStats()
	providerStats := h.collector.GetProviderStats()

	// 计算总 token 数
	totalTokens := 0
	for _, ms := range modelStats {
		totalTokens += ms.TotalTokens
	}

	c.JSON(http.StatusOK, gin.H{
		"summary": gin.H{
			"today_requests":              dashboard.TodayRequests,
			"success_rate":                dashboard.SuccessRate,
			"active_models":               dashboard.ActiveModels,
			"avg_latency_ms":              dashboard.AvgLatency,
			"uptime":                      dashboard.Uptime,
			"total_tokens":                totalTokens,
			"cache_read_input_tokens":     dashboard.CacheReadInputTokens,
			"cache_creation_input_tokens": dashboard.CacheCreationInputTokens,
			"cache_input_tokens":          dashboard.CacheInputTokens,
			"cache_usage_requests":        dashboard.CacheUsageRequests,
			"cache_hit_rate":              dashboard.CacheHitRate,
		},
		"models":    modelStats,
		"providers": providerStats,
	})
}

func (h *AdminHandler) providerPayloads() []gin.H {
	stats := make(map[string]metrics.ProviderStats)
	for _, stat := range h.collector.GetProviderStats() {
		stats[stat.Provider] = stat
	}
	result := make([]gin.H, 0)
	for _, status := range h.router.ListProviderStatuses() {
		entry := providerStatusPayload(status)
		if stat, ok := stats[status.Name]; ok {
			entry["requests"] = stat.Requests
			entry["successes"] = stat.Successes
			entry["errors"] = stat.Errors
			entry["avg_latency"] = stat.AvgLatency
			entry["last_check"] = stat.LastCheck
		} else {
			entry["requests"] = 0
			entry["successes"] = 0
			entry["errors"] = 0
			entry["avg_latency"] = 0
		}
		result = append(result, entry)
	}
	return result
}

func providerStatusPayload(status provider.RuntimeProviderStatus) gin.H {
	entry := gin.H{
		"name":                 status.Name,
		"enabled":              status.Enabled,
		"status":               status.Status,
		"state":                status.State,
		"consecutive_failures": status.ConsecutiveFailures,
		"total_failures":       status.TotalFailures,
		"total_successes":      status.TotalSuccesses,
	}
	if !status.LastFailureAt.IsZero() {
		entry["last_failure_at"] = status.LastFailureAt.Format(time.RFC3339Nano)
	}
	if !status.LastSuccessAt.IsZero() {
		entry["last_success_at"] = status.LastSuccessAt.Format(time.RFC3339Nano)
	}
	// 探测结论单独暴露：面板据此区分「探测过且在线」和「只是没失败过」。
	if status.HasProbe {
		entry["has_probe"] = true
		entry["probe_status"] = string(status.ProbeStatus)
		if status.ProbeDetail != "" {
			entry["probe_detail"] = status.ProbeDetail
		}
		if !status.LastProbeAt.IsZero() {
			entry["last_probe_at"] = status.LastProbeAt.Format(time.RFC3339Nano)
		}
	}
	if !status.OpenedAt.IsZero() {
		entry["opened_at"] = status.OpenedAt.Format(time.RFC3339Nano)
	}
	if !status.NextRetryAt.IsZero() {
		entry["next_retry_at"] = status.NextRetryAt.Format(time.RFC3339Nano)
	}
	return entry
}

func modelPayload(info provider.ModelInfo, stat metrics.ModelStats, route provider.RouteStatus) gin.H {
	status := "idle"
	if stat.Requests > 0 {
		status = "online"
		if stat.Errors > 0 && float64(stat.Errors)/float64(stat.Requests) > 0.3 {
			status = "degraded"
		}
	}
	providerNames := make([]string, 0, len(route.Providers))
	for _, item := range route.Providers {
		providerNames = append(providerNames, item.Name)
		if item.Status == "offline" && status == "idle" {
			status = "offline"
		}
	}
	entry := gin.H{
		"model":                       info.ID,
		"provider":                    info.Provider,
		"status":                      status,
		"requests":                    stat.Requests,
		"total_tokens":                stat.TotalTokens,
		"avg_latency":                 stat.AvgLatency,
		"successes":                   stat.Successes,
		"errors":                      stat.Errors,
		"cache_read_input_tokens":     stat.CacheReadInputTokens,
		"cache_creation_input_tokens": stat.CacheCreationInputTokens,
		"cache_input_tokens":          stat.CacheInputTokens,
		"cache_usage_requests":        stat.CacheUsageRequests,
		"cache_hit_rate":              stat.CacheHitRate,
		"capabilities":                info.Capabilities,
		"input_modalities":            info.InputModalities,
		"providers":                   providerNames,
	}
	if info.Protocol != "" {
		entry["protocol"] = info.Protocol
	}
	if info.MaxInputTokens > 0 {
		entry["max_input_tokens"] = info.MaxInputTokens
	}
	if info.MaxOutputTokens > 0 {
		entry["max_output_tokens"] = info.MaxOutputTokens
	}
	return entry
}

func (h *AdminHandler) writeControlError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, provider.ErrUnknownProvider) || errors.Is(err, provider.ErrUnknownChain) {
		status = http.StatusNotFound
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

// statsHistoryMaxDays 限制单次查询的区间跨度，防止一次拉取全年数据
// 生成过大响应；需要更长趋势时可分段请求。
const statsHistoryMaxDays = 400

// HandleStatsHistory GET /admin/stats/history?days=30 或 ?from=2026-09-01&to=2026-09-30
// 返回区间内按天序列与模型/渠道排行，数据来自持久化的 daily_stats，
// 跨重启可查。days 缺省 7。
func (h *AdminHandler) HandleStatsHistory(c *gin.Context) {
	if h.dailyStats == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "history stats unavailable: persistence disabled"})
		return
	}

	now := time.Now()
	from, to := now.AddDate(0, 0, -6).Format("2006-01-02"), now.Format("2006-01-02")
	if c.Query("from") != "" || c.Query("to") != "" {
		var err error
		from, to, err = parseStatsRange(c.Query("from"), c.Query("to"), now)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	} else if raw := c.Query("days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "days must be a positive integer"})
			return
		}
		if days > statsHistoryMaxDays {
			days = statsHistoryMaxDays
		}
		from = now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	}

	rows, err := h.dailyStats.GetDailyStatRows(from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, metrics.AggregateHistory(rows, from, to))
}

// parseStatsRange 校验并规范化显式区间：两端均为 YYYY-MM-DD，from <= to，
// 跨度不超过 statsHistoryMaxDays；缺省端以 today 补齐。
func parseStatsRange(fromRaw, toRaw string, now time.Time) (string, string, error) {
	today := now.Format("2006-01-02")
	if fromRaw == "" {
		fromRaw = today
	}
	if toRaw == "" {
		toRaw = today
	}
	from, err := time.Parse("2006-01-02", fromRaw)
	if err != nil {
		return "", "", errors.New("from must be YYYY-MM-DD")
	}
	to, err := time.Parse("2006-01-02", toRaw)
	if err != nil {
		return "", "", errors.New("to must be YYYY-MM-DD")
	}
	if to.Before(from) {
		return "", "", errors.New("from must not be after to")
	}
	if to.Sub(from) > statsHistoryMaxDays*24*time.Hour {
		return "", "", errors.New("range too large (max 400 days)")
	}
	if to.After(now) {
		to = now
	}
	return from.Format("2006-01-02"), to.Format("2006-01-02"), nil
}
