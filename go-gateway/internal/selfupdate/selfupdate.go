// Package selfupdate 实现 easygateway 二进制的自更新，机制对齐 easyagent /
// mygo updater：GitHub Releases 提供 ed25519 签名的更新清单（manifest），
// 更新器下载归档、校验签名与尺寸、**先在隔离端口预检**、原子替换当前二进制，
// 再拉起宿主服务（macOS launchd / Linux systemd user 单元）；重启后健康轮询
// 失败自动回滚到旧二进制。
//
// 清单格式与 mygo update.Manifest 保持一致（version/url/size/signature），
// 签名内容为归档 SHA-256 摘要的 Ed25519 签名（base64），公钥编译期内置。
// 更新源是版本 tag 的 Release（稳定边界明确），不是 main 分支。
package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// PublicKey 是更新归档必须匹配的 Ed25519 公钥（base64）。私钥保存在发布
// 侧（GitHub Secrets 的 MYGO_UPDATER_PRIVATE_KEY），泄露公钥无安全影响。
const PublicKey = "WSZKmCGYBcyCH2p6WGUMl3TPEATGA87xxsSIQpDaM/4="

// verifyKey 允许同包测试注入另一把公钥；生产路径恒为 PublicKey。
var verifyKey = PublicKey

// MaxManifestSize / MaxArchiveSize 限制下载体量，防御异常清单与归档。
const (
	MaxManifestSize = 1 << 20
	MaxArchiveSize  = 512 << 20
)

// Manifest 是一个平台的更新清单，字段与 mygo update.Manifest 一致。
type Manifest struct {
	Version string `json:"version"`
	Notes   string `json:"notes,omitempty"`
	Date    string `json:"date,omitempty"`
	URL     string `json:"url"`
	Size    int64  `json:"size"`
	// Signature 是归档 SHA-256 摘要的 Ed25519 签名（base64）。
	Signature string `json:"signature"`
}

// FetchManifest 拉取并解析清单。feed 通常是
// https://github.com/<repo>/releases/latest/download/update-gateway-<os>-<arch>.json。
func FetchManifest(ctx context.Context, feed string) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feed, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch manifest: HTTP %d", resp.StatusCode)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxManifestSize)).Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Version == "" || m.URL == "" || m.Signature == "" || m.Size <= 0 {
		return nil, errors.New("incomplete manifest (need version/url/size/signature)")
	}
	return &m, nil
}

// NewerThan 报告清单版本是否比 current 新（语义化版本比较，容忍 v 前缀；
// current 为 dev/空时视为 0.0.0，任何正式版本都视为更新）。
func (m *Manifest) NewerThan(current string) bool {
	return CompareVersions(m.Version, current) > 0
}

// DownloadArchive 下载归档到 w，并校验尺寸与签名。返回遇到的任何不匹配。
func (m *Manifest) DownloadArchive(ctx context.Context, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download archive: HTTP %d", resp.StatusCode)
	}
	if !strings.HasPrefix(m.URL, "https://") && os.Getenv("EASYGATEWAY_UPDATE_ALLOW_HTTP") != "1" {
		return errors.New("manifest url must be https (local testing: EASYGATEWAY_UPDATE_ALLOW_HTTP=1)")
	}
	pub, err := base64.StdEncoding.DecodeString(verifyKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("built-in update public key is invalid")
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, sum), io.LimitReader(resp.Body, MaxArchiveSize+1))
	if err != nil {
		return fmt.Errorf("download archive: %w", err)
	}
	if n != m.Size {
		return fmt.Errorf("archive size mismatch: downloaded %d, manifest says %d", n, m.Size)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), sum.Sum(nil), sig) {
		return errors.New("archive signature verification failed")
	}
	return nil
}

// CompareVersions 比较两个版本号（容忍 v 前缀与 - prerelease 后缀），
// 返回 -1/0/1。解析失败的部分按 0 处理，不返回错误：清单来自已验签的
// 发布流程，比较器只需尽力而为。
func CompareVersions(a, b string) int {
	parse := func(v string) (nums []int, pre string) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		if i := strings.IndexAny(v, "-+"); i >= 0 {
			pre, v = v[i:], v[:i]
		}
		for _, part := range strings.Split(v, ".") {
			n, _ := strconv.Atoi(part)
			nums = append(nums, n)
		}
		for len(nums) < 3 {
			nums = append(nums, 0)
		}
		return nums, strings.TrimPrefix(pre, "-")
	}
	an, ap := parse(a)
	bn, bp := parse(b)
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	// 同号时：无 prerelease > 有 prerelease；都有时字典序（够用于 0.3.x）。
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	case ap < bp:
		return -1
	default:
		return 1
	}
}

// DefaultFeed 返回当前平台的默认清单地址。
func DefaultFeed(repo string) string {
	suffix := runtimeOSArch()
	return fmt.Sprintf("https://github.com/%s/releases/latest/download/update-gateway-%s.json", repo, suffix)
}

// PollHealth 轮询 addr（如 http://127.0.0.1:4001/health）直到 200 或超时。
func PollHealth(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
		if err == nil {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("health check did not recover within %v", timeout)
}
