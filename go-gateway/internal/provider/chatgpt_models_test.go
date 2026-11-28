package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestRefreshChatGPTModelCatalog(t *testing.T) {
	var gotQuery, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/models" {
			t.Errorf("path = %q, want /codex/models", r.URL.Path)
		}
		gotQuery = r.URL.Query().Get("client_version")
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-7-alpha","display_name":"GPT-7 Alpha"},{"slug":"gpt-6-luna"}]}`))
	}))
	defer server.Close()

	orig := chatgptModelsEndpoint
	chatgptModelsEndpoint = server.URL + "/codex/models?client_version=" + chatgptClientVersion
	t.Cleanup(func() {
		chatgptModelsEndpoint = orig
		setChatGPTModelCatalog(nil)
	})

	authPath := filepath.Join(t.TempDir(), "auth.json")
	writeChatGPTAuthFile(t, authPath, map[string]any{
		"tokens": map[string]any{
			"access_token":  "test-access",
			"refresh_token": "test-refresh",
		},
	})
	provider := NewChatGPTProviderWithAuthPath("", authPath)

	models, err := provider.RefreshModelCatalog(context.Background())
	if err != nil {
		t.Fatalf("RefreshModelCatalog: %v", err)
	}
	if gotQuery == "" {
		t.Error("client_version query param must be sent")
	}
	if gotAuth != "Bearer test-access" {
		t.Errorf("Authorization = %q, want Bearer test-access", gotAuth)
	}
	if len(models) != 3 {
		t.Fatalf("models = %d, want 3", len(models))
	}
	// 已知 slug 沿用 curated 元数据。
	if models[0].ID != "gpt-6.1-sol" || models[0].MaxInputTokens != 272000 {
		t.Errorf("curated entry = %+v", models[0])
	}
	// 未知 slug 用通用元数据兜底。
	if models[1].ID != "gpt-7-alpha" || models[1].Name != "gpt-7-alpha" || models[1].MaxInputTokens != 272000 {
		t.Errorf("generic entry = %+v", models[1])
	}
	// 动态目录已生效。
	if got := ChatGPTModelCatalog(); len(got) != 3 {
		t.Errorf("live catalog = %d models, want 3", len(got))
	}
	// 静态回退目录不受影响。
	if len(chatGPTModelCatalog) == 0 {
		t.Error("fallback catalog must stay untouched")
	}
}

func TestParseCodexModelsVariants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"models 数组对象", `{"models":[{"slug":"a"},{"id":"b"}]}`, []string{"a", "b"}},
		{"data 数组（OpenAI 形状）", `{"data":[{"model":"c"}]}`, []string{"c"}},
		{"顶层数组", `["d","e"]`, []string{"d", "e"}},
		{"字符串条目", `{"models":["f"]}`, []string{"f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCodexModels([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseCodexModels: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("slugs = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("slugs[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseCodexModelsRejectsEmpty(t *testing.T) {
	if _, err := parseCodexModels([]byte(`{"models":[]}`)); err == nil {
		t.Fatal("empty catalog should error")
	}
	_ = json.Valid([]byte(`{}`))
}
