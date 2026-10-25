package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/nightgopher/easygateway/internal/corspolicy"
	"gopkg.in/yaml.v3"
)

// ProviderConfig 提供商配置
type ProviderConfig struct {
	Name                  string `yaml:"name"`
	Type                  string `yaml:"type"` // openai / anthropic / chatgpt / channel
	URL                   string `yaml:"url"`
	APIKeyEnv             string `yaml:"api_key_env"`
	AllowNoKey            bool   `yaml:"allow_no_key,omitempty"`
	RequestTimeoutSeconds int    `yaml:"request_timeout_seconds,omitempty"`
	// 首字超时的 per-provider 覆盖（秒，0 表示沿用全局路由策略）。思考档
	// 覆盖解决始终思考模型在长思考时被全局默认首字期限砍断的问题。
	FirstOutputTimeoutSeconds          int    `yaml:"first_output_timeout_seconds,omitempty"`
	ReasoningFirstOutputTimeoutSeconds int    `yaml:"reasoning_first_output_timeout_seconds,omitempty"`
	RateLimitScope                     string `yaml:"rate_limit_scope,omitempty"`
	// Channel 是账号渠道名（type: channel 时必填）。账号渠道的凭据由交互式登录
	// 产生，不存在 api_key_env，因此构造实例由 main 注入的工厂完成。
	Channel string        `yaml:"channel,omitempty"`
	Models  []ModelConfig `yaml:"models"`
}

// ChannelProviderFactory 由 main 注入：给定渠道名与模型 ID，构造一个绑定该模型的
// provider（内部使用账号池作为 AuthSource）。provider 包因此不必 import channel，
// 保持「provider 不感知渠道」的边界。
type ChannelProviderFactory func(channelName, modelID string) (Provider, bool)

var channelProviderFactory ChannelProviderFactory

// SetChannelProviderFactory 注册账号渠道 provider 工厂。必须在
// SetupProvidersFromConfig 之前调用，否则 type: channel 的条目会被跳过。
func SetChannelProviderFactory(factory ChannelProviderFactory) {
	channelProviderFactory = factory
}

// ModelConfig 模型配置
type ModelConfig struct {
	ID              string        `yaml:"id"`
	Aliases         []string      `yaml:"aliases"`
	ProviderName    string        `yaml:"provider_name,omitempty"` // 可选：自定义 provider 实例名，默认为模型 ID
	Protocol        string        `yaml:"protocol,omitempty"`
	Capabilities    []string      `yaml:"capabilities,omitempty"`
	InputModalities []string      `yaml:"input_modalities,omitempty"`
	MaxInputTokens  int           `yaml:"max_input_tokens,omitempty"`
	MaxOutputTokens int           `yaml:"max_output_tokens,omitempty"`
	Description     string        `yaml:"description,omitempty"` // 选型说明，经 /v1/models 暴露给客户端
	Pricing         *ModelPricing `yaml:"pricing,omitempty"`     // 单价（USD/百万 token），供用量成本统计
}

// ProvidersConfig 提供商配置文件
type ProvidersConfig struct {
	CORS      corspolicy.Config   `yaml:"cors,omitempty"`
	Providers []ProviderConfig    `yaml:"providers"`
	Chains    map[string][]string `yaml:"chains"`
}

// ProviderRegistry 提供商注册表
type ProviderRegistry struct {
	mu       sync.RWMutex
	registry map[string]func(*ProviderConfig) (Provider, error)
}

// NewProviderRegistry 创建新的提供商注册表
func NewProviderRegistry() *ProviderRegistry {
	r := &ProviderRegistry{
		registry: make(map[string]func(*ProviderConfig) (Provider, error)),
	}

	// 注册默认的提供商工厂
	r.Register("openai", NewOpenAIProviderFromConfig)
	r.Register("anthropic", NewAnthropicProviderFromConfig)
	r.Register("chatgpt", NewChatGPTProviderFromConfig)
	r.Register("systemone", func(cfg *ProviderConfig) (Provider, error) {
		return NewSystemOneProvider(&Config{Name: cfg.Name, URL: cfg.URL, APIKey: providerAPIKeyFromEnv(cfg.APIKeyEnv), RequestTimeout: time.Duration(cfg.RequestTimeoutSeconds) * time.Second}, cfg.AllowNoKey), nil
	})

	return r
}

// Register 注册提供商工厂函数
func (r *ProviderRegistry) Register(providerType string, factory func(*ProviderConfig) (Provider, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registry[providerType] = factory
}

// Create 创建提供商实例
func (r *ProviderRegistry) Create(cfg *ProviderConfig) (Provider, error) {
	r.mu.RLock()
	factory, ok := r.registry[cfg.Type]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown provider type: %s", cfg.Type)
	}

	return factory(cfg)
}

// LoadProvidersConfig 从 YAML 文件加载配置
func LoadProvidersConfig(path string) (*ProvidersConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	return parseProvidersConfig(data)
}

func parseProvidersConfig(data []byte) (*ProvidersConfig, error) {
	var cfg ProvidersConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("providers configuration must contain exactly one YAML document")
	}

	for _, p := range cfg.Providers {
		if p.RateLimitScope != "" && p.RateLimitScope != "model" && p.RateLimitScope != "channel" {
			return nil, fmt.Errorf("provider %s: invalid rate_limit_scope %q", p.Name, p.RateLimitScope)
		}
	}
	return &cfg, nil
}

// NewOpenAIProviderFromConfig 从配置创建 OpenAI 提供商
func NewOpenAIProviderFromConfig(cfg *ProviderConfig) (Provider, error) {
	apiKey := providerAPIKeyFromEnv(cfg.APIKeyEnv)
	if apiKey == "" && !cfg.AllowNoKey {
		return nil, fmt.Errorf("API key not found: %s", cfg.APIKeyEnv)
	}

	return NewOpenAIProvider(&Config{
		Name:                        cfg.Name,
		URL:                         cfg.URL,
		APIKey:                      apiKey,
		RequestTimeout:              time.Duration(cfg.RequestTimeoutSeconds) * time.Second,
		FirstOutputTimeout:          time.Duration(cfg.FirstOutputTimeoutSeconds) * time.Second,
		ReasoningFirstOutputTimeout: time.Duration(cfg.ReasoningFirstOutputTimeoutSeconds) * time.Second,
	}), nil
}

// NewAnthropicProviderFromConfig 从配置创建 Anthropic 提供商
func NewAnthropicProviderFromConfig(cfg *ProviderConfig) (Provider, error) {
	apiKey := providerAPIKeyFromEnv(cfg.APIKeyEnv)
	if apiKey == "" {
		return nil, fmt.Errorf("API key not found: %s", cfg.APIKeyEnv)
	}

	return NewAnthropicProvider(&Config{
		Name:                        cfg.Name,
		URL:                         cfg.URL,
		APIKey:                      apiKey,
		FirstOutputTimeout:          time.Duration(cfg.FirstOutputTimeoutSeconds) * time.Second,
		ReasoningFirstOutputTimeout: time.Duration(cfg.ReasoningFirstOutputTimeoutSeconds) * time.Second,
	}), nil
}

// NewChatGPTProviderFromConfig creates the subscription-backed provider. It
// intentionally does not use api_key_env: credentials come from Codex CLI or
// Pi's OAuth auth.json instead of an API key.
func NewChatGPTProviderFromConfig(_ *ProviderConfig) (Provider, error) {
	proxyURL := os.Getenv("HTTP_PROXY")
	if proxyURL == "" {
		proxyURL = os.Getenv("HTTPS_PROXY")
	}
	if proxyURL == "" {
		proxyURL = os.Getenv("https_proxy")
	}
	chatgpt := NewChatGPTProviderWithAuthPath(proxyURL, os.Getenv("CHATGPT_AUTH_FILE"))
	if !chatgpt.IsHealthy(context.Background()) {
		return nil, fmt.Errorf("ChatGPT OAuth credential is unavailable")
	}
	return chatgpt, nil
}

func providerAPIKeyFromEnv(envKey string) string {
	keys := []string{envKey}
	if envKey == "ALI_API_KEY" {
		keys = append(keys, "ALIYUN_MAAS_API_KEY", "DASHSCOPE_API_KEY")
	}
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

// SetupProvidersFromConfig 从配置文件设置提供商和路由
func SetupProvidersFromConfig(router *Router, configPath string, logger interface{ Printf(string, ...interface{}) }) (map[string]string, error) {
	cfg, err := LoadProvidersConfig(configPath)
	if err != nil {
		return nil, err
	}
	return RegisterProvidersFromConfig(router, cfg, logger), nil
}

// RegisterProvidersFromConfig 把一段 providers 配置注册进 Router（启动装载
// 与运行时增量导入共用）。existingProviderNames 作为实例名去重种子：运行中
// 增量注册时传入 router.ListProviders()，避免与既有实例同名互相覆盖。
func RegisterProvidersFromConfig(router *Router, cfg *ProvidersConfig, logger interface{ Printf(string, ...interface{}) }, existingProviderNames ...string) map[string]string {
	registry := NewProviderRegistry()
	modelToProvider := make(map[string]string)
	// provider 实例名默认直接用上游模型 ID（与上游一致，避免 glm-glm-4.7 这种
	// 供应商名与模型名重复的双写）。若不同供应商声明了同名模型，则退回 供应商-模型
	// 的限定形式，保证实例名仍然唯一。
	usedProviderNames := make(map[string]bool)
	for _, name := range existingProviderNames {
		usedProviderNames[name] = true
	}

	// 注册提供商
	for _, pc := range cfg.Providers {
		// 账号渠道必须先于 registry.Create 处理：channel 不是 registry 认识的
		// provider 类型，实例由 main 注入的工厂按渠道名构造，凭据来自交互式登录。
		if pc.Type == "channel" {
			channelName := pc.Channel
			if channelName == "" {
				channelName = pc.Name
			}
			if channelProviderFactory == nil {
				logger.Printf("Warning: provider %s is a channel but no channel factory is registered", pc.Name)
				continue
			}
			for _, mc := range pc.Models {
				base, ok := channelProviderFactory(channelName, mc.ID)
				if !ok {
					logger.Printf("Warning: channel %s not registered, skipping model %s", pc.Name, mc.ID)
					continue
				}
				capabilities := normalizeModelCapabilities(mc.Capabilities)
				bound := NewBoundModelProviderWrapper(base, mc.ID, capabilities)
				providerName := mc.ProviderName
				if providerName == "" {
					providerName = providerInstanceName(pc.Name, mc.ID, usedProviderNames)
				}
				usedProviderNames[providerName] = true
				router.RegisterProvider(providerName, bound)
				router.SetProviderChannel(providerName, channelName)
				if pc.RateLimitScope == "channel" {
					router.SetProviderRateLimitGroup(providerName, pc.Name)
				}

				// 渠道模型不必声明 protocol：入站协议由客户端端点决定，
				// 出站协议由渠道 provider 自行转换，留空即不对外标注。
				protocol := mc.Protocol
				registerModel := func(modelName string) {
					router.RegisterChain(modelName, []string{providerName})
					router.RegisterModel(ModelInfo{
						ID:              modelName,
						Provider:        pc.Name,
						Protocol:        protocol,
						Capabilities:    capabilities,
						InputModalities: modelInputModalities(capabilities, mc.InputModalities),
						MaxInputTokens:  mc.MaxInputTokens,
						MaxOutputTokens: mc.MaxOutputTokens,
						Description:     mc.Description,
						Pricing:         mc.Pricing,
					})
					modelToProvider[modelName] = providerName
				}
				registerModel(mc.ID)
				for _, alias := range mc.Aliases {
					registerModel(alias)
				}
				logger.Printf("Registered channel model: %s (channel=%s, provider=%s)", mc.ID, pc.Name, providerName)
			}
			continue
		}

		// 创建基础 provider
		baseProvider, err := registry.Create(&pc)
		if err != nil {
			logger.Printf("Warning: failed to create provider %s: %v", pc.Name, err)
			continue
		}

		// ChatGPT Codex is a native Responses passthrough provider. Register the
		// shared provider directly so the marker interface is preserved and the
		// model catalog remains authoritative in providers.yaml.
		if pc.Type == "chatgpt" {
			router.RegisterProvider(pc.Name, baseProvider)
			for _, mc := range pc.Models {
				capabilities := normalizeModelCapabilities(mc.Capabilities)
				protocol := mc.Protocol
				if protocol == "" {
					protocol = "responses"
				}
				registerModel := func(modelName string) {
					router.RegisterChain(modelName, []string{pc.Name})
					router.RegisterModel(ModelInfo{
						ID:              modelName,
						Provider:        pc.Name,
						Protocol:        protocol,
						Capabilities:    capabilities,
						InputModalities: modelInputModalities(capabilities, mc.InputModalities),
						MaxInputTokens:  mc.MaxInputTokens,
						MaxOutputTokens: mc.MaxOutputTokens,
						Description:     mc.Description,
						Pricing:         mc.Pricing,
					})
					modelToProvider[modelName] = pc.Name
				}
				// 与下面的分支保持一致：上游模型 ID 对外主名，别名作为兼容入口。
				registerModel(mc.ID)
				modelToProvider[mc.ID] = pc.Name
				for _, alias := range mc.Aliases {
					registerModel(alias)
					modelToProvider[alias] = pc.Name
				}
			}
			logger.Printf("Registered provider: %s (type=%s, url=%s)", pc.Name, pc.Type, pc.URL)
			continue
		}

		// 为每个模型创建独立的 provider 实例（绑定模型 ID）
		for _, mc := range pc.Models {
			providerName := mc.ProviderName
			if providerName == "" {
				providerName = providerInstanceName(pc.Name, mc.ID, usedProviderNames)
			}
			usedProviderNames[providerName] = true

			// 创建绑定模型的 provider，并把能力元数据绑定到这个具体模型。
			capabilities := normalizeModelCapabilities(mc.Capabilities)
			boundProvider := NewBoundModelProviderWrapper(baseProvider, mc.ID, capabilities)
			router.RegisterProvider(providerName, boundProvider)
			router.mu.Lock()
			router.runtime[providerName].anonymousProbe = pc.AllowNoKey && providerAPIKeyFromEnv(pc.APIKeyEnv) == ""
			router.mu.Unlock()
			if pc.RateLimitScope == "channel" {
				router.SetProviderRateLimitGroup(providerName, pc.Name)
			}
			protocol := mc.Protocol
			if protocol == "" {
				protocol = pc.Type
			}
			registerModel := func(modelName string) {
				router.RegisterModel(ModelInfo{
					ID:              modelName,
					Provider:        pc.Name,
					Protocol:        protocol,
					Capabilities:    capabilities,
					InputModalities: modelInputModalities(capabilities, mc.InputModalities),
					MaxInputTokens:  mc.MaxInputTokens,
					MaxOutputTokens: mc.MaxOutputTokens,
					Description:     mc.Description,
					Pricing:         mc.Pricing,
				})
			}

			// 上游模型 ID 是对外主名；aliases 是兼容旧客户端的历史代号。
			// 两者都注册，所以既可以用上游真名调用，原有别名也不会失效。
			router.RegisterChain(mc.ID, []string{providerName})
			registerModel(mc.ID)
			modelToProvider[mc.ID] = providerName
			if len(mc.Aliases) > 0 {
				for _, alias := range mc.Aliases {
					router.RegisterChain(alias, []string{providerName})
					registerModel(alias)
					modelToProvider[alias] = providerName
				}
				logger.Printf("Registered model: %s (aliases %v, provider=%s)", mc.ID, mc.Aliases, providerName)
			} else {
				logger.Printf("Registered model: %s (provider=%s)", mc.ID, providerName)
			}
		}

		logger.Printf("Registered provider: %s (type=%s, url=%s)", pc.Name, pc.Type, pc.URL)
	}

	// 注册 fallback 链
	for chainName, providers := range cfg.Chains {
		router.RegisterChain(chainName, providers)
		logger.Printf("Registered chain: %s -> %v", chainName, providers)
	}

	return modelToProvider
}

// BoundModelProviderWrapper 包装一个 provider 并绑定特定模型
type BoundModelProviderWrapper struct {
	Provider
	boundModel   string
	capabilities []string
}

// NewBoundModelProviderWrapper 创建绑定模型的 provider 包装器
func NewBoundModelProviderWrapper(p Provider, model string, declared ...[]string) *BoundModelProviderWrapper {
	capabilities := normalizeModelCapabilities(nil)
	if len(declared) > 0 {
		capabilities = normalizeModelCapabilities(declared[0])
	}
	return &BoundModelProviderWrapper{Provider: p, boundModel: model, capabilities: capabilities}
}

// BoundModel 返回绑定的模型名
func (w *BoundModelProviderWrapper) BoundModel() string {
	return w.boundModel
}

// Available 转发底层 provider 的凭据可用性。账号渠道未登录时底层报告不可用，
// 路由层据此跳过，避免每次请求都先撞一次「凭据缺失」再降级。
func (w *BoundModelProviderWrapper) Available() bool {
	if av, ok := w.Provider.(availabilityProvider); ok {
		return av.Available()
	}
	return true
}

func (w *BoundModelProviderWrapper) Capabilities() []string {
	return append([]string(nil), w.capabilities...)
}

// FirstOutputTimeoutOverride 转发底层 provider 的 per-provider 首字超时覆盖。
// 嵌入接口只会提升 Provider 自身的方法集，覆盖方法必须显式转发才能对路由层可见。
func (w *BoundModelProviderWrapper) FirstOutputTimeoutOverride() (time.Duration, time.Duration) {
	if o, ok := w.Provider.(FirstOutputOverrideProvider); ok {
		return o.FirstOutputTimeoutOverride()
	}
	return 0, 0
}

// ForwardStream 转发到底层支持流式的 provider
func (w *BoundModelProviderWrapper) ForwardStream(ctx context.Context, req *Request, out io.Writer) error {
	sp, ok := w.Provider.(StreamProvider)
	if !ok {
		return fmt.Errorf("provider %s does not support streaming", w.Provider.Name())
	}
	return sp.ForwardStream(ctx, req, out)
}

// Probe 探测底层 provider 时带上绑定的模型名：同一个上游地址下，不同模型的
// 可用性可能不同（例如账号只对部分模型有权限），用绑定名才测得到真实的那一个。
// 非聊天能力（嵌入/语音转写）的上游端点不是 chat/completions，聊天探测请求
// 只会得到 404，因此按能力走对应端点的最小请求。
func (w *BoundModelProviderWrapper) Probe(ctx context.Context) ProbeResult {
	if hasCapabilityFlag(w.capabilities, CapabilityEmbedding) {
		return w.probeEmbedding(ctx)
	}
	if hasCapabilityFlag(w.capabilities, CapabilityTranscription) {
		return w.probeTranscription(ctx)
	}
	if prober, ok := w.Provider.(modelProber); ok {
		return prober.ProbeModel(ctx, w.boundModel)
	}
	if prober, ok := w.Provider.(Prober); ok {
		return prober.Probe(ctx)
	}
	return ProbeResult{Status: ProbeUnknown, Detail: "provider 不支持上游探测"}
}

// providerInstanceName 决定 provider 实例名。
//
// 默认直接用上游模型 ID：面板和路由里显示的就是上游真名（glm-4.7、glm-5.2），
// 不再出现「glm-glm-4.7」这种供应商名与模型名重复的双写。只有当另一个供应商
// 已经占用了同一个模型 ID 时，才退回「供应商-模型」形式以保证唯一。
func providerInstanceName(providerName, modelID string, used map[string]bool) string {
	if !used[modelID] {
		return modelID
	}
	qualified := fmt.Sprintf("%s-%s", providerName, modelID)
	// 极端情况下限定名也可能撞车（两个供应商都叫同一个名字），继续加后缀兜底。
	for suffix := 2; used[qualified]; suffix++ {
		qualified = fmt.Sprintf("%s-%s-%d", providerName, modelID, suffix)
	}
	return qualified
}

func normalizeModelCapabilities(capabilities []string) []string {
	if len(capabilities) == 0 {
		// Existing YAML files did not declare capabilities. Preserve their
		// text/tool/streaming behavior; multimodal support must be explicit.
		return []string{CapabilityText, CapabilityToolCall, CapabilityStreaming, CapabilityReasoning}
	}
	seen := make(map[string]bool, len(capabilities))
	result := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" || seen[capability] {
			continue
		}
		seen[capability] = true
		result = append(result, capability)
	}
	if !seen[CapabilityText] && !seen[CapabilitySystemOne] {
		result = append(result, CapabilityText)
	}
	return result
}

func modelInputModalities(capabilities, explicit []string) []string {
	if len(explicit) > 0 {
		return append([]string(nil), explicit...)
	}
	modalities := []string{}
	for _, capability := range capabilities {
		switch capability {
		case CapabilityText:
			modalities = append(modalities, "text")
		case CapabilitySystemOne:
			modalities = append(modalities, "text")
		case CapabilityVision:
			modalities = append(modalities, "image")
		case CapabilityVideo:
			modalities = append(modalities, "video")
		case CapabilityFile:
			modalities = append(modalities, "file")
		case CapabilityAudio:
			modalities = append(modalities, "audio")
		}
	}
	return modalities
}

func (w *BoundModelProviderWrapper) UnavailableReason() string {
	if p, ok := w.Provider.(interface{ UnavailableReason() string }); ok {
		return p.UnavailableReason()
	}
	return ""
}
