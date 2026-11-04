package memory

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeStatement(t *testing.T) {
	clean, err := SanitizeStatement("  部署走 tag 驱动流水线  ")
	if err != nil || clean != "部署走 tag 驱动流水线" {
		t.Fatalf("普通命题应原样保留: %q %v", clean, err)
	}

	if _, err := SanitizeStatement("   "); err == nil {
		t.Fatal("空白命题应报错")
	}

	long := strings.Repeat("长", StatementMaxLen+1)
	if _, err := SanitizeStatement(long); err == nil {
		t.Fatal("超长命题应报错")
	}

	secrets := []string{
		"我的 key 是 sk-abcdefgh12345678 别外传",
		"github token ghp_abcdefghijklmnopqrstuvwxyz123456",
		"AKIAIOSFODNN7EXAMPLE 是示例",
		"password: hunter2",
		"-----BEGIN RSA PRIVATE KEY-----",
	}
	for _, statement := range secrets {
		if _, err := SanitizeStatement(statement); !errors.Is(err, ErrSecretLike) {
			t.Fatalf("疑似密钥应拒绝入库: %q → %v", statement, err)
		}
	}
}

func TestNormalizeScope(t *testing.T) {
	scopeType, key, err := NormalizeScope(ScopeGlobal, "")
	if err != nil || scopeType != ScopeGlobal || key != "" {
		t.Fatalf("global 合法: %v %q %v", scopeType, key, err)
	}
	if _, _, err := NormalizeScope(ScopeGlobal, "x"); err == nil {
		t.Fatal("global 不允许 scope_key")
	}
	if _, _, err := NormalizeScope(ScopeClient, ""); err == nil {
		t.Fatal("client 必须带 scope_key")
	}
	if _, _, err := NormalizeScope(ScopeProject, ""); err == nil {
		t.Fatal("project 必须带 scope_key")
	}
	if _, _, err := NormalizeScope(ScopeType("tenant"), "x"); err == nil {
		t.Fatal("未知 scope_type 应报错")
	}
}
