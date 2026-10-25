package main

import (
	"bytes"
	"github.com/egoist/mygo"
	_ "github.com/egoist/mygo/locales/zh-Hans"
	"github.com/egoist/mygo/plugins/updater"
	"github.com/egoist/mygo/ui"
	"github.com/nightgopher/easygateway/desktop/internal/connection"
	"github.com/nightgopher/easygateway/desktop/internal/console"
	"github.com/nightgopher/easygateway/desktop/internal/nativefolder"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

func main() {
	// 应用内自更新：检查/下载/安装来自 GitHub Release 的签名更新包
	//（清单与 easygateway 网关共用同一把 Ed25519 密钥）。
	mygo.Use(updater.Plugin)
	app := mygo.App
	app.SetLanguages("zh-Hans", "en")
	app.SetName("EasyGateway")
	dir, err := app.Path(mygo.PathUserData)
	if err != nil {
		log.Fatal(err)
	}
	if override := os.Getenv("GATEWAY_DESKTOP_CONFIG_DIR"); override != "" {
		dir = override
	}
	store, err := connection.New(filepath.Join(dir, "connections.json"))
	if err != nil {
		log.Fatal(err)
	}
	if !app.RequestSingleInstanceLock() {
		return
	}
	var win *mygo.Window
	state := console.New(store)
	state.OpenLoginURL = mygo.Shell.OpenExternal
	state.CopyText = mygo.Clipboard.WriteText
	state.Version = app.Version()
	show := func() {
		if win == nil || win.IsDestroyed() {
			win = mygo.NewWindow(mygo.WindowOptions{Title: "EasyGateway", Content: ui.View(state.View), Width: 1180, Height: 860, MinWidth: 900, MinHeight: 640, TitleBarStyle: mygo.TitleBarHidden, TitleBarHeight: 48, TrafficLightPosition: &mygo.Point{X: 18, Y: 18}, Vibrancy: mygo.VibrancySidebar, BackgroundColor: "light-dark(#f7f8fa, #18181b)", StateKey: "main"})
			current := win
			state.SetDispatcher(func(fn func()) {
				current.Update(func() {
					if !current.IsDestroyed() {
						fn()
					}
				})
			})
			state.PickFolder = func(done func(path string, ok bool)) {
				current.Update(func() {
					if !current.IsDestroyed() {
						nativefolder.PickSheet(current.NativeHandle(), done)
					}
				})
			}
			current.OnClosed(state.Close)
			state.Start()
		}
		win.Show()
		win.Focus()
	}
	connections := func(*mygo.MenuItem, *mygo.Window) { show(); state.ShowConnections() }
	app.OnActivate(func(bool) { show() })
	app.OnSecondInstance(func([]string, string) { show() })
	app.WhenReady(func() {
		app.SetMenu(mygo.NewMenu([]*mygo.MenuItem{{Role: mygo.RoleAppMenu}, {Label: "应用", Submenu: []*mygo.MenuItem{updater.MenuItem()}}, {Role: mygo.RoleEditMenu}, {Label: "网关", Submenu: []*mygo.MenuItem{{Label: "显示控制台", Accelerator: "CmdOrCtrl+1", Click: func(*mygo.MenuItem, *mygo.Window) { show() }}, {Label: "管理连接…", Accelerator: "CmdOrCtrl+,", Click: connections}}}, {Role: mygo.RoleViewMenu}, {Role: mygo.RoleWindowMenu}}))
		tray, err := mygo.NewTray(mygo.TrayOptions{Icon: trayIcon(), IconIsTemplate: true, ToolTip: "Gateway · 网关管理", Menu: mygo.NewMenu([]*mygo.MenuItem{{Label: "打开 Gateway", Click: func(*mygo.MenuItem, *mygo.Window) { show() }}, {Label: "管理连接…", Click: connections}, mygo.Separator(), {Role: mygo.RoleQuit}})})
		if err != nil {
			log.Printf("托盘不可用: %v", err)
		} else {
			_ = tray
			app.OnWindowAllClosed(func() {})
		}
		show()
	})
	if err = app.Run(); err != nil {
		log.Fatal(err)
	}
}
func trayIcon() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 6; y < 26; y++ {
		for x := 6; x < 26; x++ {
			if (y < 10 || y >= 22 || x < 10 || (x >= 18 && y >= 15)) && !(x >= 20 && y >= 10 && y < 15) {
				img.Set(x, y, color.Black)
			}
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}
