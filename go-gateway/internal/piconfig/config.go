package piconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

const ProviderID = "llm-gateway"

const DefaultTokenCommand = "!llm-gateway auth print-master-key"

type SetupOptions struct {
	GatewayHome string
	PiHome      string
	Endpoint    string
	DryRun      bool
	Models      []map[string]any
}

// ModelsFilePath 返回客户端模型清单的实际路径。
// pi-go 改名 easyagent 后目录布局随之变化：旧 ~/.pi 下是 agent/models.json，
// 新 ~/.easyagent 直接用根级 models.json（与 sdk/models.ResolveConfigPath 一致）。
func ModelsFilePath(home string) string {
	if filepath.Base(home) == ".easyagent" {
		return filepath.Join(home, "models.json")
	}
	return filepath.Join(home, "agent", "models.json")
}

func Setup(options SetupOptions) ([]byte, string, error) {
	if options.GatewayHome == "" || options.PiHome == "" {
		return nil, "", fmt.Errorf("gateway and Pi home directories are required")
	}

	endpoint, err := resolveEndpoint(options.Endpoint, options.GatewayHome)
	if err != nil {
		return nil, "", err
	}
	modelsPath := ModelsFilePath(options.PiHome)
	existing, err := os.ReadFile(modelsPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("read Pi models config: %w", err)
	}

	merged, err := mergeModelsConfig(existing, endpoint, DefaultTokenCommand, options.Models)
	if err != nil {
		return nil, "", err
	}
	if options.DryRun {
		return merged, modelsPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(modelsPath), 0700); err != nil {
		return nil, "", fmt.Errorf("create Pi config directory: %w", err)
	}
	if err := os.WriteFile(modelsPath, merged, 0600); err != nil {
		return nil, "", fmt.Errorf("write Pi models config: %w", err)
	}
	return merged, modelsPath, nil
}

func MergeModelsConfig(existing []byte, endpoint, tokenCommand string) ([]byte, error) {
	return mergeModelsConfig(existing, endpoint, tokenCommand, nil)
}

func mergeModelsConfig(existing []byte, endpoint, tokenCommand string, models []map[string]any) ([]byte, error) {
	document := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &document); err != nil {
			return nil, fmt.Errorf("parse Pi models.json: %w", err)
		}
	}

	providers, ok := document["providers"].(map[string]any)
	if !ok || providers == nil {
		providers = map[string]any{}
		document["providers"] = providers
	}

	gateway := gatewayProvider(normalizeEndpoint(endpoint), tokenCommand)
	if models != nil {
		gateway["models"] = models
	}
	providers[ProviderID] = gateway
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Pi models.json: %w", err)
	}
	return append(encoded, '\n'), nil
}

func MasterKey(gatewayHome string) (string, error) {
	values, err := godotenv.Read(filepath.Join(gatewayHome, ".env"))
	if err != nil {
		return "", fmt.Errorf("read gateway .env: %w", err)
	}
	key := strings.TrimSpace(values["EASYGATEWAY_MASTER_KEY"])
	if key == "" {
		key = strings.TrimSpace(values["LITELLM_MASTER_KEY"])
	}
	if key == "" {
		return "", fmt.Errorf("EASYGATEWAY_MASTER_KEY is missing from gateway .env")
	}
	return key, nil
}

// DesiredModels 是 Pi 模型选择器里 llm-gateway 组的精选清单。
// setup pi 子命令与 /admin/pi 端点都以它为唯一数据源。
func DesiredModels() []map[string]any {
	return []map[string]any{
		// 只暴露真实模型名，不再维护第二套别名；coding 是网关内唯一的降级链入口。
		// 智谱侧只保留 glm-5.3 与 glm-5.3-flash（后者兼有图片能力）。
		{"id": "coding", "name": "LLM Gateway Coding"},
		{"id": "glm-5.3", "name": "GLM-5.3"},
		{"id": "glm-5.3-flash", "name": "GLM-5.3 Flash"},
		{"id": "gemini-3.8-flash-high", "name": "Gemini 3.8 Flash"},
		{"id": "deepseek-v4.1-flash", "name": "DeepSeek V4.1 Flash"},
	}
}

func gatewayProvider(endpoint, tokenCommand string) map[string]any {
	return map[string]any{
		"baseUrl":    endpoint,
		"api":        "openai-completions",
		"apiKey":     tokenCommand,
		"authHeader": true,
		"compat": map[string]any{
			"supportsDeveloperRole":    false,
			"supportsReasoningEffort":  false,
			"supportsUsageInStreaming": false,
		},
		"models": DesiredModels(),
	}
}

// ResolveEndpoint 把显式 --endpoint 规范化为网关 v1 地址；未提供时按网关
// .env 的 PORT 推断本机地址。setup pi 与 claude/codex 接管共用。
func ResolveEndpoint(endpoint, gatewayHome string) (string, error) {
	return resolveEndpoint(endpoint, gatewayHome)
}

func resolveEndpoint(endpoint, gatewayHome string) (string, error) {
	if endpoint != "" {
		return normalizeEndpoint(endpoint), nil
	}
	values, err := godotenv.Read(filepath.Join(gatewayHome, ".env"))
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read gateway .env: %w", err)
	}
	port := strings.TrimSpace(values["PORT"])
	if port == "" {
		port = "4001"
	}
	return "http://127.0.0.1:" + port + "/v1", nil
}

func normalizeEndpoint(endpoint string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if !strings.HasSuffix(trimmed, "/v1") {
		trimmed += "/v1"
	}
	return trimmed
}
