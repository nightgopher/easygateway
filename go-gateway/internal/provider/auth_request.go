package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"syscall"
)

// postWithAuth 发送一次 POST 并注入鉴权头；上游返回 401/403 且配置了 AuthSource
// 时，先用它续期一次再重试，让 OAuth token 过期不影响本次请求。
//
// 复用已被对端提前关闭的 keep-alive 连接时，请求可能没写完就 broken pipe /
// connection reset。这类失败发生在请求完整发出之前、上游尚未处理，立即原样
// 重发一次是安全的，单 provider 链的瞬时网络抖动因此不再直接打死请求。
//
// 返回值语义：成功或非鉴权失败时返回响应，body 未读取，由调用方 Close；
// 未登录或续期失败时返回 *ProviderError，此时上游 body 已被读掉并关闭，
// 不会泄漏连接。
func postWithAuth(ctx context.Context, client *http.Client, name, url string, cfg *Config, setHeaders func(*http.Request), body []byte) (*http.Response, error) {
	if cfg.Auth != nil && !cfg.authAvailable() {
		return nil, &ProviderError{
			Provider:   name,
			StatusCode: http.StatusUnauthorized,
			Message:    "channel has no credential; login required",
		}
	}
	networkRetried, authRefreshed, rateRotated := false, false, false
	var usedToken string
	if cfg.Auth != nil {
		usedToken = cfg.Auth.BearerToken()
	}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		setHeaders(req)
		// WroteRequest 只在请求体完整写出后回调，用来区分「没发出去」和
		// 「上游可能已收到」的失败；后者不重发，避免重复生成、重复计费。
		var wroteRequest bool
		trace := &httptrace.ClientTrace{
			WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest = true },
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
		resp, err := client.Do(req)
		if err != nil {
			if !networkRetried && !wroteRequest && isTransientSendError(err) {
				networkRetried = true
				continue
			}
			return nil, fmt.Errorf("send request: %w", err)
		}
		if !authRefreshed && cfg.refreshAuth(ctx, resp.StatusCode) {
			authRefreshed = true
			if cfg.Auth != nil {
				usedToken = cfg.Auth.BearerToken()
			}
			drainAndClose(resp.Body)
			continue
		}
		// 429 先做渠道内账号轮转：同一订阅的多个账号额度独立，换号比重试同号
		// 或直接降级到链上其他 provider 都更符合「配额耗尽切账号」的语义。
		// 每个请求只轮转一次；换号后仍 429 则交给 provider 级熔断与链上降级。
		if !rateRotated && resp.StatusCode == http.StatusTooManyRequests &&
			cfg.rotateOnRateLimit(ctx, usedToken, resp.Header.Get("Retry-After")) {
			rateRotated = true
			if cfg.Auth != nil {
				usedToken = cfg.Auth.BearerToken()
			}
			drainAndClose(resp.Body)
			continue
		}
		return resp, nil
	}
}

// isTransientSendError 判断 send 阶段错误是否为对端提前断连类瞬时故障。
// 复用已被关闭的 keep-alive 连接表现为 broken pipe / connection reset / EOF。
func isTransientSendError(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// applyTransform 对请求体做渠道自定义改写。未配置 Transform 时原样返回。
func (c *Config) applyTransform(body []byte) ([]byte, error) {
	if c.Transform == nil {
		return body, nil
	}
	return c.Transform(body)
}

// Available 报告 provider 当前是否具备发起请求的条件。路由层借此跳过未登录的
// 渠道 provider，避免每次请求都先撞一次「凭据缺失」再降级。
func (p *AnthropicProvider) Available() bool { return p.config.authAvailable() }

// Available 见 AnthropicProvider.Available。
func (p *OpenAIProvider) Available() bool { return p.config.authAvailable() }

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<16))
	_ = body.Close()
}
