package piconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeModelsConfigPreservesExistingProviders(t *testing.T) {
	existing := []byte(`{"providers":{"existing":{"baseUrl":"https://example.test/v1"}}}`)

	merged, err := MergeModelsConfig(existing, "http://127.0.0.1:4001/v1", "!llm-gateway auth print-master-key")
	if err != nil {
		t.Fatalf("MergeModelsConfig() error = %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal(merged, &document); err != nil {
		t.Fatalf("unmarshal merged config: %v", err)
	}
	providers := document["providers"].(map[string]any)
	if _, ok := providers["existing"]; !ok {
		t.Fatal("existing provider was removed")
	}
	gateway := providers[ProviderID].(map[string]any)
	if gateway["baseUrl"] != "http://127.0.0.1:4001/v1" {
		t.Fatalf("baseUrl = %v", gateway["baseUrl"])
	}
	if gateway["apiKey"] != "!llm-gateway auth print-master-key" {
		t.Fatalf("apiKey = %v", gateway["apiKey"])
	}
	if gateway["authHeader"] != true {
		t.Fatalf("authHeader = %v", gateway["authHeader"])
	}
}

func TestMergeModelsConfigCreatesDocumentAndNormalizesEndpoint(t *testing.T) {
	merged, err := MergeModelsConfig(nil, "http://localhost:4001/", "!gateway auth print-master-key")
	if err != nil {
		t.Fatalf("MergeModelsConfig() error = %v", err)
	}

	var document struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			API     string `json:"api"`
			Models  []struct {
				ID string `json:"id"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(merged, &document); err != nil {
		t.Fatalf("unmarshal merged config: %v", err)
	}
	gateway := document.Providers[ProviderID]
	if gateway.BaseURL != "http://localhost:4001/v1" {
		t.Fatalf("baseUrl = %q", gateway.BaseURL)
	}
	if gateway.API != "openai-completions" {
		t.Fatalf("api = %q", gateway.API)
	}
	if len(gateway.Models) == 0 || gateway.Models[0].ID != "coding" {
		t.Fatalf("models = %#v", gateway.Models)
	}
}

// pi-go 改名 easyagent 后目录布局分化：新 ~/.easyagent 用根级 models.json，
// 旧 ~/.pi 保持 agent/models.json。路径解析必须跟布局走，否则同步会写错位置。
func TestModelsFilePathRespectsDirectoryLayout(t *testing.T) {
	if got := ModelsFilePath("/home/u/.easyagent"); got != filepath.Join("/home/u/.easyagent", "models.json") {
		t.Fatalf("easyagent home path = %q", got)
	}
	if got := ModelsFilePath("/home/u/.pi"); got != filepath.Join("/home/u/.pi", "agent", "models.json") {
		t.Fatalf("legacy pi home path = %q", got)
	}
}

func TestSetupWritesEasyAgentRootModelsFile(t *testing.T) {
	gatewayHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(gatewayHome, ".env"), []byte("LITELLM_MASTER_KEY=sk-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	easyagentHome := filepath.Join(home, ".easyagent")
	if err := os.MkdirAll(easyagentHome, 0700); err != nil {
		t.Fatal(err)
	}
	merged, path, err := Setup(SetupOptions{GatewayHome: gatewayHome, PiHome: easyagentHome})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if path != filepath.Join(easyagentHome, "models.json") {
		t.Fatalf("path = %q", path)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written models.json: %v", err)
	}
	if string(written) != string(merged) {
		t.Fatal("written content mismatch")
	}
}
