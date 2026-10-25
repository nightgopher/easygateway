// Package claudeconfig 把 Claude Code 接到网关（接管），并把接管前受影响的
// 配置原值存为快照，供一键还原（default）。语义对齐 magpie 的 agent 接管：
//
//	gateway claude            查看当前状态
//	gateway claude <model>    接管：settings.json 的 env 指向网关
//	gateway claude default    还原：恢复接管前的 env 原值
//
// 只动 settings.json 里 4 个受管键（ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN /
// ANTHROPIC_API_KEY / ANTHROPIC_MODEL），文件里其余内容（modelPicker 等）原样
// 保留。快照是键级 JSON（不是整文件备份）：接管后用户对 settings.json 的其他
// 修改不会被还原覆盖。重复接管不覆盖已有快照，保证快照始终是最初的原值。
package claudeconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// 受管理的 env 键。接管时 BASE_URL/AUTH_TOKEN 写入网关值，API_KEY 移除
// （旧版 Claude Code 里残留的 API Key 会与 AUTH_TOKEN 抢鉴权），MODEL 按需。
const (
	EnvBaseURL   = "ANTHROPIC_BASE_URL"
	EnvAuthToken = "ANTHROPIC_AUTH_TOKEN"
	EnvAPIKey    = "ANTHROPIC_API_KEY"
	EnvModel     = "ANTHROPIC_MODEL"
)

var managedKeys = []string{EnvBaseURL, EnvAuthToken, EnvAPIKey, EnvModel}

// Options 描述一次接管动作。Endpoint 是已规范化的网关 v1 地址，MasterKey
// 是网关认证 token（两者都不回显到输出）。
type Options struct {
	ClaudeHome string
	Endpoint   string
	MasterKey  string
	Model      string // 可空：不写 ANTHROPIC_MODEL
	DryRun     bool
}

func SettingsPath(home string) string { return filepath.Join(home, "settings.json") }

func snapshotPath(home string) string {
	return filepath.Join(home, "settings.json.easygateway-bak")
}

// keySnapshot 记录单个键的接管前状态：Present=false 表示原本不存在，还原时
// 应删除该键而不是写空串。
type keySnapshot struct {
	Present bool   `json:"present"`
	Value   string `json:"value,omitempty"`
}

type snapshot struct {
	// FileExisted 为 false 表示接管前 settings.json 不存在，还原时应删除
	// 整个文件而不是写回一个空对象。
	FileExisted bool                   `json:"file_existed"`
	Env         map[string]keySnapshot `json:"env"`
}

// Status 是 settings.json 的接管视图，不含任何密钥明文。
type Status struct {
	Path           string
	FileExists     bool
	BaseURL        string
	AuthTokenSet   bool
	APIKeySet      bool
	Model          string
	SnapshotExists bool
}

// ReadStatus 汇报 Claude Code 当前的接管状态。settings.json 不存在视为未接管。
func ReadStatus(home string) (Status, error) {
	status := Status{Path: SettingsPath(home)}
	raw, err := os.ReadFile(status.Path)
	if os.IsNotExist(err) {
		status.SnapshotExists = fileExists(snapshotPath(home))
		return status, nil
	}
	if err != nil {
		return status, fmt.Errorf("read Claude settings: %w", err)
	}
	status.FileExists = true
	document, err := parseSettings(raw)
	if err != nil {
		return status, err
	}
	env, _ := document["env"].(map[string]any)
	status.BaseURL, _ = env[EnvBaseURL].(string)
	status.AuthTokenSet = env[EnvAuthToken] != nil && env[EnvAuthToken] != ""
	status.APIKeySet = env[EnvAPIKey] != nil && env[EnvAPIKey] != ""
	status.Model, _ = env[EnvModel].(string)
	if _, err := os.Stat(snapshotPath(home)); err == nil {
		status.SnapshotExists = true
	}
	return status, nil
}

// Takeover 把 settings.json 的 env 指向网关，并在首次接管时保存键级快照。
// 返回写盘后的完整文件内容（DryRun 时只返回不写）。
func Takeover(options Options) ([]byte, string, error) {
	path := SettingsPath(options.ClaudeHome)
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("read Claude settings: %w", err)
	}
	document, err := parseSettings(raw)
	if err != nil {
		return nil, "", err
	}
	env, ok := document["env"].(map[string]any)
	if !ok || env == nil {
		env = map[string]any{}
		document["env"] = env
	}

	snapPath := snapshotPath(options.ClaudeHome)
	if !fileExists(snapPath) {
		snap := snapshot{FileExisted: len(raw) > 0, Env: make(map[string]keySnapshot, len(managedKeys))}
		for _, key := range managedKeys {
			value, present := env[key].(string)
			snap.Env[key] = keySnapshot{Present: present, Value: value}
		}
		encoded, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return nil, "", fmt.Errorf("encode snapshot: %w", err)
		}
		if !options.DryRun {
			if err := writeProtected(snapPath, append(encoded, '\n')); err != nil {
				return nil, "", err
			}
		}
	}

	env[EnvBaseURL] = options.Endpoint
	env[EnvAuthToken] = options.MasterKey
	delete(env, EnvAPIKey)
	if options.Model != "" {
		env[EnvModel] = options.Model
	} else {
		delete(env, EnvModel)
	}

	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("encode Claude settings: %w", err)
	}
	encoded = append(encoded, '\n')
	if !options.DryRun {
		if err := writeProtected(path, encoded); err != nil {
			return nil, "", err
		}
	}
	return encoded, path, nil
}

// Restore 恢复接管前的 env 原值并删除快照。没有快照（从未接管过）时报错，
// 不做任何修改。
func Restore(home string, dryRun bool) (string, error) {
	path := SettingsPath(home)
	snapPath := snapshotPath(home)
	raw, err := os.ReadFile(snapPath)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("no snapshot at %s: Claude Code was never taken over by easygateway", snapPath)
	}
	if err != nil {
		return "", fmt.Errorf("read snapshot: %w", err)
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return "", fmt.Errorf("parse snapshot: %w", err)
	}
	if !snap.FileExisted {
		if !dryRun {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return "", fmt.Errorf("remove Claude settings: %w", err)
			}
			if err := os.Remove(snapPath); err != nil {
				return "", fmt.Errorf("remove snapshot: %w", err)
			}
		}
		return path, nil
	}
	settingsRaw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read Claude settings: %w", err)
	}
	document, err := parseSettings(settingsRaw)
	if err != nil {
		return "", err
	}
	env, ok := document["env"].(map[string]any)
	if !ok || env == nil {
		env = map[string]any{}
		document["env"] = env
	}
	for _, key := range managedKeys {
		restored, ok := snap.Env[key]
		if !ok {
			continue // 快照里没有的键不动（向前兼容旧快照格式）。
		}
		if restored.Present {
			env[key] = restored.Value
		} else {
			delete(env, key)
		}
	}
	if len(env) == 0 {
		delete(document, "env")
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode Claude settings: %w", err)
	}
	if !dryRun {
		if err := writeProtected(path, append(encoded, '\n')); err != nil {
			return "", err
		}
		if err := os.Remove(snapPath); err != nil {
			return "", fmt.Errorf("remove snapshot: %w", err)
		}
	}
	return path, nil
}

func parseSettings(raw []byte) (map[string]any, error) {
	document := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &document); err != nil {
			return nil, fmt.Errorf("parse Claude settings.json: %w", err)
		}
	}
	// JSON null 会把 map 置 nil（合法 JSON、病态配置），统一回退为空对象，
	// 否则后续 document["env"] 赋值会 panic。
	if document == nil {
		document = map[string]any{}
	}
	return document, nil
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
