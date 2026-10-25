package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/auth"
	"github.com/nightgopher/easygateway/internal/provider"
	"github.com/nightgopher/easygateway/internal/requestmeta"
)

const systemOneTestRequest = `{"model":"jev-alias","state":{"ticket":"payment failed"},"questions":{"urgent":{"type":"noul","instructions":"Urgent?"},"team":{"type":"choice","instructions":{"question":"Which team?"},"criteria":{"billing":{"description":"payments"},"technical":"software"}},"severity":{"type":"score","instructions":"Severity?","criteria":["low","medium","high"]}},"metadata":{"trace":"preserved"}}`

func systemOneTestRouter(t *testing.T, upstream http.HandlerFunc, key string, anonymous bool) (*gin.Engine, *provider.Router) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	r := provider.NewRouter(log.New(io.Discard, "", 0))
	registry := provider.NewProviderRegistry()
	t.Setenv("SYSTEMONE_TEST_KEY", key)
	cfg := provider.ProviderConfig{Name: "systemone-test", Type: "systemone", URL: server.URL + "/inference/systemone/v1/systemone", APIKeyEnv: "SYSTEMONE_TEST_KEY", AllowNoKey: anonymous, Models: []provider.ModelConfig{{ID: "jev-1.13-free", Aliases: []string{"jev-alias"}, Capabilities: []string{provider.CapabilitySystemOne}}}}
	if _, err := registry.Create(&cfg); err != nil {
		t.Fatal(err)
	}
	provider.RegisterProvidersFromConfig(r, &provider.ProvidersConfig{Providers: []provider.ProviderConfig{cfg}}, log.New(io.Discard, "", 0))
	engine := gin.New()
	engine.Use(auth.BearerAuth("gateway-test-key", log.New(io.Discard, "", 0)))
	engine.POST("/v1/systemone", NewSystemOneHandler(r).Handle)
	return engine, r
}

func systemOneTestCall(engine http.Handler, body string, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func TestSystemOneNativeRoundTripAndMetadata(t *testing.T) {
	const result = `{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.93},"team":{"type":"choice","choice":"billing","confidence":0,"probabilities":{"billing":0.7,"technical":0.3}},"severity":{"type":"score","score":1.43,"legend":{"0":"low","1":"medium","2":"high"},"probabilities":{"1":0.57,"2":0.43}}},"usage":{"input_tokens":21,"output_tokens":4},"extension":{"kept":true}}`
	var calls atomic.Int32
	engine, r := systemOneTestRouter(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/inference/systemone/v1/systemone" {
			t.Errorf("wrong path: %s", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer workspace-test-key" {
			t.Errorf("wrong upstream auth: %q", got)
		}
		var actual, expected map[string]any
		_ = json.NewDecoder(req.Body).Decode(&actual)
		_ = json.Unmarshal([]byte(systemOneTestRequest), &expected)
		expected["model"] = "jev-1.13-free"
		a, _ := json.Marshal(actual)
		b, _ := json.Marshal(expected)
		if !bytes.Equal(a, b) {
			t.Errorf("native body changed: %s", a)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "upstream-test-id")
		_, _ = io.WriteString(w, result)
	}, "workspace-test-key", false)
	engine.Use(func(c *gin.Context) { c.Next() })
	var input, output int
	// Wrap the same handler to capture metadata after it completes.
	engine.POST("/inspect", func(c *gin.Context) {
		NewSystemOneHandler(r).Handle(c)
		input = c.GetInt(requestmeta.InputTokensKey)
		output = c.GetInt(requestmeta.OutputTokensKey)
	})
	req := httptest.NewRequest(http.MethodPost, "/inspect", strings.NewReader(systemOneTestRequest))
	req.Header.Set("Authorization", "Bearer gateway-test-key")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != result || calls.Load() != 1 {
		t.Fatalf("response=%d %s calls=%d", w.Code, w.Body.String(), calls.Load())
	}
	if input != 21 || output != 4 || w.Header().Get("X-Upstream-Request-ID") != "upstream-test-id" {
		t.Fatalf("metadata input=%d output=%d headers=%v", input, output, w.Header())
	}
	for _, model := range r.ListModelInfos() {
		if model.Protocol != "systemone" || len(model.Capabilities) != 1 || model.Capabilities[0] != provider.CapabilitySystemOne {
			t.Fatalf("bad model metadata: %+v", model)
		}
	}
	_, err := r.RouteForRequest("jev-alias", &provider.Request{Model: "jev-alias", Stream: true})
	var unsupported *provider.UnsupportedCapabilityError
	if !errors.As(err, &unsupported) || !strings.Contains(err.Error(), "/v1/systemone") {
		t.Fatalf("chat should fail with native endpoint guidance: %v", err)
	}
}

func TestSystemOneAnonymousQuotaAndSecretRedaction(t *testing.T) {
	for _, key := range []string{"", "workspace-secret-for-test"} {
		t.Run(map[bool]string{true: "anonymous", false: "keyed"}[key == ""], func(t *testing.T) {
			var calls atomic.Int32
			engine, _ := systemOneTestRouter(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if key == "" && req.Header.Get("Authorization") != "" {
					t.Error("anonymous requests must omit Authorization")
				}
				w.Header().Set("Retry-After", "60")
				w.Header().Set("X-Request-ID", "quota-id")
				w.WriteHeader(429)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "FreeUsageLimitError", "message": "quota exhausted " + key}})
			}, key, true)
			w := systemOneTestCall(engine, systemOneTestRequest, "gateway-test-key")
			if w.Code != 429 || !strings.Contains(w.Body.String(), "FreeUsageLimitError") || w.Header().Get("Retry-After") != "60" || calls.Load() != 1 {
				t.Fatalf("quota result: %d %s", w.Code, w.Body.String())
			}
			if key != "" && strings.Contains(w.Body.String(), key) {
				t.Fatal("upstream echoed the workspace credential")
			}
		})
	}
}

func TestSystemOnePaidModelRemainsVisibleWithoutKey(t *testing.T) {
	var calls atomic.Int32
	_, r := systemOneTestRouter(t, func(w http.ResponseWriter, req *http.Request) { calls.Add(1) }, "", false)
	if len(r.ListModelInfos()) != 2 {
		t.Fatal("unconfigured paid models disappeared from catalog")
	}
	if health := r.ModelHealths()["jev-alias"]; health.Available || !strings.Contains(health.Reason, "API Key") {
		t.Fatalf("missing setup guidance: %+v", health)
	}
	if _, err := r.Route("jev-alias"); err == nil {
		t.Fatal("paid request routed without a key")
	}
	if _, err := r.RouteForRequest("jev-alias", &provider.Request{Model: "jev-alias"}); !provider.IsUnsupportedCapability(err) {
		t.Fatalf("wrong protocol should fail before availability: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("unconfigured paid model called upstream")
	}
}

func TestSystemOneValidationAndAuthDoNotCallUpstream(t *testing.T) {
	var calls atomic.Int32
	engine, _ := systemOneTestRouter(t, func(w http.ResponseWriter, req *http.Request) { calls.Add(1) }, "", true)
	for _, body := range []string{`{}`, `null`, `{"model":"jev-alias","state":"x","questions":{}}`, `{"model":"jev-alias","state":"x","questions":{"q":{"type":"choice","instructions":"x","criteria":[]}}}`, `{"model":"jev-alias","state":"x","questions":{"q":{"type":"score","instructions":"x","criteria":{}}}}`, strings.Replace(systemOneTestRequest, `"metadata":`, `"stream":true,"metadata":`, 1), strings.Replace(systemOneTestRequest, `"metadata":`, `"messages":[],"metadata":`, 1)} {
		if w := systemOneTestCall(engine, body, "gateway-test-key"); w.Code != 400 {
			t.Fatalf("invalid body returned %d: %s", w.Code, body)
		}
	}
	if w := systemOneTestCall(engine, systemOneTestRequest, ""); w.Code != 401 {
		t.Fatalf("missing gateway auth: %d", w.Code)
	}
	if w := systemOneTestCall(engine, strings.Repeat("x", (4<<20)+1), "gateway-test-key"); w.Code != 413 {
		t.Fatalf("oversize body: %d", w.Code)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid requests reached upstream")
	}
}

func TestSystemOneProbeAndCancellation(t *testing.T) {
	var calls atomic.Int32
	engine, r := systemOneTestRouter(t, func(w http.ResponseWriter, req *http.Request) { calls.Add(1); <-req.Context().Done() }, "", true)
	if report, err := r.Doctor(context.Background(), true); err != nil || report.Probed != 0 || calls.Load() != 0 {
		t.Fatalf("generic doctor must not send chat probes to System One: %+v %v", report, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(systemOneTestRequest)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer gateway-test-key")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	if w.Code != 499 {
		t.Fatalf("canceled request: %d %s", w.Code, w.Body.String())
	}
}
