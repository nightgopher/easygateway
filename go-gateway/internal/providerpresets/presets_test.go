package providerpresets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nightgopher/easygateway/internal/provider"
)

const sampleConfig = `# 顶部注释
cors:
  allowed_origins: []

providers:
  - name: glm
    type: openai
    url: https://open.bigmodel.cn/api/coding/paas/v4/chat/completions
    api_key_env: GLM_API_KEY
    # 行内注释必须保留
    models:
      - id: glm-5.3
        capabilities: [text]

chains:
  coding:
    - glm-5.3
`

func TestInsertBeforeChainsKeepsComments(t *testing.T) {
	block := RenderYAML(Options{
		Preset: registry["deepseek"],
		APIKey: "sk-x",
		Models: []string{"deepseek-chat", "deepseek-reasoner"},
	})
	next := InsertIntoConfig(sampleConfig, block)

	if !strings.Contains(next, "# 顶部注释") || !strings.Contains(next, "# 行内注释必须保留") {
		t.Fatal("插入不得动原有注释")
	}
	// 新条目位于 providers 列表末尾、顶层 chains: 之前。
	deepseekAt := strings.Index(next, "  - name: deepseek")
	chainsAt := strings.Index(next, "\nchains:")
	glmAt := strings.Index(next, "- name: glm")
	if deepseekAt < 0 || chainsAt < 0 || !(glmAt < deepseekAt && deepseekAt < chainsAt) {
		t.Fatalf("插入位置错误: glm@%d deepseek@%d chains@%d", glmAt, deepseekAt, chainsAt)
	}
	// 生成的条目形态。
	for _, want := range []string{"    url: https://api.deepseek.com/chat/completions", "    api_key_env: DEEPSEEK_API_KEY", "      - id: deepseek-chat\n        capabilities: [text, tool_calling, streaming]"} {
		if !strings.Contains(next, want) {
			t.Fatalf("缺少 %q:\n%s", want, next)
		}
	}
}

func TestInsertWithoutChainsAppendsAtEnd(t *testing.T) {
	config := "providers:\n  - name: glm\n    models: []\n"
	next := InsertIntoConfig(config, RenderYAML(Options{Preset: registry["moonshot"], APIKey: "k", Models: []string{"kimi"}}))
	if !strings.Contains(next, "- id: kimi") || !strings.HasSuffix(strings.TrimRight(next, "\n"), "capabilities: [text, tool_calling, streaming]") {
		t.Fatalf("无 chains 时应追加到文件末尾:\n%s", next)
	}
}

func TestInsertIntoEmptyConfig(t *testing.T) {
	next := InsertIntoConfig("", RenderYAML(Options{Preset: registry["zhipu"], APIKey: "k", Models: []string{"glm-5.3"}}))
	if !strings.HasPrefix(next, "providers:\n") {
		t.Fatalf("空配置应从 providers: 起始:\n%s", next)
	}
}

func TestAllowNoKeyRenderedWithoutKey(t *testing.T) {
	block := RenderYAML(Options{Preset: registry["openrouter"], Models: []string{"m"}})
	if !strings.Contains(block, "allow_no_key: true") {
		t.Fatalf("无 key 时应渲染 allow_no_key:\n%s", block)
	}
}

func TestUpsertEnvFile(t *testing.T) {
	// 追加。
	next := UpsertEnvFile("EASYGATEWAY_MASTER_KEY=sk-a\n", "DEEPSEEK_API_KEY", "sk-b")
	if !strings.Contains(next, "DEEPSEEK_API_KEY=sk-b") || !strings.Contains(next, "EASYGATEWAY_MASTER_KEY=sk-a") {
		t.Fatalf("追加不得破坏既有行:\n%s", next)
	}
	// 原位替换。
	next = UpsertEnvFile(next, "DEEPSEEK_API_KEY", "sk-rotated")
	if strings.Count(next, "DEEPSEEK_API_KEY=") != 1 || !strings.Contains(next, "DEEPSEEK_API_KEY=sk-rotated") {
		t.Fatalf("替换应原位生效且只有一行:\n%s", next)
	}
	// 空文件。
	next = UpsertEnvFile("", "MOONSHOT_API_KEY", "sk-m")
	if next != "MOONSHOT_API_KEY=sk-m\n" {
		t.Fatalf("空文件应生成单行:\n%q", next)
	}
}

func TestHasProvider(t *testing.T) {
	if !HasProvider(sampleConfig, "glm") {
		t.Fatal("应识别既有 provider")
	}
	if HasProvider(sampleConfig, "deepseek") {
		t.Fatal("不存在的 provider 不应误报")
	}
	// chains: 后的行不得被误认为 provider。
	if HasProvider(sampleConfig+"\nother:\n  - name: fake\n", "fake") {
		t.Fatal("providers 区块外的 - name 不得算 provider")
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, err := Lookup("nope"); err == nil || !strings.Contains(err.Error(), "deepseek") {
		t.Fatalf("未知 preset 应报错并列出可选项: %v", err)
	}
}

// 生成的 YAML 必须能被真实的配置加载器解析，且新 provider/模型完整注册。
func TestRenderedConfigLoads(t *testing.T) {
	next := InsertIntoConfig(sampleConfig, RenderYAML(Options{
		Preset: registry["deepseek"],
		APIKey: "sk-x",
		Models: []string{"deepseek-chat"},
	}))
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(next), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := provider.LoadProvidersConfig(path)
	if err != nil {
		t.Fatalf("生成的配置应能被真实加载器解析: %v", err)
	}
	var found bool
	for _, p := range cfg.Providers {
		if p.Name == "deepseek" {
			found = len(p.Models) == 1 && p.Models[0].ID == "deepseek-chat"
		}
	}
	if !found {
		t.Fatalf("deepseek 未完整注册: %+v", cfg.Providers)
	}
}

// chains: 带行尾注释时同样是有效锚点：漏识别会把条目追加到 chains 之后，
// 破坏 YAML 结构。
func TestChainsWithTrailingComment(t *testing.T) {
	config := "providers:\n  - name: glm\n    models: []\n\nchains:  # 路由链\n  coding:\n    - glm\n"
	next := InsertIntoConfig(config, RenderYAML(Options{Preset: registry["deepseek"], APIKey: "k", Models: []string{"d"}}))
	chainsAt := strings.Index(next, "chains:")
	deepseekAt := strings.Index(next, "- name: deepseek")
	if chainsAt < 0 || deepseekAt < 0 || deepseekAt > chainsAt {
		t.Fatalf("新条目应插在 chains:（含行尾注释）之前:\n%s", next)
	}
}
