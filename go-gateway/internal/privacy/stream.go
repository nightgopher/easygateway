package privacy

import (
	"encoding/json"
	"strings"
)

// StreamRestorer restores a single logical text stream. Callers must keep
// separate restorers for different tool calls and text fields, and Finish it
// at the end of the field. It must not be used on serialized JSON or SSE.
type StreamRestorer struct {
	session      *Session
	pending      string
	jsonFragment bool
}

func (s *Session) NewStreamRestorer() *StreamRestorer { return &StreamRestorer{session: s} }
func (r *StreamRestorer) Write(chunk string) string {
	r.pending += chunk
	var out strings.Builder
	for len(r.pending) > 0 {
		index := strings.Index(r.pending, "{{SECRET_")
		if index < 0 {
			// Preserve a suffix that may begin a token split across events.
			keep := 0
			prefix := "{{SECRET_"
			for n := 1; n < len(prefix) && n <= len(r.pending); n++ {
				if strings.HasSuffix(r.pending, prefix[:n]) {
					keep = n
				}
			}
			out.WriteString(r.pending[:len(r.pending)-keep])
			r.pending = r.pending[len(r.pending)-keep:]
			break
		}
		out.WriteString(r.pending[:index])
		r.pending = r.pending[index:]
		end := strings.Index(r.pending, "}}")
		if end < 0 {
			if len(r.pending) > 128 {
				out.WriteByte(r.pending[0])
				r.pending = r.pending[1:]
				continue
			}
			break
		}
		token := r.pending[:end+2]
		if value, ok := r.session.values[token]; ok && r.jsonFragment {
			escaped, _ := json.Marshal(value)
			out.Write(escaped[1 : len(escaped)-1])
		} else {
			out.WriteString(r.session.Restore(token))
		}
		r.pending = r.pending[end+2:]
	}
	return out.String()
}
func (r *StreamRestorer) Finish() string {
	result := r.session.Restore(r.pending)
	r.pending = ""
	return result
}
