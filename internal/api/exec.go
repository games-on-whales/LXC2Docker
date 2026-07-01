package api

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/mux"
)

// execStore holds in-flight and completed exec instances.
type execStore struct {
	mu      sync.RWMutex
	records map[string]*execRecord
}

func newExecStore() *execStore {
	return &execStore{records: make(map[string]*execRecord)}
}

func (s *execStore) add(r *execRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.ID] = r
}

func (s *execStore) get(id string) *execRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.records[id]
}

// claimStart atomically marks an exec as started for the first time, returning
// false if it was already started (running or finished). Docker rejects a
// second start of the same exec instance with 409.
func (s *execStore) claimStart(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok || r.Running || !r.StartedAt.IsZero() {
		return false
	}
	r.Running = true
	r.StartedAt = time.Now()
	return true
}

// releaseStart undoes a claimStart when the exec never actually ran (e.g. the
// connection could not be hijacked), so the client can retry.
func (s *execStore) releaseStart(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[id]; ok {
		r.Running = false
		r.StartedAt = time.Time{}
	}
}

// idsForContainer returns the exec instance IDs tracked for a container, for
// ContainerJSON.ExecIDs. Returns nil (→ JSON null, as Docker emits) when none.
func (s *execStore) idsForContainer(cid string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for id, r := range s.records {
		if r.ContainerID == cid {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *execStore) update(id string, fn func(*execRecord)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[id]; ok {
		fn(r)
	}
}

// prune removes completed exec records older than 5 minutes.
func (s *execStore) prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-5 * time.Minute)
	for id, r := range s.records {
		if !r.Running && r.StartedAt.Before(cutoff) {
			delete(s.records, id)
		}
	}
}

// POST /containers/{id}/exec
func (h *Handler) execCreate(w http.ResponseWriter, r *http.Request) {
	containerID := h.resolveID(mux.Vars(r)["id"])
	if containerID == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if state, _ := h.mgr.State(containerID); state != "running" && state != "paused" {
		errResponse(w, http.StatusConflict, "Container is not running")
		return
	}

	var req ExecCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	if len(req.Cmd) == 0 {
		errResponse(w, http.StatusBadRequest, "Cmd is required")
		return
	}

	// Merge the exec's explicit env with the container's env so PATH,
	// HOME, and other image-level defaults are available to the session.
	// Request vars win over container vars, matching `docker exec` behavior.
	var mergedEnv []string
	if c := h.store.GetContainer(containerID); c != nil {
		mergedEnv = mergeEnv(c.Env, req.Env)
	} else {
		mergedEnv = req.Env
	}

	rec := &execRecord{
		ID:           generateID(),
		ContainerID:  containerID,
		Cmd:          req.Cmd,
		Tty:          req.Tty,
		DetachKeys:   req.DetachKeys,
		AttachStdin:  req.AttachStdin,
		AttachStdout: req.AttachStdout,
		AttachStderr: req.AttachStderr,
		Env:          mergedEnv,
		WorkingDir:   req.WorkingDir,
		User:         req.User,
		Privileged:   req.Privileged,
	}
	h.execs.add(rec)

	jsonResponse(w, http.StatusCreated, ExecCreateResponse{ID: rec.ID})
}

// POST /exec/{id}/start
func (h *Handler) execStart(w http.ResponseWriter, r *http.Request) {
	execID := mux.Vars(r)["id"]
	rec := h.execs.get(execID)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such exec instance")
		return
	}

	var req ExecStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Honor WorkingDir by wrapping the user's argv in `sh -c "cd <dir>
	// && exec <cmd>"`. Docker's exec honors -w this way when the base
	// runtime doesn't expose chdir-before-exec directly; lxc-attach and
	// pct exec both start at the container's default cwd, so the wrap
	// is the simplest reliable implementation.
	execCmd := rec.Cmd
	if rec.WorkingDir != "" {
		execCmd = wrapCmdWithCwd(rec.Cmd, rec.WorkingDir)
	}

	// An exec instance may only be started once. Docker rejects a second start
	// with 409. Claim it atomically so concurrent or repeat starts don't run
	// the command twice.
	if !h.execs.claimStart(rec.ID) {
		errResponse(w, http.StatusConflict, fmt.Sprintf("exec instance %s is already started", execID))
		return
	}

	if req.Detach {
		// Fire-and-forget: run the command, don't stream output. Docker returns
		// 200 (not 204) for a detached exec start.
		cmd := h.mgr.ExecAs(rec.ContainerID, execCmd, rec.Env, rec.User)
		h.startDetachedExec(rec.ID, cmd)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Hijack the connection for streaming.
	hj, ok := w.(http.Hijacker)
	if !ok {
		h.execs.releaseStart(rec.ID) // never ran; let the client retry
		errResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		h.execs.releaseStart(rec.ID)
		return
	}
	// conn is closed by runExecTTY/runExecMux or deferred below for non-TTY.
	closeConn := true
	defer func() {
		if closeConn {
			conn.Close()
		}
	}()

	// Write HTTP response preamble manually. Match Docker's exec-start
	// semantics: a client that asked to upgrade the protocol (Upgrade: tcp)
	// gets 101 Switching Protocols; every other client gets the attached
	// stream over a plain 200 OK. Returning 101 unconditionally breaks
	// non-upgrading clients — e.g. Wolf's fake-udev exec, which expects 200
	// and treats the 101 as an error, abandoning the hijacked stream
	// mid-command so the `mknod && fake-udev` chain never completes and
	// hot-plugged controllers never reach the app (SDL/Steam) container.
	// A non-TTY exec streams stdcopy-multiplexed frames (runExecMux), so the
	// advertised media type must reflect that just like attach/logs — raw for a
	// TTY exec, multiplexed for non-TTY (gated on API >= 1.42).
	writeHijackPreamble(buf, r.Header.Get("Upgrade") != "", streamContentType(r, rec.Tty))
	buf.Flush()

	// Running/StartedAt were set atomically by claimStart above.
	cmd := h.mgr.ExecAs(rec.ContainerID, execCmd, rec.Env, rec.User)

	// Resolve the detach key sequence (default ctrl-p,ctrl-q). Docker is lenient
	// on a malformed value (the client already validated), so fall back to the
	// default rather than failing the exec.
	detachKeys, dkErr := parseDetachKeys(rec.DetachKeys)
	if dkErr != nil {
		detachKeys, _ = parseDetachKeys("")
	}

	// onExit marks the exec finished. The runner calls it synchronously when the
	// process exits; on detach it fires later from the runner's background
	// waiter, so the record stays Running until the process actually ends.
	onExit := func(code int) {
		h.execs.update(rec.ID, func(r *execRecord) {
			r.Running = false
			r.Pid = 0
			r.ExitCode = code
			r.Pty = nil
		})
	}

	// The runners own the connection lifecycle (they close it on exit/detach).
	closeConn = false
	if rec.Tty {
		runExecTTY(cmd, conn, detachKeys, func(ptmx *os.File) {
			h.execs.update(rec.ID, func(r *execRecord) {
				r.Pty = ptmx
				if cmd.Process != nil {
					r.Pid = cmd.Process.Pid
				}
			})
		}, onExit)
	} else {
		runExecMux(cmd, conn, rec.AttachStdin, detachKeys, func() {
			h.execs.update(rec.ID, func(r *execRecord) {
				if cmd.Process != nil {
					r.Pid = cmd.Process.Pid
				}
			})
		}, onExit)
	}
}

func (h *Handler) startDetachedExec(execID string, cmd *exec.Cmd) {
	startedAt := time.Now()
	h.execs.update(execID, func(r *execRecord) {
		r.Running = true
		r.StartedAt = startedAt
		r.ExitCode = 0
		r.Pid = 0
	})
	go func() {
		if err := cmd.Start(); err != nil {
			h.execs.update(execID, func(r *execRecord) {
				r.Running = false
				r.Pid = 0
				r.ExitCode = 1
			})
			return
		}
		h.execs.update(execID, func(r *execRecord) {
			if cmd.Process != nil {
				r.Pid = cmd.Process.Pid
			}
		})
		err := cmd.Wait()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = 1
			}
		}
		h.execs.update(execID, func(r *execRecord) {
			r.Running = false
			r.Pid = 0
			r.ExitCode = code
		})
	}()
}

// GET /exec/{id}/json
func (h *Handler) execInspect(w http.ResponseWriter, r *http.Request) {
	execID := mux.Vars(r)["id"]
	rec := h.execs.get(execID)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such exec instance")
		return
	}

	entrypoint := ""
	args := []string{}
	if len(rec.Cmd) > 0 {
		entrypoint = rec.Cmd[0]
		args = rec.Cmd[1:]
	}

	jsonResponse(w, http.StatusOK, ExecInspect{
		ID:          rec.ID,
		ContainerID: rec.ContainerID,
		Running:     rec.Running,
		ExitCode:    rec.ExitCode,
		ProcessConfig: ExecProcessConfig{
			Tty:        rec.Tty,
			Entrypoint: entrypoint,
			Arguments:  ensureSlice(args),
			User:       rec.User,
			Privileged: rec.Privileged,
		},
		OpenStdin:  rec.AttachStdin,
		OpenStdout: rec.AttachStdout,
		OpenStderr: rec.AttachStderr,
		CanRemove:  !rec.Running,
		DetachKeys: rec.DetachKeys,
		Pid:        rec.Pid,
	})
}

// runExecTTY runs cmd with a PTY attached and proxies raw bytes between the PTY
// master and the hijacked connection. Used when Tty=true. onReady is called with
// the PTY master as soon as it's available (for resize forwarding); onExit is
// called with the process's exit code when it terminates. Returns detached=true
// if the client pressed the detach key sequence — in which case the process is
// left running and onExit fires later from a background goroutine.
func runExecTTY(cmd *exec.Cmd, conn io.ReadWriter, detachKeys []byte, onReady func(*os.File), onExit func(int)) (detached bool) {
	ptmx, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintf(conn, "error starting pty: %s\n", err)
		closeIfCloser(conn) // execStart set closeConn=false; the runner owns the conn now
		onExit(1)
		return false
	}
	if onReady != nil {
		onReady(ptmx)
	}

	exitCode := func() int {
		if cmd.ProcessState != nil {
			return cmd.ProcessState.ExitCode()
		}
		return 0
	}

	// Output drains the PTY into a swappable writer, so on detach we can
	// redirect it to io.Discard and keep draining (the process never blocks on
	// a full PTY).
	out := newSwitchableWriter(conn)
	outDone := make(chan struct{})
	go func() { io.Copy(out, ptmx); close(outDone) }()

	// Stdin: connection → PTY, scanning for the detach key sequence.
	stdinDone := make(chan error, 1)
	go func() { _, e := io.Copy(ptmx, newEscapeReader(conn, detachKeys)); stdinDone <- e }()

	waitDone := make(chan struct{})
	go func() { cmd.Wait(); close(waitDone) }()

	select {
	case <-waitDone:
		ptmx.Close() // unblocks the output drainer (EOF)
		closeIfCloser(conn)
		<-outDone
		onExit(exitCode())
		return false
	case e := <-stdinDone:
		if !errors.Is(e, errDetach) {
			// Client closed stdin or the connection dropped; the process is
			// still running — wait for it to exit, as before.
			<-waitDone
			ptmx.Close()
			closeIfCloser(conn)
			<-outDone
			onExit(exitCode())
			return false
		}
		// Detach: keep the process running. Redirect output to discard and close
		// the client connection (this unblocks any in-flight write; the drainer
		// swallows the error and keeps draining). Finish in the background.
		out.set(io.Discard)
		closeIfCloser(conn)
		go func() {
			<-waitDone
			ptmx.Close()
			<-outDone
			onExit(exitCode())
		}()
		return true
	}
}

// runExecMux runs cmd with pipes and multiplexes stdout/stderr into the
// Docker raw-stream format. Used when Tty=false. When attachStdin is set the
// hijacked connection is forwarded to the command's stdin (raw, not framed —
// matching how Docker streams exec stdin), so `docker exec -i` works.
func runExecMux(cmd *exec.Cmd, conn io.ReadWriter, attachStdin bool, detachKeys []byte, onStart func(), onExit func(int)) (detached bool) {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	var stdinW *io.PipeWriter
	if attachStdin {
		var stdinR *io.PipeReader
		stdinR, stdinW = io.Pipe()
		cmd.Stdin = stdinR
	}

	if err := cmd.Start(); err != nil {
		writeLogFrame(conn, 2, []byte("error: "+err.Error()+"\n"))
		closeIfCloser(conn) // execStart set closeConn=false; the runner owns the conn now
		onExit(1)
		return false
	}
	if onStart != nil {
		onStart()
	}

	exitCode := func() int {
		if cmd.ProcessState != nil {
			return cmd.ProcessState.ExitCode()
		}
		return 0
	}

	// Frames drain through a swappable writer so detach can redirect them to
	// io.Discard while still draining stdout/stderr (the process never blocks).
	out := newSwitchableWriter(conn)

	// Forward the client's stdin, scanning for the detach sequence.
	stdinDone := make(chan error, 1)
	if stdinW != nil {
		go func() { _, e := io.Copy(stdinW, newEscapeReader(conn, detachKeys)); stdinDone <- e }()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b := make([]byte, 32*1024)
		for {
			n, err := stdoutR.Read(b)
			if n > 0 {
				writeLogFrame(out, 1, b[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		b := make([]byte, 32*1024)
		for {
			n, err := stderrR.Read(b)
			if n > 0 {
				writeLogFrame(out, 2, b[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	waitDone := make(chan struct{})
	go func() { cmd.Wait(); close(waitDone) }()

	// With no attached stdin there is no stream to scan, so detach is
	// impossible — just wait for the process.
	if stdinW == nil {
		<-waitDone
		stdoutW.Close()
		stderrW.Close()
		closeIfCloser(conn)
		wg.Wait()
		onExit(exitCode())
		return false
	}

	select {
	case <-waitDone:
		stdoutW.Close()
		stderrW.Close()
		stdinW.Close()
		closeIfCloser(conn)
		wg.Wait()
		onExit(exitCode())
		return false
	case e := <-stdinDone:
		if !errors.Is(e, errDetach) {
			// Client closed stdin (EOF) → give the process EOF and wait.
			stdinW.Close()
			<-waitDone
			stdoutW.Close()
			stderrW.Close()
			closeIfCloser(conn)
			wg.Wait()
			onExit(exitCode())
			return false
		}
		// Detach: keep the process running. Redirect frames to discard and close
		// the client connection. Close stdinW so os/exec's stdin-copy goroutine
		// ends and cmd.Wait can complete — the client's input stream is gone, so
		// the process sees EOF on stdin (Docker's non-TTY detach behaves the
		// same); a process that ignores stdin keeps running. Finish in the
		// background when the process exits.
		out.set(io.Discard)
		closeIfCloser(conn)
		stdinW.Close()
		go func() {
			<-waitDone
			stdoutW.Close()
			stderrW.Close()
			wg.Wait()
			onExit(exitCode())
		}()
		return true
	}
}

// wrapCmdWithCwd turns argv into `sh -c "cd <dir> && exec <argv>"` so the
// command runs in the requested directory. Arguments are single-quoted for
// POSIX safety; embedded single quotes are escaped by closing the quote,
// inserting a backslash-quoted quote, and re-opening.
func wrapCmdWithCwd(argv []string, dir string) []string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(dir))
	b.WriteString(" && exec")
	for _, a := range argv {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return []string{"sh", "-c", b.String()}
}

// shellQuote wraps s in single quotes with embedded quotes escaped.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// generateID returns a 64-character hex ID matching Docker's container ID length.
func generateID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}
