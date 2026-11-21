package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/provider"
)

func TestModelCatalogHealthAndAvailableOnlyFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := log.New(io.Discard, "", 0)

	// 探测上游：restricted 模型返回 400（probe 失败 → 健康标注不可用），ready 正常。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "jev-1.13-free" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"fixture model unavailable"}}`)
			return
		}
		io.WriteString(w, `{"id":"test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	body := fmt.Sprintf("providers:\n - name: restricted\n   type: openai\n   url: %s\n   allow_no_key: true\n   models: [{id: jev-1.13-free}]\n - name: ready\n   type: openai\n   url: %s\n   allow_no_key: true\n   models: [{id: ready}]\n", upstream.URL, upstream.URL)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	r := provider.NewRouter(logger)
	if _, err := provider.SetupProvidersFromConfig(r, path, logger); err != nil {
		t.Fatal(err)
	}
	// 健康标注来自 probe 体系：只有跑过一次带探测的 Doctor，模型才有非 unknown 的健康状态。
	if _, err := r.Doctor(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	e := gin.New()
	e.GET("/v1/models", NewModelHandler(r, logger).Handle)
	for _, tt := range []struct {
		query string
		want  int
	}{{"", 2}, {"?available_only=true", 1}} {
		w := httptest.NewRecorder()
		e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models"+tt.query, nil))
		var got struct {
			Data []struct {
				ID     string               `json:"id"`
				Health provider.ModelHealth `json:"gateway_status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Data) != tt.want {
			t.Fatalf("catalog %s: %s", tt.query, w.Body.String())
		}
		for _, m := range got.Data {
			if m.Health.CheckedAt.IsZero() || m.Health.Status == "" {
				t.Fatal("health annotation missing")
			}
			if tt.query != "" && !m.Health.Available {
				t.Fatal("unavailable model returned")
			}
		}
	}
}
