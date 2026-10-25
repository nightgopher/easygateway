package ccswitchconfig

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/internal/provider"
	"gopkg.in/yaml.v3"
)

const legacyFixture = `{
  "version": 3,
  "apps": {
    "claude": {
      "current": "p1",
      "providers": {
        "p1": {
          "id": "p1",
          "name": "MyGLM",
          "settingsConfig": {"env": {
            "ANTHROPIC_BASE_URL": "https://glmx.example.com",
            "ANTHROPIC_AUTH_TOKEN": "sk-legacy-claude",
            "ANTHROPIC_DEFAULT_SONNET_MODEL": "glm-4.7",
            "ANTHROPIC_DEFAULT_OPUS_MODEL": "glm-4.9"
          }},
          "category": "custom",
          "meta": {}
        },
        "p2": {
          "id": "p2",
          "name": "Claude Official",
          "settingsConfig": {"env": {}},
          "category": "official",
          "meta": {}
        },
        "p3": {
          "id": "p3",
          "name": "NewAPI Relay",
          "settingsConfig": {"env": {
            "ANTHROPIC_BASE_URL": "https://relay.example.com/v1",
            "ANTHROPIC_AUTH_TOKEN": "sk-legacy-openai-chat",
            "ANTHROPIC_MODEL": "gpt-relay"
          }},
          "category": "custom",
          "meta": {"apiFormat": "openai_chat"}
        }
      }
    },
    "codex": {
      "current": "c1",
      "providers": {
        "c1": {
          "id": "c1",
          "name": "API Relay",
          "settingsConfig": {
            "auth": {"OPENAI_API_KEY": "sk-legacy-codex"},
            "config": "model_provider = \"custom\"\nmodel = \"gpt-relay-2\"\n\n[model_providers.custom]\nname = \"Relay\"\nbase_url = \"https://codex-relay.example.com/v1\"\nwire_api = \"chat\"\n"
          },
          "category": "custom",
          "meta": {}
        },
        "c2": {
          "id": "c2",
          "name": "ChatGPT",
          "settingsConfig": {"auth": {"auth_mode": "chatgpt"}, "config": ""},
          "category": "custom",
          "meta": {}
        }
      }
    },
    "gemini": {
      "current": "g1",
      "providers": {
        "g1": {
          "id": "g1",
          "name": "Google Official",
          "settingsConfig": {"env": {}},
          "category": "official",
          "meta": {}
        }
      }
    }
  }
}`

func writeLegacyFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(legacyFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestScanLegacy(t *testing.T) {
	home := writeLegacyFixture(t)
	scan, err := Scan(home)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if scan.Source != filepath.Join(home, "config.json") {
		t.Fatalf("source = %s", scan.Source)
	}

	byName := map[string]Entry{}
	for _, entry := range scan.Entries {
		byName[entry.AppType+"/"+entry.Name] = entry
	}

	claude := byName["claude/MyGLM"]
	if claude.BaseURL != "https://glmx.example.com" || claude.APIKey != "sk-legacy-claude" {
		t.Fatalf("claude entry = %+v", claude)
	}
	if len(claude.ModelHints) != 2 || claude.ModelHints[0] != "glm-4.7" {
		t.Fatalf("model hints = %v", claude.ModelHints)
	}
	if !claude.IsCurrent {
		t.Fatal("MyGLM should be current")
	}

	official := byName["claude/Claude Official"]
	if official.BaseURL != "" || official.APIKey != "" {
		t.Fatalf("official entry = %+v", official)
	}

	relay := byName["claude/NewAPI Relay"]
	if relay.APIFormat != "openai_chat" {
		t.Fatalf("apiFormat = %q", relay.APIFormat)
	}

	codex := byName["codex/API Relay"]
	if codex.BaseURL != "https://codex-relay.example.com/v1" || codex.WireAPI != "chat" {
		t.Fatalf("codex entry = %+v", codex)
	}
	if len(codex.ModelHints) != 1 || codex.ModelHints[0] != "gpt-relay-2" {
		t.Fatalf("codex hints = %v", codex.ModelHints)
	}
	if codex.APIKey != "sk-legacy-codex" {
		t.Fatalf("codex key missing")
	}

	oauth := byName["codex/ChatGPT"]
	if oauth.AuthMode != "chatgpt" {
		t.Fatalf("oauth auth mode = %q", oauth.AuthMode)
	}
}

// buildFixtureDB 建立与 cc-switch v4 相同的 providers 表并插入样例行。
func buildFixtureDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := `CREATE TABLE providers (
		id TEXT NOT NULL,
		app_type TEXT NOT NULL,
		name TEXT NOT NULL,
		settings_config TEXT NOT NULL,
		website_url TEXT,
		category TEXT,
		created_at INTEGER,
		sort_index INTEGER,
		notes TEXT,
		icon TEXT,
		icon_color TEXT,
		meta TEXT NOT NULL DEFAULT '{}',
		is_current BOOLEAN NOT NULL DEFAULT 0,
		in_failover_queue BOOLEAN NOT NULL DEFAULT 0,
		cost_multiplier TEXT NOT NULL DEFAULT '1.0',
		limit_daily_usd TEXT,
		limit_monthly_usd TEXT,
		provider_type TEXT,
		PRIMARY KEY (id, app_type)
	)`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	insert := func(id, appType, name, settings, category string, isCurrent bool, meta string) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO providers (id, app_type, name, settings_config, category, is_current, meta)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, appType, name, settings, category, isCurrent, meta)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("p1", "claude", "MyGLM",
		`{"env":{"ANTHROPIC_BASE_URL":"https://glmx.example.com","ANTHROPIC_AUTH_TOKEN":"sk-db-claude","ANTHROPIC_DEFAULT_SONNET_MODEL":"glm-4.7"}}`,
		"custom", true, `{}`)
	insert("p3", "claude", "Responses Relay",
		`{"env":{"ANTHROPIC_BASE_URL":"https://resp.example.com","ANTHROPIC_AUTH_TOKEN":"sk-db-resp","ANTHROPIC_MODEL":"resp-model"}}`,
		"custom", false, `{"apiFormat":"openai_responses"}`)
	insert("o1", "openclaw", "GLM-5",
		`{"baseUrl":"https://open.bigmodel.cn/api/coding/paas/v4","apiKey":"sk-db-openclaw","api":"openai-completions","models":[{"id":"glm-5","name":"GLM-5"},{"id":"glm-5-air","name":"GLM-5 Air"}]}`,
		"", false, `{"liveConfigManaged":true}`)
	insert("g1", "gemini", "Google Official", `{"env":{}}`, "official", true, `{}`)
}

func TestScanDB(t *testing.T) {
	home := t.TempDir()
	buildFixtureDB(t, filepath.Join(home, "cc-switch.db"))

	scan, err := Scan(home)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	byName := map[string]Entry{}
	for _, entry := range scan.Entries {
		byName[entry.AppType+"/"+entry.Name] = entry
	}

	claude := byName["claude/MyGLM"]
	if claude.BaseURL != "https://glmx.example.com" || claude.APIKey != "sk-db-claude" || !claude.IsCurrent {
		t.Fatalf("claude entry = %+v", claude)
	}
	resp := byName["claude/Responses Relay"]
	if resp.APIFormat != "openai_responses" {
		t.Fatalf("apiFormat = %q", resp.APIFormat)
	}
	openclaw := byName["openclaw/GLM-5"]
	if openclaw.BaseURL == "" || len(openclaw.ModelHints) != 2 || openclaw.ModelHints[0] != "glm-5" {
		t.Fatalf("openclaw entry = %+v hints=%v", openclaw, openclaw.ModelHints)
	}
}

func TestScanMissing(t *testing.T) {
	_, err := Scan(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("err = %v", err)
	}
}

func TestConvert(t *testing.T) {
	tests := []struct {
		name    string
		entry   Entry
		opts    ConvertOptions
		want    Candidate // 校验 Status/Type/URL/Name/APIKeyEnv/Reason 子集
		wantURL string
	}{
		{
			name:    "claude anthropic 中转",
			entry:   Entry{AppType: "claude", Name: "MyGLM", BaseURL: "https://glmx.example.com", APIKey: "sk", ModelHints: []string{"glm-4.7"}},
			want:    Candidate{Status: "importable", Type: "anthropic", Name: "myglm", APIKeyEnv: "CCS_MYGLM_API_KEY"},
			wantURL: "https://glmx.example.com/v1/messages",
		},
		{
			name:    "claude openai_chat 纯源补 v1",
			entry:   Entry{AppType: "claude", Name: "Relay", APIFormat: "openai_chat", BaseURL: "https://relay.example.com", APIKey: "sk", ModelHints: []string{"gpt-relay"}},
			want:    Candidate{Status: "importable", Type: "openai", Name: "relay", APIKeyEnv: "CCS_RELAY_API_KEY"},
			wantURL: "https://relay.example.com/v1/chat/completions",
		},
		{
			name:    "claude openai_chat 带路径不补 v1",
			entry:   Entry{AppType: "claude", Name: "Relay", APIFormat: "openai_chat", BaseURL: "https://relay.example.com/api/paas/v4/", APIKey: "sk", ModelHints: []string{"gpt-relay"}},
			want:    Candidate{Status: "importable", Type: "openai", Name: "relay", APIKeyEnv: "CCS_RELAY_API_KEY"},
			wantURL: "https://relay.example.com/api/paas/v4/chat/completions",
		},
		{
			name:    "codex wire chat",
			entry:   Entry{AppType: "codex", Name: "Codex Relay", BaseURL: "https://codex.example.com/v1", WireAPI: "chat", APIKey: "sk", ModelHints: []string{"gpt-relay-2"}},
			want:    Candidate{Status: "importable", Type: "openai", Name: "codex-relay", APIKeyEnv: "CCS_CODEX_RELAY_API_KEY"},
			wantURL: "https://codex.example.com/v1/chat/completions",
		},
		{
			name:    "openclaw openai-completions",
			entry:   Entry{AppType: "openclaw", Name: "GLM-5", BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4", APIFormat: "openai-completions", APIKey: "sk", ModelHints: []string{"glm-5", "glm-5-air"}},
			want:    Candidate{Status: "importable", Type: "openai", Name: "glm-5", APIKeyEnv: "CCS_GLM_5_API_KEY"},
			wantURL: "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions",
		},
		{
			name:  "codex responses 不支持",
			entry: Entry{AppType: "codex", Name: "R", BaseURL: "https://x.com", WireAPI: "responses", APIKey: "sk", ModelHints: []string{"m"}},
			want:  Candidate{Status: "skipped"},
		},
		{
			name:  "claude openai_responses 不支持",
			entry: Entry{AppType: "claude", Name: "R", APIFormat: "openai_responses", BaseURL: "https://x.com", APIKey: "sk", ModelHints: []string{"m"}},
			want:  Candidate{Status: "skipped"},
		},
		{
			name:  "codex OAuth 无需迁移",
			entry: Entry{AppType: "codex", Name: "ChatGPT", AuthMode: "chatgpt", BaseURL: "https://x.com", APIKey: "sk", ModelHints: []string{"m"}},
			want:  Candidate{Status: "skipped"},
		},
		{
			name:  "gemini 不支持",
			entry: Entry{AppType: "gemini", Name: "G", BaseURL: "https://x.com", APIKey: "sk", ModelHints: []string{"m"}},
			want:  Candidate{Status: "skipped"},
		},
		{
			name:  "指向本网关",
			entry: Entry{AppType: "claude", Name: "Self", BaseURL: "http://localhost:4001", APIKey: "sk", ModelHints: []string{"m"}},
			opts:  ConvertOptions{GatewayPort: 4001},
			want:  Candidate{Status: "skipped"},
		},
		{
			name:    "本机其他端口服务可导入",
			entry:   Entry{AppType: "claude", Name: "Local NewAPI", BaseURL: "http://127.0.0.1:3000", APIKey: "sk", ModelHints: []string{"m"}},
			opts:    ConvertOptions{GatewayPort: 4001},
			want:    Candidate{Status: "importable", Type: "anthropic", Name: "local-newapi", APIKeyEnv: "CCS_LOCAL_NEWAPI_API_KEY"},
			wantURL: "http://127.0.0.1:3000/v1/messages",
		},
		{
			name:  "无 API Key",
			entry: Entry{AppType: "claude", Name: "R", BaseURL: "https://x.com", ModelHints: []string{"m"}},
			want:  Candidate{Status: "skipped"},
		},
		{
			name:  "无模型提示",
			entry: Entry{AppType: "claude", Name: "R", BaseURL: "https://x.com", APIKey: "sk"},
			want:  Candidate{Status: "skipped"},
		},
		{
			name:  "端点已存在",
			entry: Entry{AppType: "claude", Name: "R", BaseURL: "https://x.com", APIKey: "sk", ModelHints: []string{"m"}},
			opts: ConvertOptions{
				ExistingProviders: map[string]ExistingProvider{"https://x.com/v1/messages": {Name: "x", APIKeyEnv: "CCS_X_API_KEY"}},
				ExistingEnvValues: map[string]string{"CCS_X_API_KEY": "sk"},
			},
			want: Candidate{Status: "skipped"},
		},
		{
			name:    "default 名改用主机名",
			entry:   Entry{AppType: "claude", Name: "default", BaseURL: "https://api.deepseek.com", APIKey: "sk", ModelHints: []string{"deepseek-chat"}},
			want:    Candidate{Status: "importable", Type: "anthropic", Name: "deepseek", APIKeyEnv: "CCS_DEEPSEEK_API_KEY"},
			wantURL: "https://api.deepseek.com/v1/messages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Convert([]Entry{tt.entry}, tt.opts)[0]
			if got.Status != tt.want.Status {
				t.Fatalf("status = %s reason = %s", got.Status, got.Reason)
			}
			if got.Status == "skipped" {
				if got.Reason == "" {
					t.Fatal("skipped candidate needs a reason")
				}
				return
			}
			if got.Type != tt.want.Type || got.URL != tt.wantURL {
				t.Fatalf("type/url = %s %s, want %s %s", got.Type, got.URL, tt.want.Type, tt.wantURL)
			}
			if got.Name != tt.want.Name || got.APIKeyEnv != tt.want.APIKeyEnv {
				t.Fatalf("name/env = %s %s, want %s %s", got.Name, got.APIKeyEnv, tt.want.Name, tt.want.APIKeyEnv)
			}
			if got.secret != "sk" {
				t.Fatal("secret not carried")
			}
			// 密钥不得进入 JSON 序列化。
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "\"sk\"") {
				t.Fatalf("candidate json leaks secret: %s", encoded)
			}
		})
	}
}

func TestConvertDedupesAmongCandidates(t *testing.T) {
	entries := []Entry{
		{AppType: "claude", Name: "A", BaseURL: "https://same.example.com", APIKey: "sk1", ModelHints: []string{"m1"}},
		{AppType: "claude", Name: "B", BaseURL: "https://same.example.com/", APIKey: "sk2", ModelHints: []string{"m2"}},
	}
	candidates := Convert(entries, ConvertOptions{})
	// 同端点第二条视为对第一条的密钥轮换（key-updated），不再重复建 provider。
	if candidates[0].Status != "importable" || candidates[1].Status != "key-updated" {
		t.Fatalf("statuses = %s / %s (%s)", candidates[0].Status, candidates[1].Status, candidates[1].Reason)
	}
	if candidates[1].APIKeyEnv != candidates[0].APIKeyEnv {
		t.Fatalf("key update should reuse the first env var: %s vs %s", candidates[0].APIKeyEnv, candidates[1].APIKeyEnv)
	}
}

func TestConvertRenamesCollisions(t *testing.T) {
	entries := []Entry{
		{AppType: "claude", Name: "relay", BaseURL: "https://one.example.com", APIKey: "sk", ModelHints: []string{"m1"}},
		{AppType: "claude", Name: "Relay", BaseURL: "https://two.example.com", APIKey: "sk", ModelHints: []string{"m2"}},
	}
	candidates := Convert(entries, ConvertOptions{ExistingNames: map[string]bool{"relay": true}})
	if candidates[0].Name != "relay-2" {
		t.Fatalf("first name = %s", candidates[0].Name)
	}
	if candidates[1].Name != "relay-3" {
		t.Fatalf("second name = %s", candidates[1].Name)
	}
}

const existingYAML = `# 手工维护的注释
providers:
  - name: glm
    type: openai
    url: https://open.bigmodel.cn/api/coding/paas/v4/chat/completions
    api_key_env: GLM_API_KEY
    models:
      - id: glm-5.3

chains:
  glm-chain:
    - glm-5.3
`

func TestAppendProvidersYAML(t *testing.T) {
	newProviders := []provider.ProviderConfig{{
		Name:      "myglm",
		Type:      "anthropic",
		URL:       "https://glmx.example.com/v1/messages",
		APIKeyEnv: "CCS_MYGLM_API_KEY",
		Models:    []provider.ModelConfig{{ID: "glm-4.7"}, {ID: "glm-4.9"}},
	}}
	merged := appendProvidersYAML([]byte(existingYAML), "2026-10-17 10:00", newProviders)
	text := string(merged)

	// 既有注释与 chains 必须保留；新条目插在 chains 之前。
	if !strings.Contains(text, "# 手工维护的注释") || !strings.Contains(text, "chains:") {
		t.Fatalf("existing content lost:\n%s", text)
	}
	if strings.Index(text, "name: \"myglm\"") > strings.Index(text, "chains:") {
		t.Fatalf("new provider inserted after chains:\n%s", text)
	}

	// 合并结果必须是合法 providers 配置且包含全部条目。
	var parsed provider.ProvidersConfig
	if err := unmarshalYAML(merged, &parsed); err != nil {
		t.Fatalf("merged yaml invalid: %v\n%s", err, text)
	}
	if len(parsed.Providers) != 2 || parsed.Providers[1].Name != "myglm" {
		t.Fatalf("parsed providers = %+v", parsed.Providers)
	}
	if len(parsed.Chains) != 1 || len(parsed.Chains["glm-chain"]) != 1 {
		t.Fatalf("chains lost: %+v", parsed.Chains)
	}
}

func TestAppendProvidersYAMLEmptyFile(t *testing.T) {
	merged := appendProvidersYAML(nil, "t", []provider.ProviderConfig{{
		Name: "x", Type: "openai", URL: "https://x.example.com/v1/chat/completions", APIKeyEnv: "CCS_X_API_KEY",
		Models: []provider.ModelConfig{{ID: "m"}},
	}})
	if !strings.HasPrefix(string(merged), "providers:") {
		t.Fatalf("merged = %q", merged)
	}
	var parsed provider.ProvidersConfig
	if err := unmarshalYAML(merged, &parsed); err != nil || len(parsed.Providers) != 1 {
		t.Fatalf("parse: %v %+v", err, parsed)
	}
}

func TestAppendProvidersYAMLInlineEmptyList(t *testing.T) {
	existing := []byte("providers: []\nchains:\n  x:\n    - y\n")
	merged := appendProvidersYAML(existing, "t", []provider.ProviderConfig{{
		Name: "x", Type: "openai", URL: "https://x.example.com/v1/chat/completions", APIKeyEnv: "CCS_X_API_KEY",
		Models: []provider.ModelConfig{{ID: "m"}},
	}})
	var parsed provider.ProvidersConfig
	if err := unmarshalYAML(merged, &parsed); err != nil {
		t.Fatalf("merged yaml invalid: %v\n%s", err, merged)
	}
	if len(parsed.Providers) != 1 || len(parsed.Chains) != 1 {
		t.Fatalf("parsed = %+v\n%s", parsed, merged)
	}
}

func TestAppendEnvFile(t *testing.T) {
	existing := []byte("LITELLM_MASTER_KEY=abc\n# comment\nCCS_EXISTING_API_KEY=old\n")
	merged := appendEnvFile(existing, "t", map[string]string{
		"CCS_EXISTING_API_KEY": "new-should-not-overwrite",
		"CCS_NEW_API_KEY":      "sk value with spaces",
	})
	text := string(merged)
	if strings.Contains(text, "new-should-not-overwrite") {
		t.Fatal("existing key overwritten")
	}
	if !strings.Contains(text, "CCS_NEW_API_KEY='sk value with spaces'") {
		t.Fatalf("new key missing or unquoted:\n%s", text)
	}
}

func TestImportEndToEnd(t *testing.T) {
	home := writeLegacyFixture(t)
	gatewayDir := t.TempDir()
	providersYAML := filepath.Join(gatewayDir, "providers.yaml")
	envFile := filepath.Join(gatewayDir, ".env")
	if err := os.WriteFile(providersYAML, []byte(existingYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, []byte("LITELLM_MASTER_KEY=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// dry-run 不写文件。
	dry, err := Import(ImportOptions{
		Home: home, ProvidersYAML: providersYAML, EnvFile: envFile,
		GatewayPort: 4001, DryRun: true,
		Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("dry-run Import: %v", err)
	}
	if dry.Wrote || len(dry.Imported) != 3 {
		t.Fatalf("dry result wrote=%v imported=%v", dry.Wrote, dry.Imported)
	}
	if _, err := os.Stat(providersYAML + ".pre-import.bak"); !os.IsNotExist(err) {
		t.Fatal("dry-run must not create backups")
	}

	result, err := Import(ImportOptions{
		Home: home, ProvidersYAML: providersYAML, EnvFile: envFile, GatewayPort: 4001,
		Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	// 可导入：MyGLM(claude anthropic)、NewAPI Relay(openai_chat)、API Relay(codex chat)。
	if !result.Wrote || len(result.Imported) != 3 {
		names := []string{}
		for _, c := range result.Candidates {
			names = append(names, c.CCName+":"+c.Status)
		}
		t.Fatalf("imported = %v, candidates = %v", result.Imported, names)
	}

	// 密钥进入 .env，providers.yaml 可解析且新增条目齐全。
	envRaw, _ := os.ReadFile(envFile)
	for _, key := range []string{"CCS_MYGLM_API_KEY=sk-legacy-claude", "CCS_NEWAPI_RELAY_API_KEY=sk-legacy-openai-chat", "CCS_API_RELAY_API_KEY=sk-legacy-codex"} {
		if !strings.Contains(string(envRaw), key+"\n") {
			t.Fatalf(".env missing %s:\n%s", key, envRaw)
		}
	}
	cfg, err := provider.LoadProvidersConfig(providersYAML)
	if err != nil {
		t.Fatalf("reparse providers.yaml: %v", err)
	}
	if len(cfg.Providers) != 4 {
		t.Fatalf("providers = %d", len(cfg.Providers))
	}
	for _, backup := range []string{providersYAML + ".pre-import.bak", envFile + ".pre-import.bak"} {
		if _, err := os.Stat(backup); err != nil {
			t.Fatalf("backup missing: %v", err)
		}
	}

	// 二次导入：端点已存在，不重复写入。
	again, err := Import(ImportOptions{Home: home, ProvidersYAML: providersYAML, EnvFile: envFile, GatewayPort: 4001})
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if len(again.Imported) != 0 {
		t.Fatalf("second import duplicated: %v", again.Imported)
	}

	// RegisterLive 后新模型进入运行时路由。
	router := provider.NewRouter(log.New(os.Stderr, "", 0))
	result.RegisterLive(router, log.New(os.Stderr, "", log.LstdFlags))
	names := map[string]bool{}
	for _, name := range router.ListProviders() {
		names[name] = true
	}
	for _, want := range []string{"glm-4.7", "gpt-relay", "gpt-relay-2"} {
		if !names[want] {
			t.Fatalf("router missing %s: %v", want, router.ListProviders())
		}
	}
	os.Unsetenv("CCS_MYGLM_API_KEY")
	os.Unsetenv("CCS_NEWAPI_RELAY_API_KEY")
	os.Unsetenv("CCS_API_RELAY_API_KEY")
}

// fixedNow 让迁移标注时间可预测。
func fixedNow() time.Time { return time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC) }

// unmarshalYAML 是测试用的 yaml 反序列化包装。
func unmarshalYAML(raw []byte, target any) error { return yaml.Unmarshal(raw, target) }

func TestQuoteEnvValue(t *testing.T) {
	if quoteEnvValue("sk-abc.123") != "sk-abc.123" {
		t.Fatal("bare value quoted unnecessarily")
	}
	if quoteEnvValue("a'b") != `'a'\''b'` {
		t.Fatal("single quote not escaped")
	}
}

func TestParseProviderRowsAndLegacyJSON(t *testing.T) {
	rows := []ProviderRow{
		{ID: "p1", AppType: "claude", Name: "MyGLM", IsCurrent: true,
			SettingsConfig: `{"env":{"ANTHROPIC_BASE_URL":"https://glmx.example.com","ANTHROPIC_AUTH_TOKEN":"sk-row","ANTHROPIC_MODEL":"glm-4.7"}}`,
			Meta:           `{}`},
		{ID: "c1", AppType: "codex", Name: "R",
			SettingsConfig: `{"auth":{"OPENAI_API_KEY":"k"},"config":"model_provider = \"x\"\nmodel = \"m\"\n[model_providers.x]\nbase_url = \"https://r.example.com/v1\"\nwire_api = \"chat\"\n"}`},
	}
	entries := ParseProviderRows(rows)
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].BaseURL != "https://glmx.example.com" || entries[0].APIKey != "sk-row" || !entries[0].IsCurrent {
		t.Fatalf("row entry = %+v", entries[0])
	}
	if entries[1].BaseURL != "https://r.example.com/v1" || entries[1].WireAPI != "chat" || len(entries[1].ModelHints) != 1 {
		t.Fatalf("row entry = %+v", entries[1])
	}

	legacyEntries, err := ParseLegacyJSON([]byte(legacyFixture))
	if err != nil {
		t.Fatalf("ParseLegacyJSON: %v", err)
	}
	if len(legacyEntries) == 0 {
		t.Fatal("no legacy entries parsed")
	}
}

func TestImportEntriesFromUploadedRows(t *testing.T) {
	gatewayDir := t.TempDir()
	providersYAML := filepath.Join(gatewayDir, "providers.yaml")
	envFile := filepath.Join(gatewayDir, ".env")
	if err := os.WriteFile(providersYAML, []byte("providers: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rows := []ProviderRow{
		{ID: "p1", AppType: "claude", Name: "MyGLM", IsCurrent: true,
			SettingsConfig: `{"env":{"ANTHROPIC_BASE_URL":"https://glmx.example.com","ANTHROPIC_AUTH_TOKEN":"sk-remote","ANTHROPIC_MODEL":"glm-4.7"}}`,
			Meta:           `{}`},
		{ID: "p2", AppType: "claude", Name: "Loop Relay",
			SettingsConfig: `{"env":{"ANTHROPIC_BASE_URL":"http://localhost:3000","ANTHROPIC_AUTH_TOKEN":"sk-loop","ANTHROPIC_MODEL":"loop-model"}}`,
			Meta:           `{}`},
	}
	result, err := ImportEntries(ImportOptions{
		ProvidersYAML: providersYAML, EnvFile: envFile, GatewayPort: 4001,
	}, "cc-switch cc-switch.db（桌面端上传）", ParseProviderRows(rows))
	if err != nil {
		t.Fatalf("ImportEntries: %v", err)
	}
	if !result.Wrote || len(result.Imported) != 2 {
		names := []string{}
		for _, c := range result.Candidates {
			names = append(names, c.CCName+":"+c.Status)
		}
		t.Fatalf("imported = %v candidates = %v", result.Imported, names)
	}

	// 回环地址条目带 Loopback 标记，供界面提示。
	loop := result.Candidates[1]
	if !loop.Loopback {
		t.Fatalf("loopback flag missing: %+v", loop)
	}

	envRaw, _ := os.ReadFile(envFile)
	if !strings.Contains(string(envRaw), "CCS_MYGLM_API_KEY=sk-remote") {
		t.Fatalf(".env missing uploaded key:\n%s", envRaw)
	}
	if strings.Contains(string(result2JSON(t, result)), "sk-remote") {
		t.Fatal("ImportResult JSON leaks uploaded key")
	}
}

func result2JSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestImportKeyRotation(t *testing.T) {
	home := writeLegacyFixture(t)
	gatewayDir := t.TempDir()
	providersYAML := filepath.Join(gatewayDir, "providers.yaml")
	envFile := filepath.Join(gatewayDir, ".env")
	if err := os.WriteFile(envFile, []byte("LITELLM_MASTER_KEY=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := ImportOptions{Home: home, ProvidersYAML: providersYAML, EnvFile: envFile, GatewayPort: 4001}

	first, err := Import(base)
	if err != nil || !first.Wrote || len(first.Imported) != 3 {
		t.Fatalf("first import: %v imported=%v", err, first.Imported)
	}

	// cc-switch 里 MyGLM 换了 key：改夹具后重导。
	rotated := strings.Replace(legacyFixture, "sk-legacy-claude", "sk-rotated-claude", 1)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Import(base)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if !second.Wrote {
		t.Fatal("key rotation should write .env")
	}
	if len(second.UpdatedKeys) != 1 || second.UpdatedKeys[0] != "CCS_MYGLM_API_KEY" {
		t.Fatalf("updated keys = %v", second.UpdatedKeys)
	}
	if len(second.Imported) != 1 {
		t.Fatalf("rotated provider should be re-registered, imported = %v", second.Imported)
	}

	// .env 中同名变量被覆写，providers.yaml 不新增条目。
	envRaw, _ := os.ReadFile(envFile)
	if !strings.Contains(string(envRaw), "CCS_MYGLM_API_KEY=sk-rotated-claude") || strings.Contains(string(envRaw), "sk-legacy-claude") {
		t.Fatalf(".env not rotated:\n%s", envRaw)
	}
	cfg, err := provider.LoadProvidersConfig(providersYAML)
	if err != nil || len(cfg.Providers) != 3 {
		t.Fatalf("providers.yaml should stay at 3 entries: %v (%d)", err, len(cfg.Providers))
	}

	// 密钥一致的三次导入：无事可做。
	third, err := Import(base)
	if err != nil {
		t.Fatal(err)
	}
	if third.Wrote || len(third.UpdatedKeys) != 0 {
		t.Fatalf("no-op import: wrote=%v updated=%v", third.Wrote, third.UpdatedKeys)
	}

	// 轮换后的 provider 能即时重注册（新密钥生效）。
	os.Setenv("CCS_MYGLM_API_KEY", "sk-rotated-claude")
	router := provider.NewRouter(log.New(os.Stderr, "", 0))
	second.RegisterLive(router, log.New(os.Stderr, "", log.LstdFlags))
	found := false
	for _, name := range router.ListProviders() {
		if name == "glm-4.7" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rotated provider not registered live: %v", router.ListProviders())
	}
	os.Unsetenv("CCS_MYGLM_API_KEY")
}

func TestGuessModelCapabilities(t *testing.T) {
	if guessModelCapabilities("doubao-seed-1-6-vision-250815") == nil {
		t.Fatal("vision model should get vision capability")
	}
	if guessModelCapabilities("glm-4v-flash") == nil {
		t.Fatal("4v model should get vision capability")
	}
	if guessModelCapabilities("qwen3.5-plus") != nil {
		t.Fatal("plain model should use loader defaults")
	}
}
