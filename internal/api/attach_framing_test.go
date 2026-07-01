package api

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestWriteStreamFrameTTY: a TTY stream is raw — no stdcopy header.
func TestWriteStreamFrameTTY(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := writeStreamFrame(&buf, []byte("hello"), true); err != nil {
		t.Fatalf("writeStreamFrame: %v", err)
	}
	if got := buf.String(); got != "hello" {
		t.Fatalf("TTY frame = %q, want raw %q", got, "hello")
	}
}

// TestWriteStreamFrameNonTTY: a non-TTY chunk is prefixed with the 8-byte
// stdcopy header (stream id 1 = stdout, big-endian length).
func TestWriteStreamFrameNonTTY(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	payload := []byte("hello world")
	if err := writeStreamFrame(&buf, payload, false); err != nil {
		t.Fatalf("writeStreamFrame: %v", err)
	}
	out := buf.Bytes()
	if len(out) != 8+len(payload) {
		t.Fatalf("framed length = %d, want %d", len(out), 8+len(payload))
	}
	if out[0] != 1 {
		t.Fatalf("stream id = %d, want 1 (stdout)", out[0])
	}
	if n := binary.BigEndian.Uint32(out[4:8]); int(n) != len(payload) {
		t.Fatalf("frame length header = %d, want %d", n, len(payload))
	}
	if !bytes.Equal(out[8:], payload) {
		t.Fatalf("frame payload = %q, want %q", out[8:], payload)
	}
}

// TestWriteFramedStreamNonTTYReplayIsFramed is the regression for the attach
// logs=1 bug: replayed history on a non-TTY container must be stdcopy-framed
// exactly like the live stream, so a demuxing client never reads log content
// as a bogus frame header. The whole replay must be parseable as a sequence of
// stdcopy frames whose payloads reconstruct the original log.
func TestWriteFramedStreamNonTTYReplayIsFramed(t *testing.T) {
	t.Parallel()
	// Larger than the 32KiB read buffer so the replay spans multiple frames.
	original := strings.Repeat("logline\n", 8*1024) // 64 KiB
	var buf bytes.Buffer
	if err := writeFramedStream(&buf, strings.NewReader(original), false); err != nil {
		t.Fatalf("writeFramedStream: %v", err)
	}

	// Walk the output as stdcopy frames and reassemble the payload.
	var reassembled bytes.Buffer
	rest := buf.Bytes()
	for len(rest) > 0 {
		if len(rest) < 8 {
			t.Fatalf("trailing %d bytes without a full header — stream is not cleanly framed", len(rest))
		}
		if rest[0] != 1 {
			t.Fatalf("frame stream id = %d, want 1", rest[0])
		}
		n := binary.BigEndian.Uint32(rest[4:8])
		rest = rest[8:]
		if int(n) > len(rest) {
			t.Fatalf("frame claims %d bytes but only %d remain", n, len(rest))
		}
		reassembled.Write(rest[:n])
		rest = rest[n:]
	}
	if reassembled.String() != original {
		t.Fatalf("reassembled replay does not match original (len %d vs %d)", reassembled.Len(), len(original))
	}
}

// TestWriteFramedStreamTTYReplayIsRaw: on a TTY container the replay is raw.
func TestWriteFramedStreamTTYReplayIsRaw(t *testing.T) {
	t.Parallel()
	original := "raw tty output\nno headers\n"
	var buf bytes.Buffer
	if err := writeFramedStream(&buf, strings.NewReader(original), true); err != nil {
		t.Fatalf("writeFramedStream: %v", err)
	}
	if buf.String() != original {
		t.Fatalf("TTY replay = %q, want raw %q", buf.String(), original)
	}
}
