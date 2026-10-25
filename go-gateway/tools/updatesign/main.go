// updatesign 是发布侧的更新清单签名工具：把各平台的 gateway 二进制打成
// tar.gz 归档，用 Ed25519 私钥对归档 SHA-256 摘要签名，产出与
// internal/selfupdate 清单格式一致的 update-gateway-<os>-<arch>.json。
//
//	updatesign -key "$MYGO_UPDATER_PRIVATE_KEY" -version 0.3.16 \
//	          -repo nightgopher/easygateway -dir release/
//
// 归档内二进制统一命名为 gateway（windows 为 gateway.exe）；解出侧
// ExtractBinary 取归档内第一个普通文件，不依赖该名字。
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type manifest struct {
	Version   string `json:"version"`
	Notes     string `json:"notes,omitempty"`
	Date      string `json:"date,omitempty"`
	URL       string `json:"url"`
	Size      int64  `json:"size"`
	Signature string `json:"signature"`
}

func main() {
	key := flag.String("key", os.Getenv("MYGO_UPDATER_PRIVATE_KEY"), "Ed25519 private key (base64), or @file")
	version := flag.String("version", "", "release version without the v prefix")
	repo := flag.String("repo", "nightgopher/easygateway", "owner/name serving the releases")
	dir := flag.String("dir", "release", "directory holding gateway-<os>-<arch> binaries")
	flag.Parse()
	if *key == "" || *version == "" {
		fmt.Fprintln(os.Stderr, "usage: updatesign -key KEY -version VERSION [-repo owner/name] [-dir release]")
		os.Exit(2)
	}
	priv, err := loadKey(*key)
	if err != nil {
		fatal(err)
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		fatal(err)
	}
	platforms := 0
	for _, entry := range entries {
		name := entry.Name()
		var osArch string
		if strings.HasPrefix(name, "gateway-") && !strings.Contains(name, ".tar.gz") {
			osArch = strings.TrimPrefix(name, "gateway-")
			osArch = strings.TrimSuffix(osArch, ".exe")
		} else {
			continue
		}
		if err := signOne(priv, *dir, name, osArch, *version, *repo); err != nil {
			fatal(fmt.Errorf("%s: %w", name, err))
		}
		platforms++
	}
	if platforms == 0 {
		fatal(fmt.Errorf("no gateway-<os>-<arch> binaries found in %s", *dir))
	}
	fmt.Printf("signed update manifests for %d platform(s)\n", platforms)
}

func signOne(priv ed25519.PrivateKey, dir, binary, osArch, version, repo string) error {
	inner := "gateway"
	if strings.HasSuffix(binary, ".exe") {
		inner = "gateway.exe"
	}
	archiveName := fmt.Sprintf("gateway-%s-%s.tar.gz", version, osArch)
	archivePath := filepath.Join(dir, archiveName)
	if err := packBinary(filepath.Join(dir, binary), inner, archivePath); err != nil {
		return err
	}
	sum, size, err := digestFile(archivePath)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(priv, sum)
	m := manifest{
		Version:   version,
		Date:      time.Now().UTC().Format(time.RFC3339),
		URL:       fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", repo, version, archiveName),
		Size:      size,
		Signature: base64.StdEncoding.EncodeToString(sig),
	}
	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(dir, fmt.Sprintf("update-gateway-%s.json", osArch))
	if err := os.WriteFile(manifestPath, append(encoded, '\n'), 0644); err != nil {
		return err
	}
	fmt.Printf("  %s (%d bytes, manifest %s)\n", archiveName, size, filepath.Base(manifestPath))
	return nil
}

func packBinary(binary, inner, archivePath string) error {
	in, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: inner, Mode: 0755, Size: info.Size(), Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.Copy(tw, in); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return out.Close()
}

func digestFile(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return nil, 0, err
	}
	return sum.Sum(nil), n, nil
}

func loadKey(spec string) (ed25519.PrivateKey, error) {
	if strings.HasPrefix(spec, "@") {
		raw, err := os.ReadFile(strings.TrimPrefix(spec, "@"))
		if err != nil {
			return nil, err
		}
		spec = strings.TrimSpace(string(raw))
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(spec))
	if err != nil {
		return nil, fmt.Errorf("decode key: %w", err)
	}
	if len(seed) == ed25519.PrivateKeySize {
		return ed25519.PrivateKey(seed), nil
	}
	if len(seed) == ed25519.SeedSize {
		return ed25519.NewKeyFromSeed(seed), nil
	}
	return nil, fmt.Errorf("key is %d bytes, want %d (private) or %d (seed)", len(seed), ed25519.PrivateKeySize, ed25519.SeedSize)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "updatesign:", err)
	os.Exit(1)
}
