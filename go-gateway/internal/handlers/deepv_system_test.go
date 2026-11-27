package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestHandlersDeepVSystemInstruction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name   string
		system string
		texts  []string
	}{
		{name: "absent"},
		{name: "empty string", system: `""`},
		{name: "empty blocks", system: `[]`},
		{name: "string", system: `"Only use confirmed parameters."`, texts: []string{"Only use confirmed parameters."}},
		{name: "text blocks", system: `[{"type":"text","text":"你是官网助手。"},{"type":"text","text":""},{"type":"text","text":"只使用已确认的参数。"}]`, texts: []string{"你是官网助手。", "只使用已确认的参数。"}},
	}
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/messages"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", endpoint, streaming, tc.name), func(t *testing.T) {
					captured := make(chan map[string]json.RawMessage, 1)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						wantPath := "/v1/chat/messages"
						if streaming {
							wantPath = "/v1/chat/stream"
						}
						if r.Method != http.MethodPost || r.URL.Path != wantPath {
							t.Errorf("upstream request = %s %s, want POST %s", r.Method, r.URL.Path, wantPath)
						}
						var wire map[string]json.RawMessage
						if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
							t.Errorf("decode upstream request: %v", err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						captured <- wire
						if streaming {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"fixture-ok\"}]},\"finishReason\":\"STOP\"}]}\n\ndata: [DONE]\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"fixture-ok"}]},"finishReason":"STOP"}]}`)
						}
					}))
					defer upstream.Close()

					logger := log.New(io.Discard, "", 0)
					router := provider.NewRouter(logger)
					p := provider.NewDeepVProvider(&provider.Config{
						Name: "easycode/fixture", URL: upstream.URL + "/v1/chat/messages",
						Auth: provider.NewStaticAuth("fixture-token"),
					}, t.TempDir(), "fixture").ConfigureAccountModel([]string{provider.CapabilityText, provider.CapabilityStreaming})
					router.RegisterProvider(p.Name(), p)
					router.RegisterChain("coding", []string{p.Name()})
					engine := gin.New()
					engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
					engine.POST("/v1/messages", NewMessageHandler(router, logger).Handle)

					messages := `[{"role":"user","content":"hello"}]`
					systemField := ""
					if tc.system != "" {
						if endpoint == "/v1/chat/completions" {
							messages = fmt.Sprintf(`[{"role":"system","content":%s},{"role":"user","content":"hello"}]`, tc.system)
						} else {
							systemField = `,"system":` + tc.system
						}
					}
					body := fmt.Sprintf(`{"model":"coding","max_tokens":2048,"stream":%t,"messages":%s%s}`, streaming, messages, systemField)
					req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					engine.ServeHTTP(response, req)
					if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "fixture-ok") {
						t.Fatalf("response = %d: %s", response.Code, response.Body.String())
					}
					if streaming {
						terminal := "message_stop"
						if endpoint == "/v1/chat/completions" {
							terminal = "data: [DONE]"
						}
						if !strings.Contains(response.Body.String(), terminal) {
							t.Fatalf("missing stream terminal %q: %s", terminal, response.Body.String())
						}
					}

					var wire map[string]json.RawMessage
					select {
					case wire = <-captured:
					default:
						t.Fatal("DeepV upstream was not called")
					}
					if _, present := wire["systemInstruction"]; present {
						t.Error("systemInstruction must not appear at the request root")
					}
					var config map[string]json.RawMessage
					if err := json.Unmarshal(wire["config"], &config); err != nil {
						t.Fatalf("decode config: %v", err)
					}
					system, present := config["systemInstruction"]
					if len(tc.texts) == 0 {
						if present {
							t.Errorf("empty config.systemInstruction must be omitted, got %s", system)
						}
					} else {
						var instruction struct {
							Parts []struct {
								Text string `json:"text"`
							} `json:"parts"`
						}
						if err := json.Unmarshal(system, &instruction); err != nil {
							t.Fatalf("decode config.systemInstruction: %v", err)
						}
						var got []string
						for _, part := range instruction.Parts {
							got = append(got, part.Text)
						}
						want := tc.texts
						if endpoint == "/v1/chat/completions" {
							// Chat Completions concatenates system text blocks before forwarding.
							want = []string{strings.Join(tc.texts, "")}
						}
						if !reflect.DeepEqual(got, want) {
							t.Errorf("config.systemInstruction texts = %#v, want %#v", got, want)
						}
					}

					// Exact contents ensure system text was neither lowered nor duplicated into a user turn.
					var contents []struct {
						Role  string `json:"role"`
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					}
					if err := json.Unmarshal(wire["contents"], &contents); err != nil {
						t.Fatalf("decode contents: %v", err)
					}
					if len(contents) != 1 || contents[0].Role != "user" || len(contents[0].Parts) != 1 || contents[0].Parts[0].Text != "hello" {
						t.Errorf("contents must contain only the original user message, got %s", wire["contents"])
					}
				})
			}
		}
	}
}
