package provider

import (
	"fmt"
	"sync"
	"time"

	"github.com/nightgopher/easygateway/internal/corspolicy"
)

type ConfigReloadStatus struct {
	Path          string       `json:"path"`
	Generation    uint64       `json:"generation"`
	LastAppliedAt time.Time    `json:"last_applied_at,omitempty"`
	LastError     string       `json:"last_error,omitempty"`
	Report        ConfigReport `json:"report"`
}

// ConfigReloader serializes reloads. A candidate is fully built away from the
// live router; only a successful build can replace the live routing tables.
type ConfigReloader struct {
	mu         sync.Mutex
	router     *Router
	path       string
	external   []string
	extend     func(*Router) error
	status     ConfigReloadStatus
	configured map[string]bool
	corsPolicy *corspolicy.Policy
}

func NewConfigReloader(router *Router, path string, external []string, extend func(*Router) error) *ConfigReloader {
	return &ConfigReloader{router: router, path: path, external: append([]string(nil), external...), extend: extend, status: ConfigReloadStatus{Path: path}}
}

func (r *ConfigReloader) Status() ConfigReloadStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Report.Diagnostics = append([]ConfigDiagnostic(nil), s.Report.Diagnostics...)
	return s
}

func (r *ConfigReloader) Check() (ConfigReport, error) {
	cfg, err := LoadProvidersConfig(r.path)
	if err != nil {
		return ConfigReport{}, err
	}
	report := ValidateProvidersConfig(cfg, ValidationOptions{CheckEnvironment: true, ExternalProviders: r.external})
	return report, report.Err()
}

func (r *ConfigReloader) SetCORSPolicy(policy *corspolicy.Policy) {
	r.mu.Lock()
	r.corsPolicy = policy
	r.mu.Unlock()
}

func (r *ConfigReloader) Reload() (ConfigReloadStatus, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, err := LoadProvidersConfig(r.path)
	if err != nil {
		return r.reloadFailure(err)
	}
	candidate, err := r.prepare(cfg)
	if err != nil {
		return r.reloadFailure(err)
	}
	r.apply(candidate)
	return r.status, nil
}

type preparedConfig struct {
	router     *Router
	config     *ProvidersConfig
	report     ConfigReport
	configured map[string]bool
}

func (r *ConfigReloader) reloadFailure(err error) (ConfigReloadStatus, error) {
	r.status.LastError = err.Error()
	return r.status, err
}

func (r *ConfigReloader) prepare(cfg *ProvidersConfig) (*preparedConfig, error) {
	report := ValidateProvidersConfig(cfg, ValidationOptions{CheckEnvironment: true, AllowMissingKeys: true, ExternalProviders: r.external})
	if err := report.Err(); err != nil {
		return nil, err
	}
	r.router.mu.RLock()
	candidate := NewRouterWithCircuitConfig(r.router.logger, r.router.circuitConfig)
	candidate.policy = r.router.policy
	candidate.healthTTL = r.router.healthTTL
	r.router.mu.RUnlock()
	registered := RegisterProvidersFromConfig(candidate, cfg, r.router.logger)
	// Optional, unconfigured providers can stay disabled across reloads, as at
	// startup. A new or previously active provider must not silently disappear
	// because its credentials/client failed to initialize.
	activeProviders := map[string]bool{}
	for _, m := range r.router.ListModelInfos() {
		activeProviders[m.Provider] = true
	}
	configured := map[string]bool{}
	for _, p := range cfg.Providers {
		configured[p.Name] = true
		for _, m := range p.Models {
			if _, ok := registered[m.ID]; ok {
				continue
			}
			if r.status.Generation > 0 && (!r.configured[p.Name] || activeProviders[p.Name]) {
				return nil, fmt.Errorf("provider %s could not initialize model %s; previous configuration retained", p.Name, m.ID)
			}
		}
	}
	if r.extend != nil {
		if err := r.extend(candidate); err != nil {
			return nil, err
		}
	}

	return &preparedConfig{candidate, cfg, report, configured}, nil
}

func (r *ConfigReloader) apply(candidate *preparedConfig) {
	r.router.replaceConfiguration(candidate.router)
	if r.corsPolicy != nil {
		_ = r.corsPolicy.Apply(candidate.config.CORS) // Already validated by prepare.
	}
	r.configured = candidate.configured
	r.status.Generation++
	r.status.LastAppliedAt = time.Now().UTC()
	r.status.LastError = ""
	r.status.Report = candidate.report
}

// Requests already holding a provider keep that instance and its transport.
// No client, account pool or request context is closed during this swap.
func (r *Router) replaceConfiguration(candidate *Router) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, runtime := range candidate.runtime {
		if old := r.runtime[name]; old != nil {
			runtime.enabled = old.enabled
		}
	}
	for name, override := range r.modelOverrides {
		if info, ok := candidate.models[name]; ok {
			info.Capabilities = append([]string(nil), override.capabilities...)
			info.InputModalities = append([]string(nil), override.inputModalities...)
			candidate.models[name] = info
			candidate.modelOverrides[name] = override
		}
	}
	r.providers, r.chains, r.models = candidate.providers, candidate.chains, candidate.models
	r.runtime, r.modelOverrides, r.rateGroups = candidate.runtime, candidate.modelOverrides, candidate.rateGroups
}

// IncludeMissingConfiguration keeps environment-backed integrations available
// without re-running login/token refresh while a YAML reload is being built.
func (r *Router) IncludeMissingConfiguration(base *Router) {
	base.mu.RLock()
	defer base.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, p := range base.providers {
		if _, exists := r.providers[name]; exists {
			continue
		}
		r.providers[name] = p
		r.runtime[name] = &providerRuntime{enabled: true, breaker: NewCircuitBreaker(r.circuitConfig)}
	}
	for name, chain := range base.chains {
		if _, exists := r.chains[name]; exists {
			continue
		}
		r.chains[name] = append([]string(nil), chain...)
	}
	for name, info := range base.models {
		if _, exists := r.models[name]; exists {
			continue
		}
		info.Capabilities = append([]string(nil), info.Capabilities...)
		info.InputModalities = append([]string(nil), info.InputModalities...)
		r.models[name] = info
	}
}
