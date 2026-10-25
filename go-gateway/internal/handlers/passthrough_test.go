package handlers

import (
	"bytes"
	"context"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

// fakeUpstreamProvider 指向本地 httptest 上游的假 Provider。
type fakeUpstreamProvider struct {
	name   string
	url    string
	apiKey string
}

func (p *fakeUpstreamProvider) Name() string    { return p.name }
func (p *fakeUpstreamProvider) URL() string     { return p.url }
func (p *fakeUpstreamProvider) APIKey() string  { return p.apiKey }
func (p *fakeUpstreamProvider) UseBearer() bool { return true }
func (p *fakeUpstreamProvider) IsHealthy(ctx context.Context) bool {
	return true
}
func (p *fakeUpstreamProvider) ForwardRequest(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	return nil, nil
}

func newPassthroughTestRouter(t *testing.T, upstreamURL string) *provider.Router {
	t.Helper()
	router := provider.NewRouter(log.New(io.Discard, "", 0))
	router.RegisterProvider("siliconflow", &fakeUpstreamProvider{name: "siliconflow", url: upstreamURL, apiKey: "sk-test"})
	router.RegisterChain("BAAI/bge-m3", []string{"siliconflow"})
	router.RegisterChain("TeleAI/TeleSpeech-ASR1.0", []string{"siliconflow"})
	return router
}

func newPassthroughEngine(router *provider.Router) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	h := NewPassthroughHandler(router, log.New(io.Discard, "", 0))
	engine.POST("/v1/embeddings", h.HandleEmbeddings)
	engine.POST("/v1/audio/transcriptions", h.HandleTranscriptions)
	return engine
}

func TestPassthroughEmbeddings(t *testing.T) {
	var (
		gotAuth   string
		gotCT     string
		gotBody   string
		gotStatus = http.StatusInternalServerError
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotStatus = http.StatusOK
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2]}]}`))
	}))
	defer upstream.Close()

	engine := newPassthroughEngine(newPassthroughTestRouter(t, upstream.URL))
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings",
		strings.NewReader(`{"model":"BAAI/bge-m3","input":"你好"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody != `{"model":"BAAI/bge-m3","input":"你好"}` {
		t.Errorf("upstream body = %q", gotBody)
	}
	if !strings.Contains(w.Body.String(), `"embedding"`) {
		t.Errorf("response passthrough = %q", w.Body.String())
	}
	_ = gotStatus
}

func TestPassthroughTranscriptions(t *testing.T) {
	var gotCT, gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		// 用同一 boundary 解析回 model 字段，验证 multipart 原样到达
		if mr, err := r.MultipartReader(); err == nil {
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				if part.FormName() == "model" {
					b, _ := io.ReadAll(part)
					gotModel = string(b)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"识别文本"}`))
	}))
	defer upstream.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "TeleAI/TeleSpeech-ASR1.0")
	part, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = part.Write([]byte("fake-wav-bytes"))
	_ = mw.Close()

	engine := newPassthroughEngine(newPassthroughTestRouter(t, upstream.URL))
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if gotModel != "TeleAI/TeleSpeech-ASR1.0" {
		t.Errorf("upstream model = %q", gotModel)
	}
	if !strings.Contains(gotCT, "multipart/form-data") || !strings.Contains(gotCT, "boundary=") {
		t.Errorf("upstream Content-Type = %q, want multipart with boundary", gotCT)
	}
	if !strings.Contains(w.Body.String(), "识别文本") {
		t.Errorf("response = %q", w.Body.String())
	}
}

func TestPassthroughErrors(t *testing.T) {
	engine := newPassthroughEngine(newPassthroughTestRouter(t, "http://127.0.0.1:1"))

	cases := []struct {
		name string
		path string
		body string
		ct   string
		want int
	}{
		{"unknown model", "/v1/embeddings", `{"model":"nope","input":"x"}`, "application/json", http.StatusNotFound},
		{"missing model", "/v1/embeddings", `{"input":"x"}`, "application/json", http.StatusBadRequest},
		{"empty body", "/v1/embeddings", ``, "application/json", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ct)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
