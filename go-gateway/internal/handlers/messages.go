package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/archive"
	"github.com/nightgopher/easygateway/internal/provider"
)

// MessageHandler 处理 /v1/messages 端点
type MessageHandler struct {
	router   *provider.Router
	logger   *log.Logger
	archiver *archive.Archiver
}

func NewMessageHandler(router *provider.Router, logger *log.Logger) *MessageHandler {
	return &MessageHandler{router: router, logger: logger}
}

// SetArchiver injects the conversation archiver. Uses the setter pattern so
// existing callers (and tests) that use NewMessageHandler don't break.
func (h *MessageHandler) SetArchiver(a *archive.Archiver) {
	h.archiver = a
}

// Handle 处理消息请求，支持流式和非流式
func (h *MessageHandler) Handle(c *gin.Context) {
	c.Request = c.Request.WithContext(h.router.BeginRouting(c.Request.Context()))
	// Read once so BindJSON can validate the same payload without retaining or
	// logging the full request body (which may contain credentials or Base64).
	rawBody, _ := io.ReadAll(c.Request.Body)
	logRequestSummary(h.logger, c, "messages", len(rawBody))
	// 把 body 放回去供 BindJSON 使用
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))

	var req provider.Request
	if err := c.BindJSON(&req); err != nil {
		h.logger.Printf("BindJSON error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Model is required"})
		return
	}
	if len(req.Messages) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Messages are required"})
		return
	}
	setRequestMetadata(c, req.Model, req.Stream)

	if req.Stream {
		h.handleStream(c, &req, rawBody)
	} else {
		h.handleNonStream(c, &req, rawBody)
	}
}

func (h *MessageHandler) handleNonStream(c *gin.Context, req *provider.Request, rawBody []byte) {
	resp, finalProvider, attempts, err := h.router.ForwardWithDetails(c.Request.Context(), req.Model, req)
	setForwardMetadata(c, finalProvider, attempts, err)
	if err != nil {
		h.logger.Printf("Forward failed: %s", provider.RedactedErrorText(err))
		setProviderErrorHeaders(c, err)
		// Archive the failed attempt: request body is known, response is the error.
		if h.archiver != nil && h.archiver.Enabled() {
			// Sanitize the error message for BOTH response_body and error_reason
			// to prevent credential leakage (e.g. "invalid api key: sk-secret").
			sanitized := archiveErrorReason(err)
			submitArchive(c, h.archiver, archive.ProtocolMessages, rawBody,
				[]byte(fmt.Sprintf(`{"error":%q}`, sanitized)),
				archive.StatusError, routingErrorStatus(err), archiveErrorReason(err))
		}
		c.JSON(routingErrorStatus(err), gin.H{"error": err.Error()})
		return
	}
	setUsageMetadata(c, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	setCacheUsageMetadata(c, resp.CacheReadInputTokens, resp.CacheCreationInputTokens, resp.CacheInputTokens, resp.CacheUsageKnown)

	// Archive the full request+response before sending to the client. We marshal
	// the response ourselves so the archived body matches what the client gets.
	respBody, _ := json.Marshal(resp)
	if h.archiver != nil && h.archiver.Enabled() {
		submitArchive(c, h.archiver, archive.ProtocolMessages, rawBody, respBody,
			archive.StatusCompleted, http.StatusOK, "")
	}

	c.JSON(http.StatusOK, resp)
}

func (h *MessageHandler) handleStream(c *gin.Context, req *provider.Request, rawBody []byte) {
	providerChain, err := h.router.RouteForStreamRequest(req.Model, req)
	if err != nil {
		setProviderErrorHeaders(c, err)
		submitRoutingErrorArchive(c, h.archiver, archive.ProtocolMessages, rawBody, err)
		c.JSON(routingErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	// Capture the SSE transcript for archival. We tee all bytes written to the
	// client into sink, then archive it once after the stream ends. Only
	// allocated when archiving is enabled to avoid overhead in the common case.
	var sink *archiveSink
	if h.archiver != nil && h.archiver.Enabled() {
		sink = newArchiveSink(h.archiver.MaxBodyBytes())
	}

	originalModel := req.Model
	var lastErr error
	streamOK := false
	for i, p := range providerChain {
		if h.router.ProviderAttempted(c.Request.Context(), originalModel, p) || !h.router.AllowProviderRequestFor(originalModel, p) {
			continue
		}
		started := time.Now()
		h.logger.Printf("Stream: trying provider %d/%d: %s", i+1, len(providerChain), p.Name())
		if bmp, ok := p.(provider.BoundModelProvider); ok {
			req.Model = bmp.BoundModel()
		} else {
			req.Model = h.router.MapModel(originalModel, p.Name())
		}
		if err := h.streamFromProvider(c, req, p, sink); err == nil {
			recordProviderAttempt(c, p.Name(), started, nil)
			h.router.RecordProviderSuccessFor(originalModel, p)
			lastErr = nil
			streamOK = true
			break
		} else {
			recordProviderAttempt(c, p.Name(), started, err)
			h.router.RecordProviderFailureFor(originalModel, p, err)
			lastErr = err
			h.logger.Printf("Stream provider %s failed: %s", p.Name(), provider.RedactedErrorText(err))
			if c.Writer.Written() {
				_ = writeAnthropicStreamError(c.Writer, err)
				streamOK = true // headers already sent → this is the terminal state
				break
			}
			if !provider.ShouldFallback(err) {
				setProviderErrorHeaders(c, err)
				c.JSON(routingErrorStatus(err), gin.H{"error": err.Error()})
				break
			}
		}
	}

	if sink != nil {
		// Archive the exact client request; providers may mutate the internal
		// request while mapping models or adding stream fields.
		reqBody := rawBody
		if !streamOK && lastErr == nil {
			lastErr = &provider.NoAvailableProvidersError{Model: originalModel, Reason: "disabled, unavailable, or circuit open"}
		}
		// Use sink content (not c.Writer.Written()) to distinguish pre-body
		// errors from actual stream interruptions. When a non-retryable error
		// triggers c.JSON before any SSE bytes, the sink is empty but the HTTP
		// writer reports written=true — that should be StatusError, not
		// StatusInterrupted.
		if !streamOK && sink.Len() == 0 && lastErr != nil {
			// No stream bytes were emitted → archive as a failed attempt.
			h.logger.Printf("All stream providers failed: %s", provider.RedactedErrorText(lastErr))
			if !c.Writer.Written() {
				setProviderErrorHeaders(c, lastErr)
				c.JSON(routingErrorStatus(lastErr), gin.H{"error": lastErr.Error()})
			}
			submitArchive(c, h.archiver, archive.ProtocolMessages, reqBody, nil,
				archive.StatusError, routingErrorStatus(lastErr), archiveErrorReason(lastErr))
			return
		}
		// Bytes were streamed → archive the captured transcript.
		// 成功的 fallback 已清掉旧错误；已输出后的错误仍须保留为归档中断原因。
		status, reason := parseStreamEndState(sink.Bytes(), lastErr)
		submitArchive(c, h.archiver, archive.ProtocolMessages, reqBody, sink.Bytes(),
			status, c.Writer.Status(), reason)
		return
	}

	// Non-archive path (original behavior)
	if lastErr == nil {
		lastErr = &provider.NoAvailableProvidersError{Model: req.Model, Reason: "disabled, unavailable, or circuit open"}
	}

	h.logger.Printf("All stream providers failed: %s", provider.RedactedErrorText(lastErr))
	if !c.Writer.Written() {
		setProviderErrorHeaders(c, lastErr)
		c.JSON(routingErrorStatus(lastErr), gin.H{"error": lastErr.Error()})
	}
}

// streamFromProvider 向指定提供商发流式请求并透传 SSE 到客户端。
// 若提供商实现了 StreamProvider 接口（如 OpenAI 格式），走其自有流式逻辑（含格式转换）；
// 否则直接透传上游 SSE 流（适用于 Anthropic 兼容接口）。
// 当 sink 非 nil 时，所有写入客户端的字节会被同步 tee 到 sink 以供归档。
func (h *MessageHandler) streamFromProvider(c *gin.Context, req *provider.Request, p provider.Provider, sink *archiveSink) error {
	var streamWriter io.Writer
	streamWriterBase := newDeferredStreamWriter(c)
	if sink != nil {
		streamWriter = newTeeStreamWriter(streamWriterBase, sink)
	} else {
		streamWriter = streamWriterBase
	}
	usageWriter := newUsageTrackingWriter(streamWriter, c)
	defer usageWriter.Finish()

	// 若提供商实现了 StreamProvider（如 OpenAI 格式），使用其内置流式逻辑
	if sp, ok := p.(provider.StreamProvider); ok {
		err := runGuardedStream(c, h.router, req, p, usageWriter, sp.ForwardStream)
		if err == nil {
			streamWriterBase.Commit()
		}
		return err
	}

	// 默认：直接透传 Anthropic 兼容的 SSE 流
	return runGuardedStream(c, h.router, req, p, usageWriter, func(attemptCtx context.Context, _ *provider.Request, guarded io.Writer) error {
		body, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, p.URL(), bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("User-Agent", "go-llm-gateway/1.0")
		if p.UseBearer() {
			httpReq.Header.Set("Authorization", "Bearer "+p.APIKey())
		} else {
			httpReq.Header.Set("x-api-key", p.APIKey())
		}

		client := &http.Client{} // 流式不设 Timeout，靠 context cancel 控制
		resp, err := client.Do(httpReq)
		if err != nil {
			return fmt.Errorf("send: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return provider.NewHTTPError(p.Name(), resp, b)
		}

		// 逐行透传 SSE 流
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if _, err := fmt.Fprintf(guarded, "%s\n", scanner.Bytes()); err != nil {
				return err
			}
			if f, ok := guarded.(interface{ Flush() }); ok {
				f.Flush()
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		return nil
	})
}
