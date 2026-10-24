// Package assistant embeds an EasyAgent-based resident agent inside the gateway
// (docs/MEMORY_DESIGN.md §常驻助理). The agent's LLM calls loop back through
// the gateway's own OpenAI-compatible endpoint — dogfooding the router, so
// assistant traffic lands in request_logs and inherits provider failover.
//
// 治理边界（与记忆层一致）：助理可以**提案**记忆（candidate），但不能
// 确认/退役——人拍板只发生在 /admin/memories 治理端点上。
package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	piagent "github.com/hwj123hwj/easyagent/sdk/agent"
	"github.com/hwj123hwj/easyagent/sdk/ai"
	"github.com/hwj123hwj/easyagent/sdk/ai/providers"

	"github.com/weijian/go-llm-gateway/internal/memory"
)

// Config controls the resident assistant.
type Config struct {
	Enabled bool
	// Model 是网关 /v1/models 里的模型 ID（如 glm-5.3-flash），作为默认模型。
	Model string
	// Models 是聊天端点允许按请求切换的模型清单（应包含 Model）；空则只有 Model。
	Models []string
	// ModelCatalog supplies a fresh catalog on each list and chat request.
	ModelCatalog func() []ModelOption
	// BaseURL 指向网关自身（默认 http://127.0.0.1:<port>）。
	BaseURL string
	// APIKey 默认复用 MasterKey。
	APIKey string
	// System 覆盖内置 system prompt（空 = 用 DefaultSystemPrompt）。
	// 持久化在 agent_settings 表，经 /admin/assistant/prompt 管理。
	System string
}

// Assistant is a long-lived EasyAgent agent wired to gateway-domain tools.
// 每个可用模型各持一个 agent 实例（历史独立）；Chat 按 model 参数路由。
type Assistant struct {
	mu        sync.Mutex
	agents    map[string]*piagent.Agent
	build     func(modelID, system string) *piagent.Agent
	system    string
	defaultM  string
	available []string
	catalog   func() []ModelOption
	memories  memory.Store
	log       *log.Logger
}

// New builds the resident agent. The LLM transport points back at the
// gateway itself, so assistant traffic is visible in the same metrics and
// failover chains as every other client.
func New(cfg Config, memories memory.Store, logger *log.Logger) (*Assistant, error) {
	if logger == nil {
		logger = log.New(nil, "", 0)
	}
	registry := providers.NewRegistry()
	registry.Register(providers.NewOpenAIProvider(cfg.APIKey, cfg.BaseURL))
	available := dedupeModels(append([]string{cfg.Model}, cfg.Models...))

	system := cfg.System
	if system == "" {
		system = DefaultSystemPrompt
	}
	build := func(modelID, sys string) *piagent.Agent {
		return piagent.New(piagent.Options{
			Model:    ai.Model{ID: modelID, Name: modelID, API: "openai-completions", Provider: "openai", BaseURL: cfg.BaseURL, ContextWindow: 128000, MaxTokens: 8192},
			Registry: registry,
			System:   sys,
			Tools: []piagent.Tool{
				newMemoryLookupTool(memories),
				newMemoryListPendingTool(memories),
				newMemoryProposeTool(memories),
				newGatewayStatusTool(cfg.BaseURL, cfg.APIKey),
				newGatewayLogsTool(cfg.BaseURL, cfg.APIKey),
			},
			MaxTurns: 8,
		})
	}
	return &Assistant{
		agents:    map[string]*piagent.Agent{cfg.Model: build(cfg.Model, system)},
		build:     build,
		system:    system,
		defaultM:  cfg.Model,
		available: available,
		catalog:   cfg.ModelCatalog,
		memories:  memories,
		log:       logger,
	}, nil
}

func dedupeModels(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// DefaultModel returns the model used when a chat request omits one.
func (a *Assistant) DefaultModel() string { return a.defaultM }

// ModelOption carries the identity and searchable origin of a chat model.
type ModelOption struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
	Channel  string `json:"channel,omitempty"`
}

func (a *Assistant) ModelOptions() []ModelOption {
	if a.catalog != nil {
		options := a.catalog()
		out := make([]ModelOption, 0, len(options))
		seen := map[string]bool{}
		for _, option := range options {
			if option.ID != "" && !seen[option.ID] {
				out = append(out, option)
				seen[option.ID] = true
			}
		}
		return out
	}
	out := make([]ModelOption, 0, len(a.available))
	for _, id := range a.available {
		out = append(out, ModelOption{ID: id})
	}
	return out
}

// Models lists the model IDs a chat request may select.
func (a *Assistant) Models() []string {
	out := []string{}
	for _, option := range a.ModelOptions() {
		out = append(out, option.ID)
	}
	return out
}

// UpdateSystemPrompt hot-swaps every resident agent with a new system prompt
// (Chat serializes on the same mutex, so in-flight turns finish first).
func (a *Assistant) UpdateSystemPrompt(prompt string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if prompt == "" {
		prompt = DefaultSystemPrompt
	}
	a.system = prompt
	a.agents = map[string]*piagent.Agent{}
	for _, id := range a.available {
		a.agents[id] = a.build(id, prompt)
	}
}

// SystemPrompt returns the currently effective prompt.
func (a *Assistant) SystemPrompt() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.system
}

// StreamUpdate is one SSE-mappable update from a Chat turn.
type StreamUpdate struct {
	Type    string // text_delta | tool_start | tool_end | done | error
	Content string
	Tool    string
}

// Chat runs one user turn through the agent for the requested model
// ("" = default), forwarding streamed updates. 序列化执行：同一模型的
// 会话历史在实例内，个人场景单并发足够。
func (a *Assistant) Chat(ctx context.Context, message, model string, onUpdate func(StreamUpdate)) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if model == "" {
		model = a.defaultM
	}
	if !a.modelAllowed(model) {
		return "", fmt.Errorf("模型 %q 当前不可用或未配置；可用模型：%s", model, strings.Join(a.Models(), ", "))
	}
	agent, ok := a.agents[model]
	if !ok {
		agent = a.build(model, a.system)
		a.agents[model] = agent
	}

	events, err := agent.PromptStream(ctx, ai.NewTextUserMessage(message))
	if err != nil {
		return "", fmt.Errorf("assistant prompt: %w", err)
	}
	var final string
	var turnErr error
	for ev := range events {
		switch ev.Type {
		case piagent.StreamEventTextDelta:
			if ev.TextDelta != "" && onUpdate != nil {
				onUpdate(StreamUpdate{Type: "text_delta", Content: ev.TextDelta})
			}
		case piagent.StreamEventToolStart:
			if onUpdate != nil {
				onUpdate(StreamUpdate{Type: "tool_start", Tool: ev.ToolName})
			}
		case piagent.StreamEventToolEnd:
			if onUpdate != nil {
				onUpdate(StreamUpdate{Type: "tool_end", Tool: ev.ToolName})
			}
		case piagent.StreamEventDone:
			if ev.FinalMessage.Text != "" {
				final = ev.FinalMessage.Text
			}
		case piagent.StreamEventError:
			// SDK 随后仍会发送部分结果并完成清理；读到 channel 关闭后
			// 再返回，才能保留回答并允许下一轮复用同一 agent。
			if turnErr == nil {
				turnErr = fmt.Errorf("assistant turn error: %s", ev.Error)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return final, fmt.Errorf("assistant turn error: %w", err)
	}
	if turnErr != nil {
		return final, turnErr
	}
	if final == "" {
		return "", fmt.Errorf("assistant turn finished without output")
	}
	return final, nil
}

func (a *Assistant) modelAllowed(model string) bool {
	for _, id := range a.Models() {
		if id == model {
			return true
		}
	}
	return false
}

// DefaultSystemPrompt 是未自定义时的助理人设。核心纪律（用户设定）：
// 只记「用户本人的状态」——设备与环境、进行中的目标、现场经验；
// 模型本就懂的通用知识一律不入记忆。
const DefaultSystemPrompt = `你是常驻在 EasyGateway 里的助理（网关地址即你自己的 LLM 出口）。

职责：
1. 网关运营助手：凡是关于网关当前运行状况的问题（Provider 健康、熔断、
   流量、延迟、最近错误），先用 gateway_status / gateway_logs 查询实时
   数据再回答，不要凭记忆或猜测下结论。
2. 记忆管家：按用户要求检索、审阅长期记忆；发现值得长期记住的事实时，用
   memory_propose 提案（进入 candidate，等用户在管理端确认，你无权直接生效）。
3. 网关顾问：回答关于网关自身能力、配置约定的问题。
4. 日常问答。

记忆提案判据（最重要）：
- 只提案关于**用户本人状态**的记忆：他的设备与环境（哪台机器、什么系统、怎么互连）、
  他的进行中目标与主线方向、他的偏好与踩过的坑、他的现场经验（如"网线直连可让
  电脑扫描到无法联网的迷你主机"——用户自己未必记得、模型更不可能知道的事）；
- 通用知识、操作步骤、教程类内容一律不提案——模型本来就懂，记了只会制造冗余；
- 偏离用户主线目标的偶发内容不提案（如主攻 Go 后端的人偶尔写 JS）；
- 记忆提案必须是命题化的陈述（一句话可检验），不要存聊天摘要或一次性细节。

守则：
- 任何密钥、token、密码类内容一律不写入记忆，也不要复述；
- 检索记忆时优先按用户提到的项目作用域（project scope_key 形如 host:owner/repo）；
- 回答用中文，简洁直接。`

// ── 网关域工具 ────────────────────────────────────────────────────────────

type memoryLookupTool struct{ store memory.Store }

func newMemoryLookupTool(store memory.Store) piagent.Tool { return &memoryLookupTool{store: store} }

func (t *memoryLookupTool) Name() string { return "memory_lookup" }
func (t *memoryLookupTool) Description() string {
	return "按作用域检索已生效（active）的长期记忆。project 传 host:owner/repo 形态的仓库作用域；client 传客户端名。"
}
func (t *memoryLookupTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{"type": "string", "description": "仓库作用域，如 github:nightgopher/litellm-gateway"},
			"client":  map[string]any{"type": "string", "description": "客户端作用域，如 claude-code"},
		},
	}
}
func (t *memoryLookupTool) Validate(params json.RawMessage) (json.RawMessage, error) {
	return params, nil
}
func (t *memoryLookupTool) Execute(ctx context.Context, params json.RawMessage, onUpdate func(piagent.PartialResult)) (piagent.ToolResult, error) {
	var args struct {
		Project string `json:"project"`
		Client  string `json:"client"`
	}
	_ = json.Unmarshal(params, &args)
	memories, err := t.store.LookupFor(strings.TrimSpace(args.Client), strings.TrimSpace(args.Project), 20)
	if err != nil {
		return piagent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	if len(memories) == 0 {
		return piagent.ToolResult{Content: "（该作用域暂无生效记忆）"}, nil
	}
	var b strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&b, "- [%s] %s\n", m.ScopeType, m.Statement)
	}
	return piagent.ToolResult{Content: b.String()}, nil
}

type memoryListPendingTool struct{ store memory.Store }

func newMemoryListPendingTool(store memory.Store) piagent.Tool {
	return &memoryListPendingTool{store: store}
}

func (t *memoryListPendingTool) Name() string { return "memory_list_pending" }
func (t *memoryListPendingTool) Description() string {
	return "列出待确认的候选记忆（candidate），可据此向用户建议确认或删除。"
}
func (t *memoryListPendingTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (t *memoryListPendingTool) Validate(params json.RawMessage) (json.RawMessage, error) {
	return params, nil
}
func (t *memoryListPendingTool) Execute(ctx context.Context, params json.RawMessage, onUpdate func(piagent.PartialResult)) (piagent.ToolResult, error) {
	memories, _, err := t.store.List(memory.ListFilter{Status: memory.StatusCandidate, Limit: 20})
	if err != nil {
		return piagent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	if len(memories) == 0 {
		return piagent.ToolResult{Content: "（暂无待确认候选）"}, nil
	}
	var b strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&b, "#%d [%s %s] %s\n", m.ID, m.ScopeType, m.ScopeKey, m.Statement)
	}
	return piagent.ToolResult{Content: b.String()}, nil
}

type memoryProposeTool struct{ store memory.Store }

func newMemoryProposeTool(store memory.Store) piagent.Tool { return &memoryProposeTool{store: store} }

func (t *memoryProposeTool) Name() string { return "memory_propose" }
func (t *memoryProposeTool) Description() string {
	return "提案一条长期记忆（进入 candidate，等用户在管理端确认后生效）。必须是命题化陈述；密钥/凭据会被拒绝。"
}
func (t *memoryProposeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"statement":  map[string]any{"type": "string", "description": "命题化陈述，一句话可检验"},
			"scope_type": map[string]any{"type": "string", "description": "global | client | project", "enum": []string{"global", "client", "project"}},
			"scope_key":  map[string]any{"type": "string", "description": "project: host:owner/repo；client: 客户端名；global 留空"},
		},
		"required": []string{"statement", "scope_type"},
	}
}
func (t *memoryProposeTool) Validate(params json.RawMessage) (json.RawMessage, error) {
	return params, nil
}
func (t *memoryProposeTool) Execute(ctx context.Context, params json.RawMessage, onUpdate func(piagent.PartialResult)) (piagent.ToolResult, error) {
	var args struct {
		Statement string `json:"statement"`
		ScopeType string `json:"scope_type"`
		ScopeKey  string `json:"scope_key"`
	}
	_ = json.Unmarshal(params, &args)
	scopeType, scopeKey, err := memory.NormalizeScope(memory.ScopeType(args.ScopeType), args.ScopeKey)
	if err != nil {
		return piagent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	statement, err := memory.SanitizeStatement(args.Statement)
	if err != nil {
		return piagent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	entry, _, err := t.store.Insert(memory.Memory{
		ScopeType: scopeType, ScopeKey: scopeKey, Statement: statement,
		Status: memory.StatusCandidate, Source: "agent:assistant", Confidence: 0.3,
	})
	if err != nil {
		return piagent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return piagent.ToolResult{
		Content: fmt.Sprintf("已提案 #%d（candidate，待用户在管理端确认）：%s", entry.ID, statement),
	}, nil
}
