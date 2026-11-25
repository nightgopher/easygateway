package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type modelSelectionFile struct {
	Version int      `json:"version"`
	Enabled []string `json:"enabled"`
}

// LoadModelSelection retains legacy behavior until the first explicit change.
func (r *Router) LoadModelSelection(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selectionPath = path
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var selection modelSelectionFile
	if err = json.Unmarshal(data, &selection); err != nil {
		return err
	}
	if selection.Version != 1 || selection.Enabled == nil {
		return fmt.Errorf("invalid model selection file")
	}
	r.enabledModels = make(map[string]bool, len(selection.Enabled))
	for _, id := range selection.Enabled {
		r.enabledModels[id] = true
	}
	return nil
}

func (r *Router) modelEnabledLocked(id string) bool {
	return r.enabledModels == nil || r.enabledModels[id]
}
func (r *Router) ModelEnabled(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelEnabledLocked(id)
}

// SetModelEnabled atomically persists the complete selection before publishing it.
func (r *Router) SetModelEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.models[id]; !ok {
		return &UnknownModelError{Model: id}
	}
	return r.setModelsEnabledLocked([]string{id}, enabled)
}

// SetEasyCodeModelsEnabled changes discovered EasyCode models in one transaction.
func (r *Router) SetEasyCodeModelsEnabled(enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id := range r.models {
		if strings.HasPrefix(id, "easycode/") {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("no EasyCode models discovered")
	}
	return r.setModelsEnabledLocked(ids, enabled)
}

func (r *Router) setModelsEnabledLocked(changed []string, enabled bool) error {
	if r.selectionPath == "" {
		return fmt.Errorf("model selection is not configured")
	}
	selected := make(map[string]bool)
	if r.enabledModels == nil {
		for model := range r.models {
			selected[model] = true
		}
	} else {
		for model := range r.enabledModels {
			selected[model] = true
		}
	}
	for _, id := range changed {
		if enabled {
			selected[id] = true
		} else {
			delete(selected, id)
		}
	}
	ids := make([]string, 0, len(selected))
	for model := range selected {
		ids = append(ids, model)
	}
	sort.Strings(ids)
	data, err := json.MarshalIndent(modelSelectionFile{Version: 1, Enabled: ids}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(r.selectionPath), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(r.selectionPath), ".model-selection-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), r.selectionPath); err != nil {
		return err
	}
	r.enabledModels = selected
	return nil
}
