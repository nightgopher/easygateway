package provider

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nightgopher/easygateway/internal/corspolicy"
	"gopkg.in/yaml.v3"
)

func (r *ConfigReloader) CORSConfig() corspolicy.Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.corsPolicy == nil {
		return corspolicy.Config{AllowedOrigins: []string{}}
	}
	return r.corsPolicy.Config()
}

// UpdateCORS validates and stages before writing. Failed validation/build/write
// leaves both the file and live policy untouched. Rename commits the file.
func (r *ConfigReloader) UpdateCORS(origins []string) (corspolicy.Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	normalized, err := corspolicy.NormalizeOrigins(origins)
	if err != nil {
		return corspolicy.Config{}, err
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return corspolicy.Config{}, err
	}
	cfg, err := parseProvidersConfig(data)
	if err != nil {
		return corspolicy.Config{}, err
	}
	cfg.CORS = corspolicy.Config{AllowedOrigins: normalized}
	candidate, err := r.prepare(cfg)
	if err != nil {
		return corspolicy.Config{}, err
	}
	original := append([]byte(nil), data...)
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return corspolicy.Config{}, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return corspolicy.Config{}, fmt.Errorf("configuration must be a YAML mapping")
	}
	root := doc.Content[0]
	corsNode := yamlMapValue(root, "cors")
	if corsNode == nil || corsNode.Kind != yaml.MappingNode {
		corsNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setYAMLMapValue(root, "cors", corsNode)
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, origin := range normalized {
		list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: origin})
	}
	setYAMLMapValue(corsNode, "allowed_origins", list)
	data, err = yaml.Marshal(&doc)
	if err != nil {
		return corspolicy.Config{}, err
	}
	current, err := os.ReadFile(r.path)
	if err != nil {
		return corspolicy.Config{}, err
	}
	if !bytes.Equal(current, original) {
		return corspolicy.Config{}, fmt.Errorf("providers.yaml changed while editing; reload and retry")
	}
	if err := writeConfigAtomic(r.path, data); err != nil {
		return corspolicy.Config{}, err
	}
	r.apply(candidate)
	return cfg.CORS, nil
}

func yamlMapValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func setYAMLMapValue(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func writeConfigAtomic(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".providers-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
