package privacy

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestMaskRestoreAndIsolate(t *testing.T) {
	e, err := NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	a := e.NewSession("identity-a")
	b := e.NewSession("identity-b")
	c := e.NewSession("identity-a")
	secret := "sk-" + strings.Repeat("a", 24)
	masked, err := a.Mask("write " + secret)
	if err != nil || strings.Contains(masked, secret) {
		t.Fatalf("mask failed: %v", err)
	}
	if got := a.Restore(masked); got != "write "+secret {
		t.Fatal("restore failed")
	}
	if b.Restore(masked) != masked {
		t.Fatal("different request restored token")
	}
	same, err := c.Mask("write " + secret)
	if err != nil || same != masked {
		t.Fatal("same identity must have stable placeholder")
	}
	other, err := b.Mask("write " + secret)
	if err != nil || other == masked {
		t.Fatal("identities share placeholder")
	}
}
func TestAssignmentsAndKnownValues(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	secret := "abcdefghijk12345"
	first, err := s.Mask("password=" + secret)
	if err != nil || strings.Contains(first, secret) {
		t.Fatal("assignment leaked")
	}
	again, err := s.Mask("use " + secret)
	if err != nil || strings.Contains(again, secret) || s.Restore(again) != "use "+secret {
		t.Fatal("known value leaked")
	}
}
func TestConcurrentSessions(t *testing.T) {
	e, _ := NewEngine()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := e.NewSession("scope")
			original := "sk-" + strings.Repeat("x", 20)
			masked, err := s.Mask(original)
			if err != nil || s.Restore(masked) != original {
				t.Error("roundtrip")
			}
		}()
	}
	wg.Wait()
}

func TestStreamRestoresFragmentedTokens(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	secret := "sk-" + strings.Repeat("x", 24)
	masked, err := s.Mask(secret)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= len(masked); i++ {
		r := s.NewStreamRestorer()
		got := r.Write("before "+masked[:i]) + r.Write(masked[i:]+" after") + r.Finish()
		if got != "before "+secret+" after" {
			t.Fatalf("split %d: %q", i, got)
		}
	}
}

func TestJSONLeavesProtocolFieldsAndProtectsToolArguments(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	secret := "sk-" + strings.Repeat("z", 24)
	body := `{"model":"model-1","messages":[{"role":"user","content":"` + secret + `"},{"role":"assistant","tool_calls":[{"id":"call-1","function":{"name":"write","arguments":"{\"key\":\"` + secret + `\"}"}}]}]}`
	result, err := s.MaskJSON([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), secret) || !strings.Contains(string(result), `"model":"model-1"`) || !strings.Contains(string(result), `"name":"write"`) {
		t.Fatalf("unexpected: %s", result)
	}
}
func TestJSONRejectsSensitiveSignedContent(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	secret := "sk-" + strings.Repeat("z", 24)
	_, err := s.MaskJSON([]byte(`{"content":[{"type":"thinking","thinking":"` + secret + `","signature":"abc"}]}`))
	if err != ErrSignedContent {
		t.Fatalf("expected rejection, got %v", err)
	}
}

func TestStructuredToolInputMasksUnknownCredentialFormat(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("a")
	secret := "custom-credential-987654"
	body := `{"content":[{"type":"tool_use","input":{"api_key":"` + secret + `","name":"ordinary"}}]}`
	masked, err := s.MaskJSON([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(masked), secret) {
		t.Fatal("tool credential leaked")
	}
	if !strings.Contains(s.Restore(string(masked)), secret) {
		t.Fatal("credential not restored")
	}
}

func TestJSONEncodedToolArgumentsProtectUnknownKey(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("user")
	secret := "new-vendor-credential-345"
	body := `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"write","arguments":"{\"api_key\":\"` + secret + `\"}"}}]}]}`
	result, err := s.MaskJSON([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), secret) {
		t.Fatal("encoded credential leaked")
	}
	if !strings.Contains(s.Restore(string(result)), secret) {
		t.Fatal("credential not restored")
	}
}

func TestRestoreJSONEscapesCredentialInToolArguments(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("user")
	secret := "pass\"word\\test"
	token, err := s.maskSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := json.Marshal(map[string]string{"api_key": token})
	body, _ := json.Marshal(map[string]any{"function": map[string]string{"arguments": string(inner)}})
	restored, err := s.RestoreJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Function struct {
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(restored, &result); err != nil {
		t.Fatal(err)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(result.Function.Arguments), &args); err != nil {
		t.Fatal(err)
	}
	if args["api_key"] != secret {
		t.Fatal("credential changed")
	}
}

func TestRestoreJSONPreservesSignedReasoning(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("user")
	token, _ := s.maskSecret("sensitive-key")
	b, _ := json.Marshal(map[string]any{"type": "thinking", "thinking": token, "signature": "sig"})
	got, err := s.RestoreJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), token) || strings.Contains(string(got), "sensitive-key") {
		t.Fatal("signed content was changed")
	}
}

func TestRestoreJSONRestoresToolInputFieldsNamedLikeProtocolFields(t *testing.T) {
	e, _ := NewEngine()
	s := e.NewSession("user")
	token, _ := s.maskSecret("account-name")
	body, _ := json.Marshal(map[string]any{"type": "tool_use", "name": "write", "input": map[string]any{"name": token}})
	restored, err := s.RestoreJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "account-name") {
		t.Fatalf("tool input not restored: %s", restored)
	}
}
