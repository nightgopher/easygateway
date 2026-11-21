package main

import (
	"log"
	"path/filepath"

	"github.com/nightgopher/easygateway/internal/channel"
)

// 账号渠道（第二类）装配。
//
// 与普通 provider 的差别在于：凭据来自交互式登录（网关内置登录流程），会过期、
// 需要续期，因此不能写进 providers.yaml 的 api_key_env。channel.Registry 汇总
// 所有渠道，Admin API 通过它发起登录、查看状态、登出；easycode 渠道的 provider
// 装配由渠道内部完成（见 internal/channel/easycode.go）。
//
// 2026-11 清理：minimax/cline/lobster/gemini/codebuddy/workbuddy/trae/loomy/
// raccoon/opencode 十个账号渠道已移除（本机均无登录凭据、近 30 天零流量；
// git 历史可找回）。标准 API 与订阅类上游随后由 AstrLink 承接，本网关只保留
// DeepV / workbuddy-local / easycode 等私有集成。
func setupChannelProviders(registry *channel.Registry, logger *log.Logger) *channel.EasyCodeChannel {
	dir := filepath.Join(defaultGatewayHome(), "channels")

	easycode := channel.NewEasyCodeChannel(dir)
	registry.Register(easycode)
	logger.Printf("Account channel registered: %s (credentials: %s)", easycode.Name(), filepath.Join(dir, easycode.Name()+".json"))

	for _, name := range registry.Names() {
		c, _ := registry.Get(name)
		logger.Printf("Account channel %s: %d account(s) loaded", name, c.Accounts().Len())
	}
	return easycode
}
