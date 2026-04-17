package api

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/lxc"
	"github.com/games-on-whales/docker-lxc-daemon/internal/oci"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
)

type buildState struct {
	baseRef     string
	workdir     string
	env         []string
	cmd         []string
	entrypoint  []string
	exposed     []string
	labels      map[string]string
	user        string
	stopSignal  string
	healthcheck *oci.ImageHealthcheck
}

type dockerfileInstruction struct {
	op   string
	args string
	line int
}

// buildImage implements a constrained single-stage Dockerfile builder.
// It is intentionally narrow but functional enough for basic Portainer flows:
// FROM, ARG, WORKDIR, ENV, COPY, ADD, RUN, CMD, ENTRYPOINT, EXPOSE, LABEL.
func (h *Handler) buildImage(w http.ResponseWriter, r *http.Request) {
	tag := firstCSV(r.URL.Query().Get("t"))
	if tag == "" {
		errResponse(w, http.StatusBadRequest, "build tag is required via query param t")
		return
	}

	dockerfilePath := r.URL.Query().Get("dockerfile")
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}

	// buildargs is a JSON object — Portainer's build UI populates this from
	// the "Build arguments" table. Parsed values override the defaults a
	// Dockerfile declares with ARG.
	buildArgs := map[string]string{}
	if raw := r.URL.Query().Get("buildargs"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &buildArgs); err != nil {
			errResponse(w, http.StatusBadRequest, "invalid buildargs: "+err.Error())
			return
		}
	}
	// labels is Portainer's "Image labels" override: JSON object merged on
	// top of whatever LABEL the Dockerfile declared.
	queryLabels := map[string]string{}
	if raw := r.URL.Query().Get("labels"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &queryLabels); err != nil {
			errResponse(w, http.StatusBadRequest, "invalid labels: "+err.Error())
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	send := func(v any) {
		_ = enc.Encode(v)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	fail := func(msg string) {
		send(map[string]any{
			"error":       msg,
			"errorDetail": map[string]string{"message": msg},
		})
	}

	ctxDir, err := extractBuildContext(r.Body)
	if err != nil {
		fail("extract build context: " + err.Error())
		return
	}
	defer os.RemoveAll(ctxDir)

	dfAbs, err := safeJoin(ctxDir, dockerfilePath)
	if err != nil {
		fail("invalid dockerfile path: " + err.Error())
		return
	}
	dfData, err := os.ReadFile(dfAbs)
	if err != nil {
		fail("read Dockerfile: " + err.Error())
		return
	}

	instrs, err := parseDockerfile(string(dfData))
	if err != nil {
		fail("parse Dockerfile: " + err.Error())
		return
	}
	// Resolve ARG defaults up front, then overlay the caller-supplied
	// buildargs. The resulting map is used to $VAR-substitute every other
	// instruction's args before it executes.
	argSet := map[string]string{}
	for _, inst := range instrs {
		if inst.op != "ARG" {
			continue
		}
		name, def, hasDef := strings.Cut(strings.TrimSpace(inst.args), "=")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if hasDef {
			argSet[name] = strings.Trim(strings.TrimSpace(def), `"`)
		} else if _, ok := argSet[name]; !ok {
			argSet[name] = ""
		}
	}
	for k, v := range buildArgs {
		argSet[k] = v
	}
	for i := range instrs {
		if instrs[i].op == "ARG" || instrs[i].op == "FROM" {
			continue
		}
		instrs[i].args = substituteBuildArgs(instrs[i].args, argSet)
	}
	state, err := evaluateBuildState(instrs)
	if err != nil {
		fail(err.Error())
		return
	}
	for k, v := range queryLabels {
		state.labels[k] = v
	}

	send(map[string]string{"stream": fmt.Sprintf("Step 1: resolving base image %s\n", state.baseRef)})
	if err := h.ensureBuildBaseImage(normalizeImageRef(state.baseRef), send); err != nil {
		fail("pull base image: " + err.Error())
		return
	}

	if existing := h.store.GetImage(normalizeImageRef(tag)); existing != nil {
		send(map[string]string{"stream": fmt.Sprintf("Removing existing image %s\n", normalizeImageRef(tag))})
		if err := h.mgr.RemoveImage(normalizeImageRef(tag)); err != nil {
			fail("remove existing image: " + err.Error())
			return
		}
	}

	tmpID := "build-" + generateID()[:12]
	rec := &store.ContainerRecord{
		ID:         tmpID,
		Name:       tmpID,
		Image:      normalizeImageRef(state.baseRef),
		ImageID:    normalizeImageRef(state.baseRef),
		Created:    time.Now(),
		Env:        append([]string{}, state.env...),
		Entrypoint: state.entrypoint,
		Cmd:        state.cmd,
	}
	if err := h.store.AddContainer(rec); err != nil {
		fail("create temp build record: " + err.Error())
		return
	}

	cleanupTemp := func() {
		if st, _ := h.mgr.State(tmpID); st == "running" {
			_ = h.mgr.StopContainer(tmpID, 5*time.Second)
		}
		_ = h.mgr.RemoveContainer(tmpID)
	}

	send(map[string]string{"stream": fmt.Sprintf("Step 2: cloning base image into temporary builder %s\n", tmpID)})
	if err := h.mgr.CreateContainer(tmpID, normalizeImageRef(state.baseRef), buildContainerConfigFromState(state)); err != nil {
		_ = h.store.RemoveContainer(tmpID)
		fail("create temporary builder: " + err.Error())
		return
	}

	rootfs := h.mgr.RootfsPath(tmpID)
	send(map[string]string{"stream": "Step 3: applying Dockerfile instructions\n"})
	for i, inst := range instrs {
		send(map[string]string{"stream": fmt.Sprintf("Step %d: %s %s\n", i+3, inst.op, inst.args)})
		switch inst.op {
		case "FROM", "LABEL", "ARG", "USER", "STOPSIGNAL", "HEALTHCHECK":
			continue
		case "WORKDIR":
			dst := resolveContainerPath(state.workdir, inst.args)
			if err := os.MkdirAll(filepath.Join(rootfs, strings.TrimPrefix(dst, "/")), 0o755); err != nil {
				cleanupTemp()
				fail("WORKDIR: " + err.Error())
				return
			}
			state.workdir = dst
		case "ENV":
			state.env = mergeEnv(state.env, parseEnvInstruction(inst.args))
		case "COPY", "ADD":
			if err := applyCopyInstruction(ctxDir, rootfs, state.workdir, inst.args); err != nil {
				cleanupTemp()
				fail(inst.op + ": " + err.Error())
				return
			}
		case "RUN":
			script := inst.args
			if state.workdir != "" {
				script = fmt.Sprintf("mkdir -p %q && cd %q && %s", state.workdir, state.workdir, inst.args)
			}
			cmd := exec.Command("chroot", rootfs, "/bin/sh", "-lc", script)
			cmd.Env = append(os.Environ(), state.env...)
			out, err := cmd.CombinedOutput()
			if len(out) > 0 {
				send(map[string]string{"stream": string(out)})
			}
			if err != nil {
				cleanupTemp()
				fail("RUN failed: " + err.Error())
				return
			}
		case "CMD":
			state.cmd = parseCommandInstruction(inst.args)
		case "ENTRYPOINT":
			state.entrypoint = parseCommandInstruction(inst.args)
		case "EXPOSE":
			state.exposed = append(state.exposed, strings.Fields(inst.args)...)
		default:
			cleanupTemp()
			fail(fmt.Sprintf("unsupported Dockerfile instruction %q on line %d", inst.op, inst.line))
			return
		}
	}

	send(map[string]string{"stream": fmt.Sprintf("Step %d: finalizing image %s\n", len(instrs)+3, normalizeImageRef(tag))})
	if err := h.finalizeBuiltImage(tmpID, normalizeImageRef(tag), state); err != nil {
		cleanupTemp()
		fail("finalize image: " + err.Error())
		return
	}

	send(map[string]string{"stream": fmt.Sprintf("Successfully built %s\n", normalizeImageRef(tag))})
	send(map[string]any{"aux": map[string]string{"ID": normalizeImageRef(tag)}})
}

func extractBuildContext(r io.Reader) (string, error) {
	tmpDir, err := os.MkdirTemp("", "docker-build-context-*")
	if err != nil {
		return "", err
	}
	br := bufio.NewReader(r)
	var tr *tar.Reader
	if magic, err := br.Peek(2); err == nil && len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			os.RemoveAll(tmpDir)
			return "", err
		}
		defer gz.Close()
		tr = tar.NewReader(gz)
	} else {
		tr = tar.NewReader(br)
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			os.RemoveAll(tmpDir)
			return "", err
		}
		target, err := safeJoin(tmpDir, hdr.Name)
		if err != nil {
			os.RemoveAll(tmpDir)
			return "", err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				os.RemoveAll(tmpDir)
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				os.RemoveAll(tmpDir)
				return "", err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				os.RemoveAll(tmpDir)
				return "", err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				os.RemoveAll(tmpDir)
				return "", err
			}
			_ = f.Close()
		}
	}
	return tmpDir, nil
}

func parseDockerfile(contents string) ([]dockerfileInstruction, error) {
	scanner := bufio.NewScanner(strings.NewReader(contents))
	var out []dockerfileInstruction
	var current string
	startLine := 0
	lineNo := 0
	flush := func() error {
		line := strings.TrimSpace(current)
		current = ""
		if line == "" || strings.HasPrefix(line, "#") {
			return nil
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			return nil
		}
		op := strings.ToUpper(parts[0])
		args := strings.TrimSpace(line[len(parts[0]):])
		out = append(out, dockerfileInstruction{op: op, args: args, line: startLine})
		return nil
	}
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if current == "" {
			startLine = lineNo
		}
		if strings.HasSuffix(line, "\\") {
			current += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		current += line
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(current) != "" {
		if err := flush(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func evaluateBuildState(instrs []dockerfileInstruction) (buildState, error) {
	state := buildState{
		workdir: "/",
		env:     []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		labels:  map[string]string{},
	}
	fromCount := 0
	for _, inst := range instrs {
		switch inst.op {
		case "FROM":
			fromCount++
			if fromCount > 1 {
				return state, fmt.Errorf("multi-stage builds are not supported")
			}
			state.baseRef = strings.Fields(inst.args)[0]
		case "WORKDIR":
			state.workdir = resolveContainerPath(state.workdir, inst.args)
		case "ENV":
			state.env = mergeEnv(state.env, parseEnvInstruction(inst.args))
		case "CMD":
			state.cmd = parseCommandInstruction(inst.args)
		case "ENTRYPOINT":
			state.entrypoint = parseCommandInstruction(inst.args)
		case "EXPOSE":
			state.exposed = append(state.exposed, strings.Fields(inst.args)...)
		case "LABEL":
			for k, v := range parseLabelInstruction(inst.args) {
				state.labels[k] = v
			}
		case "USER":
			state.user = strings.TrimSpace(inst.args)
		case "STOPSIGNAL":
			state.stopSignal = strings.TrimSpace(inst.args)
		case "HEALTHCHECK":
			hc, err := parseHealthcheckInstruction(inst.args)
			if err != nil {
				return state, err
			}
			state.healthcheck = hc
		}
	}
	if state.baseRef == "" {
		return state, fmt.Errorf("Dockerfile must contain FROM")
	}
	return state, nil
}

// parseHealthcheckInstruction handles the two HEALTHCHECK forms:
//
//	HEALTHCHECK NONE                               → disable
//	HEALTHCHECK [OPTIONS] CMD <cmd>                → shell or exec cmd
//
// Options are --interval=<duration>, --timeout=<duration>,
// --start-period=<duration>, --retries=<n>. Durations accept Go's time.
// ParseDuration syntax (e.g. "30s", "1m30s").
func parseHealthcheckInstruction(args string) (*oci.ImageHealthcheck, error) {
	raw := strings.TrimSpace(args)
	if strings.EqualFold(raw, "NONE") {
		return &oci.ImageHealthcheck{Test: []string{"NONE"}}, nil
	}
	out := &oci.ImageHealthcheck{}
	for {
		if !strings.HasPrefix(raw, "--") {
			break
		}
		space := strings.IndexAny(raw, " \t")
		if space < 0 {
			return nil, fmt.Errorf("HEALTHCHECK: expected CMD after options")
		}
		opt := raw[:space]
		raw = strings.TrimSpace(raw[space+1:])
		k, v, ok := strings.Cut(opt, "=")
		if !ok {
			return nil, fmt.Errorf("HEALTHCHECK: option %q requires a value", opt)
		}
		switch k {
		case "--interval":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("HEALTHCHECK --interval: %w", err)
			}
			out.Interval = int64(d)
		case "--timeout":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("HEALTHCHECK --timeout: %w", err)
			}
			out.Timeout = int64(d)
		case "--start-period":
			d, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("HEALTHCHECK --start-period: %w", err)
			}
			out.StartPeriod = int64(d)
		case "--retries":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("HEALTHCHECK --retries: %w", err)
			}
			out.Retries = n
		default:
			return nil, fmt.Errorf("HEALTHCHECK: unknown option %q", k)
		}
	}
	// Remaining tokens must start with CMD. Docker accepts both shell form
	// (`CMD curl -f ...`) and exec form (`CMD ["curl","-f","..."]`).
	if !strings.HasPrefix(strings.ToUpper(raw), "CMD") {
		return nil, fmt.Errorf("HEALTHCHECK: expected CMD, got %q", raw)
	}
	cmdArgs := strings.TrimSpace(raw[len("CMD"):])
	if strings.HasPrefix(cmdArgs, "[") {
		// Exec form: reuse the command parser which handles JSON arrays.
		out.Test = append([]string{"CMD"}, parseCommandInstruction(cmdArgs)...)
	} else {
		out.Test = []string{"CMD-SHELL", cmdArgs}
	}
	return out, nil
}

// parseLabelInstruction parses Dockerfile LABEL arg tokens. Supports both the
// single-pair shorthand (LABEL key=value or LABEL key value) and the
// space-separated multi-pair form (LABEL key1=v1 key2="v 2" key3=v3).
func parseLabelInstruction(args string) map[string]string {
	out := map[string]string{}
	tokens := splitShellTokens(args)
	if len(tokens) == 2 && !strings.Contains(tokens[0], "=") {
		out[tokens[0]] = tokens[1]
		return out
	}
	for _, t := range tokens {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		out[k] = strings.Trim(v, `"`)
	}
	return out
}

// splitShellTokens splits args on whitespace while respecting simple double-
// quoted runs so "LABEL desc=\"hello world\" name=foo" yields two tokens.
func splitShellTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case !inQuote && (c == ' ' || c == '\t'):
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// substituteBuildArgs replaces $VAR, ${VAR}, ${VAR:-default}, and
// ${VAR:+value} references using the supplied map. Unknown variables in the
// bare ${VAR} and $VAR forms expand to empty, matching Docker's behaviour.
// Escape sequences (\$) are preserved literally.
func substituteBuildArgs(s string, vars map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		var ident string
		var consumed int
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				b.WriteByte('$')
				i++
				continue
			}
			ident = s[i+2 : i+2+end]
			consumed = end + 3
		} else {
			j := i + 1
			for j < len(s) && (isAlphaNum(s[j]) || s[j] == '_') {
				j++
			}
			ident = s[i+1 : j]
			consumed = j - i
		}
		if ident == "" {
			b.WriteByte('$')
			i++
			continue
		}
		b.WriteString(expandBuildArgIdent(ident, vars))
		i += consumed
	}
	return b.String()
}

// expandBuildArgIdent evaluates a single ${...} identifier, honouring
// Docker's :- (default when unset/empty) and :+ (value when set) forms.
func expandBuildArgIdent(ident string, vars map[string]string) string {
	if idx := strings.Index(ident, ":-"); idx >= 0 {
		name := ident[:idx]
		fallback := ident[idx+2:]
		if v, ok := vars[name]; ok && v != "" {
			return v
		}
		return fallback
	}
	if idx := strings.Index(ident, ":+"); idx >= 0 {
		name := ident[:idx]
		alt := ident[idx+2:]
		if v, ok := vars[name]; ok && v != "" {
			return alt
		}
		return ""
	}
	return vars[ident]
}

func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func parseEnvInstruction(args string) []string {
	fields := strings.Fields(args)
	if len(fields) == 2 && !strings.Contains(fields[0], "=") {
		return []string{fields[0] + "=" + fields[1]}
	}
	if len(fields) > 1 && !strings.Contains(fields[0], "=") {
		return []string{fields[0] + "=" + strings.Join(fields[1:], " ")}
	}
	if strings.Contains(args, "=") {
		var out []string
		for _, field := range fields {
			if strings.Contains(field, "=") {
				out = append(out, field)
			}
		}
		return out
	}
	return nil
}

func parseCommandInstruction(args string) []string {
	args = strings.TrimSpace(args)
	if strings.HasPrefix(args, "[") && strings.HasSuffix(args, "]") {
		var out []string
		if err := json.Unmarshal([]byte(args), &out); err == nil {
			return out
		}
	}
	if args == "" {
		return nil
	}
	return []string{"/bin/sh", "-lc", args}
}

func applyCopyInstruction(ctxDir, rootfs, workdir, args string) error {
	parts := strings.Fields(args)
	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.HasPrefix(p, "--") {
			continue
		}
		clean = append(clean, p)
	}
	if len(clean) < 2 {
		return fmt.Errorf("COPY/ADD requires at least one source and a destination")
	}
	dest := resolveContainerPath(workdir, clean[len(clean)-1])
	destAbs := filepath.Join(rootfs, strings.TrimPrefix(dest, "/"))
	srcs := clean[:len(clean)-1]
	if len(srcs) > 1 {
		if err := os.MkdirAll(destAbs, 0o755); err != nil {
			return err
		}
	}
	for _, src := range srcs {
		srcAbs, err := safeJoin(ctxDir, src)
		if err != nil {
			return err
		}
		info, err := os.Stat(srcAbs)
		if err != nil {
			return err
		}
		target := destAbs
		if len(srcs) > 1 || (info.IsDir() && filepath.Base(dest) == "." || strings.HasSuffix(dest, "/")) {
			target = filepath.Join(destAbs, filepath.Base(src))
		}
		if err := copyTree(srcAbs, target); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dst, rel)
			if info.IsDir() {
				return os.MkdirAll(target, info.Mode())
			}
			return copyFile(path, target, info.Mode())
		})
	}
	return copyFile(src, dst, info.Mode())
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func resolveContainerPath(workdir, path string) string {
	if path == "" {
		return workdir
	}
	if strings.HasPrefix(path, "/") {
		return filepath.Clean(path)
	}
	base := workdir
	if base == "" {
		base = "/"
	}
	return filepath.Clean(filepath.Join(base, path))
}

func buildContainerConfigFromState(state buildState) lxc.ContainerConfig {
	return lxc.ContainerConfig{
		Env:        append([]string{}, state.env...),
		Entrypoint: append([]string{}, state.entrypoint...),
		Cmd:        append([]string{}, state.cmd...),
		WorkingDir: state.workdir,
	}
}

func (h *Handler) ensureBuildBaseImage(ref string, send func(any)) error {
	rec := h.store.GetImage(ref)
	if rec != nil && buildImageTemplateReady(h.mgr.LXCPath(), rec) {
		return nil
	}
	if h.mgr.UsePVE() {
		guessedDataset := fmt.Sprintf("%s/dld-templates/%s", h.mgr.PVEStorage(), oci.SafeDirName(ref))
		if exec.Command("zfs", "list", "-t", "snapshot", "-o", "name", "-H", guessedDataset+"@tmpl").Run() == nil {
			send(map[string]string{"stream": fmt.Sprintf("Recovering dataset-backed template for %s\n", ref)})
			recovered := &store.ImageRecord{
				ID:              "oci_" + oci.SafeDirName(ref),
				Ref:             ref,
				Arch:            "amd64",
				TemplateDataset: guessedDataset,
				Created:         time.Now(),
			}
			if rec != nil {
				recovered = rec
				recovered.TemplateDataset = guessedDataset
			}
			if err := h.store.AddImage(recovered); err != nil {
				return err
			}
			return nil
		}
	}
	if rec != nil {
		send(map[string]string{"stream": fmt.Sprintf("Repairing stale image metadata for %s\n", ref)})
		if err := h.mgr.RemoveImage(ref); err != nil {
			return err
		}
	}
	return h.mgr.PullImage(ref, "amd64", func(msg string) {
		send(map[string]string{"stream": msg + "\n"})
	})
}

func buildImageTemplateReady(lxcPath string, rec *store.ImageRecord) bool {
	if rec == nil {
		return false
	}
	switch {
	case rec.TemplateDataset != "":
		return exec.Command("zfs", "list", "-t", "snapshot", "-o", "name", "-H", rec.TemplateDataset+"@tmpl").Run() == nil
	case rec.TemplateVMID > 0:
		_, err := os.Stat(fmt.Sprintf("/etc/pve/lxc/%d.conf", rec.TemplateVMID))
		return err == nil
	case rec.TemplateName != "":
		_, err := os.Stat(filepath.Join(lxcPath, rec.TemplateName, "config"))
		return err == nil
	default:
		return false
	}
}

func firstCSV(v string) string {
	if v == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(v, ",")[0])
}

func safeTemplateName(ref string) string {
	ref = normalizeImageRef(ref)
	ref = strings.NewReplacer(":", "_", "/", "_", ".", "_", " ", "_").Replace(ref)
	return "__template_build_" + ref
}

func (h *Handler) finalizeBuiltImage(tmpID, ref string, state buildState) error {
	tmpRec := h.store.GetContainer(tmpID)
	if tmpRec == nil {
		return fmt.Errorf("temporary build container record missing")
	}
	if st, _ := h.mgr.State(tmpID); st == "running" {
		if err := h.mgr.StopContainer(tmpID, 10*time.Second); err != nil {
			return err
		}
	}

	if h.mgr.UsePVE() {
		storage := tmpRec.Storage
		if storage == "" {
			storage = "large"
		}
		sourceDS := fmt.Sprintf("%s/lxc-%s", storage, tmpID)
		targetDS := fmt.Sprintf("%s/dld-templates/%s", storage, strings.TrimPrefix(safeTemplateName(ref), "__template_build_"))
		_, _ = exec.Command("zfs", "destroy", "-r", targetDS).CombinedOutput()
		if out, err := exec.Command("zfs", "rename", sourceDS, targetDS).CombinedOutput(); err != nil {
			return fmt.Errorf("zfs rename %s -> %s: %s: %w", sourceDS, targetDS, out, err)
		}
		if out, err := exec.Command("zfs", "snapshot", targetDS+"@tmpl").CombinedOutput(); err != nil {
			return fmt.Errorf("zfs snapshot %s@tmpl: %s: %w", targetDS, out, err)
		}
		_ = os.RemoveAll(filepath.Join(h.mgr.LXCPath(), tmpID))
		_ = h.store.RemoveContainer(tmpID)
		return h.store.AddImage(&store.ImageRecord{
			ID:              "build_" + strings.TrimPrefix(safeTemplateName(ref), "__template_build_"),
			Ref:             ref,
			Arch:            "amd64",
			TemplateDataset: targetDS,
			Created:         time.Now(),
			OCIEntrypoint:   state.entrypoint,
			OCICmd:          state.cmd,
			OCIEnv:          state.env,
			OCIWorkingDir:   state.workdir,
			OCIPorts:        state.exposed,
			OCILabels:       state.labels,
			OCIUser:         state.user,
			OCIStopSignal:   state.stopSignal,
			OCIHealthcheck:  buildHealthcheckToStore(state.healthcheck),
		})
	}

	targetName := safeTemplateName(ref)
	targetDir := filepath.Join(h.mgr.LXCPath(), targetName)
	_ = os.RemoveAll(targetDir)
	if err := os.Rename(filepath.Join(h.mgr.LXCPath(), tmpID), targetDir); err != nil {
		return err
	}
	minimalConfig := fmt.Sprintf("lxc.include = /usr/share/lxc/config/common.conf\nlxc.arch = linux64\nlxc.rootfs.path = dir:%s\nlxc.uts.name = %s\n",
		filepath.Join(targetDir, "rootfs"), targetName)
	if err := os.WriteFile(filepath.Join(targetDir, "config"), []byte(minimalConfig), 0o644); err != nil {
		return err
	}
	_ = h.store.RemoveContainer(tmpID)
	return h.store.AddImage(&store.ImageRecord{
		ID:             "build_" + strings.TrimPrefix(targetName, "__template_build_"),
		Ref:            ref,
		Arch:           "amd64",
		TemplateName:   targetName,
		Created:        time.Now(),
		OCIEntrypoint:  state.entrypoint,
		OCICmd:         state.cmd,
		OCIEnv:         state.env,
		OCIWorkingDir:  state.workdir,
		OCIPorts:       state.exposed,
		OCILabels:      state.labels,
		OCIUser:        state.user,
		OCIStopSignal:  state.stopSignal,
		OCIHealthcheck: buildHealthcheckToStore(state.healthcheck),
	})
}

// buildHealthcheckToStore converts an oci.ImageHealthcheck (captured from
// the Dockerfile) into the store's HealthcheckConfig shape. Mirrors
// imageHealthcheckToStore in internal/lxc — kept here to avoid widening
// the oci↔store surface for a one-off conversion.
func buildHealthcheckToStore(h *oci.ImageHealthcheck) *store.HealthcheckConfig {
	if h == nil {
		return nil
	}
	return &store.HealthcheckConfig{
		Test:        append([]string{}, h.Test...),
		Interval:    h.Interval,
		Timeout:     h.Timeout,
		StartPeriod: h.StartPeriod,
		Retries:     h.Retries,
	}
}
