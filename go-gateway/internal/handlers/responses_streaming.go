package handlers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/weijian/go-llm-gateway/internal/archive"
	"github.com/weijian/go-llm-gateway/internal/provider"
)

// handleStream 处理流式 Responses API 请求
func (h *responsesHandler) handleStream(c *gin.Context, req *responsesRequest, rawBody []byte) {
	providerReq, err := responsesToProviderRequest(req)
	if err != nil {
		setProviderErrorHeaders(c, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
		}})
		return
	}

	providerChain, err := h.router.RouteForStreamRequest(providerReq.Model, providerReq)
	if err != nil {
		err = nativeFallbackError(c, err)
	}
	if err != nil {
		setProviderErrorHeaders(c, err)
		submitRoutingErrorArchive(c, h.archiver, archive.ProtocolResponses, rawBody, err)
		c.JSON(routingErrorStatus(err), gin.H{"error": gin.H{
			"message": err.Error(),
			"type":    "invalid_request_error",
		}})
		return
	}

	var sink *archiveSink
	if h.archiver != nil && h.archiver.Enabled() {
		sink = newArchiveSink(h.archiver.MaxBodyBytes())
	}

	originalModel := req.Model
	lastErr := nativeFallbackError(c, nil)
	streamOK := false
	for i, p := range providerChain {
		if h.router.ProviderAttempted(c.Request.Context(), originalModel, p) || !h.router.AllowProviderRequestFor(originalModel, p) {
			continue
		}
		started := time.Now()
		h.logger.Printf("Responses stream: trying provider %d/%d: %s", i+1, len(providerChain), p.Name())

		if bmp, ok := p.(provider.BoundModelProvider); ok {
			providerReq.Model = bmp.BoundModel()
		} else {
			providerReq.Model = h.router.MapModel(originalModel, p.Name())
		}

		if err := h.streamFromProvider(c, providerReq, p, originalModel, sink); err == nil {
			recordProviderAttempt(c, p.Name(), started, nil)
			h.router.RecordProviderSuccessFor(originalModel, p)
			lastErr = nil
			streamOK = true
			break
		} else {
			recordProviderAttempt(c, p.Name(), started, err)
			h.router.RecordProviderFailureFor(originalModel, p, err)
			lastErr = err
			h.logger.Printf("Responses stream provider %s failed: %s", p.Name(), provider.RedactedErrorText(err))
			if c.Writer.Written() {
				_ = writeResponsesStreamError(c.Writer, err)
				streamOK = true
				break
			}
			if !provider.ShouldFallback(err) {
				setProviderErrorHeaders(c, err)
				c.JSON(routingErrorStatus(err), gin.H{"error": gin.H{
					"message": err.Error(),
					"type":    "server_error",
				}})
				break
			}
		}
	}

	if sink != nil {
		if !streamOK && lastErr == nil {
			lastErr = &provider.NoAvailableProvidersError{Model: originalModel, Reason: "disabled, unavailable, or circuit open"}
		}
		if !streamOK && sink.Len() == 0 && lastErr != nil {
			setProviderErrorHeaders(c, lastErr)
			submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody, nil,
				archive.StatusError, routingErrorStatus(lastErr), archiveErrorReason(lastErr))
			c.JSON(routingErrorStatus(lastErr), gin.H{"error": gin.H{
				"message": fmt.Sprintf("all providers failed: %v", lastErr),
				"type":    "server_error",
			}})
			return
		}
		status, reason := parseStreamEndState(sink.Bytes(), lastErr)
		submitArchive(c, h.archiver, archive.ProtocolResponses, rawBody, sink.Bytes(),
			status, c.Writer.Status(), reason)
		return
	}

	if lastErr == nil {
		lastErr = &provider.NoAvailableProvidersError{Model: originalModel, Reason: "disabled, unavailable, or circuit open"}
	}

	if !c.Writer.Written() {
		setProviderErrorHeaders(c, lastErr)
		c.JSON(routingErrorStatus(lastErr), gin.H{"error": gin.H{
			"message": fmt.Sprintf("all providers failed: %v", lastErr),
			"type":    "server_error",
		}})
	}
}

// streamFromProvider 从指定 provider 获取流式响应并转换为 Responses SSE
func (h *responsesHandler) streamFromProvider(c *gin.Context, req *provider.Request, p provider.Provider, model string, sink *archiveSink) error {
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	pr, pw := io.Pipe()
	streamWriterBase := newDeferredStreamWriter(c)
	var streamWriter io.Writer = streamWriterBase
	if sink != nil {
		streamWriter = newTeeStreamWriter(streamWriterBase, sink)
	}
	// 流计时由后台 goroutine 经 errCh 的 happens-before 传回，再由请求 goroutine 写入。
	var streamTiming provider.StreamTiming
	errCh := make(chan error, 1)

	go func() {
		defer pw.Close()
		var err error
		if sp, ok := p.(provider.StreamProvider); ok {
			streamTiming, err = runGuardedStreamContext(ctx, h.router, req, p, pw, sp.ForwardStream)
		} else {
			streamTiming, err = runGuardedStreamContext(ctx, h.router, req, p, pw, func(ctx context.Context, _ *provider.Request, w io.Writer) error {
				return h.forwardRawStream(ctx, req, p, w)
			})
		}
		errCh <- err
	}()

	err := anthropicSSEToResponsesSSEWithUsage(pr, streamWriter, model, c)
	if err != nil {
		cancel()
	}
	_ = pr.Close()

	streamErr := <-errCh
	c.Set("attempt_stream_timing", streamTiming)
	if streamErr != nil && err == nil {
		return streamErr
	}
	if err != nil {
		return err
	}
	streamWriterBase.Commit()
	return err
}

// forwardRawStream 对非 StreamProvider 做原始 Anthropic SSE 转发
func (h *responsesHandler) forwardRawStream(ctx context.Context, req *provider.Request, p provider.Provider, w io.Writer) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL(), strings.NewReader(string(body)))
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

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return provider.NewHTTPError(p.Name(), resp, b)
	}

	_, err = io.Copy(w, resp.Body)
	return err
}

// ─── Anthropic SSE → Responses SSE 转换 ────────────────────────────────────
// 严格对齐 Codex CLI 的解析要求：
//   - SSE 格式：event: <type>\ndata: <json>\n\n
//   - 每个 data JSON 必须有 "type" 字段
//   - response.completed 只需 response.id（其他字段可选）
//   - response.output_item.done 的 item 必须有完整结构
//   - response.content_part.added 被 Codex 忽略，不需要发

func anthropicSSEToResponsesSSE(r io.Reader, w io.Writer, model string) error {
	return anthropicSSEToResponsesSSEWithUsage(r, w, model, nil)
}

// anthropicSSEToResponsesSSEWithUsage is like anthropicSSEToResponsesSSE but
// also propagates parsed usage into gin.Context for archive token metadata.
// c may be nil (unit tests) — usage propagation is skipped.
func anthropicSSEToResponsesSSEWithUsage(r io.Reader, w io.Writer, model string, c *gin.Context) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

	var eventType string
	var dataLines []string

	respID := fmt.Sprintf("resp_%d", time.Now().UnixNano()/1000000)
	created := time.Now().Unix()
	outputIndex := 0
	textStarted := false
	thinkingStarted := false
	toolStarted := false
	toolCallID := ""
	toolCallName := ""
	inputTokens := 0
	inputTokensKnown := false
	outputTokens := 0
	cacheReadInputTokens := 0
	cacheCreationInputTokens := 0
	cacheUsageKnown := false

	// 收集完整文本内容（用于 output_item.done）
	var fullText strings.Builder
	// 收集完整思维链（思考模式下客户端必须回传，否则下一轮上游拒绝）
	var fullReasoning strings.Builder
	// 收集完整 tool arguments
	var fullToolArgs strings.Builder

	createdSent := false
	ensureResponseCreated := func() {
		if createdSent {
			return
		}
		writeResponsesSSE(w, "response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": respID, "object": "response", "created_at": created, "status": "in_progress", "model": model},
		})
		createdSent = true
	}

	flushEvent := func() error {
		if eventType == "" || len(dataLines) == 0 {
			return nil
		}
		// Do not commit response.created until the upstream has produced its
		// first event. If the provider fails before that point, the caller can
		// still fall back or return the upstream HTTP status.
		ensureResponseCreated()
		payload := strings.Join(dataLines, "\n")

		switch eventType {
		case "message_start":
			// 提取 input_tokens
			var evt struct {
				Message struct {
					Usage struct {
						InputTokens              *int `json:"input_tokens"`
						CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				if evt.Message.Usage.InputTokens != nil {
					inputTokens = *evt.Message.Usage.InputTokens
					inputTokensKnown = true
				}
				if evt.Message.Usage.CacheReadInputTokens != nil {
					cacheReadInputTokens = *evt.Message.Usage.CacheReadInputTokens
					cacheUsageKnown = true
				}
				if evt.Message.Usage.CacheCreationInputTokens != nil {
					cacheCreationInputTokens = *evt.Message.Usage.CacheCreationInputTokens
					cacheUsageKnown = true
				}
			}

		case "content_block_start":
			var evt struct {
				Index int `json:"index"`
				Block struct {
					Type  string `json:"type"`
					ID    string `json:"id"`
					Name  string `json:"name"`
					Input any    `json:"input"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				return nil
			}

			switch evt.Block.Type {
			case "thinking":
				thinkingStarted = true
				fullReasoning.Reset()
				writeResponsesSSE(w, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]any{
						"type":    "reasoning",
						"id":      fmt.Sprintf("rs_%d", outputIndex),
						"status":  "in_progress",
						"summary": []any{},
					},
				})
			case "text":
				textStarted = true
				fullText.Reset()
				// output_item.added (message type)
				writeResponsesSSE(w, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]any{
						"type":    "message",
						"id":      fmt.Sprintf("msg_%d", outputIndex),
						"status":  "in_progress",
						"role":    "assistant",
						"content": []any{},
					},
				})
			case "tool_use":
				toolStarted = true
				toolCallID = evt.Block.ID
				toolCallName = evt.Block.Name
				fullToolArgs.Reset()
				// output_item.added (function_call type)
				writeResponsesSSE(w, "response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]any{
						"type":      "function_call",
						"id":        fmt.Sprintf("fc_%s", evt.Block.ID),
						"call_id":   evt.Block.ID,
						"name":      evt.Block.Name,
						"arguments": "",
						"status":    "in_progress",
					},
				})
				// 部分上游（如 deepv）在 content_block_start 就携带完整 input，不再发 input_json_delta
				if evt.Block.Input != nil {
					if inputJSON, err := json.Marshal(evt.Block.Input); err == nil && string(inputJSON) != "{}" {
						fullToolArgs.Write(inputJSON)
						writeResponsesSSE(w, "response.function_call_arguments.delta", map[string]any{
							"type":         "response.function_call_arguments.delta",
							"item_id":      fmt.Sprintf("fc_%s", evt.Block.ID),
							"output_index": outputIndex,
							"delta":        string(inputJSON),
						})
					}
				}
			}

		case "content_block_delta":
			var meta struct {
				Index int `json:"index"`
				Delta struct {
					Type string `json:"type"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(payload), &meta); err != nil {
				return nil
			}

			switch meta.Delta.Type {
			case "text_delta":
				var evt struct {
					Delta struct {
						Text string `json:"text"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(payload), &evt); err != nil {
					return nil
				}
				fullText.WriteString(evt.Delta.Text)
				writeResponsesSSE(w, "response.output_text.delta", map[string]any{
					"type":          "response.output_text.delta",
					"item_id":       fmt.Sprintf("msg_%d", outputIndex),
					"output_index":  outputIndex,
					"content_index": 0,
					"delta":         evt.Delta.Text,
				})

			case "thinking_delta":
				var evt struct {
					Delta struct {
						Thinking string `json:"thinking"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(payload), &evt); err != nil {
					return nil
				}
				fullReasoning.WriteString(evt.Delta.Thinking)
				writeResponsesSSE(w, "response.reasoning_summary_text.delta", map[string]any{
					"type":          "response.reasoning_summary_text.delta",
					"item_id":       fmt.Sprintf("rs_%d", outputIndex),
					"output_index":  outputIndex,
					"summary_index": 0,
					"delta":         evt.Delta.Thinking,
				})

			case "input_json_delta":
				var evt struct {
					Index int `json:"index"`
					Delta struct {
						PartialJSON string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal([]byte(payload), &evt); err != nil {
					return nil
				}
				fullToolArgs.WriteString(evt.Delta.PartialJSON)
				writeResponsesSSE(w, "response.function_call_arguments.delta", map[string]any{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fmt.Sprintf("fc_%s", toolCallID),
					"output_index": outputIndex,
					"delta":        evt.Delta.PartialJSON,
				})
			}

		case "content_block_stop":
			if thinkingStarted {
				finalReasoning := fullReasoning.String()
				writeResponsesSSE(w, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]any{
						"type":   "reasoning",
						"id":     fmt.Sprintf("rs_%d", outputIndex),
						"status": "completed",
						"summary": []any{
							map[string]any{"type": "summary_text", "text": finalReasoning},
						},
					},
				})
				thinkingStarted = false
				outputIndex++
			} else if textStarted {
				finalText := fullText.String()
				// output_item.done — 带完整内容
				writeResponsesSSE(w, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]any{
						"type":   "message",
						"id":     fmt.Sprintf("msg_%d", outputIndex),
						"status": "completed",
						"role":   "assistant",
						"content": []any{
							map[string]any{
								"type": "output_text",
								"text": finalText,
							},
						},
					},
				})
				textStarted = false
				outputIndex++
			} else if toolStarted {
				finalArgs := fullToolArgs.String()
				writeResponsesSSE(w, "response.output_item.done", map[string]any{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]any{
						"type":      "function_call",
						"id":        fmt.Sprintf("fc_%s", toolCallID),
						"call_id":   toolCallID,
						"name":      toolCallName,
						"arguments": finalArgs,
						"status":    "completed",
					},
				})
				toolStarted = false
				outputIndex++
			}

		case "message_delta":
			var evt struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					InputTokens              *int `json:"input_tokens"`
					OutputTokens             *int `json:"output_tokens"`
					CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				return nil
			}
			if evt.Usage.InputTokens != nil {
				inputTokens = *evt.Usage.InputTokens
				inputTokensKnown = true
			}
			if evt.Usage.OutputTokens != nil {
				outputTokens = *evt.Usage.OutputTokens
			}
			if evt.Usage.CacheReadInputTokens != nil {
				cacheReadInputTokens = *evt.Usage.CacheReadInputTokens
				cacheUsageKnown = true
			}
			if evt.Usage.CacheCreationInputTokens != nil {
				cacheCreationInputTokens = *evt.Usage.CacheCreationInputTokens
				cacheUsageKnown = true
			}

			status := "completed"
			if evt.Delta.StopReason == "max_tokens" {
				status = "incomplete"
			}

			// response.completed — 只需 id，其他可选
			promptTokens := inputTokens
			if inputTokensKnown {
				promptTokens += cacheReadInputTokens + cacheCreationInputTokens
			}
			usage := map[string]any{
				"input_tokens":  promptTokens,
				"output_tokens": outputTokens,
				"total_tokens":  promptTokens + outputTokens,
			}
			if cacheUsageKnown {
				usage["input_tokens_details"] = map[string]int{"cached_tokens": cacheReadInputTokens}
			}
			writeResponsesSSE(w, "response.completed", map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":         respID,
					"object":     "response",
					"created_at": created,
					"status":     status,
					"model":      model,
					"output":     []any{},
					"usage":      usage,
				},
			})
			// Propagate usage to gin.Context for archive metadata.
			if c != nil {
				setUsageMetadata(c, promptTokens, outputTokens)
				setCacheUsageMetadata(c, cacheReadInputTokens, cacheCreationInputTokens, promptTokens, cacheUsageKnown && inputTokensKnown)
			}

		case "message_stop":
			// 已通过 response.completed 处理

		case "ping":
			// 跳过
		}

		eventType = ""
		dataLines = nil
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flushEvent(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}
	return flushEvent()
}

// writeResponsesSSE 写一条 Responses API 格式的 SSE 事件
func writeResponsesSSE(w io.Writer, event string, data map[string]any) {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(dataJSON))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
