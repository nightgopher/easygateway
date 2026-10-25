package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/gwkeys"
)

const (
	authFailureWindow = time.Minute
	authFailureMax    = 20
)

// extractToken 从请求中提取 token
func extractToken(c *gin.Context) string {
	token := ""
	authHeader := c.GetHeader("Authorization")
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && parts[0] == "Bearer" {
			token = parts[1]
		}
	}
	if token == "" {
		token = c.GetHeader("x-api-key")
	}
	return token
}

// secureTokenEqual 常量时间比较两个 token。先哈希再比较，
// 避免明文按字节短路比较的计时侧信道与长度泄露。
func secureTokenEqual(a, b string) bool {
	sumA := sha256.Sum256([]byte(a))
	sumB := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(sumA[:], sumB[:]) == 1
}

// failureLimiter 按来源 IP 统计认证失败，超限后临时拒绝，
// 阻止对 master/admin token 的在线暴力猜解。
type failureLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	maxFail int
	fails   map[string][]time.Time
}

func newFailureLimiter(window time.Duration, maxFail int) *failureLimiter {
	return &failureLimiter{window: window, maxFail: maxFail, fails: make(map[string][]time.Time)}
}

// allow 报告该 IP 在当前窗口内的失败次数是否仍低于上限。
func (f *failureLimiter) allow(ip string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recentLocked(ip, now)) < f.maxFail
}

// recordFail 记录一次认证失败。
func (f *failureLimiter) recordFail(ip string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[ip] = append(f.recentLocked(ip, now), now)
}

// recentLocked 清理并返回窗口内的失败记录；调用方需持锁。
func (f *failureLimiter) recentLocked(ip string, now time.Time) []time.Time {
	cutoff := now.Add(-f.window)
	recent := f.fails[ip][:0]
	for _, ts := range f.fails[ip] {
		if ts.After(cutoff) {
			recent = append(recent, ts)
		}
	}
	if len(recent) == 0 {
		delete(f.fails, ip)
	} else {
		f.fails[ip] = recent
	}
	return recent
}

// BearerAuth keeps the data-plane protected by the master key alone.
func BearerAuth(masterKey string, logger *log.Logger) gin.HandlerFunc {
	return BearerAuthWithAdminToken(masterKey, "", logger)
}

// BearerAuthWithAdminToken keeps the data-plane protected by the master key
// while allowing the separately configured admin token to reach /admin. The
// admin group still runs AdminAuth afterwards, so this only changes the
// outer gate and does not weaken non-admin endpoints.
//
// gatewayKeys 可为 nil：不启用多密钥。启用时，master key 校验失败的 token
// 会继续尝试 gateway key：命中且启用即通过外层门（/admin 仍由 AdminAuth
// 把守，gateway key 进不去），预算耗尽返回 429 与窗口重置时间，密钥身份
// 写入 gin context（gwkeys.ContextKey）供模型白名单与用量记账使用。
func BearerAuthWithAdminToken(masterKey string, adminToken string, logger *log.Logger) gin.HandlerFunc {
	return BearerAuthWithGatewayKeys(masterKey, adminToken, nil, logger)
}

// BearerAuthWithGatewayKeys 在 BearerAuthWithAdminToken 基础上接入 gateway
// key 多密钥体系（见 BearerAuthWithAdminToken 注释）。
func BearerAuthWithGatewayKeys(masterKey, adminToken string, gatewayKeys *gwkeys.Store, logger *log.Logger) gin.HandlerFunc {
	failures := newFailureLimiter(authFailureWindow, authFailureMax)
	tooMany := func(c *gin.Context) {
		logger.Printf("Too many failed auth attempts from %s", c.ClientIP())
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed auth attempts, retry later"})
		c.Abort()
	}
	return func(c *gin.Context) {
		// Liveness/readiness probes and CORS preflight do not need credentials.
		if c.Request.URL.Path == "/health" || c.Request.URL.Path == "/readyz" || c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}

		ip := c.ClientIP()
		now := time.Now()
		if !failures.allow(ip, now) {
			tooMany(c)
			return
		}

		token := extractToken(c)
		if adminToken != "" && isAdminPath(c.Request.URL.Path) && secureTokenEqual(token, adminToken) {
			c.Next()
			return
		}

		if token == "" {
			logger.Printf("Missing Authorization header from %s", ip)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing Authorization header"})
			c.Abort()
			return
		}

		if !secureTokenEqual(token, masterKey) {
			if gatewayKeys != nil {
				if key, ok, reason := gatewayKeys.Lookup(token); ok {
					// 有效 gateway key：预算预检（最后一个请求可能略微超出，
					// 先放行后计数，见 gwkeys 包注释）。
					if state := gatewayKeys.Budget(key, time.Now()); !state.OK {
						logger.Printf("Gateway key %s (%s) budget exhausted: %s window %d tokens", key.ID, key.Name, state.Window, state.Limit)
						c.Header("Retry-After", strconv.Itoa(max(int(state.RetryAfter.Seconds())+1, 1)))
						c.JSON(http.StatusTooManyRequests, gin.H{
							"error":       "gateway key budget exhausted",
							"window":      state.Window,
							"reset_after": state.RetryAfter.Round(time.Second).String(),
						})
						c.Abort()
						return
					}
					c.Set(gwkeys.ContextKey, key.ID)
					c.Set(gwkeys.GuardContextKey, &gwkeys.Guard{AllowedModels: key.AllowedModels})
					c.Next()
					return
				} else if reason == "disabled" {
					logger.Printf("Disabled gateway key used from %s", ip)
					c.JSON(http.StatusUnauthorized, gin.H{"error": "Gateway key is disabled"})
					c.Abort()
					return
				}
			}
			failures.recordFail(ip, now)
			logger.Printf("Invalid token from %s", ip)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			c.Abort()
			return
		}

		c.Next()
	}
}

func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}

// AdminAuth 创建管理端点认证中间件
// 如果设置了 adminToken，则管理端点需要 adminToken
// 如果未设置 adminToken，则管理端点使用 masterKey
func AdminAuth(masterKey string, adminToken string, logger *log.Logger) gin.HandlerFunc {
	failures := newFailureLimiter(authFailureWindow, authFailureMax)
	return func(c *gin.Context) {
		ip := c.ClientIP()
		now := time.Now()
		if !failures.allow(ip, now) {
			logger.Printf("Too many failed admin auth attempts from %s", ip)
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many failed auth attempts, retry later"})
			c.Abort()
			return
		}

		token := extractToken(c)

		if token == "" {
			logger.Printf("Missing Authorization header for admin endpoint from %s", ip)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing Authorization header"})
			c.Abort()
			return
		}

		// 如果设置了 adminToken，优先验证 adminToken
		if adminToken != "" {
			if secureTokenEqual(token, adminToken) {
				c.Next()
				return
			}
			// adminToken 不匹配，也允许 masterKey
			if secureTokenEqual(token, masterKey) {
				c.Next()
				return
			}
		} else {
			// 未设置 adminToken，使用 masterKey
			if secureTokenEqual(token, masterKey) {
				c.Next()
				return
			}
		}

		failures.recordFail(ip, now)
		logger.Printf("Invalid admin token from %s", ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid admin token"})
		c.Abort()
	}
}
