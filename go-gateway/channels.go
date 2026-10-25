package main

import (
	"log"
	"path/filepath"

	"github.com/nightgopher/easygateway/internal/channel"
	"github.com/nightgopher/easygateway/internal/provider"
)

// 账号渠道（第二类）装配。
//
// 与普通 provider 的差别在于：凭据来自交互式登录（网关内置登录流程），会过期、
// 需要续期，因此不能写进 providers.yaml 的 api_key_env。这里把「渠道实例」与
// 「由它派生的 provider」分开：
//   - channel.Registry 汇总所有渠道，Admin API 通过它发起登录、查看状态、登出；
//   - provider 侧只拿到每个渠道的账号池（作为 AuthSource）与一个绑定模型的
//     provider 实例，模型清单仍在 providers.yaml 里声明（type: channel）。
//
// 两者在 provider 包与 channel 包之间不产生 import 依赖：provider 只认 AuthSource
// 接口，channel.Accounts 结构化满足它。
//
// 登录方式分三类，但对 Admin API 暴露成一回事：minimax/cline 是设备码轮询；
// lobster/gemini 是授权码 + 本机回环回调（见 channel.callbackServer）；codebuddy/
// workbuddy 是「申请 state + 轮询」，登录地址由网关向 /v2/plugin/auth/state 取得；
// trae 是「授权页 + 固定端口回环回调」（18080，占用则退回系统分配端口，见
// channel.traeCallbackServer）；loomy / raccoon 是「网关自建登录页 + 随机端口回环」
// （loomy 扫码后可能要求补绑手机号；raccoon 提供微信扫码与短信两条路径，二维码
// 由网关本地渲染）。这些渠道都把登录地址放进 DeviceCode.
// VerificationURIComplete，Admin API 原样回成 login_url，因此登录入口与前端都不需要
// 按渠道分支。
func setupChannelProviders(router *provider.Router, registry *channel.Registry, logger *log.Logger) {
	dir := filepath.Join(defaultGatewayHome(), "channels")

	instances := channelInstances{
		minimax:   channel.NewMiniMaxChannel(dir),
		cline:     channel.NewClineChannel(dir),
		lobster:   channel.NewLobsterChannel(dir),
		gemini:    channel.NewGeminiChannel(dir),
		codebuddy: channel.NewCodeBuddyChannel(dir),
		workbuddy: channel.NewWorkBuddyChannel(dir),
		trae:      channel.NewTraeChannel(dir),
		loomy:     channel.NewLoomyChannel(dir),
		raccoon:   channel.NewRaccoonChannel(dir),
		opencode:  channel.NewOpenCodeChannel(dir),
	}

	for _, c := range []channel.Channel{instances.minimax, instances.cline, instances.lobster, instances.gemini, instances.codebuddy, instances.workbuddy, instances.trae, instances.loomy, instances.raccoon, instances.opencode} {
		registry.Register(c)
		logger.Printf("Account channel registered: %s (credentials: %s)", c.Name(), filepath.Join(dir, c.Name()+".json"))
	}

	// 工厂按「渠道名 + 模型 ID」构造绑定模型的 provider。渠道实例与账号池在这里
	// 复用同一份，保证登录写回的令牌立即对 provider 生效。
	provider.SetChannelProviderFactory(func(channelName, modelID string) (provider.Provider, bool) {
		return buildChannelProvider(channelName, modelID, instances)
	})

	for _, name := range registry.Names() {
		c, _ := registry.Get(name)
		logger.Printf("Account channel %s: %d account(s) loaded", name, c.Accounts().Len())
	}
}

// channelInstances 汇总已装配的渠道实例，按渠道名分发给 buildChannelProvider。
// 收敛成一个结构体而不是逐个传参：新增渠道时只改这里与 switch，不必再改所有
// 调用点的签名。
type channelInstances struct {
	minimax   *channel.MiniMaxChannel
	cline     *channel.ClineChannel
	lobster   *channel.LobsterChannel
	gemini    *channel.GeminiChannel
	codebuddy *channel.BuddyChannel
	workbuddy *channel.BuddyChannel
	trae      *channel.TraeChannel
	loomy     *channel.LoomyChannel
	raccoon   *channel.RaccoonChannel
	opencode  *channel.OpenCodeChannel
}

// buildChannelProvider 是渠道 → provider 的装配开关，独立成函数以便测试直接断言
// 装配结果（UseBearer、协议类型、请求头），不需要起完整的网关。
func buildChannelProvider(channelName, modelID string, ch channelInstances) (provider.Provider, bool) {
	switch channelName {
	case "minimax":
		// 上游是 Anthropic Messages 协议：复用 AnthropicProvider 的传输与
		// SSE 透传，仅补一个请求体改写（M3.1 系列强制 adaptive 思考）。
		// UseBearer 必须为 true：该端点鉴权是 Authorization: Bearer，
		// 不是 Anthropic 官方的 x-api-key（AnthropicProvider 默认走后者）。
		return provider.NewAnthropicProvider(&provider.Config{
			Name:      modelID,
			URL:       ch.minimax.MessagesURL(),
			Auth:      ch.minimax.Accounts(),
			UseBearer: true,
			Transform: channel.MiniMaxTransformRequest,
		}), true
	case "cline":
		// 上游是 OpenAI Chat Completions 协议：复用 OpenAIProvider 的协议转换，
		// 补上复刻官方客户端所需的固定请求头（含 workos: 令牌前缀）。
		return provider.NewOpenAIProvider(&provider.Config{
			Name:         modelID,
			URL:          ch.cline.ChatURL(),
			Auth:         ch.cline.Accounts(),
			ExtraHeaders: channel.ClineClientHeaders(),
		}), true
	case "lobster":
		// 上游同样是 OpenAI Chat Completions 协议（/api/proxy/v1/chat/completions），
		// 复用 OpenAIProvider；渠道层提供令牌、复刻头与续期。上游强制 stream:true，
		// 走 provider 统一的流式路径。
		return provider.NewOpenAIProvider(&provider.Config{
			Name:         modelID,
			URL:          ch.lobster.ChatURL(),
			Auth:         ch.lobster.Accounts(),
			ExtraHeaders: ch.lobster.ClientHeaders(),
		}), true
	case "gemini":
		// 上游是 CloudCode 私有协议（v1internal:streamGenerateContent），既不是
		// OpenAI 也不是 Anthropic：用专门的 CloudCodeProvider 做信封与 SSE 翻译。
		// Auth 用渠道的 AuthSource（账号池 + CloudCode 项目缓存）。
		return provider.NewCloudCodeProvider(&provider.Config{
			Name: modelID,
			Auth: ch.gemini.AuthSource(),
		}), true
	case "codebuddy", "workbuddy":
		// CodeBuddy（腾讯）/ WorkBuddy（国际版）共用一份实现，只是产品常量不同。
		// 上游是 OpenAI Chat Completions 协议（/v2/chat/completions），复用
		// OpenAIProvider。三处渠道特有逻辑必须齐全，缺一上游都会拒绝：
		//   - ExtraHeaders 复刻官方客户端头（X-Domain/X-Product-Code/X-IDE-*/UA），
		//     User-Agent 还需按模型家族解析；
		//   - Transform 把对外模型名的 `codebuddy/`、`workbuddy/` 前缀剥掉再发上游。
		// 注意 ExtraHeaders 里也含 User-Agent：provider.applyAuth 在默认 UA 之后
		// 才写 ExtraHeaders，因此这里能覆盖掉网关默认 UA。
		channel := ch.codebuddy
		if channelName == "workbuddy" {
			channel = ch.workbuddy
		}
		return provider.NewOpenAIProvider(&provider.Config{
			Name:         modelID,
			URL:          channel.ChatURL(),
			Auth:         channel.Accounts(),
			ExtraHeaders: channel.ClientHeaders(modelID),
			Transform:    channel.TransformRequest,
		}), true
	case "trae":
		// 上游是 TRAE 私有 SOLO 协议（/api/agent/v3/llm_utils_chat）：请求体是
		// SOLO 形态，鉴权是 Authorization: Cloud-IDE-JWT（applyAuth 表达不了），
		// 响应是私有 SSE，故用专门的 TraeProvider。Auth 用渠道的 AuthSource
		// （账号池 + 复刻客户端身份头），模型名的 `trae/` 前缀由 provider 侧剥掉。
		return provider.NewTraeProvider(&provider.Config{
			Name: modelID,
			Auth: ch.trae.AuthSource(),
		}), true
	case "loomy":
		// 上游是标准 OpenAI Chat Completions 协议（/api/v1/chat/completions），
		// 复用 OpenAIProvider。三处渠道特有逻辑必须齐全，缺一上游都会拒绝：
		//   - Authorization: Bearer 由 applyAuth 写；
		//   - 自定义头 `token` 必须是当前账号令牌，且每个请求都重新求值，故走
		//     DynamicHeaders（静态 ExtraHeaders 表达不了）；
		//   - Transform 把对外模型名的 `loomy/` 前缀剥掉再发上游（8 个模型 ID
		//     与既有供应商重名，不加前缀会与路由器注册表冲突）。
		return provider.NewOpenAIProvider(&provider.Config{
			Name:           modelID,
			URL:            ch.loomy.ChatURL(),
			Auth:           ch.loomy.Accounts(),
			ExtraHeaders:   ch.loomy.ClientHeaders(),
			DynamicHeaders: ch.loomy.DynamicHeaders(),
			Transform:      channel.LoomyTransformRequest,
		}), true
	case "raccoon":
		// 上游是标准 OpenAI Chat Completions 协议（/api/web/llm/v2/chat/completions），
		// 复用 OpenAIProvider。三处渠道特有逻辑必须齐全，缺一上游会拒绝或静默忽略：
		//   - Authorization: Bearer 由 applyAuth 写；
		//   - X-Org-Code 必须是**当前账号**的 office_identity（个人账号为空），每个
		//     请求都要重新求值，故走 DynamicHeaders；其余固定头（Accept: text/event-stream、
		//     X-Raccoon-Language、X-Client-Platform）放 ExtraHeaders；
		//   - Transform 把思考档位折进 extra_body.thinking（顶层放会被静默忽略）。
		// 模型 ID 用上游原生的 `sn-` 前缀，与既有供应商无重名，无需命名空间。
		return provider.NewOpenAIProvider(&provider.Config{
			Name:           modelID,
			URL:            ch.raccoon.ChatURL(),
			Auth:           ch.raccoon.Accounts(),
			ExtraHeaders:   ch.raccoon.ClientHeaders(),
			DynamicHeaders: ch.raccoon.DynamicHeaders(),
			Transform:      channel.RaccoonTransformRequest,
		}), true
	case "opencode":
		// 上游是标准 OpenAI Chat Completions 协议（/zen/v1/chat/completions），
		// 复用 OpenAIProvider 的协议转换，但用 OpenCodeProvider 覆盖非流式路径：
		// 免费通道强制 stream:true，非流式请求必须内部走流式端点再累积回完整响应。
		// 渠道层三处特有逻辑必须齐全，缺一上游都会 403 FreeTierError：
		//   - Auth 用 AuthSource()：未登录时退化为匿名 Bearer "public"（免费模型无需
		//     账号），不能像其它渠道那样直接给 Accounts（空令牌会让 applyAuth 什么都不写）；
		//   - ExtraHeaders 提供 User-Agent: opencode/<version> 等固定头；
		//   - DynamicHeaders 每个请求重新生成 x-opencode-session（上游唯一校验的动态头）；
		//   - Transform 强制 stream:true 并补齐 bash/read 两个占位工具。
		return provider.NewOpenCodeProvider(&provider.Config{
			Name:           modelID,
			URL:            ch.opencode.ChatURL(),
			Auth:           ch.opencode.AuthSource(),
			ExtraHeaders:   ch.opencode.ClientHeaders(),
			DynamicHeaders: ch.opencode.DynamicHeaders(),
			Transform:      channel.OpenCodeTransformRequest,
		}), true
	}
	return nil, false
}
