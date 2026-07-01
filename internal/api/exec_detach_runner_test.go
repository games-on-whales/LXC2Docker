package api

import (
	"bytes"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// pipeConn is a minimal in-memory stand-in for the hijacked exec connection:
// Read yields queued input (the client's stdin), Write captures output, and
// Close unblocks a pending Read.
type pipeConn struct {
	in     chan []byte
	rest   []byte
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	out    bytes.Buffer
}

func newPipeConn() *pipeConn {
	return &pipeConn{in: make(chan []byte, 4), closed: make(chan struct{})}
}

func (c *pipeConn) Read(p []byte) (int, error) {
	for len(c.rest) == 0 {
		select {
		case b, ok := <-c.in:
			if !ok {
				return 0, io.EOF
			}
			c.rest = b
		case <-c.closed:
			return 0, io.EOF
		}
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}

func (c *pipeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}

func (c *pipeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// TestRunExecMuxDetachKeepsProcessAlive is the load-bearing teardown test:
// pressing the detach sequence must return detached=true, leave the process
// RUNNING (not killed), and defer onExit until the process actually exits.
func TestRunExecMuxDetachKeepsProcessAlive(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	cmd := exec.Command("sleep", "30")
	conn := newPipeConn()
	conn.in <- []byte{0x10, 0x11} // the default detach sequence

	exitCh := make(chan int, 1)
	detached := runExecMux(cmd, conn, true, []byte{0x10, 0x11}, nil, func(code int) { exitCh <- code })

	if !detached {
		t.Fatal("expected detached=true")
	}
	if cmd.Process == nil {
		t.Fatal("process was never started")
	}
	// The process must still be alive right after detach (signal 0 = liveness).
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process should be alive after detach, got: %v", err)
	}
	// onExit must NOT have fired yet — the process is still running.
	select {
	case c := <-exitCh:
		t.Fatalf("onExit fired (code %d) before the process exited", c)
	case <-time.After(100 * time.Millisecond):
	}
	// Now end the process; onExit must fire from the background waiter.
	_ = cmd.Process.Kill()
	select {
	case <-exitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("onExit never fired after the process exited")
	}
}

// TestRunExecMuxNormalExitCallsOnExit: without a detach, the process runs to
// completion and onExit is called with its exit code (no detach).
func TestRunExecMuxNormalExitCallsOnExit(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("true not available")
	}
	cmd := exec.Command("sh", "-c", "exit 7")
	conn := newPipeConn()
	// No input, then EOF (client closes stdin).
	close(conn.in)

	exitCh := make(chan int, 1)
	detached := runExecMux(cmd, conn, true, []byte{0x10, 0x11}, nil, func(code int) { exitCh <- code })

	if detached {
		t.Fatal("no detach sequence sent; expected detached=false")
	}
	select {
	case code := <-exitCh:
		if code != 7 {
			t.Fatalf("onExit code = %d, want 7", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onExit never fired on normal exit")
	}
}
