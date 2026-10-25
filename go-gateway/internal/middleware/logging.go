package middleware

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

// Logging 创建请求日志中间件，同时记录指标
func Logging(logger *log.Logger, collector *metrics.Collector) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)
		status := c.Writer.Status()
		requestID := c.GetString(requestmeta.RequestIDKey)

		logger.Printf("request_id=%s %s %s -> %d (%v)",
			requestID,
			c.Request.Method,
			c.Request.URL.Path,
			status,
			latency,
		)

		// The collector owns the business-request boundary. Keeping this
		// middleware path-agnostic prevents the exclusion list from drifting
		// away from the gateway's actual model API routes.
		if collector != nil {
			path := c.Request.URL.Path
			model := c.GetString(requestmeta.ModelKey)
			provider := c.GetString(requestmeta.ProviderKey)
			requestError := c.GetString(requestmeta.RequestErrorKey)
			if requestError == "" && status >= http.StatusBadRequest {
				requestError = http.StatusText(status)
			}
			var attempts []requestmeta.ProviderAttempt
			if value, ok := c.Get(requestmeta.ProviderAttemptsKey); ok {
				attempts, _ = value.([]requestmeta.ProviderAttempt)
			}
			collector.Record(metrics.RequestRecord{
				Timestamp:                start,
				RequestID:                requestID,
				Method:                   c.Request.Method,
				Path:                     path,
				Model:                    model,
				Provider:                 provider,
				ProviderAttempts:         attempts,
				StatusCode:               status,
				Latency:                  float64(latency.Milliseconds()),
				IsStream:                 c.GetBool(requestmeta.RequestIsStreamKey),
				InputTokens:              c.GetInt(requestmeta.InputTokensKey),
				OutputTokens:             c.GetInt(requestmeta.OutputTokensKey),
				CacheReadInputTokens:     c.GetInt(requestmeta.CacheReadInputTokensKey),
				CacheCreationInputTokens: c.GetInt(requestmeta.CacheCreationInputTokensKey),
				CacheInputTokens:         c.GetInt(requestmeta.CacheInputTokensKey),
				CacheUsageKnown:          c.GetBool(requestmeta.CacheUsageKnownKey),
				Error:                    requestError,
			})
		}
	}
}
