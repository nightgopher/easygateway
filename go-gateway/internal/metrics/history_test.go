package metrics

import "testing"

func TestAggregateHistoryFillsMissingDaysAndAggregates(t *testing.T) {
	rows := []DailyStatRow{
		{Date: "2026-10-20", Model: "glm-4.7", Provider: "glm", Requests: 10, Successes: 9, Errors: 1, TotalTokens: 1000, TotalLatencyMS: 1000, CacheInputTokens: 800, CacheReadInputTokens: 400, CacheUsageRequests: 5},
		{Date: "2026-10-20", Model: "glm-4.7", Provider: "backup", Requests: 4, Successes: 4, Errors: 0, TotalTokens: 400, TotalLatencyMS: 400},
		{Date: "2026-10-21", Model: "kimi-k2", Provider: "kimi", Requests: 2, Successes: 2, TotalTokens: 200, TotalLatencyMS: 300},
	}
	h := AggregateHistory(rows, "2026-10-20", "2026-10-22")

	if len(h.Days) != 3 {
		t.Fatalf("expected 3 days (missing day zero-filled), got %d", len(h.Days))
	}
	if h.Days[0].Date != "2026-10-20" || h.Days[2].Date != "2026-10-22" {
		t.Fatalf("days not sorted or wrong range: %+v", h.Days)
	}
	if h.Days[0].Requests != 14 || h.Days[0].Errors != 1 || h.Days[0].TotalTokens != 1400 {
		t.Errorf("day totals wrong: %+v", h.Days[0])
	}
	if h.Days[0].AvgLatencyMS != 100 { // (1000+400)/14 = 100
		t.Errorf("avg latency = %v, want 100", h.Days[0].AvgLatencyMS)
	}
	if h.Days[0].CacheHitRate == nil || *h.Days[0].CacheHitRate != 50 { // 400/800
		t.Errorf("cache hit rate = %v, want 50", h.Days[0].CacheHitRate)
	}
	if h.Days[2].Requests != 0 || h.Days[2].CacheHitRate != nil {
		t.Errorf("empty day should be zero without cache rate: %+v", h.Days[2])
	}

	if len(h.Models) != 2 || h.Models[0].Model != "glm-4.7" {
		t.Fatalf("model ranking wrong: %+v", h.Models)
	}
	if h.Models[0].Provider != "glm" { // 10 > 4
		t.Errorf("dominant provider = %q, want glm", h.Models[0].Provider)
	}
	if h.Models[0].Requests != 14 || h.Models[0].TotalTokens != 1400 {
		t.Errorf("model totals wrong: %+v", h.Models[0])
	}

	if len(h.Providers) != 3 || h.Providers[0].Provider != "glm" {
		t.Fatalf("provider ranking wrong: %+v", h.Providers)
	}
	if h.Providers[1].Provider != "backup" || h.Providers[1].Requests != 4 {
		t.Errorf("provider aggregate wrong: %+v", h.Providers[1])
	}
}

func TestAggregateHistoryBadRangeIsEmpty(t *testing.T) {
	h := AggregateHistory(nil, "not-a-date", "2026-10-22")
	if len(h.Days) != 0 {
		t.Errorf("unparseable range should produce no days, got %d", len(h.Days))
	}
	h = AggregateHistory(nil, "2026-10-22", "2026-10-20")
	if len(h.Days) != 0 {
		t.Errorf("inverted range should produce no days, got %d", len(h.Days))
	}
}
