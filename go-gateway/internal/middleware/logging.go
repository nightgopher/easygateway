package middleware

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/gwkeys"
	"github.com/nightgopher/easygateway/internal/metrics"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

// Logging 创建请求日志中间件，同时记录指标。gatewayKeys 非 nil 时，业务
// 请求的 token 用量会同步计入所用 gateway key 的窗口计数（见 gwkeys 包
// 注释：先放行后计数，窗口内最后一个请求可能略微超出预算）。
func Logging(logger *log.Logger, collector *metrics.Collector, gatewayKeys *gwkeys.Store) gin.HandlerFunc {
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
			if gatewayKeys != nil && metrics.IsBusinessRequest(c.Request.Method, path) {
				if keyID := c.GetString(gwkeys.ContextKey); keyID != "" {
					input := c.GetInt(requestmeta.InputTokensKey)
					if cacheInput := c.GetInt(requestmeta.CacheInputTokensKey); c.GetBool(requestmeta.CacheUsageKnownKey) && cacheInput > input {
						input = cacheInput
					}
					gatewayKeys.RecordUsage(keyID, int64(input+c.GetInt(requestmeta.OutputTokensKey)), time.Now())
				}
			}
		}
	}
}
