package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func runtimeOSArch() string { return runtime.GOOS + "-" + runtime.GOARCH }

// ExtractBinary 从 tar.gz 归档解出二进制并写入 path（0755）。归档里第一个
// 可执行类普通文件即二进制（发布归档只装一个 gateway 二进制）。
func ExtractBinary(archive, path string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("archive holds no binary")
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		// 目标可能是 CreateTemp 建出的 0600 文件，OpenFile 的权限位对已
		// 存在文件不生效，落位后显式补可执行位。
		return os.Chmod(path, 0755)
	}
}

// Preflight 在隔离端口 + 隔离主目录跑候选二进制，等 /health 返回 200。
// 预检不过的版本绝不替换线上二进制——与 deploy-local-gateway.sh 同一条铁律。
func Preflight(ctx context.Context, binary string, port int) error {
	home, err := os.MkdirTemp("", "easygateway-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("EASYGATEWAY_MASTER_KEY=sk-update-preflight\n"), 0600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, binary)
	cmd.Dir = home
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"LLM_GATEWAY_HOME="+home,
		"SKILLS_MARKET_ENABLED=false",
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("preflight start: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// 结果 channel 只由这一个 defer 消费：正文再读一次会和 defer 抢同一条
	// 消息，输家永久阻塞。
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()
	health := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	if err := PollHealth(ctx, health, 15*time.Second); err != nil {
		return fmt.Errorf("preflight health check: %w", err)
	}
	return nil
}

// Install 把新二进制原子替换到 current：旧件先挪到 current+".rollback"
// （保留一个可手动恢复的副本），rename 新件到位。调用方须已通过预检。
func Install(current, incoming string) (rollbackPath string, err error) {
	rollbackPath = current + ".rollback"
	if err := os.Remove(rollbackPath); err != nil && !os.IsNotExist(err) {
		return rollbackPath, fmt.Errorf("clear old rollback copy: %w", err)
	}
	if err := os.Rename(current, rollbackPath); err != nil {
		return rollbackPath, fmt.Errorf("backup current binary: %w", err)
	}
	if err := os.Rename(incoming, current); err != nil {
		_ = os.Rename(rollbackPath, current) // 立即恢复
		return rollbackPath, fmt.Errorf("swap binary: %w", err)
	}
	return rollbackPath, nil
}

// Rollback 用 rollback 副本恢复二进制并再次拉起服务。
func Rollback(current, rollbackPath string) error {
	if _, err := os.Stat(rollbackPath); err != nil {
		return fmt.Errorf("rollback copy missing: %w", err)
	}
	if err := copyFile(rollbackPath, current); err != nil {
		return err
	}
	return RestartSupervisor()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// RestartSupervisor 拉起宿主服务：macOS 优先 kickstart launchd（默认标签
// local.go-gateway，可用 EASYGATEWAY_LAUNCHAGENT 覆盖）；Linux 依次尝试
// systemd user 单元与系统级单元（默认 easygateway，可用
// EASYGATEWAY_SYSTEMD_UNIT 覆盖）；都不可用时返回 ErrManualRestart——
// 二进制已就位，重启进程即生效，调用方按提示而不是失败处理。
func RestartSupervisor() error {
	switch runtime.GOOS {
	case "darwin":
		label := envOr("EASYGATEWAY_LAUNCHAGENT", "local.go-gateway")
		out, err := exec.Command("launchctl", "kickstart", "-k",
			fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).CombinedOutput()
		if err == nil {
			return nil
		}
		// 标签不存在（手动运行、非 launchd 部署）：二进制已替换，提示手动重启。
		if strings.Contains(string(out), "Could not find service") {
			return ErrManualRestart
		}
		return fmt.Errorf("launchctl kickstart: %v: %s", err, strings.TrimSpace(string(out)))
	case "linux":
		unit := envOr("EASYGATEWAY_SYSTEMD_UNIT", "easygateway")
		if out, err := exec.Command("systemctl", "--user", "try-restart", unit).CombinedOutput(); err == nil {
			return nil
		} else if !strings.Contains(string(out), "not found") {
			return fmt.Errorf("systemctl --user try-restart: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("systemctl", "try-restart", unit).CombinedOutput(); err == nil {
			return nil
		} else if strings.Contains(string(out), "not found") {
			return ErrManualRestart
		} else {
			return fmt.Errorf("systemctl try-restart: %v: %s", err, strings.TrimSpace(string(out)))
		}
	default:
		return ErrManualRestart
	}
}

// ErrManualRestart 表示没有可用的服务管理器：二进制已替换成功，重启进程
// 后生效。调用方把它作为提示而不是错误处理。
var ErrManualRestart = errors.New("no service manager detected; restart the gateway process to apply the update")

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
