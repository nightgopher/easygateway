package handlers

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestRelayKitProviderAcrossGatewayEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Contains(payload, []byte(`"messages"`)) {
			t.Errorf("upstream missing messages: %s", payload)
		}
		if bytes.Contains(payload, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"id\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"id\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"id","model":"m","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		}
	}))
	defer upstream.Close()
	logger := log.New(io.Discard, "", 0)
	router := provider.NewRouter(logger)
	router.RegisterProvider("p", provider.NewOpenAIProvider(&provider.Config{Name: "p", URL: upstream.URL}))
	router.RegisterChain("coding", []string{"p"})
	engine := gin.New()
	engine.POST("/v1/messages", NewMessageHandler(router, logger).Handle)
	engine.POST("/v1/chat/completions", NewChatCompletionsHandler(router, logger).Handle)
	engine.POST("/v1/responses", NewResponsesHandler(router, logger).Handle)
	for _, endpoint := range []struct{ path, body string }{
		{"/v1/messages", `{"model":"coding","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"coding","input":"hi"}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(endpoint.path+fmt.Sprint(stream), func(t *testing.T) {
				body := strings.TrimSuffix(endpoint.body, "}") + fmt.Sprintf(`,"stream":%t}`, stream)
				w := httptest.NewRecorder()
				req := httptest.NewRequest("POST", endpoint.path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				engine.ServeHTTP(w, req)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "hello") {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				if stream {
					var terminal string
					switch endpoint.path {
					case "/v1/messages":
						terminal = "event: message_stop"
					case "/v1/chat/completions":
						terminal = "data: [DONE]"
					default:
						terminal = "response.completed"
					}
					if !strings.Contains(w.Body.String(), terminal) {
						t.Fatalf("missing %s: %s", terminal, w.Body.String())
					}
				}
			})
		}
	}
}
