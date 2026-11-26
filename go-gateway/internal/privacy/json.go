package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var ErrSignedContent = errors.New("privacy: signed content cannot be safely rewritten")

// MaskJSON is an experimental primitive. It is not wired to handlers until
// every protocol and streaming path has been verified.
func (s *Session) MaskJSON(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var obj any
	if err := dec.Decode(&obj); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("privacy: invalid trailing JSON")
	}
	for pass := 0; pass < 2; pass++ {
		v, err := s.maskValue(obj, "", false, false)
		if err != nil {
			return nil, err
		}
		if pass == 1 {
			obj = v
		}
	}
	return json.Marshal(obj)
}
func (s *Session) maskValue(v any, key string, inArgs, signed bool) (any, error) {
	switch v := v.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		signed = signed || t == "thinking" || t == "redacted_thinking" || t == "reasoning"
		result := make(map[string]any, len(v))
		for k, x := range v {
			args := inArgs || k == "arguments" || (k == "input" && strings.HasSuffix(t, "tool_use"))
			if !args && (k == "signature" || k == "thoughtSignature" || k == "thought_signature" || k == "encrypted_content") {
				result[k] = x
				continue
			}
			y, err := s.maskValue(x, k, args, signed)
			if err != nil {
				return nil, err
			}
			result[k] = y
		}
		return result, nil
	case []any:
		result := make([]any, len(v))
		for i, x := range v {
			y, err := s.maskValue(x, key, inArgs, signed)
			if err != nil {
				return nil, err
			}
			result[i] = y
		}
		return result, nil
	case string:
		// OpenAI tool calls encode a second JSON document inside arguments.
		// Parse it separately so custom credential values under api_key are
		// found, and marshal it again to preserve JSON escaping.
		if key == "arguments" && json.Valid([]byte(v)) {
			var nested any
			decoder := json.NewDecoder(strings.NewReader(v))
			decoder.UseNumber()
			if err := decoder.Decode(&nested); err != nil {
				return nil, err
			}
			changed, err := s.maskValue(nested, "", true, signed)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(changed)
			if err != nil {
				return nil, err
			}
			return string(encoded), nil
		}
		if !inArgs && protocolField(key) {
			return v, nil
		}
		var masked string
		var err error
		if inArgs && secretField(key) && v != "" {
			masked, err = s.maskSecret(v)
		} else {
			masked, err = s.Mask(v)
		}
		if err != nil {
			return nil, err
		}
		if signed && masked != v {
			return nil, ErrSignedContent
		}
		return masked, nil
	default:
		return v, nil
	}
}
func protocolField(k string) bool {
	switch k {
	case "id", "model", "role", "type", "name", "tool_call_id", "call_id", "item_id", "media_type", "mime_type", "data", "url", "image_url", "file_id", "previous_response_id":
		return true
	}
	return false
}

func secretField(key string) bool {
	key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	switch key {
	case "apikey", "password", "passwd", "accesstoken", "refreshtoken", "secret":
		return true
	}
	return false
}

// RestoreJSON restores placeholders in JSON string values while preserving
// escaped JSON documents carried inside tool-call arguments.
func (s *Session) RestoreJSON(body []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("privacy: invalid trailing JSON")
	}
	result, err := s.restoreValue(v, "", false)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}
func (s *Session) restoreValue(v any, key string, inArgs bool) (any, error) {
	switch v := v.(type) {
	case map[string]any:
		typ, _ := v["type"].(string)
		if !inArgs && (typ == "thinking" || typ == "redacted_thinking" || typ == "reasoning") {
			return v, nil
		}
		out := make(map[string]any, len(v))
		for k, x := range v {
			if !inArgs && (k == "signature" || k == "thoughtSignature" || k == "thought_signature" || k == "encrypted_content") {
				out[k] = x
				continue
			}
			toolArgs := inArgs || k == "arguments" || (k == "input" && strings.HasSuffix(typ, "tool_use"))
			next, err := s.restoreValue(x, k, toolArgs)
			if err != nil {
				return nil, err
			}
			out[k] = next
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			next, err := s.restoreValue(x, key, inArgs)
			if err != nil {
				return nil, err
			}
			out[i] = next
		}
		return out, nil
	case string:
		if key == "arguments" && json.Valid([]byte(v)) {
			dec := json.NewDecoder(strings.NewReader(v))
			dec.UseNumber()
			var nested any
			if err := dec.Decode(&nested); err != nil {
				return nil, err
			}
			value, err := s.restoreValue(nested, "", true)
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			return string(b), nil
		}
		if !inArgs && protocolField(key) {
			return v, nil
		}
		return s.Restore(v), nil
	default:
		return v, nil
	}
}
