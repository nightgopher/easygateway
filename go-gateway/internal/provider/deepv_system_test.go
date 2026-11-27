package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDeepVSystemInstructionWireFormat(t *testing.T) {
	cases := []struct {
		name   string
		system string
		texts  []string
	}{
		{name: "absent"},
		{name: "empty string", system: `""`},
		{name: "empty blocks", system: `[]`},
		{name: "string", system: `"Only reply with fixture-ok."`, texts: []string{"Only reply with fixture-ok."}},
		{name: "text blocks", system: `[{"type":"text","text":"你是官网助手。"},{"type":"text","text":""},{"type":"text","text":"只使用已确认的参数。"}]`, texts: []string{"你是官网助手。", "只使用已确认的参数。"}},
	}
	for _, streaming := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("stream=%t/%s", streaming, tc.name), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wantPath := "/v1/chat/messages"
					if streaming {
						wantPath = "/v1/chat/stream"
					}
					if r.URL.Path != wantPath {
						t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
					}
					var wire map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if _, present := wire["systemInstruction"]; present {
						t.Error("systemInstruction must not be sent at the request root")
					}
					var config struct {
						System *struct {
							Parts []struct {
								Text string `json:"text"`
							} `json:"parts"`
						} `json:"systemInstruction"`
						MaxOutputTokens int `json:"maxOutputTokens"`
					}
					if err := json.Unmarshal(wire["config"], &config); err != nil {
						t.Error(err)
					}
					if config.MaxOutputTokens != 2048 {
						t.Errorf("maxOutputTokens = %d, want 2048", config.MaxOutputTokens)
					}
					var got []string
					if config.System != nil {
						for _, part := range config.System.Parts {
							got = append(got, part.Text)
						}
					}
					if !reflect.DeepEqual(got, tc.texts) {
						t.Errorf("config.systemInstruction text = %#v, want %#v", got, tc.texts)
					}
					if len(tc.texts) == 0 && config.System != nil {
						t.Error("empty systemInstruction must be omitted")
					}
					if strings.Contains(string(wire["contents"]), "fixture-ok") || strings.Contains(string(wire["contents"]), "官网助手") {
						t.Error("system instruction must not be lowered into user contents")
					}
					if streaming {
						fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\ndata: [DONE]\n\n")
					} else {
						fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
					}
				}))
				defer server.Close()
				p := NewDeepVProvider(&Config{
					Name: "easycode/fixture", URL: server.URL + "/v1/chat/messages",
					Auth: &deepVTestAuth{token: "fixture-token"},
				}, t.TempDir(), "fixture").ConfigureAccountModel([]string{CapabilityText, CapabilityStreaming})
				var req Request
				if err := json.Unmarshal([]byte(`{"model":"easycode/fixture","max_tokens":2048,"messages":[{"role":"user","content":"hello"}]}`), &req); err != nil {
					t.Fatal(err)
				}
				if tc.system != "" {
					var system any
					if err := json.Unmarshal([]byte(tc.system), &system); err != nil {
						t.Fatal(err)
					}
					if err := req.SetRawField("system", system); err != nil {
						t.Fatal(err)
					}
				}
				if streaming {
					var output bytes.Buffer
					if err := p.ForwardStream(context.Background(), &req, &output); err != nil {
						t.Fatal(err)
					}
				} else if _, err := p.ForwardRequest(context.Background(), &req); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
