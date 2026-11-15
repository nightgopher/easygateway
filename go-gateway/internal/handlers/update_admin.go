package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nightgopher/easygateway/internal/selfupdate"
)

// UpdateAdminHandler 提供网关自更新的管理端入口：检查走 Release 签名清单；
// 一键更新把 `gateway update` CLI 作为**分离子进程**拉起——替换二进制、重启
// 宿主服务、健康轮询失败自动回滚的完整安全链都在 CLI 侧完成，子进程独立于
// 当前服务进程存活（重启杀掉本进程也不影响收尾）；本文件只做校验、防并发
// 与启动。
type UpdateAdminHandler struct {
	version string
	// feed / spawn / dataDir 可在测试中替换；生产路径用下面的默认实现。
	feed    string
	spawn   func(exe string, args []string, logPath string) (int, error)
	dataDir func() string
}

// NewUpdateAdminHandler 创建网关更新管理端 handler；version 是编译期注入的
// 二进制版本（ldflags -X main.version，dev 构建会附加 vcs 短修订展示）。
func NewUpdateAdminHandler(version string) *UpdateAdminHandler {
	h := &UpdateAdminHandler{
		version: version,
		feed:    selfupdate.DefaultFeed("nightgopher/easygateway"),
		dataDir: defaultUpdateDataDir,
	}
	h.spawn = spawnDetachedUpdate
	return h
}

func defaultUpdateDataDir() string {
	if home := strings.TrimSpace(os.Getenv("LLM_GATEWAY_HOME")); home != "" {
		return filepath.Join(home, "data")
	}
	return filepath.Join(".", "data")
}

// displayVersion 返回给用户看的版本：dev 构建附加 vcs 短修订，让本地与
// 定时通道构建也能对上自己跑的是哪个提交；Release 构建直接用注入版本。
func (h *UpdateAdminHandler) displayVersion() string {
	if h.version != "" && h.version != "dev" {
		return h.version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev, dirty string
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				rev = setting.Value
			case "vcs.modified":
				if setting.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
		if rev != "" {
			short := rev
			if len(short) > 7 {
				short = short[:7]
			}
			return "dev (" + short + dirty + ")"
		}
	}
	return h.version
}

// updateFeedURL 返回清单地址（默认 releases/latest/download 固定重定向到
// 最新 Release；测试注入 httptest 地址）。
func (h *UpdateAdminHandler) updateFeedURL() string {
	return h.feed
}

// HandleUpdateCheck GET /admin/update/check
// 实时拉取最新 Release 清单并与当前版本比较；不下载归档、不做任何变更。
func (h *UpdateAdminHandler) HandleUpdateCheck(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	manifest, err := selfupdate.FetchManifest(ctx, h.updateFeedURL())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "拉取更新清单失败：" + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"version":          h.displayVersion(),
		"latest":           manifestPayload(manifest),
		"update_available": manifest.NewerThan(h.version),
	})
}

// HandleUpdateApply POST /admin/update/apply
// 校验清单确有新版本后，拉起分离的 `gateway update` 子进程并立即返回。
// 进度与结果写 data/gateway-update.log，桌面端轮询 /admin/update/status
// 的版本变化判断完成，重启窗口内的请求失败由桌面端静默重试。
func (h *UpdateAdminHandler) HandleUpdateApply(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	manifest, err := selfupdate.FetchManifest(ctx, h.updateFeedURL())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "拉取更新清单失败：" + err.Error()})
		return
	}
	if !manifest.NewerThan(h.version) {
		c.JSON(http.StatusOK, gin.H{"started": false, "reason": "已是最新版本（" + h.displayVersion() + "）"})
		return
	}

	dir := h.dataDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建 data 目录失败：" + err.Error()})
		return
	}
	pidPath := filepath.Join(dir, "gateway-update.pid")
	if pid, err := readPIDFile(pidPath); err == nil {
		if pidAlive(pid) {
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("更新已在进行中（pid %d），请等待其完成", pid)})
			return
		}
		_ = os.Remove(pidPath) // 残留的旧 pid，清掉
	}

	exe, err := os.Executable()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "定位网关二进制失败：" + err.Error()})
		return
	}
	logPath := filepath.Join(dir, "gateway-update.log")
	pid, err := h.spawn(exe, []string{"update"}, logPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "启动更新子进程失败：" + err.Error()})
		return
	}
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0600)
	c.JSON(http.StatusAccepted, gin.H{
		"started":        true,
		"target_version": manifest.Version,
		"log":            logPath,
	})
}

// HandleUpdateStatus GET /admin/update/status
// 轻量只读：当前版本 + 最近一次更新的日志尾部，供桌面端展示"上次更新"
// 与判断一键更新是否完成（版本变化即完成）。
func (h *UpdateAdminHandler) HandleUpdateStatus(c *gin.Context) {
	payload := gin.H{"version": h.displayVersion()}
	logPath := filepath.Join(h.dataDir(), "gateway-update.log")
	if info, err := os.Stat(logPath); err == nil {
		payload["apply_log"] = gin.H{
			"updated_at": info.ModTime().UTC().Format(time.RFC3339),
			"tail":       tailFile(logPath, 15),
		}
	}
	c.JSON(http.StatusOK, payload)
}

// spawnDetachedUpdate 把 `gateway update` 作为分离进程启动：独立进程组、
// 输出重定向到日志文件，父进程（正在被更新的网关）退出不影响其收尾
// （健康轮询与失败回滚）。返回子进程 pid。
func spawnDetachedUpdate(exe string, args []string, logPath string) (int, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return 0, fmt.Errorf("open update log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	cmd.SysProcAttr = detachedSysProcAttr()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() { _ = cmd.Wait() }() // 收尸，避免僵尸进程
	return cmd.Process.Pid, nil
}

func readPIDFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, errors.New("invalid pid file")
	}
	return pid, nil
}

// tailFile 返回文件最后 maxLines 非空行（超长整体截断到 2000 字符）。
func tailFile(path string, maxLines int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > 2000 {
		tail = tail[len(tail)-2000:]
	}
	return tail
}

func manifestPayload(m *selfupdate.Manifest) gin.H {
	return gin.H{
		"version": m.Version,
		"date":    m.Date,
		"notes":   m.Notes,
	}
}
