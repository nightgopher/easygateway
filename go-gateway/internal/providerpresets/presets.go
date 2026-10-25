// Package providerpresets 实现常用供应商的快速接入：一条命令把 preset
// 追加进 providers.yaml 并把 API Key 写入 .env，替代「手改 YAML + 手补
// env + 重启」三步。语义对齐 magpie 的 provider add。
//
// providers.yaml 是重注释的手工配置，本包做**文本级插入**而不是解析后
// 重新序列化：新条目插在顶层 chains: 键之前（providers 列表的末尾），
// 其余内容与注释一字不动。模型清单由 --models 显式给出（preset 不内置
// 模型目录，避免维护一份容易过期的清单）；重复的 provider 名直接报错。
package providerpresets

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Preset 是一个内置供应商的接入模板。
type Preset struct {
	Name      string
	URL       string
	Type      string // openai / anthropic
	APIKeyEnv string
	Note      string
}

var registry = map[string]Preset{
	"zhipu": {
		Name:      "zhipu",
		URL:       "https://open.bigmodel.cn/api/paas/v4/chat/completions",
		Type:      "openai",
		APIKeyEnv: "ZHIPU_API_KEY",
		Note:      "智谱 BigModel 开放平台（标准 PaaS 端点）",
	},
	"deepseek": {
		Name:      "deepseek",
		URL:       "https://api.deepseek.com/chat/completions",
		Type:      "openai",
		APIKeyEnv: "DEEPSEEK_API_KEY",
	},
	"moonshot": {
		Name:      "moonshot",
		URL:       "https://api.moonshot.cn/v1/chat/completions",
		Type:      "openai",
		APIKeyEnv: "MOONSHOT_API_KEY",
	},
	"openrouter": {
		Name:      "openrouter",
		URL:       "https://openrouter.ai/api/v1/chat/completions",
		Type:      "openai",
		APIKeyEnv: "OPENROUTER_API_KEY",
	},
	"siliconflow": {
		Name:      "siliconflow",
		URL:       "https://api.siliconflow.cn/v1/chat/completions",
		Type:      "openai",
		APIKeyEnv: "SILICONFLOW_API_KEY",
	},
}

// Lookup 返回指定 preset；不存在时报错并列出全部可选名。
func Lookup(name string) (Preset, error) {
	if preset, ok := registry[name]; ok {
		return preset, nil
	}
	return Preset{}, fmt.Errorf("unknown preset %q; available: %s", name, strings.Join(Names(), ", "))
}

// Names 返回全部 preset 名（字典序）。
func Names() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// All 返回全部 preset（按名排序），供列表命令展示。
func All() []Preset {
	out := make([]Preset, 0, len(registry))
	for _, name := range Names() {
		out = append(out, registry[name])
	}
	return out
}

// Options 描述一次接入动作。
type Options struct {
	Preset Preset
	APIKey string // 可空：allow_no_key 供应商
	Models []string
}

// RenderYAML 生成要插入 providers.yaml 的条目块（带两个空格的列表缩进）。
func RenderYAML(options Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  - name: %s\n", options.Preset.Name)
	fmt.Fprintf(&b, "    type: %s\n", options.Preset.Type)
	fmt.Fprintf(&b, "    url: %s\n", options.Preset.URL)
	fmt.Fprintf(&b, "    api_key_env: %s\n", options.Preset.APIKeyEnv)
	if options.APIKey == "" {
		b.WriteString("    allow_no_key: true\n")
	}
	b.WriteString("    models:\n")
	for _, model := range options.Models {
		fmt.Fprintf(&b, "      - id: %s\n", model)
		b.WriteString("        capabilities: [text, tool_calling, streaming]\n")
	}
	return b.String()
}

var chainsHeader = regexp.MustCompile(`(?m)^chains:\s*$`)

// InsertIntoConfig 把条目块插入 providers.yaml 文本：顶层 chains: 键之前
// （providers 列表末尾）；没有 chains: 时（先保证结尾换行）追加到文件末尾。
// 原有内容与注释一字不动。
func InsertIntoConfig(config, block string) string {
	if location := chainsHeader.FindStringIndex(config); location != nil {
		return config[:location[0]] + block + "\n" + config[location[0]:]
	}
	trimmed := strings.TrimRight(config, "\n")
	if trimmed == "" {
		return "providers:\n" + block
	}
	return trimmed + "\n\n" + block
}

// UpsertEnvFile 在 .env 文本中设置 key=value：已有该变量则原位替换，
// 否则追加到末尾。保留其他行与注释不动。
func UpsertEnvFile(env, key, value string) string {
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=.*$`)
	line := key + "=" + value
	if pattern.MatchString(env) {
		return pattern.ReplaceAllString(env, line)
	}
	trimmed := strings.TrimRight(env, "\n")
	if trimmed == "" {
		return line + "\n"
	}
	return trimmed + "\n" + line + "\n"
}

// HasProvider 报告 providers.yaml 文本里是否已有同名 provider，供重名检查。
func HasProvider(config, name string) bool {
	inProviders := false
	for _, line := range strings.Split(config, "\n") {
		if line == "providers:" {
			inProviders = true
			continue
		}
		if inProviders && !strings.HasPrefix(line, " ") && line != "" {
			inProviders = false
			continue
		}
		if inProviders && strings.HasPrefix(line, "  - name:") {
			if strings.TrimSpace(strings.TrimPrefix(line, "  - name:")) == name {
				return true
			}
		}
	}
	return false
}
