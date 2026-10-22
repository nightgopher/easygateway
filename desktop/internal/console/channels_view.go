package console

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/egoist/mygo/ui"
)

func (a *App) channelsView(c *ui.Context) {
	if a.unloadedOrEmpty(c, len(a.channels), "网关尚未提供账号渠道，请确认已更新网关。") {
		return
	}
	muted(c, "先登录账号，再验证模型调用。无需额外应用密钥的渠道排在前面。")
	if login := a.channelLogin; login != nil {
		card(c, func() {
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c).Size(16, 16)
				ui.Text(c, "等待 "+channelTitle(login.Channel)+" 授权").Bold().Grow(1)
				ui.Button(c, "取消授权").OnClick(a.stop)
			})
			muted(c, "在浏览器完成登录；此页面会自动确认结果。有效期至 "+login.expiresAt.Format("15:04:05"))
			if login.UserCode != "" {
				ui.Row(c).Gap(12).Children(func() {
					ui.Text(c, "授权码："+login.UserCode).FontSize(18).Selectable()
					ui.Button(c, "复制授权码").Disabled(a.CopyText == nil).OnClick(func() { a.CopyText(login.UserCode) })
				})
			}
			ui.Row(c).Gap(10).Children(func() {
				ui.Button(c, "重新打开授权页").OnClick(a.openChannelLogin)
				ui.Button(c, "复制授权链接").Disabled(a.CopyText == nil).OnClick(func() { a.CopyText(login.URL) })
				if a.busy == "" {
					ui.Button(c, "继续检查授权").OnClick(a.pollChannelLogin)
				}
			})
		})
	}
	ui.List(c, &a.channelList, len(a.channels), func(i int) {
		ch := a.channels[i]
		name := channelTitle(ch.ID)
		ui.Column(c.Key(ch.ID)).Children(func() {
			row(c, func() {
				ui.Row(c).Gap(12).Children(func() {
					ui.Text(c, name).FontSize(18).Bold().Grow(1)
					switch {
					case ch.LoggedIn:
						chip(c, "已登录", okGreen)
					case len(ch.Accounts) > 0:
						chip(c, "需重新登录", c.Theme().Accent)
					default:
						chip(c, "未登录", c.Theme().TextMuted)
					}
					if ch.Anonymous {
						chip(c, "免费模型可匿名使用", c.Theme().Accent)
					}
					label := "登录"
					if len(ch.Accounts) > 0 {
						label = "添加账号"
					}
					ui.Button(c, label).Label(label + " " + name).Disabled(a.busy != "" || a.channelLogin != nil || !ch.canLogin(a.localConnection())).OnClick(func() { a.startChannelLogin(ch) })
					if ch.NeedsClientConfig {
						ui.Button(c, "配置说明").Label(name + " 配置说明").OnClick(func() {
							if a.channelSetup == ch.ID {
								a.channelSetup = ""
							} else {
								a.channelSetup = ch.ID
							}
						})
					}
				})
				switch {
				case ch.LocalOnly && !a.localConnection():
					muted(c, "此渠道需在网关所在电脑完成登录；请切换到本机网关。")
				case ch.LoginAvailable != nil && !*ch.LoginAvailable:
					muted(c, "缺少客户端配置，请查看配置说明。")
				case ch.LoggedIn:
					muted(c, fmt.Sprintf("已保存 %d 个账号 · 模型调用仍需验证", len(ch.Accounts)))
				case ch.Anonymous:
					muted(c, "匿名调用仍受上游额度与限流约束；登录可连接自己的账号。")
				case ch.NeedsClientConfig:
					muted(c, "客户端配置已就绪，点击登录完成账号授权。")
				default:
					if ch.SetupHint != "" {
						muted(c, ch.SetupHint)
					} else {
						muted(c, "点击登录，在浏览器完成账号授权，无需额外应用密钥。")
					}
				}
				if len(ch.ModelHealth) > 0 {
					available, verified := 0, 0
					for _, health := range ch.ModelHealth {
						if health.Available {
							available++
						}
						if health.Status == "available" {
							verified++
						}
					}
					muted(c, fmt.Sprintf("模型可尝试 %d/%d · 近期探活通过 %d", available, len(ch.ModelHealth), verified))
					if available == 0 {
						chip(c, "模型不可用", c.Theme().Danger)
					}
				}
				if a.channelSetup == ch.ID {
					ui.Text(c, ch.SetupHint).FontSize(13).TextColor(c.Theme().TextMuted).Selectable()
				}
				for index, account := range ch.Accounts {
					title := account.Nickname
					if title == "" {
						title = account.AccountID
					}
					if title == "" {
						title = "账号 " + strconv.Itoa(index+1)
					}
					detail := "访问令牌期限由上游管理"
					if expiry, err := time.Parse(time.RFC3339, account.ExpiresAt); err == nil {
						detail = "访问令牌到期：" + expiry.Local().Format("2006-01-02 15:04")
					}
					if account.RefreshAvailable {
						if account.Expired {
							detail = "访问令牌已到期 · 调用时会尝试自动续期"
						} else {
							detail += " · 支持自动续期"
						}
					} else if account.Expired {
						detail = "访问令牌已到期 · 无续期凭据，请重新登录"
					}
					ui.Row(c.Key(strconv.Itoa(index))).Gap(10).Children(func() {
						ui.Text(c, title).Selectable()
						if account.Expired {
							chip(c, "已失效", c.Theme().Danger)
						} else if expiry, err := time.Parse(time.RFC3339, account.ExpiresAt); err == nil {
							if until := time.Until(expiry); until > 0 && until < 24*time.Hour {
								chip(c, "即将过期", c.Theme().Accent)
							}
						}
						ui.Text(c, detail).FontSize(12).TextColor(c.Theme().TextMuted).Grow(1)
						ui.Button(c, "移除账号").Label("移除 " + name + " " + title).Disabled(a.busy != "" || a.channelLogin != nil).OnClick(func() {
							a.editorTitle = "移除 " + name + " 的账号凭据：" + title
							a.deleteAction = func() {
								a.mutate("正在移除账号", "DELETE", "/channels/"+url.PathEscape(ch.ID)+"/accounts/"+strconv.Itoa(index), nil)
							}
							a.deleteOpen = true
						})
					})
				}
			})
		})
	}).Grow(1)
}
