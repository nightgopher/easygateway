package handlers

import (
	"context"
	"errors"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

func runGuardedStream(c *gin.Context, router *provider.Router, req *provider.Request, p provider.Provider, w io.Writer, forward func(context.Context, *provider.Request, io.Writer) error) error {
	timing, err := runGuardedStreamTimed(c.Request.Context(), router, req, p, w, forward)
	// 同步调用方运行在请求 goroutine 上，与后续 recordProviderAttempt 读取有序。
	c.Set("attempt_stream_timing", timing)
	return err
}

// runGuardedStreamTimed 执行一次带守卫的流式尝试并返回流计时。
func runGuardedStreamTimed(ctx context.Context, router *provider.Router, req *provider.Request, p provider.Provider, w io.Writer, forward func(context.Context, *provider.Request, io.Writer) error) (provider.StreamTiming, error) {
	return router.RunStreamAttempt(ctx, req, p, w, func(ctx context.Context, dst io.Writer) error { return forward(ctx, req, dst) })
}

// runGuardedStreamContext 供后台流式 goroutine 使用。gin.Context 非线程安全，
// 后台 goroutine 直接 c.Set 会与请求 goroutine 的 usage 写入构成并发 map 写；
// 这里改为返回流计时，由请求 goroutine 在排空 errCh 后自行写入。
func runGuardedStreamContext(ctx context.Context, router *provider.Router, req *provider.Request, p provider.Provider, w io.Writer, forward func(context.Context, *provider.Request, io.Writer) error) (provider.StreamTiming, error) {
	return runGuardedStreamTimed(ctx, router, req, p, w, forward)
}

func nativeFallbackError(c *gin.Context, err error) error {
	var unavailable *provider.NoAvailableProvidersError
	if err != nil && !errors.As(err, &unavailable) {
		return err
	}
	if value, ok := c.Get("native_passthrough_error"); ok {
		if previous, ok := value.(error); ok {
			return previous
		}
	}
	return err
}
