package provider

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ModelHealth distinguishes local readiness from a recent upstream probe.
// Unknown can be locally available; a key's presence is not proof of service.
type ModelHealth struct {
	Status         string      `json:"status"`
	Available      bool        `json:"available"`
	Reason         string      `json:"reason"`
	CheckedAt      time.Time   `json:"checked_at"`
	ProbeStatus    ProbeStatus `json:"probe_status,omitempty"`
	LastProbeAt    *time.Time  `json:"last_probe_at,omitempty"`
	ProbeExpiresAt *time.Time  `json:"probe_expires_at,omitempty"`
}

func (r *Router) SetModelHealthTTL(ttl time.Duration) {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	r.mu.Lock()
	r.healthTTL = ttl
	r.mu.Unlock()
}

func (r *Router) ModelHealths() map[string]ModelHealth { return r.modelHealthsAt(time.Now().UTC()) }

func (r *Router) modelHealthsAt(now time.Time) map[string]ModelHealth {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelHealthsLocked(now)
}

type CatalogModel struct {
	Info   ModelInfo
	Health ModelHealth
}

// ModelCatalog snapshots metadata and health from one routing generation.
func (r *Router) ModelCatalog() []CatalogModel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	healths := r.modelHealthsLocked(time.Now().UTC())
	result := make([]CatalogModel, 0, len(r.models))
	for _, info := range r.models {
		info.Capabilities = append([]string(nil), info.Capabilities...)
		info.InputModalities = append([]string(nil), info.InputModalities...)
		result = append(result, CatalogModel{Info: info, Health: healths[info.ID]})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Info.ID < result[j].Info.ID })
	return result
}

func (r *Router) modelHealthsLocked(now time.Time) map[string]ModelHealth {
	states := make(map[string]ModelHealth, len(r.providers))
	for name, p := range r.providers {
		rt := r.runtime[name]
		h := ModelHealth{Status: "unknown", Available: true, Reason: "locally configured; upstream not verified", CheckedAt: now}
		if rt == nil || !rt.enabled {
			h.Status, h.Available, h.Reason = "unavailable", false, "provider disabled"
		} else if !providerAvailable(p) {
			h.Status, h.Available, h.Reason = "unavailable", false, "provider credentials unavailable"
			if reason, ok := p.(interface{ UnavailableReason() string }); ok && reason.UnavailableReason() != "" {
				h.Reason = reason.UnavailableReason()
			}
		} else {
			if rt.hasProbe {
				at, expires := rt.lastProbeAt.UTC(), rt.lastProbeAt.Add(r.healthTTL).UTC()
				h.LastProbeAt, h.ProbeExpiresAt, h.ProbeStatus = &at, &expires, rt.lastProbe.Status
				if now.Before(expires) {
					switch rt.lastProbe.Status {
					case ProbeOnline:
						h.Status, h.Reason = "available", "recent upstream probe succeeded"
					case ProbeOffline:
						h.Status, h.Available, h.Reason = "unavailable", false, "recent upstream probe rejected access"
					case ProbeDegraded:
						h.Status, h.Available, h.Reason = "degraded", false, "recent upstream probe failed temporarily"
					}
					if rt.lastProbe.StatusCode >= 400 {
						h.Reason += fmt.Sprintf(" (HTTP %d)", rt.lastProbe.StatusCode)
					}
				} else {
					h.Reason = "upstream probe expired; availability unverified"
				}
			}
			if !rt.breaker.CanAttempt() || (rt.quota != nil && !rt.quota.CanAttempt()) {
				h.Status, h.Available, h.Reason = "degraded", false, "temporary circuit or rate-limit cooldown"
			}
		}
		states[name] = h
	}
	result := make(map[string]ModelHealth, len(r.models))
	for model := range r.models {
		best := ModelHealth{Status: "unavailable", Reason: "no configured provider is available", CheckedAt: now}
		for _, name := range r.chains[model] {
			h, ok := states[name]
			if !ok {
				continue
			}
			if healthPriority(h) > healthPriority(best) {
				best = h
			}
		}
		// Single-provider failures should retain their actionable explanation.
		if len(r.chains[model]) == 1 {
			if h, ok := states[r.chains[model][0]]; ok {
				best = h
			}
		}
		result[model] = best
	}
	return result
}

func healthPriority(h ModelHealth) int {
	if h.Available {
		if h.Status == "available" {
			return 4
		}
		return 3
	}
	if h.Status == "degraded" {
		return 2
	}
	return 1
}

type DoctorReport struct {
	CheckedAt time.Time              `json:"checked_at"`
	Probed    int                    `json:"probed"`
	Models    map[string]ModelHealth `json:"models"`
}

// Doctor only makes inference calls for YAML allow_no_key providers that were
// actually initialized without a key. Paid and account providers are never
// probed here. Explicit per-provider manual probes remain available separately.
func (r *Router) Doctor(ctx context.Context, probeAnonymous bool) (DoctorReport, error) {
	if !r.doctorMu.TryLock() {
		return DoctorReport{}, fmt.Errorf("a model doctor is already running")
	}
	defer r.doctorMu.Unlock()
	type target struct {
		name string
		p    Provider
		rt   *providerRuntime
	}
	var targets []target
	r.mu.RLock()
	if probeAnonymous {
		for name, p := range r.providers {
			rt := r.runtime[name]
			if rt == nil || !rt.enabled || !rt.anonymousProbe || !providerAvailable(p) {
				continue
			}
			if cp, ok := p.(CapabilityProvider); ok && (hasCapabilityFlag(cp.Capabilities(), CapabilityEmbedding) || hasCapabilityFlag(cp.Capabilities(), CapabilityTranscription)) {
				continue
			}
			targets = append(targets, target{name, p, rt})
		}
	}
	r.mu.RUnlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	jobs := make(chan target)
	var wg sync.WaitGroup
	var countMu sync.Mutex
	count := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if ctx.Err() != nil {
					continue
				}
				probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
				model := t.p.Name()
				if bound, ok := t.p.(BoundModelProvider); ok {
					model = bound.BoundModel()
				}
				result := probeVia(probeCtx, model, 1, t.p.ForwardRequest)
				cancel()
				if ctx.Err() != nil {
					continue
				}
				r.mu.Lock()
				if r.runtime[t.name] == t.rt && providersEqual(r.providers[t.name], t.p) {
					t.rt.lastProbe, t.rt.lastProbeAt, t.rt.hasProbe = result, time.Now().UTC(), true
				}
				r.mu.Unlock()
				countMu.Lock()
				count++
				countMu.Unlock()
			}
		}()
	}
	for _, t := range targets {
		select {
		case jobs <- t:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	return DoctorReport{CheckedAt: time.Now().UTC(), Probed: count, Models: r.ModelHealths()}, ctx.Err()
}
