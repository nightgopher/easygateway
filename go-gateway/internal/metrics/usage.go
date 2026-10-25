// 用量成本统计：把 daily_stats 的 token 用量按 providers.yaml 声明的单价
// 折算为成本，支持按模型与渠道两个维度回答「这段时间花了多少钱」。
package metrics

import (
	"sort"

	"github.com/nightgopher/easygateway/internal/provider"
)

// UsageRow 是区间内一个「模型 × 渠道」组合的用量与成本。
type UsageRow struct {
	Model        string  `json:"model"`
	Provider     string  `json:"provider"`
	Requests     int     `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheRead    int64   `json:"cache_read_tokens"`
	CacheWrite   int64   `json:"cache_write_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	// Priced=false 表示该行没有可用单价或没有 token 拆分（升级前的历史行），
	// CostUSD 为 0 只是「未知」，不是「免费」。
	Priced bool `json:"priced"`
}

// UsageProviderRow 是渠道维度的用量与成本汇总。
type UsageProviderRow struct {
	Provider     string  `json:"provider"`
	Requests     int     `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Priced       bool    `json:"priced"`
}

// Usage 是 GET /admin/usage 的响应体。
type Usage struct {
	From         string             `json:"from"`
	To           string             `json:"to"`
	Rows         []UsageRow         `json:"rows"`
	Providers    []UsageProviderRow `json:"providers"`
	TotalCostUSD float64            `json:"total_cost_usd"`
	// UnpricedModels 列出没有任何单价、无法折算成本的模型，提示补配 pricing。
	UnpricedModels []string `json:"unpriced_models,omitempty"`
}

// AggregateUsage 把按天行聚合为模型/渠道维度的成本报表。
// priceOf 返回某个 key（先试请求模型名，再试渠道实例名）的单价；返回 nil
// 或未配置任何单价时该行记为 unpriced。
//
// 计费口径：input_tokens 按输入单价、output_tokens 按输出单价；缓存读/写
// 单独按缓存单价计。上游把缓存读计入 input_tokens 时（OpenAI 风格），
// 计费输入扣掉缓存读避免双计；上游分开上报时（Anthropic 风格）缓存读不在
// input_tokens 里，扣除后下限为 0。两语义下都是「不高于标价」的保守估计。
func AggregateUsage(rows []DailyStatRow, priceOf func(key string) *provider.ModelPricing) Usage {
	if priceOf == nil {
		priceOf = func(string) *provider.ModelPricing { return nil }
	}
	usage := Usage{}
	type providerAgg struct {
		row    UsageProviderRow
		priced bool
	}
	byModel := make(map[string]*UsageRow)
	byProvider := make(map[string]*providerAgg)
	unpriced := make(map[string]bool)

	for _, r := range rows {
		row, ok := byModel[r.Model+"/"+r.Provider]
		if !ok {
			row = &UsageRow{Model: r.Model, Provider: r.Provider}
			byModel[r.Model+"/"+r.Provider] = row
		}
		row.Requests += r.Requests
		row.InputTokens += int64(r.InputTokens)
		row.OutputTokens += int64(r.OutputTokens)
		row.CacheRead += int64(r.CacheReadInputTokens)
		row.CacheWrite += int64(r.CacheCreationInputTokens)

		if r.Provider != "" {
			agg, ok := byProvider[r.Provider]
			if !ok {
				agg = &providerAgg{}
				agg.row.Provider = r.Provider
				byProvider[r.Provider] = agg
			}
			agg.row.Requests += r.Requests
			agg.row.InputTokens += int64(r.InputTokens)
			agg.row.OutputTokens += int64(r.OutputTokens)
		}
	}

	for _, row := range byModel {
		pricing := priceOf(row.Model)
		if pricing == nil || !pricing.Priced() {
			pricing = priceOf(row.Provider)
		}
		if pricing != nil && pricing.Priced() && row.InputTokens+row.OutputTokens > 0 {
			row.Priced = true
			billableInput := row.InputTokens - row.CacheRead
			if billableInput < 0 {
				billableInput = 0
			}
			row.CostUSD = costOf(billableInput, pricing.InputPerM) +
				costOf(row.OutputTokens, pricing.OutputPerM) +
				costOf(row.CacheRead, pricing.CacheReadPerM) +
				costOf(row.CacheWrite, pricing.CacheWritePerM)
			usage.TotalCostUSD += row.CostUSD
		} else {
			if row.Model != "" {
				unpriced[row.Model] = true
			}
		}
		usage.Rows = append(usage.Rows, *row)
	}
	// 行按成本降序、再按请求数排，最贵的组合排最前。
	sort.Slice(usage.Rows, func(i, j int) bool {
		if usage.Rows[i].CostUSD != usage.Rows[j].CostUSD {
			return usage.Rows[i].CostUSD > usage.Rows[j].CostUSD
		}
		return usage.Rows[i].Requests > usage.Rows[j].Requests
	})

	for name, agg := range byProvider {
		// 渠道成本是它名下模型行成本的和；priced 由模型行聚合而来。
		for _, row := range usage.Rows {
			if row.Provider == name && row.Priced {
				agg.row.CostUSD += row.CostUSD
				agg.priced = true
			}
		}
		agg.row.Priced = agg.priced
		usage.Providers = append(usage.Providers, agg.row)
	}
	sort.Slice(usage.Providers, func(i, j int) bool {
		return usage.Providers[i].CostUSD > usage.Providers[j].CostUSD
	})

	for model := range unpriced {
		usage.UnpricedModels = append(usage.UnpricedModels, model)
	}
	sort.Strings(usage.UnpricedModels)
	return usage
}

// costOf 把 token 数按「每百万」单价折算为美元。
func costOf(tokens int64, perMillion float64) float64 {
	if tokens <= 0 || perMillion <= 0 {
		return 0
	}
	return float64(tokens) / 1_000_000 * perMillion
}
