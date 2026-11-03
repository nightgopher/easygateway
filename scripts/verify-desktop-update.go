// Verify the exact desktop update archives that will be published with a release.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

type desktopConfig struct {
	Version string `json:"version"`
	Updates struct {
		PublicKey string `json:"publicKey"`
		GitHub    string `json:"github"`
		TagPrefix string `json:"tagPrefix"`
	} `json:"updates"`
}

type desktopManifest struct {
	Version   string `json:"version"`
	URL       string `json:"url"`
	Size      int64  `json:"size"`
	Signature string `json:"signature"`
}

func verifyDesktopUpdates(configPath, directory string, targets []string) error {
	var config desktopConfig
	if err := readJSON(configPath, &config); err != nil {
		return err
	}
	publicKey, err := base64.StdEncoding.DecodeString(config.Updates.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid desktop update public key")
	}
	if config.Version == "" || config.Updates.GitHub == "" || config.Updates.TagPrefix == "" || len(targets) == 0 {
		return fmt.Errorf("incomplete update configuration or target list")
	}
	for _, target := range targets {
		switch target {
		case "darwin-arm64", "linux-amd64", "linux-arm64":
		default:
			return fmt.Errorf("unexpected desktop target %q", target)
		}
		var manifest desktopManifest
		if err := readJSON(filepath.Join(directory, "update-"+target+".json"), &manifest); err != nil {
			return err
		}
		archiveName := "easygateway-" + config.Version + "-" + target + ".tar.gz"
		expectedURL := "https://github.com/" + config.Updates.GitHub + "/releases/download/" + config.Updates.TagPrefix + config.Version + "/" + url.PathEscape(archiveName)
		if manifest.Version != config.Version || manifest.URL != expectedURL {
			return fmt.Errorf("%s manifest version or archive URL is incorrect", target)
		}
		file, err := os.Open(filepath.Join(directory, archiveName))
		if err != nil {
			return err
		}
		digest := sha256.New()
		size, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if size == 0 || manifest.Size != size {
			return fmt.Errorf("%s archive size does not match its manifest", target)
		}
		signature, err := base64.StdEncoding.DecodeString(manifest.Signature)
		if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), digest.Sum(nil), signature) {
			return fmt.Errorf("%s archive signature does not match the desktop update key", target)
		}
		fmt.Printf("Verified signed desktop update: %s\n", target)
	}
	return nil
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: verify-desktop-update <mygo.json> <artifact-directory> <target>...")
		os.Exit(2)
	}
	if err := verifyDesktopUpdates(os.Args[1], os.Args[2], os.Args[3:]); err != nil {
		fmt.Fprintln(os.Stderr, "desktop update verification:", err)
		os.Exit(1)
	}
}
