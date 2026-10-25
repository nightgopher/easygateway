package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

const originalConfig = `# 我自己的注释，还原时必须一字不差
model = "gpt-6.1"
model_provider = "openai"

[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"
`

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestTakeoverAndRestoreRoundTrip(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, originalConfig)

	if _, _, err := Takeover(Options{CodexHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw", Model: "coding"}); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	raw, err := os.ReadFile(ConfigPath(home))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var document map[string]any
	if err := toml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse written config: %v", err)
	}
	if document["model_provider"] != ProviderID || document["model"] != "coding" {
		t.Fatalf("provider/model = %v/%v", document["model_provider"], document["model"])
	}
	provider := document["model_providers"].(map[string]any)[ProviderID].(map[string]any)
	if provider["base_url"] != "http://127.0.0.1:4001/v1" || provider["wire_api"] != "responses" {
		t.Fatalf("provider block = %v", provider)
	}
	if provider["experimental_bearer_token"] != "sk-gw" {
		t.Fatalf("bearer token = %v", provider["experimental_bearer_token"])
	}
	// 原有 provider 块保留，用户切换回去不需要重配。
	if document["model_providers"].(map[string]any)["openai"] == nil {
		t.Fatal("接管不应删除既有的 model_providers 条目")
	}

	if _, err := Restore(home, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	raw, err = os.ReadFile(ConfigPath(home))
	if err != nil {
		t.Fatalf("read restored config: %v", err)
	}
	if string(raw) != originalConfig {
		t.Fatalf("还原必须是字节级原文（含注释），got:\n%s", raw)
	}
	if fileExists(backupPath(home)) {
		t.Fatal("还原后快照应被删除")
	}
}

func TestTakeoverKeepsFirstSnapshot(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, originalConfig)
	for _, model := range []string{"coding", "glm-5.3"} {
		if _, _, err := Takeover(Options{CodexHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw", Model: model}); err != nil {
			t.Fatalf("takeover %s: %v", model, err)
		}
	}
	if _, err := Restore(home, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	raw, _ := os.ReadFile(ConfigPath(home))
	if !strings.Contains(string(raw), "# 我自己的注释") {
		t.Fatal("重复接管不得覆盖首次快照，还原应回到最初原文")
	}
}

func TestTakeoverWithoutExistingConfig(t *testing.T) {
	home := t.TempDir()
	content, path, err := Takeover(Options{CodexHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw"})
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if !fileExists(path) {
		t.Fatal("接管应创建 config.toml")
	}
	var written map[string]any
	if err := toml.Unmarshal(content, &written); err != nil {
		t.Fatalf("parse written config: %v", err)
	}
	if written["model"] != DefaultModel {
		t.Fatalf("未指定模型时应用默认模型 %s, got %v", DefaultModel, written["model"])
	}
	if _, err := Restore(home, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if fileExists(ConfigPath(home)) {
		t.Fatal("原本没有 config.toml 时，还原应删除该文件")
	}
}

func TestRestoreWithoutSnapshotFails(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, originalConfig)
	if _, err := Restore(home, false); err == nil {
		t.Fatal("没有快照时还原应报错")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, originalConfig)
	if _, _, err := Takeover(Options{CodexHome: home, Endpoint: "http://x/v1", MasterKey: "sk", DryRun: true}); err != nil {
		t.Fatalf("dry-run takeover: %v", err)
	}
	raw, _ := os.ReadFile(ConfigPath(home))
	if string(raw) != originalConfig {
		t.Fatal("dry-run 不应写盘")
	}
	if fileExists(backupPath(home)) {
		t.Fatal("dry-run 不应写快照")
	}
}

func TestReadStatus(t *testing.T) {
	home := t.TempDir()
	status, err := ReadStatus(home)
	if err != nil {
		t.Fatalf("status on empty home: %v", err)
	}
	if status.FileExists || status.SnapshotExists {
		t.Fatalf("空目录应视为未接管: %+v", status)
	}

	if _, _, err := Takeover(Options{CodexHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw", Model: "coding"}); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	status, err = ReadStatus(home)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.ModelProvider != ProviderID || status.Model != "coding" {
		t.Fatalf("provider/model = %s/%s", status.ModelProvider, status.Model)
	}
	if status.BaseURL != "http://127.0.0.1:4001/v1" || !status.SnapshotExists {
		t.Fatalf("状态不完整: %+v", status)
	}
}
