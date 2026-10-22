package provider

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ValidationOptions separates offline syntax checks from credential readiness.
// ExternalProviders are instances installed by the application outside YAML.
type ValidationOptions struct {
	CheckEnvironment  bool
	AllowMissingKeys  bool
	ExternalProviders []string
}

type ConfigDiagnostic struct {
	Level   string `json:"level"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ConfigReport struct {
	Providers   int                `json:"providers"`
	Models      int                `json:"models"`
	Chains      int                `json:"chains"`
	Diagnostics []ConfigDiagnostic `json:"diagnostics"`
}

func (r ConfigReport) Valid() bool {
	for _, d := range r.Diagnostics {
		if d.Level == "error" {
			return false
		}
	}
	return true
}

func (r ConfigReport) Err() error {
	var messages []string
	for _, d := range r.Diagnostics {
		if d.Level == "error" {
			messages = append(messages, d.Path+": "+d.Message)
		}
	}
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("invalid providers configuration: %s", strings.Join(messages, "; "))
}

// ValidateProvidersConfig never opens a network connection or reads OAuth files.
// It reports environment variable names, never their values.
func ValidateProvidersConfig(cfg *ProvidersConfig, opts ValidationOptions) ConfigReport {
	r := ConfigReport{Diagnostics: []ConfigDiagnostic{}}
	add := func(level, path, message string) {
		r.Diagnostics = append(r.Diagnostics, ConfigDiagnostic{level, path, message})
	}
	if cfg == nil {
		add("error", "providers", "configuration is empty")
		return r
	}
	r.Providers, r.Chains = len(cfg.Providers), len(cfg.Chains)
	names, public, instances := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, name := range opts.ExternalProviders {
		instances[name] = true
	}
	usedInstances := map[string]bool{}
	channels := map[string]bool{"minimax": true, "cline": true, "lobster": true, "gemini": true, "codebuddy": true, "workbuddy": true, "trae": true, "loomy": true, "raccoon": true, "opencode": true}
	capabilities := map[string]bool{"text": true, "vision": true, "video": true, "file": true, "audio": true, "tool_calling": true, "streaming": true, "reasoning": true, "embedding": true, "audio_transcription": true}
	modalities := map[string]bool{"text": true, "image": true, "video": true, "file": true, "audio": true}
	for i, p := range cfg.Providers {
		path := fmt.Sprintf("providers[%d]", i)
		if strings.TrimSpace(p.Name) == "" || names[p.Name] {
			add("error", path+".name", "provider name must be nonempty and unique")
		}
		names[p.Name] = true
		switch p.Type {
		case "openai", "anthropic", "chatgpt":
			if !validProviderURL(p.URL) {
				add("error", path+".url", "must be an absolute HTTP(S) URL without credentials, query or fragment")
			}
		case "channel":
			channelName := p.Channel
			if channelName == "" {
				channelName = p.Name
			}
			if !channels[channelName] {
				add("error", path+".channel", "unknown account channel")
			}
			if p.URL != "" && !validProviderURL(p.URL) {
				add("error", path+".url", "must be an absolute HTTP(S) URL without credentials, query or fragment")
			}
		default:
			add("error", path+".type", "unknown provider type")
		}
		if p.RateLimitScope != "" && p.RateLimitScope != "model" && p.RateLimitScope != "channel" {
			add("error", path+".rate_limit_scope", "must be model or channel")
		}
		if p.RequestTimeoutSeconds < 0 || p.FirstOutputTimeoutSeconds < 0 || p.ReasoningFirstOutputTimeoutSeconds < 0 {
			add("error", path, "timeouts cannot be negative")
		}
		if p.AllowNoKey && p.Type != "openai" {
			add("error", path+".allow_no_key", "only supported by openai providers")
		}
		if (p.Type == "openai" || p.Type == "anthropic") && opts.CheckEnvironment && providerAPIKeyFromEnv(p.APIKeyEnv) == "" {
			level := "error"
			if p.AllowNoKey || opts.AllowMissingKeys {
				level = "warning"
			}
			message := "api_key_env is empty"
			if p.APIKeyEnv != "" {
				message = "environment variable " + p.APIKeyEnv + " is not set"
			}
			add(level, path+".api_key_env", message)
		}
		if len(p.Models) == 0 {
			add("error", path+".models", "must declare at least one model")
		}
		if p.Type == "chatgpt" {
			usedInstances[p.Name] = true
			instances[p.Name] = true
		}
		for j, m := range p.Models {
			r.Models++
			mp := fmt.Sprintf("%s.models[%d]", path, j)
			for _, id := range append([]string{m.ID}, m.Aliases...) {
				if strings.TrimSpace(id) == "" || public[id] {
					add("error", mp+".id", "model IDs and aliases must be nonempty and globally unique: "+id)
				}
				public[id] = true
			}
			if m.MaxInputTokens < 0 || m.MaxOutputTokens < 0 {
				add("error", mp, "token limits cannot be negative")
			}
			for _, c := range m.Capabilities {
				if !capabilities[c] {
					add("error", mp+".capabilities", "unknown capability: "+c)
				}
			}
			for _, v := range m.InputModalities {
				if !modalities[v] {
					add("error", mp+".input_modalities", "unknown modality: "+v)
				}
			}
			if m.Protocol != "" && m.Protocol != "openai" && m.Protocol != "anthropic" && m.Protocol != "responses" {
				add("error", mp+".protocol", "unknown protocol")
			}
			if p.Type == "chatgpt" {
				continue
			}
			instance := m.ProviderName
			if instance == "" {
				instance = providerInstanceName(p.Name, m.ID, usedInstances)
			}
			if usedInstances[instance] {
				add("error", mp+".provider_name", "duplicate provider instance: "+instance)
			}
			usedInstances[instance], instances[instance] = true, true
		}
	}
	chainNames := make([]string, 0, len(cfg.Chains))
	for name := range cfg.Chains {
		chainNames = append(chainNames, name)
	}
	sort.Strings(chainNames)
	for _, name := range chainNames {
		members := cfg.Chains[name]
		if strings.TrimSpace(name) == "" || public[name] {
			add("error", "chains."+name, "chain name must be nonempty and must not shadow a model or alias")
		}
		if len(members) == 0 {
			add("error", "chains."+name, "chain must not be empty")
		}
		seen := map[string]bool{}
		for _, member := range members {
			if !instances[member] {
				add("error", "chains."+name, "unknown provider instance: "+member)
			}
			if seen[member] {
				add("error", "chains."+name, "duplicate provider instance: "+member)
			}
			seen[member] = true
		}
	}
	return r
}

func validProviderURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
