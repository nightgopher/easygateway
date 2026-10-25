package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/gwkeys"
	"github.com/nightgopher/easygateway/internal/provider"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

const maxRequestErrorBytes = 512

// setRequestMetadata 记录请求元数据，并对 gateway key 请求做模型白名单
// 校验。返回 false 表示请求已被拒绝（403 已写回、handler 须立即 return）；
// 未携带 gateway key 的请求恒为 true。
func setRequestMetadata(c *gin.Context, model string, stream bool) bool {
	if value, ok := c.Get(gwkeys.GuardContextKey); ok {
		if guard, ok := value.(*gwkeys.Guard); ok && !guard.ModelAllowed(model) {
			c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("model %q is not allowed for this gateway key", model)})
			c.Abort()
			return false
		}
	}
	c.Set(requestmeta.ModelKey, model)
	c.Set(requestmeta.RequestIsStreamKey, stream)
	return true
}

func setUsageMetadata(c *gin.Context, inputTokens, outputTokens int) {
	c.Set(requestmeta.InputTokensKey, inputTokens)
	c.Set(requestmeta.OutputTokensKey, outputTokens)
}

func setCacheUsageMetadata(c *gin.Context, cacheReadInputTokens, cacheCreationInputTokens, cacheInputTokens int, known bool) {
	c.Set(requestmeta.CacheReadInputTokensKey, cacheReadInputTokens)
	c.Set(requestmeta.CacheCreationInputTokensKey, cacheCreationInputTokens)
	c.Set(requestmeta.CacheInputTokensKey, cacheInputTokens)
	c.Set(requestmeta.CacheUsageKnownKey, known)
}

func totalPromptTokens(resp *provider.Response) int {
	if resp.CacheUsageKnown && resp.CacheInputTokens > resp.Usage.InputTokens {
		return resp.CacheInputTokens
	}
	return resp.Usage.InputTokens
}

func setForwardMetadata(c *gin.Context, finalProvider string, attempts []requestmeta.ProviderAttempt, err error) {
	if finalProvider != "" {
		c.Set(requestmeta.ProviderKey, finalProvider)
	}
	if len(attempts) > 0 {
		c.Set(requestmeta.ProviderAttemptsKey, attempts)
	}
	if err != nil {
		c.Set(requestmeta.RequestErrorKey, summarizeRequestError(err))
	} else {
		c.Set(requestmeta.RequestErrorKey, "")
	}
}

func recordProviderAttempt(c *gin.Context, providerName string, started time.Time, err error) {
	attempt := requestmeta.ProviderAttempt{
		Provider:  providerName,
		Status:    "success",
		LatencyMs: float64(time.Since(started).Microseconds()) / 1000,
	}
	if timing, ok := c.Get("attempt_stream_timing"); ok {
		if t, ok := timing.(provider.StreamTiming); ok {
			attempt.FirstOutputMs = t.FirstOutputMs
		}
		c.Set("attempt_stream_timing", nil)
	}
	if err != nil {
		attempt.Status = "error"
		attempt.Error = summarizeRequestError(err)
		var providerErr *provider.ProviderError
		if errors.As(err, &providerErr) {
			attempt.StatusCode = providerErr.StatusCode
		}
		c.Set(requestmeta.RequestErrorKey, attempt.Error)
	} else {
		c.Set(requestmeta.RequestErrorKey, "")
	}

	attempts := providerAttempts(c)
	attempts = append(attempts, attempt)
	c.Set(requestmeta.ProviderAttemptsKey, attempts)
	c.Set(requestmeta.ProviderKey, providerName)
}

func providerAttempts(c *gin.Context) []requestmeta.ProviderAttempt {
	value, ok := c.Get(requestmeta.ProviderAttemptsKey)
	if !ok {
		return nil
	}
	attempts, _ := value.([]requestmeta.ProviderAttempt)
	return append([]requestmeta.ProviderAttempt(nil), attempts...)
}

func summarizeRequestError(err error) string {
	if err == nil {
		return ""
	}
	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) {
		providerName := providerErr.Provider
		if providerName == "" {
			providerName = "upstream"
		}
		message := providerErr.Message
		if message == "" {
			message = "request failed"
		}
		if providerErr.StatusCode > 0 {
			return truncateRequestError(fmt.Sprintf("%s HTTP %d: %s", providerName, providerErr.StatusCode, message))
		}
		return truncateRequestError(fmt.Sprintf("%s: %s", providerName, message))
	}
	return truncateRequestError(err.Error())
}

func truncateRequestError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxRequestErrorBytes {
		return value
	}
	return value[:maxRequestErrorBytes] + "..."
}

func logRequestSummary(logger interface{ Printf(string, ...interface{}) }, c *gin.Context, endpoint string, size int) {
	logger.Printf("[%s] request_id=%s body_bytes=%d", endpoint, c.GetString(requestmeta.RequestIDKey), size)
}
