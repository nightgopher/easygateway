package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Capability names are deliberately small and protocol independent. They are
// used by routing, model discovery and validation; provider-specific options
// remain in Request.raw and are not converted into capabilities.
const (
	CapabilityText      = "text"
	CapabilityVision    = "vision"
	CapabilityVideo     = "video"
	CapabilityFile      = "file"
	CapabilityAudio     = "audio"
	CapabilityToolCall  = "tool_calling"
	CapabilityStreaming = "streaming"
	CapabilityReasoning = "reasoning"
)

// CapabilityProvider is optional so providers registered by older code keep
// working. Providers that implement it are filtered before a request is sent.
type CapabilityProvider interface {
	Provider
	Capabilities() []string
}

// ModelInfo is the public model registry entry returned by /v1/models.
// The extra fields are additive to the OpenAI model object.
type ModelInfo struct {
	ID              string   `json:"id"`
	Provider        string   `json:"provider,omitempty"`
	Protocol        string   `json:"protocol,omitempty"`
	Capabilities    []string `json:"capabilities"`
	InputModalities []string `json:"input_modalities,omitempty"`
	MaxInputTokens  int      `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Description     string   `json:"description,omitempty"`
}

// UnsupportedCapabilityError is a client error: the requested model exists,
// but none of its configured providers can handle the request modalities.
type UnsupportedCapabilityError struct {
	Model    string
	Required []string
	Endpoint string
}

func (e *UnsupportedCapabilityError) Error() string {
	if e.Endpoint != "" {
		return fmt.Sprintf("model %q uses the System One decision protocol; send state and questions to %s", e.Model, e.Endpoint)
	}
	return fmt.Sprintf("model %q does not support request capabilities: %s", e.Model, joinCapabilities(e.Required))
}

func IsUnsupportedCapability(err error) bool {
	var target *UnsupportedCapabilityError
	return errors.As(err, &target)
}

// RequiredCapabilities inspects content and request flags without looking at
// provider-specific fields. This prevents a vision request from falling back
// to a text-only model and silently losing the image.
func (r *Request) RequiredCapabilities() []string {
	seen := map[string]bool{CapabilityText: true}
	if r == nil {
		return []string{CapabilityText}
	}
	if r.Stream {
		seen[CapabilityStreaming] = true
	}
	if raw, ok := r.RawField("tools"); ok && rawCollectionHasItems(raw) {
		seen[CapabilityToolCall] = true
	}
	if requestUsesReasoning(r) {
		seen[CapabilityReasoning] = true
	}
	for _, message := range r.Messages {
		for _, block := range message.Content.Blocks() {
			switch block.Type {
			case "image_url", "image", "input_image":
				seen[CapabilityVision] = true
			case "video_url", "video", "input_video":
				seen[CapabilityVideo] = true
			case "file", "file_url", "input_file":
				seen[CapabilityFile] = true
			case "audio", "input_audio":
				seen[CapabilityAudio] = true
			case "tool_use", "tool_result":
				seen[CapabilityToolCall] = true
			}
		}
	}

	result := make([]string, 0, len(seen))
	for capability := range seen {
		result = append(result, capability)
	}
	sort.Strings(result)
	return result
}

func rawCollectionHasItems(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var array []json.RawMessage
	if err := json.Unmarshal(trimmed, &array); err == nil {
		return len(array) > 0
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err == nil {
		return len(object) > 0
	}
	return true
}

func requestUsesReasoning(r *Request) bool {
	if raw, ok := r.RawField("reasoning_effort"); ok {
		var effort string
		if json.Unmarshal(raw, &effort) == nil {
			effort = strings.ToLower(strings.TrimSpace(effort))
			if effort != "" && effort != "none" && effort != "disabled" {
				return true
			}
		} else if rawCollectionHasItems(raw) {
			return true
		}
	}
	for _, field := range []string{"thinking", "reasoning"} {
		if raw, ok := r.RawField(field); ok && reasoningObjectEnabled(raw) {
			return true
		}
	}
	if raw, ok := r.RawField("extra_body"); ok {
		var extra struct {
			Thinking  json.RawMessage `json:"thinking"`
			Reasoning json.RawMessage `json:"reasoning"`
		}
		if json.Unmarshal(raw, &extra) == nil &&
			(reasoningObjectEnabled(extra.Thinking) || reasoningObjectEnabled(extra.Reasoning)) {
			return true
		}
	}
	return false
}

func reasoningObjectEnabled(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var state struct {
		Type   string `json:"type"`
		Effort string `json:"effort"`
	}
	if json.Unmarshal(trimmed, &state) == nil {
		kind := strings.ToLower(strings.TrimSpace(state.Type))
		effort := strings.ToLower(strings.TrimSpace(state.Effort))
		if kind == "disabled" || kind == "none" || effort == "none" || effort == "disabled" {
			return false
		}
	}
	return rawCollectionHasItems(trimmed)
}

func supportsCapabilities(p Provider, required []string) bool {
	cp, ok := p.(CapabilityProvider)
	if !ok {
		// Legacy providers do not declare metadata. Keep them routable for
		// backwards compatibility; configured providers are explicit.
		return true
	}
	declared := make(map[string]bool)
	for _, capability := range cp.Capabilities() {
		declared[capability] = true
	}
	if len(declared) == 0 {
		return false
	}
	for _, capability := range required {
		if !declared[capability] {
			return false
		}
	}
	return true
}

func joinCapabilities(capabilities []string) string {
	if len(capabilities) == 0 {
		return "none"
	}
	return fmt.Sprintf("%v", capabilities)
}
