package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyDesktopUpdates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string, *desktopManifest)
		valid  bool
	}{
		{name: "valid signed update", valid: true},
		{name: "wrong URL", mutate: func(_ *testing.T, _ string, m *desktopManifest) { m.URL += ".other" }},
		{name: "wrong version", mutate: func(_ *testing.T, _ string, m *desktopManifest) { m.Version = "9.9.9" }},
		{name: "wrong size", mutate: func(_ *testing.T, _ string, m *desktopManifest) { m.Size++ }},
		{name: "bad signature", mutate: func(_ *testing.T, _ string, m *desktopManifest) {
			m.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		}},
		{name: "tampered archive", mutate: func(t *testing.T, archive string, _ *desktopManifest) {
			if err := os.WriteFile(archive, []byte("tampered"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing archive", mutate: func(t *testing.T, archive string, _ *desktopManifest) {
			if err := os.Remove(archive); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			var config desktopConfig
			config.Version = "1.2.3"
			config.Updates.PublicKey = base64.StdEncoding.EncodeToString(publicKey)
			config.Updates.GitHub = "owner/repo"
			config.Updates.TagPrefix = "v"
			configPath := filepath.Join(dir, "mygo.json")
			writeTestJSON(t, configPath, config)
			archiveName := "easygateway-1.2.3-darwin-arm64.tar.gz"
			archivePath := filepath.Join(dir, archiveName)
			contents := []byte("original")
			if err := os.WriteFile(archivePath, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(contents)
			manifest := desktopManifest{
				Version:   config.Version,
				URL:       "https://github.com/owner/repo/releases/download/v1.2.3/" + archiveName,
				Size:      int64(len(contents)),
				Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, digest[:])),
			}
			if tc.mutate != nil {
				tc.mutate(t, archivePath, &manifest)
			}
			writeTestJSON(t, filepath.Join(dir, "update-darwin-arm64.json"), manifest)
			err = verifyDesktopUpdates(configPath, dir, []string{"darwin-arm64"})
			if (err == nil) != tc.valid {
				t.Fatalf("verification error = %v, want valid = %v", err, tc.valid)
			}
		})
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
