package api

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// errDetach is returned by escapeReader.Read when the detach key sequence is
// seen. io.Copy(dst, escapeReader) forwards the bytes before the sequence and
// then returns errDetach, so the caller can detach without killing the process.
var errDetach = errors.New("detached")

// defaultDetachKeys matches Docker's daemon default (ctrl-p, ctrl-q).
const defaultDetachKeys = "ctrl-p,ctrl-q"

// parseDetachKeys turns a Docker DetachKeys string ("ctrl-p,ctrl-q", "a,ctrl-x",
// …) into the raw byte sequence. An empty string yields the default. Port of
// moby's pkg/term ToBytes (implemented locally to avoid the dependency).
func parseDetachKeys(keys string) ([]byte, error) {
	if keys == "" {
		keys = defaultDetachKeys
	}
	var out []byte
	for _, tok := range strings.Split(keys, ",") {
		switch {
		case len(tok) == 1:
			out = append(out, tok[0])
		case strings.HasPrefix(tok, "ctrl-") && len(tok) == len("ctrl-")+1:
			c := tok[len("ctrl-")]
			switch {
			case c >= 'a' && c <= 'z':
				out = append(out, c-'a'+1) // ctrl-a=1 … ctrl-z=26
			case c == '@':
				out = append(out, 0)
			case c == '[':
				out = append(out, 27)
			case c == '\\':
				out = append(out, 28)
			case c == ']':
				out = append(out, 29)
			case c == '^':
				out = append(out, 30)
			case c == '_':
				out = append(out, 31)
			default:
				return nil, fmt.Errorf("invalid detach key %q", tok)
			}
		default:
			return nil, fmt.Errorf("invalid detach key %q", tok)
		}
	}
	return out, nil
}

// escapeReader wraps an input stream (the hijacked client connection) and scans
// it for the detach key sequence. Non-matching bytes pass through unchanged; the
// escape sequence itself is swallowed and Read returns errDetach once it is
// fully matched. A partial match at the end of a Read is withheld and carried
// into the next Read, so a sequence split across reads is still detected.
type escapeReader struct {
	r       io.Reader
	keys    []byte
	matched int    // length of the leading keys prefix matched so far (withheld)
	pending []byte // decided output that didn't fit the caller's buffer
}

func newEscapeReader(r io.Reader, keys []byte) *escapeReader {
	return &escapeReader{r: r, keys: keys}
}

func (er *escapeReader) Read(p []byte) (int, error) {
	if len(er.keys) == 0 {
		return er.r.Read(p)
	}
	if len(er.pending) > 0 {
		n := copy(p, er.pending)
		er.pending = er.pending[n:]
		return n, nil
	}
	tmp := make([]byte, len(p))
	n, err := er.r.Read(tmp)
	if n == 0 {
		return 0, err
	}

	out := make([]byte, 0, n+er.matched)
	for i := 0; i < n; i++ {
		b := tmp[i]
		if b == er.keys[er.matched] {
			er.matched++
			if er.matched == len(er.keys) {
				// Full sequence: emit the bytes decided before it, swallow the
				// sequence, drop the rest of this read (we're detaching), signal.
				er.matched = 0
				nc := copy(p, out)
				if nc < len(out) {
					er.pending = append(er.pending, out[nc:]...)
				}
				return nc, errDetach
			}
			continue
		}
		// Mismatch: the withheld prefix was real input — flush it, then re-test
		// this byte as a possible new start of the sequence.
		out = append(out, er.keys[:er.matched]...)
		er.matched = 0
		if b == er.keys[0] {
			er.matched = 1
		} else {
			out = append(out, b)
		}
	}
	nc := copy(p, out)
	if nc < len(out) {
		er.pending = append(er.pending, out[nc:]...)
	}
	return nc, err
}

// switchableWriter is a writer whose destination can be swapped atomically and
// which never reports an error to its caller. Exec output drains through it: on
// detach the destination is swapped from the (closed) client connection to
// io.Discard, and swallowing the write error keeps the single drain goroutine
// running so the process never blocks writing to a full pty/pipe.
type switchableWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newSwitchableWriter(w io.Writer) *switchableWriter { return &switchableWriter{w: w} }

func (s *switchableWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	_, _ = w.Write(p) // best-effort; errors swallowed so the drainer keeps going
	return len(p), nil
}

func (s *switchableWriter) set(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

// closeIfCloser closes v when it implements io.Closer.
func closeIfCloser(v any) {
	if c, ok := v.(io.Closer); ok {
		_ = c.Close()
	}
}
