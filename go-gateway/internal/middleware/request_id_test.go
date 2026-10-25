package middleware

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

func TestRequestIDPreservesSafeIncomingValueAndRecordsMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	collector := metrics.NewCollector()
	engine := gin.New()
	engine.Use(RequestID())
	engine.Use(Logging(log.New(io.Discard, "", 0), collector))
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set(requestmeta.ModelKey, "coding")
		c.Set(requestmeta.ProviderKey, "glm")
		c.Set(requestmeta.ProviderAttemptsKey, []requestmeta.ProviderAttempt{{Provider: "glm", Status: "success"}})
		c.Status(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("X-Request-ID", "client-trace-42")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	if got := response.Header().Get("X-Request-ID"); got != "client-trace-42" {
		t.Fatalf("X-Request-ID = %q, want preserved value", got)
	}
	records := collector.GetRecentLogs(1)
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	if records[0].RequestID != "client-trace-42" || records[0].Provider != "glm" {
		t.Fatalf("record metadata = %#v", records[0])
	}
	if len(records[0].ProviderAttempts) != 1 || records[0].ProviderAttempts[0].Status != "success" {
		t.Fatalf("provider attempts = %#v", records[0].ProviderAttempts)
	}
}

func TestRequestIDReplacesUnsafeIncomingValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RequestID())
	engine.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })

	request := httptest.NewRequest(http.MethodGet, "/health", nil)
	request.Header.Set("X-Request-ID", "bad value\r\n")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	got := response.Header().Get("X-Request-ID")
	if got == "" || got == "bad value\r\n" || !validRequestID(got) {
		t.Fatalf("generated request ID = %q", got)
	}
}

func TestLoggingExcludesHealthChecksFromMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	collector := metrics.NewCollector()
	engine := gin.New()
	engine.Use(RequestID())
	engine.Use(Logging(log.New(io.Discard, "", 0), collector))
	engine.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if records := collector.GetRecentLogs(1); len(records) != 0 {
		t.Fatalf("health records = %#v, want none", records)
	}
}
