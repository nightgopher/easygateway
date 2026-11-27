package privacy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gin-gonic/gin"
)

// Settings persists the single opt-in switch on the gateway host.
type Settings struct {
	mu   sync.Mutex
	path string
	gate *Gate
}
type settingsFile struct {
	Enabled bool `json:"enabled"`
}

func OpenSettings(path string, g *Gate) (*Settings, error) {
	s := &Settings{path: path, gate: g}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var v struct {
		Enabled *bool `json:"enabled"`
	}
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if v.Enabled == nil {
		return nil, fmt.Errorf("privacy settings missing enabled")
	}
	g.SetEnabled(*v.Enabled)
	return s, nil
}
func (s *Settings) HandleGet(c *gin.Context) { c.JSON(200, settingsFile{Enabled: s.gate.Enabled()}) }
func (s *Settings) HandlePut(c *gin.Context) {
	var b struct {
		Enabled *bool `json:"enabled"`
	}
	if c.ShouldBindJSON(&b) != nil || b.Enabled == nil {
		c.JSON(400, gin.H{"error": "enabled is required"})
		return
	}
	if err := s.Set(*b.Enabled); err != nil {
		c.JSON(500, gin.H{"error": "cannot save privacy settings"})
		return
	}
	c.JSON(200, settingsFile{Enabled: *b.Enabled})
}
func (s *Settings) Set(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(settingsFile{Enabled: enabled})
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".privacy-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("privacy settings: %w", err)
	}
	s.gate.SetEnabled(enabled)
	return nil
}

// HandlePreview tests current rules without sending content to an upstream.
func (s *Settings) HandlePreview(c *gin.Context) {
	var b struct {
		Text string `json:"text"`
	}
	if c.ShouldBindJSON(&b) != nil || len(b.Text) > 8192 {
		c.JSON(400, gin.H{"error": "preview text exceeds 8192 bytes"})
		return
	}
	e, err := NewEngine()
	if err != nil {
		c.JSON(500, gin.H{"error": "cannot create preview"})
		return
	}
	session := e.NewSession("preview")
	masked, err := session.Mask(b.Text)
	if err != nil {
		c.JSON(400, gin.H{"error": "preview cannot be processed"})
		return
	}
	c.JSON(200, gin.H{"masked": masked, "count": len(session.values)})
}
