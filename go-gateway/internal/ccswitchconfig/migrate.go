package ccswitchconfig

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nightgopher/easygateway/internal/provider"
)

// Candidate 是一条 cc-switch 条目转换后的结果。Status 为 importable 表示
// 可以写入网关；skipped 附带原因。API Key 只放在未导出的 secret 字段，
// 永不出现在 JSON 序列化结果里。
type Candidate struct {
	CCName    string   `json:"cc_name"`     // cc-switch 里的原始条目名
	Name      string   `json:"name"`        // 网关 provider 名（冲突时自动改名）
	AppType   string   `json:"app_type"`    // claude / codex / openclaw / ...
	Type      string   `json:"type"`        // 网关 provider 类型：anthropic / openai
	BaseURL   string   `json:"base_url"`    // cc-switch 里的上游 base URL
	URL       string   `json:"url"`         // 网关实际请求的完整端点
	APIKeyEnv string   `json:"api_key_env"` // 写入 .env 的密钥变量名
	Models    []string `json:"models"`
	IsCurrent bool     `json:"is_current"`
	// Loopback 为 true 表示上游 base URL 是回环地址。本机迁移时它通常是
	// 同一台机器上的服务；桌面端远程上传时，回环地址相对网关主机才有效，
	// 界面据此提示。
	Loopback bool   `json:"loopback"`
	Status   string `json:"status"` // importable / skipped
	Reason   string `json:"reason,omitempty"`

	secret string
}

// Secret 返回该条目的 API Key，仅供写入 .env 与进程内注册使用。
func (c *Candidate) Secret() string { return c.secret }

// ConvertOptions 提供转换所需的网关现状（用于排除自指、去重与命名冲突）。
type ConvertOptions struct {
	GatewayPort       int                         // 网关自身端口，指向它的条目不导入
	ExistingProviders map[string]ExistingProvider // 规范化端点 → 已有 provider（去重与密钥比对）
	ExistingNames     map[string]bool             // 已有 provider 名
	ExistingEnvKeys   map[string]bool             // 已被占用的密钥变量名
	ExistingEnvValues map[string]string           // .env 已有变量的当前值（检测密钥变化）
}

// ExistingProvider 描述 providers.yaml 里已存在的一个 provider。
type ExistingProvider struct {
	Name      string
	APIKeyEnv string
}

// LoadContext 读取网关现有的 providers.yaml 与 .env，构建转换去重上下文。
// providers.yaml 不存在视为空配置；存在但解析失败返回错误（避免盲目追加
// 破坏一份手工维护的配置）。
func LoadContext(providersYAML, envFile string, gatewayPort int) (ConvertOptions, error) {
	opts := ConvertOptions{
		GatewayPort:       gatewayPort,
		ExistingProviders: map[string]ExistingProvider{},
		ExistingNames:     map[string]bool{},
		ExistingEnvKeys:   map[string]bool{},
		ExistingEnvValues: map[string]string{},
	}

	if fileExists(providersYAML) {
		cfg, err := provider.LoadProvidersConfig(providersYAML)
		if err != nil {
			return opts, fmt.Errorf("解析 %s: %w", providersYAML, err)
		}
		for _, p := range cfg.Providers {
			if p.URL != "" {
				opts.ExistingProviders[NormalizeEndpoint(p.URL)] = ExistingProvider{Name: p.Name, APIKeyEnv: p.APIKeyEnv}
			}
			opts.ExistingNames[p.Name] = true
			if p.APIKeyEnv != "" {
				opts.ExistingEnvKeys[p.APIKeyEnv] = true
			}
		}
	}
	for key, value := range parseEnvKeys(readFileOrDefault(envFile)) {
		opts.ExistingEnvKeys[key] = true
		opts.ExistingEnvValues[key] = value
	}
	return opts, nil
}

// Convert 把扫描到的条目转换为网关 provider 候选。不能迁移的条目保留在
// 结果中并标注 skipped 原因，便于在面板与 CLI 里完整呈现。
func Convert(entries []Entry, opts ConvertOptions) []Candidate {
	if opts.ExistingNames == nil {
		opts.ExistingNames = map[string]bool{}
	}
	if opts.ExistingEnvKeys == nil {
		opts.ExistingEnvKeys = map[string]bool{}
	}
	if opts.ExistingEnvValues == nil {
		opts.ExistingEnvValues = map[string]string{}
	}
	if opts.ExistingProviders == nil {
		opts.ExistingProviders = map[string]ExistingProvider{}
	}
	usedNames := make(map[string]bool, len(opts.ExistingNames))
	for name := range opts.ExistingNames {
		usedNames[name] = true
	}
	usedEnvKeys := make(map[string]bool, len(opts.ExistingEnvKeys))
	for key := range opts.ExistingEnvKeys {
		usedEnvKeys[key] = true
	}
	seen := make(map[string]ExistingProvider, len(opts.ExistingProviders))
	for url, existing := range opts.ExistingProviders {
		seen[url] = existing
	}

	candidates := make([]Candidate, 0, len(entries))
	for _, entry := range entries {
		candidates = append(candidates, convertEntry(entry, opts, usedNames, usedEnvKeys, seen))
	}
	return candidates
}

func convertEntry(entry Entry, opts ConvertOptions, usedNames, usedEnvKeys map[string]bool, seen map[string]ExistingProvider) Candidate {
	c := Candidate{
		CCName:    entry.Name,
		AppType:   entry.AppType,
		BaseURL:   entry.BaseURL,
		IsCurrent: entry.IsCurrent,
		secret:    entry.APIKey,
	}
	skip := func(reason string) Candidate {
		c.Status = "skipped"
		c.Reason = reason
		return c
	}

	if entry.ParseError != "" {
		return skip("条目配置损坏：" + entry.ParseError)
	}
	switch entry.AppType {
	case "claude", "codex", "openclaw", "gemini":
	default:
		return skip("暂不支持 " + entry.AppType + " 应用的配置格式")
	}
	if entry.BaseURL == "" {
		return skip("没有 base URL（官方登录或占位条目）")
	}
	if entry.APIKey == "" {
		return skip("没有 API Key")
	}
	c.Loopback = isLoopbackBaseURL(entry.BaseURL)
	if self, port := pointsAtGateway(entry.BaseURL, opts.GatewayPort); self {
		return skip(fmt.Sprintf("指向本网关自身（%s）", port))
	}

	// 上游协议 → 网关 provider 类型与完整端点。
	apiFormat := entry.APIFormat
	if entry.AppType == "openclaw" {
		// openclaw 的取值：openai-completions / anthropic-messages。
		switch {
		case apiFormat == "" || strings.Contains(apiFormat, "openai"):
			apiFormat = "openai_chat"
		case strings.Contains(apiFormat, "anthropic"):
			apiFormat = "anthropic"
		}
	}
	if entry.AppType == "codex" {
		if entry.AuthMode == "chatgpt" || entry.RequiresOpenAIAuth {
			return skip("OAuth 订阅凭据：网关 chatgpt 渠道直接读取 ~/.codex/auth.json，无需迁移")
		}
		if entry.WireAPI == "responses" {
			return skip("上游为 Responses API 端点（API-Key 直连），网关暂不支持")
		}
		// codex 的 wire_api=chat/缺省 都是 chat/completions 端点。
		apiFormat = "openai_chat"
	}
	switch apiFormat {
	case "", "anthropic":
		c.Type = "anthropic"
		c.URL = anthropicMessagesEndpoint(entry.BaseURL)
	case "openai_chat":
		c.Type = "openai"
		c.URL = openAIChatEndpoint(entry.BaseURL)
	case "openai_responses":
		return skip("上游为 Responses API 端点（API-Key 直连），网关暂不支持")
	default:
		// claude 条目 meta.apiFormat 的未知取值按 anthropic 对待。
		c.Type = "anthropic"
		c.URL = anthropicMessagesEndpoint(entry.BaseURL)
	}
	if entry.AppType == "gemini" {
		return skip("网关暂无原生 Gemini 协议 provider")
	}

	normalized := NormalizeEndpoint(c.URL)
	if existing, dup := seen[normalized]; dup {
		// 端点已存在：cc-switch 里的密钥若有变化，转为 key-updated 而不是
		// 直接跳过，让 Import 更新 .env 并重注册（密钥一致才真正跳过）。
		if existing.APIKeyEnv != "" && entry.APIKey != "" && entry.APIKey != opts.ExistingEnvValues[existing.APIKeyEnv] {
			c.Name = existing.Name
			c.APIKeyEnv = existing.APIKeyEnv
			c.Status = "key-updated"
			c.Reason = "端点已存在，cc-switch 中密钥已变化，将更新 .env 并即时生效"
			return c
		}
		if existing.APIKeyEnv != "" && entry.APIKey == opts.ExistingEnvValues[existing.APIKeyEnv] {
			return skip("端点已存在且密钥一致，无需重复导入")
		}
		return skip("端点已存在于 providers.yaml，无需重复导入")
	}
	if len(entry.ModelHints) == 0 {
		return skip("条目中没有模型名，请在 providers.yaml 手动补 models 后使用")
	}

	c.Name = uniqueProviderName(providerName(entry), usedNames)
	c.APIKeyEnv = uniqueEnvKey(c.Name, usedEnvKeys)
	c.Models = append([]string(nil), entry.ModelHints...)
	c.Status = "importable"
	seen[normalized] = ExistingProvider{Name: c.Name, APIKeyEnv: c.APIKeyEnv}
	return c
}

// ImportOptions 描述一次迁移。
type ImportOptions struct {
	Home          string // cc-switch 主目录
	ProvidersYAML string // 网关 providers.yaml（默认由 ImportOptions 调用方解析）
	EnvFile       string // 网关 .env
	GatewayPort   int
	Include       map[string]bool // 只导入这些 cc-switch 条目名或网关名；空为全部
	DryRun        bool
	Now           func() time.Time // 迁移标注时间，测试可注入
}

// ImportResult 是一次迁移的结果。
type ImportResult struct {
	Source        string      `json:"source"`
	Home          string      `json:"home"`
	Candidates    []Candidate `json:"candidates"`
	ProvidersYAML string      `json:"providers_yaml"`
	EnvFile       string      `json:"env_file"`
	Wrote         bool        `json:"wrote"`        // 是否已写入文件（DryRun 恒为 false）
	Imported      []string    `json:"imported"`     // 实际写入的网关 provider 名
	UpdatedKeys   []string    `json:"updated_keys"` // 端点已存在但密钥有变化、被更新的变量名

	secrets map[string]string
	configs []provider.ProviderConfig
}

// RegisterLive 把导入的 provider 注册进运行中的 Router：先 os.Setenv 补上
// 刚写入 .env 的密钥（进程内生效，重启后由 godotenv 从 .env 加载），再走
// providers.yaml 同一套装载逻辑。DryRun 未写入时注册为空操作。
func (r *ImportResult) RegisterLive(router *provider.Router, logger interface{ Printf(string, ...interface{}) }) {
	if len(r.configs) == 0 {
		return
	}
	for key, value := range r.secrets {
		os.Setenv(key, value)
	}
	cfg := &provider.ProvidersConfig{Providers: r.configs}
	provider.RegisterProvidersFromConfig(router, cfg, logger, router.ListProviders()...)
}

// Import 扫描网关主机的 cc-switch 配置并把可导入条目合并进网关的
// providers.yaml 与 .env。写入前轮转备份（.pre-import.bak），已有端点不重复
// 导入，cc-switch 数据本身只读不动。
func Import(options ImportOptions) (*ImportResult, error) {
	if options.Home == "" {
		options.Home = DefaultHome()
	}
	scan, err := Scan(options.Home)
	if err != nil {
		return nil, err
	}
	return ImportEntries(options, scan.Source, scan.Entries)
}

// ImportEntries 对已解析的条目执行转换与写入。网关本机扫描与桌面端上传
// （远程迁移）共用这一条路径，保证去重、备份与注册行为完全一致。
func ImportEntries(options ImportOptions, source string, entries []Entry) (*ImportResult, error) {
	if options.ProvidersYAML == "" || options.EnvFile == "" {
		return nil, fmt.Errorf("providers.yaml 与 .env 路径必填")
	}
	if options.Now == nil {
		options.Now = time.Now
	}

	context, err := LoadContext(options.ProvidersYAML, options.EnvFile, options.GatewayPort)
	if err != nil {
		return nil, err
	}
	candidates := Convert(entries, context)

	result := &ImportResult{
		Source:        source,
		Home:          options.Home,
		Candidates:    candidates,
		ProvidersYAML: options.ProvidersYAML,
		EnvFile:       options.EnvFile,
		secrets:       map[string]string{},
	}

	selected := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Status != "importable" && candidate.Status != "key-updated" {
			continue
		}
		if len(options.Include) > 0 && !options.Include[candidate.CCName] && !options.Include[candidate.Name] {
			continue
		}
		selected = append(selected, candidate)
	}
	if len(selected) == 0 {
		return result, nil
	}

	stamp := options.Now().Format("2006-01-02 15:04")
	newProviders := make([]provider.ProviderConfig, 0, len(selected))
	keyUpdates := map[string]string{} // 密钥有变化、需要覆写 .env 的变量
	for _, candidate := range selected {
		if candidate.Status == "key-updated" {
			// 端点已在 providers.yaml：只覆写同名密钥变量并重注册，不动 yaml。
			keyUpdates[candidate.APIKeyEnv] = candidate.secret
			result.secrets[candidate.APIKeyEnv] = candidate.secret // RegisterLive 需要
			result.UpdatedKeys = append(result.UpdatedKeys, candidate.APIKeyEnv)
			continue
		}
		pc := provider.ProviderConfig{
			Name:      candidate.Name,
			Type:      candidate.Type,
			URL:       candidate.URL,
			APIKeyEnv: candidate.APIKeyEnv,
		}
		for _, id := range candidate.Models {
			pc.Models = append(pc.Models, provider.ModelConfig{ID: id, Capabilities: guessModelCapabilities(id)})
		}
		newProviders = append(newProviders, pc)
		result.secrets[candidate.APIKeyEnv] = candidate.secret
		result.Imported = append(result.Imported, candidate.Name)
	}

	// key-updated 的 provider 以 providers.yaml 里的清单为准重注册（同名实例
	// 覆盖，密钥即时生效），避免与人工编辑过的模型清单漂移。
	if len(keyUpdates) > 0 {
		if existing, err := provider.LoadProvidersConfig(options.ProvidersYAML); err == nil {
			for _, p := range existing.Providers {
				if _, changed := keyUpdates[p.APIKeyEnv]; changed {
					result.configs = append(result.configs, p)
					result.Imported = append(result.Imported, p.Name)
				}
			}
		}
	}
	result.configs = append(result.configs, newProviders...)

	yamlRaw := readFileOrDefault(options.ProvidersYAML)
	envRaw := readFileOrDefault(options.EnvFile)
	newYAML := appendProvidersYAML(yamlRaw, stamp, newProviders)
	// 先覆写有变化的密钥变量，再追加新变量（appendEnvFile 会跳过已存在的名字）。
	newEnv := rewriteEnvValues(envRaw, keyUpdates)
	newEnv = appendEnvFile(newEnv, stamp, result.secrets)

	// 写入前先验证合并结果仍是合法 YAML 且包含全部条目，避免半写坏文件。
	var verify provider.ProvidersConfig
	if err := yaml.Unmarshal(newYAML, &verify); err != nil {
		return nil, fmt.Errorf("合并后的 providers.yaml 不合法: %w", err)
	}
	if len(verify.Providers) < len(newProviders) {
		return nil, fmt.Errorf("合并后的 providers.yaml 条目数异常（%d），拒绝写入", len(verify.Providers))
	}

	if options.DryRun {
		return result, nil
	}
	if err := backupFile(options.ProvidersYAML); err != nil {
		return nil, fmt.Errorf("备份 providers.yaml 失败: %w", err)
	}
	if err := backupFile(options.EnvFile); err != nil {
		return nil, fmt.Errorf("备份 .env 失败: %w", err)
	}
	if err := os.WriteFile(options.ProvidersYAML, newYAML, 0o644); err != nil {
		return nil, fmt.Errorf("写入 providers.yaml 失败: %w", err)
	}
	if err := os.WriteFile(options.EnvFile, newEnv, 0o600); err != nil {
		return nil, fmt.Errorf("写入 .env 失败: %w", err)
	}
	result.Wrote = true
	return result, nil
}

// ─── 端点构造与规范化 ────────────────────────────────────────────────────────

// anthropicMessagesEndpoint 复刻 Claude Code 的语义：把 ANTHROPIC_BASE_URL
// 原样拼上 /v1/messages。cc-switch 里的 claude 条目本来就是给 Claude Code
// 用的，同样的拼接在中转服务上行为一致。
func anthropicMessagesEndpoint(base string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + "/v1/messages"
}

// openAIChatEndpoint 把 base URL 拼成 chat/completions 完整端点。只有纯源
// （无路径）时补 /v1，与 cc-switch 的 codex 模板规则一致；带路径（如
// /api/coding/paas/v4）的 base 原样保留。
func openAIChatEndpoint(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return base + "/chat/completions"
	}
	if parsed.Path == "" || parsed.Path == "/" {
		return base + "/v1/chat/completions"
	}
	return base + "/chat/completions"
}

// NormalizeEndpoint 规范化端点用于去重：小写 scheme/host、去默认端口、
// 去尾部斜杠与 query/fragment。
func NormalizeEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	endpoint := parsed.Scheme + "://" + host
	if port != "" {
		endpoint += ":" + port
	}
	return endpoint + strings.TrimRight(parsed.Path, "/")
}

// isLoopbackBaseURL 报告 base URL 的主机是否为回环地址。
func isLoopbackBaseURL(base string) bool {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0"
}

// pointsAtGateway 判断 base URL 是否指向网关自身（回环地址 + 同端口）。
func pointsAtGateway(base string, gatewayPort int) (bool, string) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" {
		return false, ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "0.0.0.0" {
		return false, ""
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return port == fmt.Sprintf("%d", gatewayPort), parsed.Host
}

// ─── 命名 ────────────────────────────────────────────────────────────────────

var providerNameSanitizer = regexp.MustCompile(`[^a-z0-9-]+`)

// providerName 由 cc-switch 条目名推导网关 provider 名；"default" 这类占位
// 名改用 base URL 的主机名（api./www. 前缀取第二段，如 api.deepseek.com →
// deepseek）。
func providerName(entry Entry) string {
	name := strings.ToLower(strings.TrimSpace(entry.Name))
	name = providerNameSanitizer.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" || name == "default" || name == "custom" || name == "official" {
		if parsed, err := url.Parse(strings.TrimSpace(entry.BaseURL)); err == nil && parsed.Host != "" {
			labels := strings.Split(strings.ToLower(parsed.Hostname()), ".")
			label := ""
			for i := 0; i < len(labels); i++ {
				if labels[i] == "api" || labels[i] == "www" {
					continue
				}
				label = labels[i]
				break
			}
			if label == "" {
				label = strings.ReplaceAll(strings.Join(labels, "-"), "--", "-")
			}
			name = strings.Trim(label, "-")
		}
		if name == "" {
			name = "cc-switch-relay"
		}
	}
	return name
}

func uniqueProviderName(base string, used map[string]bool) string {
	name := base
	for suffix := 2; used[name]; suffix++ {
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
	used[name] = true
	return name
}

func uniqueEnvKey(providerName string, used map[string]bool) string {
	base := "CCS_" + strings.ToUpper(strings.ReplaceAll(providerName, "-", "_")) + "_API_KEY"
	key := base
	for suffix := 2; used[key]; suffix++ {
		key = fmt.Sprintf("%s_%d", base, suffix)
	}
	used[key] = true
	return key
}

// ─── 文件合并 ────────────────────────────────────────────────────────────────

var topLevelKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*:`)

// appendProvidersYAML 把新 provider 条目追加进 providers.yaml 文本。只做
// 定点插入，不重排也不重写既有内容，保住手工维护的注释与顺序。条目插在
// providers 列表末尾（下一个顶层键之前）；文件没有 providers 键时补建；
// 内联空列表 providers: [] 会先展开成块样式。
func appendProvidersYAML(existing []byte, stamp string, providers []provider.ProviderConfig) []byte {
	var block strings.Builder
	block.WriteString(fmt.Sprintf("# ── 以下 %d 个供应商自 cc-switch 迁移导入（%s）──\n", len(providers), stamp))
	for _, pc := range providers {
		block.WriteString(fmt.Sprintf("  - name: %q\n", pc.Name))
		block.WriteString(fmt.Sprintf("    type: %q\n", pc.Type))
		block.WriteString(fmt.Sprintf("    url: %q\n", pc.URL))
		block.WriteString(fmt.Sprintf("    api_key_env: %q\n", pc.APIKeyEnv))
		if len(pc.Models) > 0 {
			block.WriteString("    models:\n")
			for _, mc := range pc.Models {
				block.WriteString(fmt.Sprintf("      - id: %q\n", mc.ID))
			}
		}
	}
	// models 为空的 provider 不产生任何可路由实例，但仍写入配置占位，
	// 由 Convert 阶段保证到这里一定有模型。

	lines := splitLines(existing)
	providersKeyAt := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "providers:") {
			providersKeyAt = i
			break
		}
	}

	if providersKeyAt < 0 {
		// 没有 providers 键：文件为空时直接创建；否则在末尾补一个顶层键。
		base := strings.TrimRight(string(existing), "\n")
		prefix := "\n\n"
		if base == "" {
			base, prefix = "", ""
		}
		return []byte(base + prefix + "providers:\n" + block.String())
	}

	rest := strings.TrimSpace(strings.TrimPrefix(lines[providersKeyAt], "providers:"))
	// providers: [...] 非空内联列表无法安全追加文本；按普通流程拼接，
	// 合并结果不合法时由 Import 的校验拦截并拒绝写入。
	inlineEmpty := rest == "[]"

	insertAt := len(lines)
	if inlineEmpty {
		// 内联空列表原地展开为块样式，新条目紧随其后。
		insertAt = providersKeyAt + 1
	} else {
		for i := providersKeyAt + 1; i < len(lines); i++ {
			if topLevelKey.MatchString(lines[i]) {
				insertAt = i
				break
			}
		}
	}

	var out strings.Builder
	for i, line := range lines {
		if i == insertAt {
			out.WriteString(block.String())
		}
		if i == providersKeyAt && inlineEmpty {
			out.WriteString("providers:\n")
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	if insertAt >= len(lines) {
		out.WriteString(block.String())
	}
	return []byte(out.String())
}

// appendEnvFile 把迁移密钥追加进 .env，已存在的变量名不重复写入。
func appendEnvFile(existing []byte, stamp string, secrets map[string]string) []byte {
	present := parseEnvKeys(existing)
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)

	var block strings.Builder
	block.WriteString(fmt.Sprintf("# ── cc-switch 迁移导入（%s）──\n", stamp))
	for _, name := range names {
		if _, exists := present[name]; exists {
			continue
		}
		block.WriteString(name + "=" + quoteEnvValue(secrets[name]) + "\n")
	}
	if strings.TrimRight(string(existing), "\n") == "" {
		return []byte(block.String())
	}
	if !strings.HasSuffix(string(existing), "\n") {
		existing = append(existing, '\n')
	}
	return append(existing, []byte(block.String())...)
}

// rewriteEnvValues 覆写 .env 中已存在变量的值（密钥轮换用），保持行位置与
// 其余内容不变；不存在的变量名跳过。
func rewriteEnvValues(raw []byte, updates map[string]string) []byte {
	if len(updates) == 0 {
		return raw
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if key, _, ok := strings.Cut(trimmed, "="); ok {
			if value, changed := updates[strings.TrimSpace(key)]; changed {
				lines[i] = key + "=" + quoteEnvValue(value)
			}
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// guessModelCapabilities 按模型名做保守的能力启发：cc-switch 条目不带能力
// 元数据，名字里的 vision/vl/4v 线索比默认全文本更接近事实。返回 nil 表示
// 沿用装载器的默认能力。
func guessModelCapabilities(id string) []string {
	lower := strings.ToLower(id)
	if strings.Contains(lower, "vision") || strings.Contains(lower, "-vl") || strings.Contains(lower, "vl-") || strings.Contains(lower, "4v") {
		return []string{"text", "vision", "tool_calling", "streaming", "reasoning"}
	}
	return nil
}

// quoteEnvValue 按 godotenv 规则引用值：普通字符集裸写，其余单引号包裹。
func quoteEnvValue(value string) string {
	if value == "" {
		return `''`
	}
	if strings.ContainsAny(value, " \t#'\"\n\r") {
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	}
	return value
}

func parseEnvKeys(raw []byte) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return values
}

func readFileOrDefault(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return raw
}

func splitLines(raw []byte) []string {
	content := strings.TrimSuffix(string(raw), "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

// backupFile 把现有文件轮转备份为 path.pre-import.bak；文件不存在时跳过。
func backupFile(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(path+".pre-import.bak", raw, 0o600)
}
