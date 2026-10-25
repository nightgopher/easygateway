// Package codexconfig 把 Codex CLI 接到网关（接管），并支持一键还原。
// 语义与 claudeconfig 一致，对齐 magpie 的 agent 接管：
//
//	gateway codex            查看当前状态
//	gateway codex <model>    接管：config.toml 指向网关 provider
//	gateway codex default    还原：恢复接管前的 config.toml 原文
//
// config.toml 是 TOML，map 往返会丢注释与键序，因此快照用**整文件字节备份**：
// 接管期间的写盘可能丢失注释，`default` 还原的是接管前的原文，一字不差。
// 接管只新增/覆盖三处：顶层 model_provider、model 与 [model_providers.easygateway]
// 块（字段形态沿用 scripts/codex-model 验证过的组合），其余顶层键原样保留。
package codexconfig

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

// ProviderID 是写入 config.toml 的网关 provider 标识。
const ProviderID = "easygateway"

// noOriginalMarker 作为快照文件内容，表示接管前 config.toml 不存在，
// 还原时应删除整个文件。
const noOriginalMarker = "__easygateway_no_original__"

// DefaultModel 是未指定模型时的接管模型（网关的降级链入口）。
const DefaultModel = "coding"

// Options 描述一次接管动作。Endpoint 是已规范化的网关 v1 地址。
type Options struct {
	CodexHome string
	Endpoint  string
	MasterKey string
	Model     string // 空时用 DefaultModel
	DryRun    bool
}

func ConfigPath(home string) string { return filepath.Join(home, "config.toml") }

func backupPath(home string) string {
	return filepath.Join(home, "config.toml.easygateway-bak")
}

// Status 是 config.toml 的接管视图，不含任何密钥明文。
type Status struct {
	Path           string
	FileExists     bool
	ModelProvider  string
	Model          string
	BaseURL        string
	SnapshotExists bool
}

// ReadStatus 汇报 Codex CLI 当前的接管状态。config.toml 不存在视为未接管。
func ReadStatus(home string) (Status, error) {
	status := Status{Path: ConfigPath(home)}
	raw, err := os.ReadFile(status.Path)
	if os.IsNotExist(err) {
		status.SnapshotExists = fileExists(backupPath(home))
		return status, nil
	}
	if err != nil {
		return status, fmt.Errorf("read Codex config: %w", err)
	}
	status.FileExists = true
	var document map[string]any
	if err := toml.Unmarshal(raw, &document); err != nil {
		return status, fmt.Errorf("parse Codex config.toml: %w", err)
	}
	status.ModelProvider, _ = document["model_provider"].(string)
	status.Model, _ = document["model"].(string)
	if providers, ok := document["model_providers"].(map[string]any); ok {
		if entry, ok := providers[ProviderID].(map[string]any); ok {
			status.BaseURL, _ = entry["base_url"].(string)
		}
	}
	if fileExists(backupPath(home)) {
		status.SnapshotExists = true
	}
	return status, nil
}

// Takeover 把 config.toml 指向网关 provider，首次接管时保存整文件字节快照。
// 返回写盘后的完整文件内容（DryRun 时只返回不写）。
func Takeover(options Options) ([]byte, string, error) {
	path := ConfigPath(options.CodexHome)
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("read Codex config: %w", err)
	}
	document := map[string]any{}
	if len(raw) > 0 {
		if err := toml.Unmarshal(raw, &document); err != nil {
			return nil, "", fmt.Errorf("parse Codex config.toml: %w", err)
		}
	}

	backup := backupPath(options.CodexHome)
	if !fileExists(backup) {
		content := raw
		if len(content) == 0 {
			content = []byte(noOriginalMarker)
		}
		if !options.DryRun {
			if err := writeProtected(backup, content); err != nil {
				return nil, "", err
			}
		}
	}

	model := options.Model
	if model == "" {
		model = DefaultModel
	}
	document["model_provider"] = ProviderID
	document["model"] = model
	document["disable_response_storage"] = true

	providers, ok := document["model_providers"].(map[string]any)
	if !ok || providers == nil {
		providers = map[string]any{}
		document["model_providers"] = providers
	}
	providers[ProviderID] = map[string]any{
		"name":                      "EasyGateway",
		"base_url":                  options.Endpoint,
		"wire_api":                  "responses",
		"requires_openai_auth":      true,
		"experimental_bearer_token": options.MasterKey,
	}

	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(document); err != nil {
		return nil, "", fmt.Errorf("encode Codex config.toml: %w", err)
	}
	if !options.DryRun {
		if err := writeProtected(path, encoded.Bytes()); err != nil {
			return nil, "", err
		}
	}
	return encoded.Bytes(), path, nil
}

// Restore 恢复接管前的 config.toml 原文（字节级，含注释与键序）并删除快照。
// 没有快照（从未接管过）时报错，不做任何修改。
func Restore(home string, dryRun bool) (string, error) {
	path := ConfigPath(home)
	backup := backupPath(home)
	content, err := os.ReadFile(backup)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("no snapshot at %s: Codex CLI was never taken over by easygateway", backup)
	}
	if err != nil {
		return "", fmt.Errorf("read snapshot: %w", err)
	}
	if dryRun {
		return path, nil
	}
	if bytes.Equal(content, []byte(noOriginalMarker)) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove Codex config: %w", err)
		}
	} else if err := writeProtected(path, content); err != nil {
		return "", err
	}
	if err := os.Remove(backup); err != nil {
		return "", fmt.Errorf("remove snapshot: %w", err)
	}
	return path, nil
}

func writeProtected(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
