package console

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/easygateway/desktop/internal/connection"
)

type CORSConfig struct {
	AllowedOrigins []string `json:"allowed_origins"`
}
type GatewayConfigStatus struct {
	Generation    uint64 `json:"generation"`
	LastAppliedAt string `json:"last_applied_at"`
	LastError     string `json:"last_error"`
}

func (a *App) gatewaySettingsView(c *ui.Context) {
	card(c, func() {
		ui.Text(c, "网关配置").FontSize(18).Bold()
		if a.gatewayConfigErr != "" {
			muted(c, a.gatewayConfigErr)
			return
		}
		muted(c, fmt.Sprintf("配置代次 %d · 最近生效 %s", a.gatewayConfig.Generation, a.gatewayConfig.LastAppliedAt))
		if a.gatewayConfig.LastError != "" {
			ui.Text(c, a.gatewayConfig.LastError).TextColor(c.Theme().Danger).Selectable()
		}
		ui.Row(c).Wrap().Gap(8).Children(func() {
			ui.Button(c, "预检配置").Disabled(a.busy != "").OnClick(func() {
				a.work("正在预检配置", 35*time.Second, func(ctx context.Context, client *connection.Client) (func(), error) {
					var result struct {
						Report struct{ Providers, Models, Chains int }
					}
					err := client.JSON(ctx, "POST", "/config/check", map[string]any{}, &result)
					return func() {
						a.notice = fmt.Sprintf("预检通过：%d providers / %d models / %d chains", result.Report.Providers, result.Report.Models, result.Report.Chains)
					}, err
				})
			})
			ui.Button(c, "热加载配置").Disabled(a.busy != "").OnClick(func() {
				a.work("正在热加载配置", 35*time.Second, func(ctx context.Context, client *connection.Client) (func(), error) {
					var result GatewayConfigStatus
					err := client.JSON(ctx, "POST", "/config/reload", map[string]any{}, &result)
					if err != nil {
						return nil, err
					}
					var cors CORSConfig
					corsErr := client.JSON(ctx, "GET", "/config/cors", nil, &cors)
					return func() {
						a.gatewayConfig = result
						if corsErr == nil {
							a.corsConfig = cors
							a.corsDraft = strings.Join(cors.AllowedOrigins, "\n")
						}
						a.notice = "网关配置已热加载"
					}, err
				})
			})
		})
		ui.Text(c, "浏览器跨域来源").Bold()
		muted(c, "每行填写一个来源，例如 https://nightgopher.github.io。留空允许所有来源；网页仍需提供有效 API key。")
		ui.TextArea(c, &a.corsDraft).Height(100).Label("允许的跨域来源").Disabled(a.busy != "")
		ui.Row(c).Gap(8).Children(func() {
			ui.Button(c, "撤销来源修改").Disabled(a.busy != "").OnClick(func() { a.corsDraft = strings.Join(a.corsConfig.AllowedOrigins, "\n") })
			ui.PrimaryButton(c, "保存跨域来源").Disabled(a.busy != "").OnClick(a.saveCORS)
		})
	})
}

func (a *App) saveCORS() {
	origins := []string{}
	for _, line := range strings.Split(a.corsDraft, "\n") {
		if origin := strings.TrimSpace(line); origin != "" {
			origins = append(origins, origin)
		}
	}
	a.work("正在保存跨域来源", 35*time.Second, func(ctx context.Context, client *connection.Client) (func(), error) {
		var result CORSConfig
		err := client.JSON(ctx, "PUT", "/config/cors", CORSConfig{AllowedOrigins: origins}, &result)
		if err != nil {
			return nil, err
		}
		var status GatewayConfigStatus
		statusErr := client.JSON(ctx, "GET", "/config/status", nil, &status)
		return func() {
			a.corsConfig = result
			a.corsDraft = strings.Join(result.AllowedOrigins, "\n")
			if statusErr == nil {
				a.gatewayConfig = status
			}
			a.notice = "跨域来源已保存并热加载"
		}, err
	})
}

func (a *App) probeAnonymousModels() {
	a.work("正在探活匿名模型", 70*time.Second, func(ctx context.Context, client *connection.Client) (func(), error) {
		err := client.JSON(ctx, "POST", "/models/doctor", map[string]bool{"probe_anonymous": true}, nil)
		return func() { a.reload(); a.notice = "匿名模型探活已完成" }, err
	})
}
