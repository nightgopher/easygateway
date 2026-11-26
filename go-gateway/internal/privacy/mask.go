// Package privacy contains request-scoped reversible masking primitives.
// Values are held only by a request's Session, never logged or persisted.
package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"regexp"
	"sort"
	"strings"
)

var ErrCapacity = errors.New("privacy: too many sensitive values")

// Engine's random key is process-local. Callers must supply a stable, trusted
// authenticated scope, never a caller-supplied session header by itself.
type Engine struct{ key [32]byte }

func NewEngine() (*Engine, error) {
	e := new(Engine)
	_, err := rand.Read(e.key[:])
	if err != nil {
		return nil, err
	}
	return e, nil
}

type Session struct {
	engine    *Engine
	scope     string
	values    map[string]string
	originals map[string]string
}

func (e *Engine) NewSession(scope string) *Session {
	return &Session{engine: e, scope: scope, values: map[string]string{}, originals: map[string]string{}}
}

var patterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:sk-[A-Za-z0-9._-]{12,}|gh[pousr]_[A-Za-z0-9_]{12,}|github_pat_[A-Za-z0-9_]{12,}|AIza[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16})\b`),
	regexp.MustCompile(`(?i)\bBearer\s+([A-Za-z0-9._~+/=-]{8,})`),
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|password|passwd|secret)\s*[:=]\s*["']?([^\s"',;]{8,})`),
}

// Mask masks detected credentials and values already identified in this request.
// It does not perform protocol parsing or identify every possible credential.
func (s *Session) Mask(text string) (string, error) {
	type span struct{ start, end int }
	var spans []span
	for _, p := range patterns {
		for _, m := range p.FindAllStringSubmatchIndex(text, -1) {
			if len(m) > 3 && m[2] >= 0 {
				spans = append(spans, span{m[2], m[3]})
			} else {
				spans = append(spans, span{m[0], m[1]})
			}
		}
	}
	for value := range s.originals {
		for at := 0; at < len(text); {
			i := strings.Index(text[at:], value)
			if i < 0 {
				break
			}
			i += at
			spans = append(spans, span{i, i + len(value)})
			at = i + len(value)
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	var b strings.Builder
	pos := 0
	for _, v := range spans {
		if v.start < pos {
			continue
		}
		original := text[v.start:v.end]
		placeholder, err := s.maskSecret(original)
		if err != nil {
			return "", err
		}
		b.WriteString(text[pos:v.start])
		b.WriteString(placeholder)
		pos = v.end
	}
	b.WriteString(text[pos:])
	return b.String(), nil
}

func (s *Session) maskSecret(original string) (string, error) {
	if placeholder, ok := s.originals[original]; ok {
		return placeholder, nil
	}
	if len(s.values) >= 512 {
		return "", ErrCapacity
	}
	mac := hmac.New(sha256.New, s.engine.key[:])
	mac.Write([]byte(s.scope))
	mac.Write([]byte{0})
	mac.Write([]byte(original))
	placeholder := "{{SECRET_" + strings.TrimRight(base32.StdEncoding.EncodeToString(mac.Sum(nil)[:16]), "=") + "}}"
	s.values[placeholder] = original
	s.originals[original] = placeholder
	return placeholder, nil
}

// Restore replaces only tokens emitted by this request. Input from a different
// authenticated scope, a stale request, or a model-invented token is untouched.
func (s *Session) Restore(text string) string {
	for placeholder, value := range s.values {
		text = strings.ReplaceAll(text, placeholder, value)
	}
	return text
}
