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
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/weijian/go-llm-gateway/internal/archive"
	"github.com/weijian/go-llm-gateway/internal/provider"
)

type openAIChatCompletionsHandler struct {
	router   *provider.Router
	logger   *log.Logger
	archiver *archive.Archiver
}

type openAIChatCompletionsRequest struct {
	Model     string              `json:"model"`
	Messages  []openAIChatMessage `json:"messages"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
	Stream    bool                `json:"stream,omitempty"`
	Tools     []openAIChatTool    `json:"tools,omitempty"`
	raw       map[string]json.RawMessage
}

type openAIChatMessage struct {
	Role             string               `json:"role"`
	Content          openAIMessageContent `json:"content"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
}

// openAIMessageContent 支持字符串和数组两种 content 格式
type openAIMessageContent struct {
	Str    string               `json:"-"`
	Blocks []openAIContentBlock `json:"-"`
	IsStr  bool                 `json:"-"`
}

type openAIContentBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ImageURL   json.RawMessage `json:"image_url,omitempty"`
	VideoURL   json.RawMessage `json:"video_url,omitempty"`
	File       json.RawMessage `json:"file,omitempty"`
	InputAudio json.RawMessage `json:"input_audio,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

func (b *openAIContentBlock) UnmarshalJSON(data []byte) error {
	type alias openAIContentBlock
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*b = openAIContentBlock(decoded)
	b.Raw = append(b.Raw[:0], data...)
	return nil
}

func (b openAIContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	type alias openAIContentBlock
	return json.Marshal(alias(b))
}

func (c openAIMessageContent) String() string {
	if c.IsStr {
		return c.Str
	}
	var result string
	for _, b := range c.Blocks {
		if b.Type == "text" {
			result += b.Text
		}
	}
	return result
}

func (c openAIMessageContent) IsArray() bool {
	return !c.IsStr
}

func (c openAIMessageContent) TextBlocks() []openAIContentBlock {
	if c.IsStr {
		if c.Str == "" {
			return nil
		}
		return []openAIContentBlock{{Type: "text", Text: c.Str}}
	}
	return c.Blocks
}

func (c openAIMessageContent) MarshalJSON() ([]byte, error) {
	if c.IsStr {
		return json.Marshal(c.Str)
	}
	return json.Marshal(c.Blocks)
}

func (c *openAIMessageContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Str = s
		c.IsStr = true
		return nil
	}
	var blocks []openAIContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	c.Blocks = blocks
	c.IsStr = false
	return nil
}

type openAIChatTool struct {
	Type     string                 `json:"type"`
	Function openAIChatToolFunction `json:"function"`
}

type openAIChatToolFunction struct {
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type openAIChatToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openAIChatToolFunction `json:"function"`
}

type openAIChatCompletionResponse struct {
	ID      string                    `json:"id"`
	Object  string                    `json:"object"`
	Created int64                     `json:"created"`
	Model   string                    `json:"model"`
	Choices []openAIChatChoice        `json:"choices"`
	Usage   openAIChatCompletionUsage `json:"usage"`
}

type openAIChatChoice struct {
	Index        int               `json:"index"`
	Message      openAIChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type openAIChatCompletionUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type openAIStreamChunkResponse struct {
	ID      string                    `json:"id,omitempty"`
	Object  string                    `json:"object"`
	Created int64                     `json:"created"`
	Model   string                    `json:"model,omitempty"`
	Choices []openAIStreamChunkChoice `json:"choices"`
	Usage   *openAIStreamUsage        `json:"usage,omitempty"`
}

// openAIStreamUsage 流式最后一个 chunk 的 usage 字段
type openAIStreamUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type openAIStreamChunkChoice struct {
	Index        int               `json:"index"`
	Delta        openAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type openAIStreamToolCallDelta struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function openAIChatToolFunction `json:"function,omitempty"`
}

type openAIStreamDelta struct {
	Role             string                      `json:"role,omitempty"`
	Content          string                      `json:"content,omitempty"`
	ReasoningContent string                      `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIStreamToolCallDelta `json:"tool_calls,omitempty"`
}

func NewChatCompletionsHandler(router *provider.Router, logger *log.Logger) *openAIChatCompletionsHandler {
	return &openAIChatCompletionsHandler{router: router, logger: logger}
}

// SetArchiver injects the conversation archiver.
func (h *openAIChatCompletionsHandler) SetArchiver(a *archive.Archiver) {
	h.archiver = a
}

func (h *openAIChatCompletionsHandler) Handle(c *gin.Context) {
	c.Request = c.Request.WithContext(h.router.BeginRouting(c.Request.Context()))
	rawBody, _ := io.ReadAll(c.Request.Body)
	logRequestSummary(h.logger, c, "chat.completions", len(rawBody))
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))

	var req openAIChatCompletionsRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if err := json.Unmarshal(rawBody, &req.raw); err != nil {
		req.raw = map[string]json.RawMessage{}
	}
	if req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Model is required"})
		return
	}
	if len(req.Messages) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Messages are required"})
		return
	}

	providerReq, err := toProviderRequest(&req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	setRequestMetadata(c, req.Model, req.Stream)

	if req.Stream {
		h.handleStream(c, providerReq, rawBody)
		return
	}

	resp, finalProvider, attempts, err := h.router.ForwardWithDetails(c.Request.Context(), providerReq.Model, providerReq)
	setForwardMetadata(c, finalProvider, attempts, err)
	if err != nil {
		h.logger.Printf("Forward failed: %v", err)
		setProviderErrorHeaders(c, err)
		if h.archiver != nil && h.archiver.Enabled() {
			// Sanitize the error message for BOTH response_body and error_reason
			// to prevent credential leakage (e.g. "invalid api key: sk-secret").
			sanitized := archiveErrorReason(err)
			submitArchive(c, h.archiver, archive.ProtocolChatCompletions, rawBody,
				[]byte(fmt.Sprintf(`{"error":%q}`, sanitized)),
				archive.StatusError, routingErrorStatus(err), archiveErrorReason(err))
		}
		c.JSON(routingErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	setUsageMetadata(c, totalPromptTokens(resp), resp.Usage.OutputTokens)
	setCacheUsageMetadata(c, resp.CacheReadInputTokens, resp.CacheCreationInputTokens, resp.CacheInputTokens, resp.CacheUsageKnown)

	responseObj := toOpenAIChatCompletionResponse(resp)
	if h.archiver != nil && h.archiver.Enabled() {
		submitArchive(c, h.archiver, archive.ProtocolChatCompletions, rawBody,
			[]byte(marshalCompact(responseObj)),
			archive.StatusCompleted, http.StatusOK, "")
	}

	c.JSON(http.StatusOK, responseObj)
}

func (h *openAIChatCompletionsHandler) handleStream(c *gin.Context, req *provider.Request, rawBody []byte) {
	providerChain, err := h.router.RouteForStreamRequest(req.Model, req)
	if err != nil {
		setProviderErrorHeaders(c, err)
		submitRoutingErrorArchive(c, h.archiver, archive.ProtocolChatCompletions, rawBody, err)
		c.JSON(routingErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	var sink *archiveSink
	if h.archiver != nil && h.archiver.Enabled() {
		sink = newArchiveSink(h.archiver.MaxBodyBytes())
	}

	originalModel := req.Model
	var lastErr error
	streamOK := false
	for _, p := range providerChain {
		if h.router.ProviderAttempted(c.Request.Context(), originalModel, p) || !h.router.AllowProviderRequestFor(originalModel, p) {
			continue
		}
		started := time.Now()
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
			h.logger.Printf("chat.completions stream provider %s failed: %v", p.Name(), err)
			if c.Writer.Written() {
				_ = writeOpenAIStreamError(c.Writer, err)
				streamOK = true
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
		// Archive the exact client request rather than the provider-mapped request
		// that the retry loop mutates for upstream routing.
		reqBody := rawBody
		if !streamOK && lastErr == nil {
			lastErr = &provider.NoAvailableProvidersError{Model: originalModel, Reason: "disabled, unavailable, or circuit open"}
		}
		if !streamOK && sink.Len() == 0 && lastErr != nil {
			setProviderErrorHeaders(c, lastErr)
			submitArchive(c, h.archiver, archive.ProtocolChatCompletions, reqBody, nil,
				archive.StatusError, routingErrorStatus(lastErr), archiveErrorReason(lastErr))
			c.JSON(routingErrorStatus(lastErr), gin.H{"error": fmt.Sprintf("all providers failed: %v", lastErr)})
			return
		}
		status, reason := parseStreamEndState(sink.Bytes(), lastErr)
		submitArchive(c, h.archiver, archive.ProtocolChatCompletions, reqBody, sink.Bytes(),
			status, c.Writer.Status(), reason)
		return
	}

	if lastErr == nil {
		lastErr = &provider.NoAvailableProvidersError{Model: originalModel, Reason: "disabled, unavailable, or circuit open"}
	}
	if !c.Writer.Written() {
		setProviderErrorHeaders(c, lastErr)
		c.JSON(routingErrorStatus(lastErr), gin.H{"error": fmt.Sprintf("all providers failed: %v", lastErr)})
	}
}

func (h *openAIChatCompletionsHandler) streamFromProvider(c *gin.Context, req *provider.Request, p provider.Provider, sink *archiveSink) error {
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	pr, pw := io.Pipe()
	streamWriterBase := newDeferredStreamWriter(c)
	var streamWriter io.Writer = streamWriterBase
	if sink != nil {
		streamWriter = newTeeStreamWriter(streamWriterBase, sink)
	}
	errCh := make(chan error, 1)
	go func() {
		if sp, ok := p.(provider.StreamProvider); ok {
			errCh <- runGuardedStreamContext(c, ctx, h.router, req, p, pw, sp.ForwardStream)
		} else {
			errCh <- fmt.Errorf("provider %s does not support streaming", p.Name())
		}
		_ = pw.Close()
	}()

	if err := anthropicSSEToOpenAISSEWithUsage(pr, streamWriter, c); err != nil {
		cancel()
		_ = pr.Close()
		<-errCh
		return err
	}
	if err := <-errCh; err != nil {
		return err
	}
	streamWriterBase.Commit()
	return nil
}

func toProviderRequest(req *openAIChatCompletionsRequest) (*provider.Request, error) {
	out := &provider.Request{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		Stream:    req.Stream,
	}
	for key, raw := range req.raw {
		if key == "messages" || key == "tools" || key == "model" || key == "max_tokens" || key == "stream" {
			continue
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err == nil {
			_ = out.SetRawField(key, decoded)
		}
	}

	var systemTexts []string
	for _, msg := range req.Messages {
		textContent := msg.Content.String()
		switch msg.Role {
		case "system":
			if textContent != "" {
				systemTexts = append(systemTexts, textContent)
			}
		case "tool":
			out.Messages = append(out.Messages, provider.Message{
				Role: "user",
				Content: provider.NewBlocksContent([]provider.ContentBlock{{
					Type:       "tool_result",
					ToolUseID:  msg.ToolCallID,
					ContentStr: textContent,
				}}),
			})
		default:
			contentBlocks := msg.Content.TextBlocks()
			blocks := make([]provider.ContentBlock, 0, len(contentBlocks)+len(msg.ToolCalls))
			for _, tb := range contentBlocks {
				blocks = append(blocks, provider.ContentBlock{
					Type:       tb.Type,
					Text:       tb.Text,
					ImageURL:   tb.ImageURL,
					VideoURL:   tb.VideoURL,
					File:       tb.File,
					InputAudio: tb.InputAudio,
					Raw:        append([]byte(nil), tb.Raw...),
				})
			}
			for _, tc := range msg.ToolCalls {
				input := json.RawMessage(`{}`)
				if tc.Function.Arguments != "" {
					if json.Valid([]byte(tc.Function.Arguments)) {
						input = json.RawMessage(tc.Function.Arguments)
					} else {
						return nil, fmt.Errorf("invalid tool arguments for %s", tc.Function.Name)
					}
				}
				blocks = append(blocks, provider.ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
			content := provider.NewStringContent(textContent)
			hasNonTextBlock := false
			for _, block := range blocks {
				if block.Type != "text" {
					hasNonTextBlock = true
					break
				}
			}
			if len(blocks) > 0 && (len(blocks) > 1 || hasNonTextBlock || len(msg.ToolCalls) > 0 || msg.Role == "assistant") {
				content = provider.NewBlocksContent(blocks)
			} else if len(contentBlocks) > 1 {
				// 多 content block（如文件读取），即使无 tool_calls 也用数组格式
				content = provider.NewBlocksContent(blocks)
			}
			out.Messages = append(out.Messages, provider.Message{Role: msg.Role, Content: content})
		}
	}

	if len(systemTexts) > 0 {
		_ = out.SetRawField("system", joinStrings(systemTexts, "\n\n"))
	}
	if len(out.Messages) > 0 {
		_ = out.SetRawField("messages", out.Messages)
	}
	if req.MaxTokens > 0 {
		_ = out.SetRawField("max_tokens", req.MaxTokens)
	}
	if req.Stream {
		_ = out.SetRawField("stream", req.Stream)
	}
	if len(req.Tools) > 0 {
		anthropicTools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			anthropicTools = append(anthropicTools, map[string]any{
				"name":         tool.Function.Name,
				"description":  tool.Function.Description,
				"input_schema": json.RawMessage(tool.Function.Parameters),
			})
		}
		_ = out.SetRawField("tools", anthropicTools)
	}
	return out, nil
}

func toOpenAIChatCompletionResponse(resp *provider.Response) *openAIChatCompletionResponse {
	message := openAIChatMessage{Role: "assistant"}
	var content string
	var reasoningContent string
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			content += block.Text
		case "thinking":
			reasoningContent += block.Thinking
		case "tool_use":
			arguments := "{}"
			if len(block.Input) > 0 {
				arguments = string(block.Input)
			}
			message.ToolCalls = append(message.ToolCalls, openAIChatToolCall{
				ID:   block.ID,
				Type: "function",
				Function: openAIChatToolFunction{
					Name:      block.Name,
					Arguments: arguments,
				},
			})
		}
	}
	message.Content = openAIMessageContent{Str: content, IsStr: true}
	message.ReasoningContent = reasoningContent
	promptTokens := totalPromptTokens(resp)
	usage := openAIChatCompletionUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		TotalTokens:      promptTokens + resp.Usage.OutputTokens,
	}
	if resp.CacheUsageKnown {
		usage.PromptTokensDetails = &promptTokensDetails{CachedTokens: resp.CacheReadInputTokens}
	}
	return &openAIChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []openAIChatChoice{{
			Index:        0,
			Message:      message,
			FinishReason: mapStopReasonToOpenAI(resp.StopReason),
		}},
		Usage: usage,
	}
}

func anthropicSSEToOpenAISSE(r io.Reader, w io.Writer) error {
	return anthropicSSEToOpenAISSEWithUsage(r, w, nil)
}

// anthropicSSEToOpenAISSEWithUsage is like anthropicSSEToOpenAISSE but also
// propagates parsed usage (input/output tokens) into the gin.Context so the
// archive can record accurate token counts for streamed conversations.
// c may be nil (e.g. in unit tests) — usage propagation is skipped.
func anthropicSSEToOpenAISSEWithUsage(r io.Reader, w io.Writer, c *gin.Context) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

	var eventType string
	var dataLines []string
	created := time.Now().Unix()
	messageID := ""
	model := ""
	var pendingToolCalls map[int]*openAIChatToolCall
	accInputTokens := 0
	accInputTokensKnown := false
	accOutputTokens := 0
	accCachedTokens := 0
	accCacheCreationTokens := 0
	cacheUsageKnown := false

	toolIndexMap := make(map[int]int)

	flushEvent := func() error {
		if eventType == "" || len(dataLines) == 0 {
			return nil
		}
		payload := strings.Join(dataLines, "\n")

		// 从 Anthropic SSE 事件中累积 token 信息
		switch eventType {
		case "message_start":
			var ms struct {
				Message struct {
					Usage struct {
						InputTokens              *int `json:"input_tokens"`
						CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(payload), &ms); err == nil {
				if ms.Message.Usage.InputTokens != nil {
					accInputTokens = *ms.Message.Usage.InputTokens
					accInputTokensKnown = true
				}
				if ms.Message.Usage.CacheReadInputTokens != nil {
					accCachedTokens = *ms.Message.Usage.CacheReadInputTokens
					cacheUsageKnown = true
				}
				if ms.Message.Usage.CacheCreationInputTokens != nil {
					accCacheCreationTokens = *ms.Message.Usage.CacheCreationInputTokens
					cacheUsageKnown = true
				}
			}
		case "message_delta":
			var md struct {
				Usage struct {
					InputTokens              *int `json:"input_tokens"`
					OutputTokens             *int `json:"output_tokens"`
					CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(payload), &md); err == nil {
				if md.Usage.InputTokens != nil {
					accInputTokens = *md.Usage.InputTokens
					accInputTokensKnown = true
				}
				if md.Usage.OutputTokens != nil {
					accOutputTokens = *md.Usage.OutputTokens
				}
				if md.Usage.CacheReadInputTokens != nil {
					accCachedTokens = *md.Usage.CacheReadInputTokens
					cacheUsageKnown = true
				}
				if md.Usage.CacheCreationInputTokens != nil {
					accCacheCreationTokens = *md.Usage.CacheCreationInputTokens
					cacheUsageKnown = true
				}
			}
		}

		chunk, done, err := anthropicEventToOpenAIChunk(eventType, payload, created, messageID, model, pendingToolCalls, toolIndexMap)
		if err != nil {
			return err
		}
		if chunk != nil {
			if chunk.ID != "" {
				messageID = chunk.ID
			}
			if chunk.Model != "" {
				model = chunk.Model
			}
			if err := writeOpenAIChunk(w, chunk); err != nil {
				return err
			}
		}
		if done {
			promptTokens := accInputTokens
			if accInputTokensKnown {
				promptTokens += accCachedTokens + accCacheCreationTokens
			}
			usage := &openAIStreamUsage{
				PromptTokens:     promptTokens,
				CompletionTokens: accOutputTokens,
				TotalTokens:      promptTokens + accOutputTokens,
			}
			if cacheUsageKnown {
				usage.PromptTokensDetails = &promptTokensDetails{CachedTokens: accCachedTokens}
			}
			// 发送最终 usage chunk（OpenAI 标准：最后一条带 usage，choices 为空）
			if err := writeOpenAIChunk(w, &openAIStreamChunkResponse{
				ID:      messageID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []openAIStreamChunkChoice{},
				Usage:   usage,
			}); err != nil {
				return err
			}
			// Propagate usage to gin.Context for archive metadata.
			if c != nil {
				setUsageMetadata(c, promptTokens, accOutputTokens)
				setCacheUsageMetadata(c, accCachedTokens, accCacheCreationTokens, promptTokens, cacheUsageKnown && accInputTokensKnown)
			}
			eventType = ""
			dataLines = nil
			_, err := fmt.Fprint(w, "data: [DONE]\n\n")
			return err
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
			if eventType == "message_start" {
				pendingToolCalls = make(map[int]*openAIChatToolCall)
			}
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

func anthropicEventToOpenAIChunk(eventType, payload string, created int64, currentID, currentModel string, pendingToolCalls map[int]*openAIChatToolCall, toolIndexMap map[int]int) (*openAIStreamChunkResponse, bool, error) {
	switch eventType {
	case "ping", "content_block_stop":
		return nil, false, nil
	case "message_start":
		var evt struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			return nil, false, err
		}
		return &openAIStreamChunkResponse{
			ID:      evt.Message.ID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   evt.Message.Model,
			Choices: []openAIStreamChunkChoice{{
				Index: 0,
				Delta: openAIStreamDelta{Role: "assistant"},
			}},
		}, false, nil
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
			return nil, false, err
		}
		if evt.Block.Type != "tool_use" {
			return nil, false, nil
		}
		oaiIndex, ok := toolIndexMap[evt.Index]
		if !ok {
			oaiIndex = len(toolIndexMap)
			toolIndexMap[evt.Index] = oaiIndex
		}
		if pendingToolCalls != nil {
			pendingToolCalls[evt.Index] = &openAIChatToolCall{
				ID:   evt.Block.ID,
				Type: "function",
				Function: openAIChatToolFunction{
					Name: evt.Block.Name,
				},
			}
		}
		// 部分上游（如 deepv）在 content_block_start 就携带完整 input，不再发 input_json_delta
		fullArgs := ""
		if evt.Block.Input != nil {
			if inputJSON, err := json.Marshal(evt.Block.Input); err == nil && string(inputJSON) != "{}" {
				fullArgs = string(inputJSON)
				if pendingToolCalls != nil {
					pendingToolCalls[evt.Index].Function.Arguments = fullArgs
				}
			}
		}
		return &openAIStreamChunkResponse{
			ID:      currentID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   currentModel,
			Choices: []openAIStreamChunkChoice{{
				Index: 0,
				Delta: openAIStreamDelta{ToolCalls: []openAIStreamToolCallDelta{{
					Index: oaiIndex,
					ID:    evt.Block.ID,
					Type:  "function",
					Function: openAIChatToolFunction{
						Name:      evt.Block.Name,
						Arguments: fullArgs,
					},
				}}},
			}},
		}, false, nil
	case "content_block_delta":
		var meta struct {
			Delta struct {
				Type string `json:"type"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &meta); err != nil {
			return nil, false, err
		}
		switch meta.Delta.Type {
		case "text_delta":
			var evt struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				return nil, false, err
			}
			return &openAIStreamChunkResponse{
				ID:      currentID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   currentModel,
				Choices: []openAIStreamChunkChoice{{
					Index: 0,
					Delta: openAIStreamDelta{Content: evt.Delta.Text},
				}},
			}, false, nil
		case "thinking_delta":
			var evt struct {
				Delta struct {
					Thinking string `json:"thinking"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				return nil, false, err
			}
			return &openAIStreamChunkResponse{
				ID:      currentID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   currentModel,
				Choices: []openAIStreamChunkChoice{{
					Index: 0,
					Delta: openAIStreamDelta{ReasoningContent: evt.Delta.Thinking},
				}},
			}, false, nil
		case "input_json_delta":
			var evt struct {
				Index int `json:"index"`
				Delta struct {
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				return nil, false, err
			}
			call, ok := pendingToolCalls[evt.Index]
			if !ok {
				return nil, false, nil
			}
			call.Function.Arguments += evt.Delta.PartialJSON
			oaiIndex, ok := toolIndexMap[evt.Index]
			if !ok {
				oaiIndex = len(toolIndexMap)
				toolIndexMap[evt.Index] = oaiIndex
			}
			return &openAIStreamChunkResponse{
				ID:      currentID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   currentModel,
				Choices: []openAIStreamChunkChoice{{
					Index: 0,
					Delta: openAIStreamDelta{ToolCalls: []openAIStreamToolCallDelta{{
						Index: oaiIndex,
						Function: openAIChatToolFunction{
							Arguments: evt.Delta.PartialJSON,
						},
					}}},
				}},
			}, false, nil
		default:
			return nil, false, nil
		}
	case "message_delta":
		var evt struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			return nil, false, err
		}
		finishReason := mapStopReasonToOpenAI(evt.Delta.StopReason)
		return &openAIStreamChunkResponse{
			ID:      currentID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   currentModel,
			Choices: []openAIStreamChunkChoice{{
				Index:        0,
				Delta:        openAIStreamDelta{},
				FinishReason: &finishReason,
			}},
		}, false, nil
	case "message_stop":
		return nil, true, nil
	default:
		return nil, false, nil
	}
}

func writeOpenAIChunk(w io.Writer, chunk *openAIStreamChunkResponse) error {
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func mapStopReasonToOpenAI(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func joinStrings(items []string, sep string) string {
	if len(items) == 0 {
		return ""
	}
	result := items[0]
	for i := 1; i < len(items); i++ {
		result += sep + items[i]
	}
	return result
}
