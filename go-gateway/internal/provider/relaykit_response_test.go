package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestRelayKitResponseThroughProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected upstream method or authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"id","model":"m","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"lookup","arguments":"{\"x\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`))
	}))
	defer srv.Close()
	p := NewOpenAIProvider(&Config{Name: "test", URL: srv.URL, APIKey: "test-key"})
	got, err := p.ForwardRequest(context.Background(), &Request{Model: "m", Messages: []Message{{Role: "user", Content: NewStringContent("hello")}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "tool_use" || len(got.Content) != 1 || got.Content[0].Name != "lookup" || string(got.Content[0].Input) != `{"x":1}` || got.Usage.InputTokens != 6 || got.CacheReadInputTokens != 4 {
		t.Fatalf("unexpected converted response: %#v, content: %#v", got, got.Content)
	}
}

func TestRelayKitResponseCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		useRelayKit bool
	}{
		{"text and cache", `{"id":"id","model":"m","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}`, true},
		{"reasoning and tool", `{"id":"id","model":"m","choices":[{"message":{"role":"assistant","reasoning_content":"think","tool_calls":[{"id":"call","type":"function","function":{"name":"lookup","arguments":"{\"x\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`, true},
		{"empty text and zero cache", `{"choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}}`, true},
		{"invalid tool input", `{"choices":[{"message":{"tool_calls":[{"id":"call","type":"function","function":{"name":"lookup","arguments":"broken"}}]},"finish_reason":"tool_calls"}]}`, false},
		{"content parts", `{"choices":[{"message":{"content":[{"type":"text","text":"hello"}]},"finish_reason":"stop"}]}`, false},
		{"multiple choices", `{"choices":[{"message":{"content":"first"},"finish_reason":"stop"},{"message":{"content":"second"},"finish_reason":"stop"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var source openAIResponse
			if err := json.Unmarshal([]byte(tc.body), &source); err != nil {
				t.Fatal(err)
			}
			got, ok := relayKitOpenAIResponse(&source)
			if ok != tc.useRelayKit {
				t.Fatalf("relaykit used: %v", ok)
			}
			if !ok {
				got = fromOpenAIResponse(&source)
			}
			want := fromOpenAIResponseLegacy(&source)
			actualJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			expectedJSON, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var actualValue, expectedValue any
			if err := json.Unmarshal(actualJSON, &actualValue); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(expectedJSON, &expectedValue); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actualValue, expectedValue) {
				t.Fatalf("JSON got %s; want %s", actualJSON, expectedJSON)
			}
			// Raw holds serialization details, not response semantics.
			for i := range got.Content {
				got.Content[i].Raw = nil
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v\nwant %#v\ngot content %#v\nwant content %#v", got, want, got.Content, want.Content)
			}
		})
	}
}
