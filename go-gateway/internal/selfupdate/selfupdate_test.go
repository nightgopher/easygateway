package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.16", "0.3.15", 1},
		{"0.3.16", "0.3.16", 0},
		{"v0.4.0", "0.3.99", 1},
		{"0.3.16", "0.4.0", -1},
		{"1.0.0", "1.0", 0},
		{"0.3.0-beta.1", "0.3.0", -1},
		{"0.3.0", "0.3.0-beta.1", 1},
		{"0.3.1", "0.3.1-rc.2", 1},
		{"dev", "0.0.1", -1}, // dev 构建视为最旧：任何正式版本都可更新
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("CompareVersions(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestFetchManifest(t *testing.T) {
	good := Manifest{Version: "9.9.9", URL: "https://example/a.tar.gz", Size: 10, Signature: "c2ln"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(good)
	}))
	defer server.Close()
	m, err := FetchManifest(context.Background(), server.URL)
	if err != nil || m.Version != "9.9.9" {
		t.Fatalf("fetch: %v %+v", err, m)
	}
	if !m.NewerThan("0.3.16") || m.NewerThan("9.9.9") {
		t.Fatal("NewerThan 判断错误")
	}

	server404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server404.Close()
	if _, err := FetchManifest(context.Background(), server404.URL); err == nil {
		t.Fatal("404 应报错")
	}
}

// 用测试密钥签归档，返回签名（与生产同一套语义：SHA-256 摘要的 Ed25519）。
func signForTest(t *testing.T, priv ed25519.PrivateKey, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sum[:]))
}

func TestDownloadArchiveVerifiesSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	original := verifyKey
	verifyKey = base64.StdEncoding.EncodeToString(pub)
	defer func() { verifyKey = original }()
	t.Setenv("EASYGATEWAY_UPDATE_ALLOW_HTTP", "1")

	archive := bytes.Repeat([]byte("gateway"), 1000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	defer server.Close()

	// 正确签名 + 尺寸：通过。
	m := &Manifest{Version: "9.9.9", URL: server.URL, Size: int64(len(archive)), Signature: signForTest(t, priv, archive)}
	var buf bytes.Buffer
	if err := m.DownloadArchive(context.Background(), &buf); err != nil {
		t.Fatalf("valid archive rejected: %v", err)
	}

	// 篡改一个字节：验签必须失败。
	tampered := append([]byte(nil), archive...)
	tampered[0] ^= 0xFF
	m.Signature = signForTest(t, priv, tampered)
	buf.Reset()
	if err := m.DownloadArchive(context.Background(), &buf); err == nil {
		t.Fatal("被篡改的归档必须被拒绝")
	}

	// 尺寸不符：拒绝。
	m.Signature = signForTest(t, priv, archive)
	m.Size++
	buf.Reset()
	if err := m.DownloadArchive(context.Background(), &buf); err == nil {
		t.Fatal("尺寸不符必须被拒绝")
	}

	// 另一把钥匙签的：拒绝。
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	m.Size = int64(len(archive))
	m.Signature = signForTest(t, other, archive)
	buf.Reset()
	if err := m.DownloadArchive(context.Background(), &buf); err == nil {
		t.Fatal("非内置公钥的签名必须被拒绝")
	}

	// 非 https 且未开测试开关：拒绝。
	t.Setenv("EASYGATEWAY_UPDATE_ALLOW_HTTP", "")
	m.Signature = signForTest(t, priv, archive)
	buf.Reset()
	if err := m.DownloadArchive(context.Background(), &buf); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("非 https 必须被拒绝: %v", err)
	}
}

func TestExtractBinary(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	content := []byte("fake gateway binary")
	if err := tw.WriteHeader(&tar.Header{Name: "gateway", Mode: 0755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	path := filepath.Join(t.TempDir(), "gateway")
	if err := os.WriteFile(path+".tar.gz", archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractBinary(path+".tar.gz", path); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, content) {
		t.Fatalf("解出的二进制内容不对: %q", got)
	}
	info, _ := os.Stat(path)
	if info.Mode()&0111 == 0 {
		t.Fatal("解出的二进制必须可执行")
	}
}

func TestInstallKeepsRollbackAndRestores(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "gateway")
	incoming := filepath.Join(dir, "gateway.new")
	if err := os.WriteFile(current, []byte("OLD"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(incoming, []byte("NEW"), 0755); err != nil {
		t.Fatal(err)
	}

	rollbackPath, err := Install(current, incoming)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	got, _ := os.ReadFile(current)
	if string(got) != "NEW" {
		t.Fatalf("替换后应为 NEW, got %q", got)
	}
	old, _ := os.ReadFile(rollbackPath)
	if string(old) != "OLD" {
		t.Fatalf("回滚副本应为 OLD, got %q", old)
	}
}

func TestPollHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := PollHealth(context.Background(), server.URL, 2*time.Second); err != nil {
		t.Fatalf("健康服务应通过: %v", err)
	}
	if err := PollHealth(context.Background(), "http://127.0.0.1:1/health", 300*time.Millisecond); err == nil {
		t.Fatal("不可达服务应超时报错")
	}
}

// 预检失败路径：候选不是网关二进制时必须失败，绝不放行替换。
func TestPreflightRejectsNonGateway(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "not-a-gateway")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 5\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := Preflight(ctx, fake, 24567); err == nil {
		t.Fatal("非网关二进制的预检必须失败")
	}
}
