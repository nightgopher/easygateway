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
