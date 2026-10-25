package claudeconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeSettings(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(body), 0600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
}

func readEnv(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	env, _ := document["env"].(map[string]any)
	return env
}

func TestTakeoverAndRestoreRoundTrip(t *testing.T) {
	home := t.TempDir()
	writeSettings(t, home, `{
  "modelPicker": {"options": [{"model": "glm-5.3"}]},
  "env": {
    "ANTHROPIC_BASE_URL": "https://origin.example/api",
    "ANTHROPIC_API_KEY": "sk-origin",
    "OTHER": "keep-me"
  }
}`)

	if _, _, err := Takeover(Options{ClaudeHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw", Model: "coding"}); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	env := readEnv(t, home)
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:4001/v1" {
		t.Fatalf("base url = %v", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-gw" {
		t.Fatalf("auth token = %v", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if _, still := env["ANTHROPIC_API_KEY"]; still {
		t.Fatal("接管应移除 ANTHROPIC_API_KEY，避免与 AUTH_TOKEN 抢鉴权")
	}
	if env["ANTHROPIC_MODEL"] != "coding" {
		t.Fatalf("model = %v", env["ANTHROPIC_MODEL"])
	}
	if env["OTHER"] != "keep-me" {
		t.Fatal("接管不得动受管键以外的内容")
	}

	if _, err := Restore(home, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	env = readEnv(t, home)
	if env["ANTHROPIC_BASE_URL"] != "https://origin.example/api" {
		t.Fatalf("restored base url = %v", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_API_KEY"] != "sk-origin" {
		t.Fatalf("restored api key = %v", env["ANTHROPIC_API_KEY"])
	}
	if env["OTHER"] != "keep-me" {
		t.Fatal("还原不得丢非受管键")
	}
	if _, still := env["ANTHROPIC_AUTH_TOKEN"]; still {
		t.Fatal("原本不存在的 AUTH_TOKEN 还原后应被删除")
	}
	if fileExists(snapshotPath(home)) {
		t.Fatal("还原后快照应被删除")
	}
}

func TestTakeoverKeepsFirstSnapshot(t *testing.T) {
	home := t.TempDir()
	writeSettings(t, home, `{"env": {"ANTHROPIC_BASE_URL": "https://origin.example"}}`)

	if _, _, err := Takeover(Options{ClaudeHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw"}); err != nil {
		t.Fatalf("first takeover: %v", err)
	}
	// 换模型再次接管：快照必须保留最初的原值，否则还原会回到网关地址。
	if _, _, err := Takeover(Options{ClaudeHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw", Model: "glm-5.3"}); err != nil {
		t.Fatalf("second takeover: %v", err)
	}
	if _, err := Restore(home, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := readEnv(t, home)["ANTHROPIC_BASE_URL"]; got != "https://origin.example" {
		t.Fatalf("restored base url = %v, want 最初原值", got)
	}
}

func TestRestoreWithoutSnapshotFails(t *testing.T) {
	home := t.TempDir()
	writeSettings(t, home, `{"env": {}}`)
	if _, err := Restore(home, false); err == nil {
		t.Fatal("没有快照时还原应报错，而不是清空配置")
	}
}

func TestTakeoverWithoutExistingSettingsFile(t *testing.T) {
	home := t.TempDir()
	if _, _, err := Takeover(Options{ClaudeHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw"}); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if !fileExists(SettingsPath(home)) {
		t.Fatal("接管应创建 settings.json")
	}
	if _, err := Restore(home, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if fileExists(SettingsPath(home)) {
		t.Fatal("原本没有 settings.json 时，还原应删除该文件")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	writeSettings(t, home, `{"env": {"ANTHROPIC_BASE_URL": "https://origin.example"}}`)
	if _, _, err := Takeover(Options{ClaudeHome: home, Endpoint: "http://x/v1", MasterKey: "sk", DryRun: true}); err != nil {
		t.Fatalf("dry-run takeover: %v", err)
	}
	if got := readEnv(t, home)["ANTHROPIC_BASE_URL"]; got != "https://origin.example" {
		t.Fatalf("dry-run 不应写盘, base url = %v", got)
	}
	if fileExists(snapshotPath(home)) {
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

	writeSettings(t, home, `{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:4001/v1", "ANTHROPIC_AUTH_TOKEN": "sk"}}`)
	if _, _, err := Takeover(Options{ClaudeHome: home, Endpoint: "http://127.0.0.1:4001/v1", MasterKey: "sk-gw", Model: "coding"}); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	status, err = ReadStatus(home)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.FileExists || !status.SnapshotExists || !status.AuthTokenSet {
		t.Fatalf("接管后的状态不完整: %+v", status)
	}
	if status.APIKeySet {
		t.Fatal("接管后不应残留 API key")
	}
	if status.Model != "coding" {
		t.Fatalf("model = %s", status.Model)
	}
}
