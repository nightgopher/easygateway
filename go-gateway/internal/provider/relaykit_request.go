package provider

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// Use RelayKit only when its message and tool output agrees with the existing
// gateway conversion. This keeps extension fields and unusual tool/media
// payloads on the existing path until their compatibility has been verified.
func relayKitRequest(req *Request, legacy *openAIRequest) (*openAIRequest, bool) {
	payload := map[string]any{"model": req.Model, "messages": req.Messages, "max_tokens": req.MaxTokens, "stream": req.Stream}
	for _, field := range []string{"system", "tools"} {
		if raw, ok := req.raw[field]; ok {
			payload[field] = raw
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	var source dto.ClaudeRequest
	if err = json.Unmarshal(data, &source); err != nil {
		return nil, false
	}
	converted, err := relayconvert.ConvertRequest(context.Background(), nil, types.RelayFormatOpenAI, &source)
	if err != nil || len(converted.Diagnostics) > 0 {
		return nil, false
	}
	data, err = json.Marshal(converted.Value)
	if err != nil {
		return nil, false
	}
	var candidate openAIRequest
	if err = json.Unmarshal(data, &candidate); err != nil {
		return nil, false
	}
	sameJSON := func(a, b any) bool {
		aa, e1 := json.Marshal(a)
		bb, e2 := json.Marshal(b)
		return e1 == nil && e2 == nil && bytes.Equal(aa, bb)
	}
	if !sameJSON(candidate.Messages, legacy.Messages) || !sameJSON(candidate.Tools, legacy.Tools) {
		return nil, false
	}
	result := *legacy
	result.Messages = candidate.Messages
	result.Tools = candidate.Tools
	return &result, true
}
