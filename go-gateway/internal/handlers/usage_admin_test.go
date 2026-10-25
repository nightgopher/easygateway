package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/provider"
)

func newUsageRouter(t *testing.T, source DailyStatsSource) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := provider.NewRouter(log.New(os.Stderr, "", 0))
	router.RegisterModel(provider.ModelInfo{
		ID: "glm-5.3", Provider: "zhipu", Capabilities: []string{"text"},
		Pricing: &provider.ModelPricing{InputPerM: 3, OutputPerM: 12},
	})
	handler := NewAdminHandler(router, metrics.NewCollector(), log.New(os.Stderr, "", 0), source)
	e := gin.New()
	e.GET("/admin/usage", handler.HandleUsage)
	return e
}

func TestUsageComputesCostFromPricing(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	source := stubDailyStats{rows: []metrics.DailyStatRow{
		{Date: today, Model: "coding", Provider: "glm-5.3", Requests: 4, InputTokens: 1_000_000, OutputTokens: 500_000},
	}}
	w := httptest.NewRecorder()
	newUsageRouter(t, source).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/usage?days=7", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var usage metrics.Usage
	if err := json.Unmarshal(w.Body.Bytes(), &usage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if usage.From == "" || usage.To != today {
		t.Fatalf("range echo wrong: %+v", usage)
	}
	if len(usage.Rows) != 1 || !usage.Rows[0].Priced {
		t.Fatalf("rows = %+v", usage.Rows)
	}
	want := 1.0*3 + 0.5*12
	if diff := usage.TotalCostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("total = %v, want %v (链别名借渠道实例名单价)", usage.TotalCostUSD, want)
	}
}

func TestUsageListsUnpricedModels(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	source := stubDailyStats{rows: []metrics.DailyStatRow{
		{Date: today, Model: "no-price-model", Provider: "p1", Requests: 1, InputTokens: 10, OutputTokens: 10},
	}}
	w := httptest.NewRecorder()
	newUsageRouter(t, source).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/usage", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var usage metrics.Usage
	if err := json.Unmarshal(w.Body.Bytes(), &usage); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(usage.UnpricedModels) != 1 || usage.UnpricedModels[0] != "no-price-model" {
		t.Fatalf("unpriced = %v", usage.UnpricedModels)
	}
	if usage.TotalCostUSD != 0 {
		t.Fatalf("无单价时总成本应为 0（未知，非免费）: %v", usage.TotalCostUSD)
	}
}

func TestUsageWithoutPersistence(t *testing.T) {
	w := httptest.NewRecorder()
	newUsageRouter(t, nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/usage", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without persistence, got %d", w.Code)
	}
}

func TestUsageRejectsBadDays(t *testing.T) {
	w := httptest.NewRecorder()
	newUsageRouter(t, stubDailyStats{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/usage?days=zero", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
