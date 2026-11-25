package provider

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func TestModelSelectionPersistenceAndNewModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selection.json")
	newRouter := func() *Router {
		r := NewRouter(log.New(io.Discard, "", 0))
		r.RegisterProvider("p", &errorPolicyProvider{name: "p"})
		r.RegisterChain("a", []string{"p"})
		r.RegisterChain("b", []string{"p"})
		if err := r.LoadModelSelection(path); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := newRouter()
	if !r.ModelEnabled("a") || !r.ModelEnabled("b") {
		t.Fatal("legacy models should remain enabled")
	}
	if err := r.SetModelEnabled("a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Route("a"); err == nil {
		t.Fatal("disabled model is routable")
	}
	if _, err := r.Route("b"); err != nil {
		t.Fatalf("shared provider disabled: %v", err)
	}
	r.RegisterChain("new", []string{"p"})
	if r.ModelEnabled("new") {
		t.Fatal("new model automatically enabled")
	}
	restarted := newRouter()
	if restarted.ModelEnabled("a") || !restarted.ModelEnabled("b") {
		t.Fatal("selection not restored")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file permissions: %v %v", info, err)
	}
}

func TestInvalidSelectionDoesNotDefaultToEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selection.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(log.New(io.Discard, "", 0))
	if err := r.LoadModelSelection(path); err == nil {
		t.Fatal("corrupt selection accepted")
	}
}

func TestEasyCodeBulkSelectionLeavesOtherModelsEnabled(t *testing.T) {
	r := NewRouter(log.New(io.Discard, "", 0))
	r.RegisterProvider("p", &errorPolicyProvider{name: "p"})
	r.RegisterChain("easycode/a", []string{"p"})
	r.RegisterChain("easycode/b", []string{"p"})
	r.RegisterChain("coding", []string{"p"})
	if err := r.LoadModelSelection(filepath.Join(t.TempDir(), "selection.json")); err != nil {
		t.Fatal(err)
	}
	if err := r.SetEasyCodeModelsEnabled(false); err != nil {
		t.Fatal(err)
	}
	if r.ModelEnabled("easycode/a") || r.ModelEnabled("easycode/b") || !r.ModelEnabled("coding") {
		t.Fatal("bulk selection changed the wrong models")
	}
}
