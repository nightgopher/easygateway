package metrics

import (
	"testing"

	"github.com/nightgopher/easygateway/internal/provider"
)

func priceOf(table map[string]*provider.ModelPricing) func(string) *provider.ModelPricing {
	return func(key string) *provider.ModelPricing { return table[key] }
}

func TestAggregateUsageComputesCost(t *testing.T) {
	rows := []DailyStatRow{
		{Date: "2026-10-01", Model: "coding", Provider: "glm-5.3", Requests: 10, InputTokens: 2_000_000, OutputTokens: 1_000_000, CacheReadInputTokens: 500_000},
		{Date: "2026-10-02", Model: "glm-5.3", Provider: "glm-5.3", Requests: 5, InputTokens: 1_000_000, OutputTokens: 500_000},
	}
	table := map[string]*provider.ModelPricing{
		"glm-5.3": {InputPerM: 3, OutputPerM: 12, CacheReadPerM: 0.6},
	}
	usage := AggregateUsage(rows, priceOf(table))
	if len(usage.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(usage.Rows))
	}
	// coding 行：计费输入 = 2M - 0.5M 缓存读 = 1.5M；缓存读按 0.6/M 单独计。
	wantCoding := 1.5*3 + 1.0*12 + 0.5*0.6
	var codingRow *UsageRow
	for i := range usage.Rows {
		if usage.Rows[i].Model == "coding" {
			codingRow = &usage.Rows[i]
		}
	}
	if codingRow == nil || !codingRow.Priced {
		t.Fatalf("coding 行应借渠道实例名单价计价: %+v", codingRow)
	}
	if diff := codingRow.CostUSD - wantCoding; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("coding cost = %v, want %v", codingRow.CostUSD, wantCoding)
	}
	wantTotal := wantCoding + (1.0*3 + 0.5*12)
	if diff := usage.TotalCostUSD - wantTotal; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("total = %v, want %v", usage.TotalCostUSD, wantTotal)
	}
	// 渠道维度：glm-5.3 渠道聚合两行成本。
	if len(usage.Providers) != 1 || usage.Providers[0].Provider != "glm-5.3" {
		t.Fatalf("providers = %+v", usage.Providers)
	}
	if diff := usage.Providers[0].CostUSD - wantTotal; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("provider cost = %v, want %v", usage.Providers[0].CostUSD, wantTotal)
	}
	if len(usage.UnpricedModels) != 0 {
		t.Fatalf("不应有 unpriced 模型: %v", usage.UnpricedModels)
	}
}

func TestAggregateUsageMarksUnpriced(t *testing.T) {
	rows := []DailyStatRow{
		{Date: "2026-10-01", Model: "mystery", Provider: "p1", Requests: 3, InputTokens: 100, OutputTokens: 100},
		{Date: "2026-10-01", Model: "glm-5.3", Provider: "p1", Requests: 1, InputTokens: 100, OutputTokens: 100},
	}
	usage := AggregateUsage(rows, priceOf(map[string]*provider.ModelPricing{
		"glm-5.3": {InputPerM: 3},
	}))
	for _, row := range usage.Rows {
		if row.Model == "mystery" {
			if row.Priced || row.CostUSD != 0 {
				t.Fatalf("无单价模型不应计价: %+v", row)
			}
		}
	}
	if len(usage.UnpricedModels) != 1 || usage.UnpricedModels[0] != "mystery" {
		t.Fatalf("unpriced = %v", usage.UnpricedModels)
	}
}

// 升级前的历史行没有 input/output 拆分（两列为 0）：即使配了单价也不计成本，
// 记为 unpriced 提示，而不是把缺失数据当成免费。
func TestAggregateUsageLegacyRowsWithoutSplit(t *testing.T) {
	rows := []DailyStatRow{
		{Date: "2026-09-01", Model: "glm-5.3", Provider: "p1", Requests: 9, TotalTokens: 5000},
	}
	usage := AggregateUsage(rows, priceOf(map[string]*provider.ModelPricing{
		"glm-5.3": {InputPerM: 3, OutputPerM: 12},
	}))
	if usage.Rows[0].Priced || usage.TotalCostUSD != 0 {
		t.Fatalf("无拆分的历史行不应计价: %+v", usage.Rows[0])
	}
	if len(usage.UnpricedModels) != 1 {
		t.Fatalf("unpriced = %v", usage.UnpricedModels)
	}
}

// 上游把缓存读计入 input_tokens 时（OpenAI 风格），计费输入扣除缓存读避免双计。
func TestAggregateUsageAvoidsDoubleBillingCache(t *testing.T) {
	rows := []DailyStatRow{
		{Date: "2026-10-01", Model: "glm-5.3", Provider: "p1", Requests: 1, InputTokens: 1_000_000, OutputTokens: 0, CacheReadInputTokens: 400_000},
	}
	usage := AggregateUsage(rows, priceOf(map[string]*provider.ModelPricing{
		"glm-5.3": {InputPerM: 3, CacheReadPerM: 0.6},
	}))
	want := 0.6*3 + 0.4*0.6
	if diff := usage.Rows[0].CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want %v (缓存读只按缓存价计一次)", usage.Rows[0].CostUSD, want)
	}
}

func TestAggregateUsageEmptyPriceOf(t *testing.T) {
	usage := AggregateUsage([]DailyStatRow{{Model: "m", Requests: 1, InputTokens: 5}}, nil)
	if usage.Rows[0].Priced || usage.TotalCostUSD != 0 {
		t.Fatalf("无单价表时全部 unpriced: %+v", usage.Rows[0])
	}
}

// 空模型名的行只计入渠道汇总，不产生 model="" 的报表行。
func TestAggregateUsageSkipsEmptyModelRows(t *testing.T) {
	rows := []DailyStatRow{
		{Date: "2026-10-01", Model: "", Provider: "p1", Requests: 2, InputTokens: 100, OutputTokens: 50},
		{Date: "2026-10-01", Model: "glm-5.3", Provider: "p1", Requests: 1, InputTokens: 100, OutputTokens: 50},
	}
	usage := AggregateUsage(rows, priceOf(map[string]*provider.ModelPricing{
		"glm-5.3": {InputPerM: 3, OutputPerM: 12},
	}))
	for _, row := range usage.Rows {
		if row.Model == "" {
			t.Fatalf("不应有空模型名的报表行: %+v", usage.Rows)
		}
	}
	if len(usage.Providers) != 1 || usage.Providers[0].Requests != 3 || usage.Providers[0].InputTokens != 200 {
		t.Fatalf("空模型行应计入渠道汇总: %+v", usage.Providers)
	}
}
