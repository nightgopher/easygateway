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

type stubDailyStats struct {
	rows []metrics.DailyStatRow
	err  error
}

func (s stubDailyStats) GetDailyStatRows(from, to string) ([]metrics.DailyStatRow, error) {
	return s.rows, s.err
}

func newStatsHistoryRouter(t *testing.T, source DailyStatsSource) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := provider.NewRouter(log.New(os.Stderr, "", 0))
	handler := NewAdminHandler(router, metrics.NewCollector(), log.New(os.Stderr, "", 0), source)
	e := gin.New()
	e.GET("/admin/stats/history", handler.HandleStatsHistory)
	return e
}

func TestStatsHistoryDaysParam(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	e := newStatsHistoryRouter(t, stubDailyStats{rows: []metrics.DailyStatRow{{Date: today, Model: "m", Provider: "p", Requests: 3, Successes: 3}}})
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/stats/history?days=7", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var h metrics.History
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(h.Days) != 7 || h.Days[6].Requests != 3 {
		t.Fatalf("days=7 should return 7-day series ending today, got %+v", h.Days)
	}
	if h.From != h.Days[0].Date || h.To != today {
		t.Errorf("range echo wrong: from=%s to=%s", h.From, h.To)
	}
}
