package api

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestParseDetachKeys(t *testing.T) {
	t.Parallel()
	ok := map[string][]byte{
		"":              {0x10, 0x11}, // default ctrl-p,ctrl-q
		"ctrl-p,ctrl-q": {0x10, 0x11},
		"ctrl-a":        {1},
		"ctrl-z":        {26},
		"ctrl-@":        {0},
		"ctrl-_":        {31},
		"a":             {'a'},
		"a,ctrl-x":      {'a', 24},
	}
	for in, want := range ok {
		got, err := parseDetachKeys(in)
		if err != nil {
			t.Errorf("parseDetachKeys(%q) error: %v", in, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("parseDetachKeys(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"ctrl-1", "ctrl-ab", "foobar", "ctrl-"} {
		if _, err := parseDetachKeys(bad); err == nil {
			t.Errorf("parseDetachKeys(%q) should error", bad)
		}
	}
}

// chunkReader hands out its data in the exact chunk boundaries given, so tests
// can force a sequence to straddle two Reads.
type chunkReader struct {
	chunks [][]byte
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	c.chunks[0] = c.chunks[0][n:]
	if len(c.chunks[0]) == 0 {
		c.chunks = c.chunks[1:]
	}
	return n, nil
}

// drain reads er via io.Copy and reports the forwarded bytes and whether detach
// fired.
func drain(t *testing.T, er *escapeReader) (forwarded []byte, detached bool) {
	t.Helper()
	var buf bytes.Buffer
	_, err := io.Copy(&buf, er)
	return buf.Bytes(), errors.Is(err, errDetach)
}

func TestEscapeReaderNoSequence(t *testing.T) {
	t.Parallel()
	er := newEscapeReader(bytes.NewReader([]byte("hello world")), []byte{0x10, 0x11})
	got, detached := drain(t, er)
	if detached {
		t.Fatal("no sequence present, must not detach")
	}
	if string(got) != "hello world" {
		t.Fatalf("forwarded %q, want unchanged", got)
	}
}

func TestEscapeReaderFullSequenceOneRead(t *testing.T) {
	t.Parallel()
	er := newEscapeReader(bytes.NewReader([]byte{'a', 'b', 0x10, 0x11, 'c'}), []byte{0x10, 0x11})
	got, detached := drain(t, er)
	if !detached {
		t.Fatal("sequence present, must detach")
	}
	// Bytes before the sequence forwarded; the sequence swallowed; 'c' dropped.
	if !bytes.Equal(got, []byte{'a', 'b'}) {
		t.Fatalf("forwarded %v, want [a b]", got)
	}
}

func TestEscapeReaderSplitAcrossReads(t *testing.T) {
	t.Parallel()
	// 0x10 alone in read 1, 0x11 alone in read 2 → detach; neither escape byte
	// is forwarded.
	er := newEscapeReader(&chunkReader{chunks: [][]byte{{'x', 0x10}, {0x11, 'y'}}}, []byte{0x10, 0x11})
	got, detached := drain(t, er)
	if !detached {
		t.Fatal("split sequence must still detach")
	}
	if !bytes.Equal(got, []byte{'x'}) {
		t.Fatalf("forwarded %v, want [x] (escape bytes withheld)", got)
	}
}

func TestEscapeReaderBrokenPartial(t *testing.T) {
	t.Parallel()
	// 0x10 then a non-0x11 byte: the withheld 0x10 must be flushed and both
	// forwarded; no detach.
	er := newEscapeReader(bytes.NewReader([]byte{0x10, 'A', 'B'}), []byte{0x10, 0x11})
	got, detached := drain(t, er)
	if detached {
		t.Fatal("broken partial must not detach")
	}
	if !bytes.Equal(got, []byte{0x10, 'A', 'B'}) {
		t.Fatalf("forwarded %v, want [0x10 A B]", got)
	}
}

func TestEscapeReaderDoubleStart(t *testing.T) {
	t.Parallel()
	// 0x10 0x10 0x11: first 0x10 is real input, the last two are the sequence.
	er := newEscapeReader(bytes.NewReader([]byte{0x10, 0x10, 0x11}), []byte{0x10, 0x11})
	got, detached := drain(t, er)
	if !detached {
		t.Fatal("must detach on the trailing sequence")
	}
	if !bytes.Equal(got, []byte{0x10}) {
		t.Fatalf("forwarded %v, want [0x10] (first byte real input)", got)
	}
}

func TestEscapeReaderEmptyKeys(t *testing.T) {
	t.Parallel()
	// No keys → pure pass-through, even for bytes that would otherwise match.
	er := newEscapeReader(bytes.NewReader([]byte{0x10, 0x11, 'z'}), nil)
	got, detached := drain(t, er)
	if detached {
		t.Fatal("empty keys must never detach")
	}
	if !bytes.Equal(got, []byte{0x10, 0x11, 'z'}) {
		t.Fatalf("forwarded %v, want all bytes", got)
	}
}

func TestSwitchableWriterSwallowsAndSwaps(t *testing.T) {
	t.Parallel()
	var target bytes.Buffer
	s := newSwitchableWriter(&target)
	if _, err := s.Write([]byte("hi")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	// Swap to a writer that errors — switchableWriter must still report success
	// (so a draining io.Copy keeps going).
	s.set(errWriter{})
	if n, err := s.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("after swap Write = (%d,%v), want (4,nil)", n, err)
	}
	// Swap to discard.
	s.set(io.Discard)
	if _, err := s.Write([]byte("x")); err != nil {
		t.Fatalf("discard Write error: %v", err)
	}
	if target.String() != "hi" {
		t.Fatalf("first target got %q, want hi", target.String())
	}
}

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("boom") }
