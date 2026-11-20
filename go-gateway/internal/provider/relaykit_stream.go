package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// Delay successful terminal events until the upstream stream is verified.
func (p *OpenAIProvider) forwardRelayKitStream(ctx context.Context, cancel context.CancelFunc, body io.Reader, model string, w io.Writer) error {
	id := "msg_oai_" + time.Now().Format("20060102150405")
	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAI, types.RelayFormatClaude, relayconvert.ResponseStreamOptions{ID: id, Model: model, IncludeUsage: true})
	if err != nil {
		return err
	}
	meta := &convmeta.Values{OriginModelName: model, UpstreamModelName: model, IsStream: true}
	var pending []*dto.ClaudeResponse
	emit := func(results []relayconvert.ResponseResult) error {
		for _, result := range results {
			event, ok := result.Value.(*dto.ClaudeResponse)
			if !ok {
				return fmt.Errorf("unexpected relaykit stream event %T", result.Value)
			}
			if event.Type == "message_delta" || event.Type == "message_stop" {
				pending = append(pending, event)
				continue
			}
			data, err := json.Marshal(event)
			if err != nil {
				return err
			}
			writeSSE(w, event.Type, string(data))
		}
		return nil
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	kick, idleTripped := startStreamIdleWatchdog(ctx, p.requestTimeout(), cancel)
	terminal := false
	var usage *dto.Usage
	var gatewayUsage *openAIStreamUsage
	finishReason := ""
	for scanner.Scan() {
		kick()
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			terminal = true
			break
		}
		if err := streamPayloadError("", []byte(payload)); err != nil {
			return err
		}
		var chunk dto.ChatCompletionsStreamResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Id == "" {
			chunk.Id = id
		}
		if chunk.Model == "" {
			chunk.Model = model
		}
		// A usage-only chunk is not necessarily the last frame. RelayKit treats
		// it as terminal, so defer the most recent usage until the upstream has
		// actually ended; this also permits later usage to replace earlier usage.
		if chunk.Usage != nil {
			usage = chunk.Usage
			var gatewayChunk openAIStreamChunk
			if json.Unmarshal([]byte(payload), &gatewayChunk) == nil {
				gatewayUsage = gatewayChunk.Usage
			}
			chunk.Usage = nil
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
			terminal = true
			finishReason = *chunk.Choices[0].FinishReason
		}
		events, err := relayconvert.ConvertStreamResponseChunk(ctx, meta, state, &chunk)
		if err != nil {
			return fmt.Errorf("convert stream: %w", err)
		}
		if err := emit(events); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if idleTripped() {
			return fmt.Errorf("upstream stream idle over %v: no data received", p.requestTimeout())
		}
		return err
	}
	if !terminal {
		return incompleteStreamError(p.Name())
	}
	if usage != nil {
		events, err := relayconvert.ConvertStreamResponseChunk(ctx, meta, state, &dto.ChatCompletionsStreamResponse{Id: id, Model: model, Usage: usage})
		if err != nil {
			return fmt.Errorf("convert stream usage: %w", err)
		}
		if err := emit(events); err != nil {
			return err
		}
	}
	events, err := relayconvert.FinalizeStreamResponse(ctx, meta, state)
	if err != nil {
		return fmt.Errorf("finalize stream: %w", err)
	}
	if err := emit(events); err != nil {
		return err
	}
	for _, event := range pending {
		if event.Type == "message_delta" {
			input, output := 0, 0
			counts := map[string]int{}
			if gatewayUsage != nil {
				input, output = gatewayUsage.PromptTokens, gatewayUsage.CompletionTokens
				if details := gatewayUsage.PromptTokensDetails; details != nil && details.cacheTokensKnown {
					counts["cache_read_input_tokens"] = details.CachedTokens
					input -= details.CachedTokens
					if input < 0 {
						input = 0
					}
				}
			}
			counts["input_tokens"], counts["output_tokens"] = input, output
			data, err := json.Marshal(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": mapFinishReason(finishReason), "stop_sequence": nil}, "usage": counts})
			if err != nil {
				return err
			}
			writeSSE(w, event.Type, string(data))
			continue
		}
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		writeSSE(w, event.Type, string(data))
	}
	return nil
}
