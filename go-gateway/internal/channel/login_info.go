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
	switch ch.Name() {
	case "easycode":
		info.SetupHint = "使用飞书登录 EasyCode，模型请求使用该账号自己的额度。"
	}
	if DisabledChannelLogin[ch.Name()] {
		info.Available = false
		info.SetupHint = disabledChannelLoginHint
	}
	return info
}
