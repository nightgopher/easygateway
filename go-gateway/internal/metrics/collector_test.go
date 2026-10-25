package metrics

import (
	"net/http"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/internal/requestmeta"
)

type memoryStore struct {
	records []RequestRecord
}

func (s *memoryStore) SaveRecord(RequestRecord) error { return nil }
func (s *memoryStore) GetRecentLogs(int) ([]RequestRecord, error) {
	return append([]RequestRecord(nil), s.records...), nil
}

func TestCollectorRestoresRecentLogsFromStore(t *testing.T) {
	store := &memoryStore{records: []RequestRecord{
		{RequestID: "newer", Timestamp: time.Now(), Method: http.MethodPost, Path: "/v1/chat/completions", ProviderAttempts: []requestmeta.ProviderAttempt{{Provider: "glm", Status: "success"}}},
		{RequestID: "older", Timestamp: time.Now().Add(-time.Minute), Method: http.MethodPost, Path: "/v1/messages"},
		{RequestID: "favicon", Timestamp: time.Now(), Method: http.MethodGet, Path: "/favicon.ico"},
	}}
	collector := NewCollector()
	collector.SetStore(store)

	records := collector.GetRecentLogs(2)
	if len(records) != 2 || records[0].RequestID != "newer" || records[1].RequestID != "older" {
		t.Fatalf("restored records = %#v", records)
	}
	if len(records[0].ProviderAttempts) != 1 {
		t.Fatalf("restored attempts = %#v", records[0].ProviderAttempts)
	}
}

func TestCollectorOnlyRecordsModelAPIRequests(t *testing.T) {
	collector := NewCollector()
	now := time.Now()
	for _, record := range []RequestRecord{
		{Timestamp: now, Method: http.MethodGet, Path: "/favicon.ico", StatusCode: http.StatusNoContent},
		{Timestamp: now, Method: http.MethodGet, Path: "/v1/models", StatusCode: http.StatusOK},
		{Timestamp: now, Method: http.MethodGet, Path: "/admin/dashboard", StatusCode: http.StatusOK},
		{Timestamp: now, Method: http.MethodPost, Path: "/v1/chat/completions", Model: "coding", StatusCode: http.StatusOK},
	} {
		collector.Record(record)
	}

	summary := collector.GetDashboard()
	if summary.TodayRequests != 1 || summary.SuccessRate != 100 || summary.ActiveModels != 1 {
		t.Fatalf("summary = %#v, want one successful model API request", summary)
	}
	logs := collector.GetRecentLogs(10)
	if len(logs) != 1 || logs[0].Path != "/v1/chat/completions" {
		t.Fatalf("business logs = %#v, want only model API request", logs)
	}
}

func TestIsBusinessRequestRecognizesAllModelProtocols(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   bool
	}{
		{method: http.MethodPost, path: "/v1/messages", want: true},
		{method: http.MethodPost, path: "/messages", want: true},
		{method: http.MethodPost, path: "/v1/chat/completions", want: true},
		{method: http.MethodPost, path: "/chat/completions", want: true},
		{method: http.MethodPost, path: "/v1/responses", want: true},
		{method: http.MethodPost, path: "/responses", want: true},
		{method: http.MethodGet, path: "/v1/chat/completions", want: false},
		{method: http.MethodGet, path: "/", want: false},
		{method: http.MethodGet, path: "/favicon.ico", want: false},
	}
	for _, test := range tests {
		if got := IsBusinessRequest(test.method, test.path); got != test.want {
			t.Errorf("IsBusinessRequest(%q, %q) = %v, want %v", test.method, test.path, got, test.want)
		}
	}
}

// 回归：重启后看板 KPI 与模型/供应商聚合必须从持久化日志恢复「今日」部分，
// 跨天记录不能计入今日；恢复后新请求在恢复值上继续累计。
func TestCollectorRestoresTodayAggregates(t *testing.T) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	store := &memoryStore{records: []RequestRecord{
		// 昨天：不应计入今日
		{Timestamp: todayStart.Add(-time.Hour), Method: http.MethodPost, Path: "/v1/chat/completions", Model: "yesterday-model", Provider: "p0", StatusCode: 200, Latency: 100},
		// 今天：2 成功 1 失败
		{Timestamp: todayStart.Add(time.Hour), Method: http.MethodPost, Path: "/v1/chat/completions", Model: "today-model", Provider: "p1", StatusCode: 200, Latency: 200},
		{Timestamp: todayStart.Add(2 * time.Hour), Method: http.MethodPost, Path: "/v1/chat/completions", Model: "today-model", Provider: "p1", StatusCode: 200, Latency: 400},
		{Timestamp: todayStart.Add(3 * time.Hour), Method: http.MethodPost, Path: "/v1/chat/completions", Model: "today-model", Provider: "p1", StatusCode: 500, Latency: 600},
		// 非业务请求：不计入
		{Timestamp: todayStart.Add(time.Hour), Method: http.MethodGet, Path: "/v1/models", Model: "today-model", Provider: "p1", StatusCode: 200, Latency: 5},
	}}

	c := NewCollector()
	c.SetStore(store)

	summary := c.GetDashboard()
	if summary.TodayRequests != 3 {
		t.Fatalf("today_requests = %d, want 3", summary.TodayRequests)
	}
	wantRate := float64(2) / 3 * 100
	if diff := summary.SuccessRate - wantRate; diff > 0.01 || diff < -0.01 {
		t.Fatalf("success_rate = %v, want %v", summary.SuccessRate, wantRate)
	}
	if summary.ActiveModels != 1 {
		t.Fatalf("active_models = %d, want 1", summary.ActiveModels)
	}

	stats := c.GetModelStats()
	if len(stats) != 1 || stats[0].Model != "today-model" {
		t.Fatalf("model stats = %+v, want only today-model", stats)
	}
	if stats[0].Requests != 3 || stats[0].Successes != 2 || stats[0].Errors != 1 {
		t.Fatalf("model stats = %+v, want 3/2/1", stats[0])
	}
	wantAvg := (200 + 400 + 600) / 3
	if diff := stats[0].AvgLatency - float64(wantAvg); diff > 0.01 || diff < -0.01 {
		t.Fatalf("avg latency = %v, want %v", stats[0].AvgLatency, wantAvg)
	}

	// 恢复后新请求继续累计
	c.mu.Lock()
	c.applyToAggregates(RequestRecord{Timestamp: now, Method: http.MethodPost, Path: "/v1/chat/completions", Model: "today-model", Provider: "p1", StatusCode: 200, Latency: 800})
	c.mu.Unlock()
	if summary := c.GetDashboard(); summary.TodayRequests != 4 {
		t.Fatalf("today_requests after new record = %d, want 4", summary.TodayRequests)
	}
}

func TestDashboardSummaryAggregatesTodayTokens(t *testing.T) {
	collector := NewCollector()
	now := time.Now()
	for _, record := range []RequestRecord{
		{Timestamp: now, Method: http.MethodPost, Path: "/v1/chat/completions", Model: "coding", StatusCode: http.StatusOK, InputTokens: 100, OutputTokens: 50},
		{Timestamp: now, Method: http.MethodPost, Path: "/v1/chat/completions", Model: "coding", StatusCode: http.StatusOK, InputTokens: 30, OutputTokens: 20},
	} {
		collector.Record(record)
	}
	if summary := collector.GetDashboard(); summary.TodayTokens != 200 {
		t.Fatalf("today tokens = %d, want 200", summary.TodayTokens)
	}
}

type dailyStore struct {
	memoryStore
	stats []ModelStats
}

func (s *dailyStore) GetDailyStats(string) ([]ModelStats, error) { return s.stats, nil }

func TestCollectorRestoresDailyTokensBeyondLogWindow(t *testing.T) {
	store := &dailyStore{stats: []ModelStats{
		{Model: "first", Requests: 2100, Successes: 2100, TotalTokens: 210000},
		{Model: "second", Requests: 1, Successes: 1, TotalTokens: 120},
	}}
	c := NewCollector()
	c.SetStore(store)
	if got := c.GetDashboard().TodayTokens; got != 210120 {
		t.Fatalf("restored today_tokens = %d, want 210120", got)
	}
	c.Record(RequestRecord{Timestamp: time.Now(), Method: http.MethodPost, Path: "/v1/messages", Model: "second", StatusCode: 200,
		InputTokens: 10, OutputTokens: 20, CacheUsageKnown: true, CacheInputTokens: 110})
	if got := c.GetDashboard().TodayTokens; got != 210250 {
		t.Fatalf("today_tokens after cached request = %d, want 210250", got)
	}
}
