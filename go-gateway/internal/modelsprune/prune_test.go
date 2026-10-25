package modelsprune

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/internal/provider"
)

const sampleYAML = `# 顶部注释，清理不得丢失
cors:
  allowed_origins: []

providers:
  - name: glm
    type: openai
    url: https://open.bigmodel.cn/api/coding/paas/v4/chat/completions
    api_key_env: GLM_API_KEY
    models:
      - id: glm-5.3
        capabilities: [text, tool_calling, streaming]
      # flash 的说明注释，随条目一起移除
      - id: glm-5.3-flash
        capabilities: [text, vision]

  # dead-provider 的说明注释，随 provider 块一起移除
  - name: dead-provider
    type: openai
    url: https://dead.example/v1
    api_key_env: DEAD_KEY
    models:
      - id: gone-model
        capabilities: [text]
      - id: gone-model-2
        capabilities: [text]

chains:
  coding:
    - glm-5.3
    - gone-model
    - glm-5.3-flash
  solo:
    - gone-model-2
`

func TestCandidatesFiltersByDefinitiveRejection(t *testing.T) {
	now := time.Now()
	catalog := []provider.CatalogModel{
		{Info: provider.ModelInfo{ID: "dead", Provider: "p1"}, Health: provider.ModelHealth{
			Status: "unavailable", Available: false, ProbeStatus: provider.ProbeOffline, Reason: "recent upstream probe rejected access (HTTP 404)",
		}},
		{Info: provider.ModelInfo{ID: "flaky", Provider: "p1"}, Health: provider.ModelHealth{
			Status: "degraded", Available: false, ProbeStatus: provider.ProbeDegraded, Reason: "recent upstream probe failed temporarily (HTTP 429)",
		}},
		{Info: provider.ModelInfo{ID: "fresh", Provider: "p1"}, Health: provider.ModelHealth{
			Status: "available", Available: true, ProbeStatus: provider.ProbeOnline, Reason: "recent upstream probe succeeded",
		}},
		{Info: provider.ModelInfo{ID: "unknown", Provider: "p1"}, Health: provider.ModelHealth{
			Status: "unknown", Available: true, Reason: "locally configured; upstream not verified",
		}},
		// 探测过 offline 但已过期（Status 回到 unavailable 之外）不参与清理。
		{Info: provider.ModelInfo{ID: "stale", Provider: "p1"}, Health: provider.ModelHealth{
			Status: "unknown", Available: true, ProbeStatus: provider.ProbeOffline, Reason: "upstream probe expired",
			LastProbeAt: &now,
		}},
	}
	got := Candidates(catalog)
	if len(got) != 1 || got[0].Model != "dead" {
		t.Fatalf("只应选出确定性拒绝的 dead: %+v", got)
	}
	if !strings.Contains(got[0].Reason, "404") {
		t.Fatalf("原因应保留 HTTP 状态: %+v", got[0])
	}
}

func TestRemoveModelsBlockAndCascade(t *testing.T) {
	next, result, err := RemoveModels(sampleYAML, []string{"gone-model", "gone-model-2", "not-in-yaml"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}

	if strings.Contains(next, "gone-model") || strings.Contains(next, "dead-provider") {
		t.Fatalf("被删模型与其空壳 provider 应消失:\n%s", next)
	}
	if !strings.Contains(next, "# 顶部注释，清理不得丢失") || !strings.Contains(next, "- id: glm-5.3\n") || !strings.Contains(next, "glm-5.3-flash") {
		t.Fatalf("无关内容（注释/健康模型）必须原样保留:\n%s", next)
	}
	if len(result.Removed) != 2 {
		t.Fatalf("removed = %+v", result.Removed)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Model != "not-in-yaml" {
		t.Fatalf("未声明的模型应记入 skipped: %+v", result.Skipped)
	}
	if len(result.ProvidersRemoved) != 1 || result.ProvidersRemoved[0] != "dead-provider" {
		t.Fatalf("空壳 provider 应级联移除: %+v", result.ProvidersRemoved)
	}
	// chains 引用清理：coding 剩 2 项，solo 变空链。
	if strings.Count(next, "gone-model") != 0 {
		t.Fatalf("chains 引用未清理:\n%s", next)
	}
	var coding, solo ChainImpact
	for _, impact := range result.ChainsAffected {
		if impact.Chain == "coding" {
			coding = impact
		}
		if impact.Chain == "solo" {
			solo = impact
		}
	}
	if len(coding.Remaining) != 2 || coding.Remaining[0] != "glm-5.3" {
		t.Fatalf("coding 剩余成员不对: %+v", coding)
	}
	if len(solo.Remaining) != 0 {
		t.Fatalf("solo 应为空链: %+v", solo)
	}
	// 清空的链连键行一起移除（空键过不了配置校验，也没有路由意义）。
	if strings.Contains(next, "solo") {
		t.Fatalf("空链 solo 的键行应移除:\n%s", next)
	}
	if !strings.Contains(next, "coding:") {
		t.Fatalf("仍有成员的 coding 链必须保留:\n%s", next)
	}
}

func TestRemoveModelsKeepsProviderWhenSomeModelsRemain(t *testing.T) {
	next, result, err := RemoveModels(sampleYAML, []string{"glm-5.3-flash"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(next, "name: glm") {
		t.Fatalf("glm provider 应保留:\n%s", next)
	}
	if strings.Contains(next, "glm-5.3-flash") {
		t.Fatalf("flash 条目与其 chains 引用都应消失:\n%s", next)
	}
	if len(result.ProvidersRemoved) != 0 {
		t.Fatalf("仍有模型的 provider 不应移除: %+v", result.ProvidersRemoved)
	}
}

func TestRemoveModelsRejectsDuplicateDeclaration(t *testing.T) {
	dup := strings.ReplaceAll(sampleYAML, "- id: gone-model-2", "- id: gone-model")
	if _, _, err := RemoveModels(dup, []string{"gone-model"}); err == nil {
		t.Fatal("重复声明的模型应报错而不是删错块")
	}
}

// 清理后的 YAML 必须能被真实加载器解析，且被删模型不再注册。
func TestPrunedConfigLoads(t *testing.T) {
	next, _, err := RemoveModels(sampleYAML, []string{"gone-model", "gone-model-2"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(next), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := provider.LoadProvidersConfig(path)
	if err != nil {
		t.Fatalf("清理后的配置应能被真实加载器解析: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "glm" {
		t.Fatalf("dead-provider 应被移除: %+v", cfg.Providers)
	}
	if len(cfg.Providers[0].Models) != 2 {
		t.Fatalf("glm 的两个模型应保留: %+v", cfg.Providers[0].Models)
	}
	if len(cfg.Chains["coding"]) != 2 || len(cfg.Chains["solo"]) != 0 {
		t.Fatalf("chains 引用应清理: %+v", cfg.Chains)
	}
}
