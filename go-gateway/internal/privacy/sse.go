package privacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SSEDecoder incrementally restores JSON strings without joining unrelated
// streams (e.g. separate parallel tool calls). Output frames may be held while
// a placeholder prefix is incomplete. It has not yet been wired to HTTP.
type SSEDecoder struct {
	session   *Session
	input     []byte
	held      []*sseFrame
	heldBytes int
	streams   map[string]*sseStream
}
type sseFrame struct {
	raw            []byte
	value          any
	prefix, suffix []byte
	modified       bool
}
type sseStream struct {
	restorer *StreamRestorer
	last     *sseSlot
}
type sseSlot struct {
	object map[string]any
	key    string
}

func (s *Session) NewSSEDecoder() *SSEDecoder {
	return &SSEDecoder{session: s, streams: map[string]*sseStream{}}
}

var ErrSSELimit = errors.New("privacy: buffered stream limit exceeded")

const maxSSEBuffer = 2 << 20

// Push accepts arbitrary transport chunks and returns zero or more complete
// SSE frames. The caller must abort the stream if an error is returned.
func (d *SSEDecoder) Push(chunk []byte) ([]byte, error) {
	d.input = append(d.input, chunk...)
	if len(d.input) > maxSSEBuffer {
		return nil, ErrSSELimit
	}
	var out []byte
	for {
		at := bytes.Index(d.input, []byte("\n\n"))
		width := 2
		cr := bytes.Index(d.input, []byte("\r\n\r\n"))
		if cr >= 0 && (at < 0 || cr < at) {
			at = cr
			width = 4
		}
		if at < 0 {
			break
		}
		frame := append([]byte(nil), d.input[:at+width]...)
		d.input = d.input[at+width:]
		f, terminal, err := d.decode(frame)
		if err != nil {
			return nil, err
		}
		d.held = append(d.held, f)
		d.heldBytes += len(frame)
		if d.heldBytes > maxSSEBuffer {
			return nil, ErrSSELimit
		}
		if terminal {
			d.finishPending()
		}
		if len(d.held) > 128 {
			return nil, ErrSSELimit
		}
		if !d.pending() {
			part, err := d.drain()
			if err != nil {
				return nil, err
			}
			out = append(out, part...)
		}
	}
	return out, nil
}
func (d *SSEDecoder) Finish() ([]byte, error) {
	if len(d.input) > 0 {
		return nil, errors.New("privacy: incomplete SSE frame")
	}
	d.finishPending()
	return d.drain()
}
func (d *SSEDecoder) pending() bool {
	for _, s := range d.streams {
		if s.restorer.pending != "" {
			return true
		}
	}
	return false
}
func (d *SSEDecoder) finishPending() {
	for _, s := range d.streams {
		tail := s.restorer.Finish()
		if tail != "" && s.last != nil {
			s.last.object[s.last.key] = s.last.object[s.last.key].(string) + tail
		}
	}
}
func (d *SSEDecoder) drain() ([]byte, error) {
	var out []byte
	for _, f := range d.held {
		if !f.modified {
			out = append(out, f.raw...)
			continue
		}
		b, err := json.Marshal(f.value)
		if err != nil {
			return nil, err
		}
		out = append(out, f.prefix...)
		out = append(out, b...)
		out = append(out, f.suffix...)
	}
	d.held = nil
	d.heldBytes = 0
	return out, nil
}
func (d *SSEDecoder) decode(raw []byte) (*sseFrame, bool, error) {
	f := &sseFrame{raw: raw}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	pos := -1
	off := 0
	for _, line := range lines {
		trim := bytes.TrimRight(line, "\r\n")
		if bytes.HasPrefix(trim, []byte("data:")) {
			if pos >= 0 {
				return nil, false, errors.New("privacy: multiline SSE data unsupported")
			}
			pos = off + 5
			if pos < len(raw) && raw[pos] == ' ' {
				pos++
			}
			end := off + len(trim)
			f.prefix = append([]byte(nil), raw[:pos]...)
			f.suffix = append([]byte(nil), raw[end:]...)
			payload := raw[pos:end]
			if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
				return f, true, nil
			}
			dec := json.NewDecoder(bytes.NewReader(payload))
			dec.UseNumber()
			if err := dec.Decode(&f.value); err != nil {
				return nil, false, err
			}
		}
		off += len(line)
	}
	if pos < 0 {
		return f, false, nil
	}
	root, ok := f.value.(map[string]any)
	if !ok {
		return f, false, nil
	}
	typ, _ := root["type"].(string)
	terminal := typ == "message_stop" || typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete"
	context := typ + ":" + fmt.Sprint(root["output_index"]) + ":" + fmt.Sprint(root["content_index"]) + ":" + fmt.Sprint(root["index"]) + ":" + fmt.Sprint(root["item_id"])
	d.walk(root, context, "", false, f)
	return f, terminal, nil
}
func (d *SSEDecoder) walk(value any, ctx, path string, args bool, f *sseFrame) {
	switch v := value.(type) {
	case map[string]any:
		typ, _ := v["type"].(string)
		if !args && (typ == "thinking" || typ == "redacted_thinking" || typ == "reasoning") {
			return
		}
		identity := ""
		if i, ok := v["index"]; ok {
			identity = "#" + fmt.Sprint(i)
		}
		for k, x := range v {
			next := path + "/" + k + identity
			tool := args || k == "arguments" || k == "partial_json" || (k == "input" && strings.HasSuffix(typ, "tool_use"))
			if str, ok := x.(string); ok {
				if (!tool && protocolField(k)) || k == "signature" || k == "thoughtSignature" || k == "encrypted_content" {
					continue
				}
				id := ctx + next
				st := d.streams[id]
				if st == nil {
					st = &sseStream{restorer: d.session.NewStreamRestorer()}
					st.restorer.jsonFragment = tool && (k == "arguments" || k == "partial_json")
					d.streams[id] = st
				}
				changed := st.restorer.Write(str)
				if changed != str {
					v[k] = changed
					f.modified = true
				}
				st.last = &sseSlot{object: v, key: k}
			} else {
				d.walk(x, ctx, next, tool, f)
			}
		}
	case []any:
		for i, x := range v {
			d.walk(x, ctx, path+"/"+strconv.Itoa(i), args, f)
		}
	}
}
