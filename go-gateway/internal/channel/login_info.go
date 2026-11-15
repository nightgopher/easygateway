package channel

// DisabledChannelLogin 列出禁用登录入口的渠道名。由启动流程在加载配置后填充
// （config.DisabledChannelLogin ← EASYGATEWAY_DISABLE_CHANNEL_LOGIN，逗号分隔），
// 不能在包 init 时读环境变量——那会早于 godotenv 加载 .env。包内测试可直接覆写。
//
// 被禁用的渠道仍会出现在 /admin/channels（登录态可见、可登出、可删账号），但
// login_available=false，管理面板不再提供登录入口。场景：渠道额度改由伴生网关
// 统一供给（如 workbuddy → workbuddy-gateway），refresh token 独占续期，
// easygateway 侧再登录同一账号会两处续期互相作废，必须在登录入口上禁掉。
var DisabledChannelLogin = map[string]bool{}

const disabledChannelLoginHint = "该渠道登录已被 EASYGATEWAY_DISABLE_CHANNEL_LOGIN 禁用；额度由伴生网关统一供给，refresh token 独占续期，勿在此登录同一账号。"

// LoginInfo describes prerequisites without exposing application credentials.
type LoginInfo struct {
	Available         bool   `json:"login_available"`
	NeedsClientConfig bool   `json:"needs_client_config"`
	LocalOnly         bool   `json:"local_login_only"`
	SetupHint         string `json:"setup_hint,omitempty"`
	Anonymous         bool   `json:"anonymous_available"`
}

func LoginInfoFor(ch Channel) LoginInfo {
	info := LoginInfo{Available: true}
	switch c := ch.(type) {
	case *GeminiChannel:
		info.NeedsClientConfig = true
		info.Available = c.configured()
		info.SetupHint = "在网关主机配置 GEMINI_CHANNEL_CLIENT_ID / GEMINI_CHANNEL_CLIENT_SECRET，或 gemini-client.json；然后重启网关。"
	case *TraeChannel:
		info.NeedsClientConfig = true
		info.Available = c.configured()
		info.SetupHint = "可自动读取网关主机已安装的 TRAE CN 客户端配置；或配置 TRAE_CHANNEL_CLIENT_ID / TRAE_CHANNEL_APP_ID、trae-client.json，然后重启网关。"
		if c.localClient != nil {
			info.NeedsClientConfig = false
			info.SetupHint = "已识别本机 TRAE CN 客户端配置，点击登录完成浏览器授权。"
		}
	case *LoomyChannel:
		info.NeedsClientConfig = true
		info.Available = c.configured()
		info.SetupHint = "在网关主机配置 LOOMY_ACCESS_KEY_ID / LOOMY_ACCESS_KEY_SECRET，或 loomy-client.json；然后重启网关。"
	}
	switch ch.Name() {
	case "lobster", "gemini", "trae", "loomy", "raccoon":
		info.LocalOnly = true
	case "opencode":
		info.SetupHint = "OpenCode 免费档仅供官方 OpenCode 客户端使用；登录不会解除限制。网关调用需添加并选择已授权的付费模型。"
	case "easycode":
		info.SetupHint = "使用飞书登录 EasyCode，模型请求使用该账号自己的额度。"
	}
	if DisabledChannelLogin[ch.Name()] {
		info.Available = false
		info.SetupHint = disabledChannelLoginHint
	}
	return info
}
