package metrics

import (
	"sort"
	"time"
)

// DailyStatRow 是 daily_stats 一行（按天 × 模型 × 渠道的聚合增量），
// 由存储层读出，是 History 聚合的输入。
type DailyStatRow struct {
	Date                     string  `json:"date"`
	Model                    string  `json:"model"`
	Provider                 string  `json:"provider"`
	Requests                 int     `json:"requests"`
	Successes                int     `json:"successes"`
	Errors                   int     `json:"errors"`
	TotalTokens              int     `json:"total_tokens"`
	TotalLatencyMS           float64 `json:"total_latency_ms"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	CacheInputTokens         int     `json:"cache_input_tokens"`
	CacheUsageRequests       int     `json:"cache_usage_requests"`
	// InputTokens/OutputTokens 是 input/output 拆分（成本统计用）。列是后来
	// 加的：升级前的历史行两列为 0，成本统计据 Priced 标记而非按 0 冒充。
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// HistoryDay 趋势序列中的一天。
type HistoryDay struct {
	Date         string   `json:"date"`
	Requests     int      `json:"requests"`
	Successes    int      `json:"successes"`
	Errors       int      `json:"errors"`
	TotalTokens  int64    `json:"total_tokens"`
	AvgLatencyMS float64  `json:"avg_latency_ms"`
	SuccessRate  float64  `json:"success_rate"`
	CacheHitRate *float64 `json:"cache_hit_rate"`
}

// HistoryModel 区间内单模型的汇总；Provider 为请求数最多的渠道。
type HistoryModel struct {
	Model        string   `json:"model"`
	Provider     string   `json:"provider"`
	Requests     int      `json:"requests"`
	Successes    int      `json:"successes"`
	Errors       int      `json:"errors"`
	TotalTokens  int64    `json:"total_tokens"`
	AvgLatencyMS float64  `json:"avg_latency_ms"`
	CacheHitRate *float64 `json:"cache_hit_rate"`
}

// HistoryProvider 区间内单渠道的汇总。
type HistoryProvider struct {
	Provider     string  `json:"provider"`
	Requests     int     `json:"requests"`
	Successes    int     `json:"successes"`
	Errors       int     `json:"errors"`
	TotalTokens  int64   `json:"total_tokens"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

// History 是 /admin/stats/history 的响应体：按天序列 + 模型/渠道排行。
type History struct {
	From      string            `json:"from"`
	To        string            `json:"to"`
	Days      []HistoryDay      `json:"days"`
	Models    []HistoryModel    `json:"models"`
	Providers []HistoryProvider `json:"providers"`
}

type historyCacheStats struct {
	read, creation, input, usageRequests int
}

func (cs *historyCacheStats) hitRate() *float64 {
	return cacheHitRate(CacheStats{
		CacheReadInputTokens:     cs.read,
		CacheCreationInputTokens: cs.creation,
		CacheInputTokens:         cs.input,
		CacheUsageRequests:       cs.usageRequests,
	})
}

// AggregateHistory 把存储层的按天行聚合为趋势序列与排行。
// 区间内缺失的日期补零，保证序列连续、客户端可直接画图。
func AggregateHistory(rows []DailyStatRow, from, to string) History {
	h := History{From: from, To: to}

	dayCache := make(map[string]*historyCacheStats)
	byDay := make(map[string]*HistoryDay, 32)
	for date := range dateRange(from, to) {
		byDay[date] = &HistoryDay{Date: date}
		dayCache[date] = &historyCacheStats{}
	}

	type modelAgg struct {
		agg    HistoryModel
		cache  historyCacheStats
		byProv map[string]int
	}
	models := make(map[string]*modelAgg)
	providers := make(map[string]*HistoryProvider)

	for _, r := range rows {
		if day, ok := byDay[r.Date]; ok {
			day.Requests += r.Requests
			day.Successes += r.Successes
			day.Errors += r.Errors
			day.TotalTokens += int64(r.TotalTokens)
			day.AvgLatencyMS += r.TotalLatencyMS
			cs := dayCache[r.Date]
			cs.read += r.CacheReadInputTokens
			cs.creation += r.CacheCreationInputTokens
			cs.input += r.CacheInputTokens
			cs.usageRequests += r.CacheUsageRequests
		}

		if r.Model != "" {
			m, ok := models[r.Model]
			if !ok {
				m = &modelAgg{byProv: make(map[string]int)}
				m.agg.Model = r.Model
				models[r.Model] = m
			}
			m.agg.Requests += r.Requests
			m.agg.Successes += r.Successes
			m.agg.Errors += r.Errors
			m.agg.TotalTokens += int64(r.TotalTokens)
			m.agg.AvgLatencyMS += r.TotalLatencyMS
			m.cache.read += r.CacheReadInputTokens
			m.cache.creation += r.CacheCreationInputTokens
			m.cache.input += r.CacheInputTokens
			m.cache.usageRequests += r.CacheUsageRequests
			if r.Provider != "" {
				m.byProv[r.Provider] += r.Requests
			}
		}

		if r.Provider != "" {
			p, ok := providers[r.Provider]
			if !ok {
				p = &HistoryProvider{Provider: r.Provider}
				providers[r.Provider] = p
			}
			p.Requests += r.Requests
			p.Successes += r.Successes
			p.Errors += r.Errors
			p.TotalTokens += int64(r.TotalTokens)
			p.AvgLatencyMS += r.TotalLatencyMS
		}
	}

	for date, day := range byDay {
		if day.Requests > 0 {
			day.SuccessRate = float64(day.Successes) / float64(day.Requests) * 100
			day.AvgLatencyMS /= float64(day.Requests)
			day.CacheHitRate = dayCache[date].hitRate()
		}
		h.Days = append(h.Days, *day)
	}
	sort.Slice(h.Days, func(i, j int) bool { return h.Days[i].Date < h.Days[j].Date })

	for _, m := range models {
		if m.agg.Requests > 0 {
			m.agg.AvgLatencyMS /= float64(m.agg.Requests)
			m.agg.CacheHitRate = m.cache.hitRate()
		}
		best, bestReq := "", 0
		for name, req := range m.byProv {
			if req > bestReq {
				best, bestReq = name, req
			}
		}
		m.agg.Provider = best
		h.Models = append(h.Models, m.agg)
	}
	sort.Slice(h.Models, func(i, j int) bool { return h.Models[i].Requests > h.Models[j].Requests })

	for _, p := range providers {
		if p.Requests > 0 {
			p.AvgLatencyMS /= float64(p.Requests)
		}
		h.Providers = append(h.Providers, *p)
	}
	sort.Slice(h.Providers, func(i, j int) bool { return h.Providers[i].Requests > h.Providers[j].Requests })

	return h
}

// dateRange 返回 [from, to] 内的 YYYY-MM-DD 集合；无法解析时返回空。
// 上限 400 天，与 handler 的区间校验一致，防御异常参数。
func dateRange(from, to string) map[string]struct{} {
	out := map[string]struct{}{}
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return out
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil || end.Before(start) {
		return out
	}
	for d := start; !d.After(end) && len(out) < 400; d = d.AddDate(0, 0, 1) {
		out[d.Format("2006-01-02")] = struct{}{}
	}
	return out
}
