package provider

import (
	"os"
	"strings"
	"testing"

	"github.com/weijian/go-llm-gateway/internal/corspolicy"
)

func TestCORSUpdatePersistsAndInvalidChangesKeepFileAndPolicy(t *testing.T) {
	path := writeTempProvidersConfig(t, "# keep this comment\n"+reloadTestYAML("https://example.invalid", "model"))
	p := corspolicy.New()
	r := NewConfigReloader(NewRouter(nil), path, nil, nil)
	r.SetCORSPolicy(p)
	if _, err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UpdateCORS([]string{"https://trusted.example"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "keep this comment") {
		t.Fatal("YAML comment lost")
	}
	cfg, err := LoadProvidersConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CORS.AllowedOrigins) != 1 || p.Config().AllowedOrigins[0] != "https://trusted.example" {
		t.Fatal("policy was not persisted/applied")
	}
	if _, err := r.UpdateCORS([]string{"https://*.example"}); err == nil {
		t.Fatal("invalid origin accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("invalid update changed disk config")
	}
	if err := os.WriteFile(path, []byte("cors:\n allowed_origins: ['*']\nproviders: []"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reload(); err == nil {
		t.Fatal("invalid file policy accepted")
	}
	if p.Config().AllowedOrigins[0] != "https://trusted.example" {
		t.Fatal("failed reload opened CORS policy")
	}
}

func TestCORSEditRejectsConcurrentFileChange(t *testing.T) {
	path := writeTempProvidersConfig(t, reloadTestYAML("https://example.invalid", "model"))
	changeDuringBuild := false
	external := reloadTestYAML("https://example.invalid", "external-model")
	r := NewConfigReloader(NewRouter(nil), path, nil, func(*Router) error {
		if changeDuringBuild {
			return os.WriteFile(path, []byte(external), 0600)
		}
		return nil
	})
	policy := corspolicy.New()
	r.SetCORSPolicy(policy)
	if _, err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	changeDuringBuild = true
	if _, err := r.UpdateCORS([]string{"https://trusted.example"}); err == nil {
		t.Fatal("concurrent file edit overwritten")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != external {
		t.Fatal("external file change lost")
	}
	if len(policy.Config().AllowedOrigins) != 0 {
		t.Fatal("failed edit changed live policy")
	}
}
