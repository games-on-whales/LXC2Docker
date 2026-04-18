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

	"github.com/games-on-whales/LXC2Docker/internal/lxc"
	"github.com/games-on-whales/LXC2Docker/internal/oci"
	"github.com/games-on-whales/LXC2Docker/internal/store"
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
	healthcheck *store.HealthcheckConfig
	volumes     []string
	shell       []string
}

type dockerfileInstruction struct {
	op   string
	args string
	line int
}

type dockerfileStage struct {
	baseRef      string
	name         string
	instructions []dockerfileInstruction
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
	targetStage := strings.TrimSpace(r.URL.Query().Get("target"))

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
		if instrs[i].op == "ARG" {
			continue
		}
		instrs[i].args = substituteBuildArgs(instrs[i].args, argSet)
	}

	stages, err := splitDockerfileStages(instrs)
	if err != nil {
		fail("parse Dockerfile: " + err.Error())
		return
	}
	targetIdx, err := selectBuildTarget(stages, targetStage)
	if err != nil {
		fail(err.Error())
		return
	}

	if existing := h.store.GetImage(normalizeImageRef(tag)); existing != nil {
		send(map[string]string{"stream": fmt.Sprintf("Removing existing image %s\n", normalizeImageRef(tag))})
		if err := h.mgr.RemoveImage(normalizeImageRef(tag)); err != nil {
			fail("remove existing image: " + err.Error())
			return
		}
	}

	stageRefs := map[string]string{}
	tempRefs := []string{}
	buildNonce := generateID()[:8]
	cleanupStageImages := func() {
		for _, ref := range tempRefs {
			_ = h.mgr.RemoveImage(ref)
		}
	}

	step := 1
	for idx, stage := range stages[:targetIdx+1] {
		state, err := evaluateBuildStage(stage)
		if err != nil {
			cleanupStageImages()
			fail(err.Error())
			return
		}
		for k, v := range queryLabels {
			state.labels[k] = v
		}
		resolvedBaseRef := resolveStageBaseRef(stage.baseRef, stageRefs)
		scratchBase := isScratchBuildRef(resolvedBaseRef)
		baseRef := normalizeImageRef(resolvedBaseRef)
		if scratchBase {
			baseRef = "scratch"
		}
		send(map[string]string{"stream": fmt.Sprintf("Step %d: resolving base image %s\n", step, baseRef)})
		step++
		if !scratchBase {
			if err := h.ensureBuildBaseImage(baseRef, send); err != nil {
				cleanupStageImages()
				fail("pull base image: " + err.Error())
				return
			}
		} else {
			send(map[string]string{"stream": "Using empty scratch rootfs\n"})
		}

		tmpID := "build-" + generateID()[:12]
		rootfs := h.mgr.RootfsPath(tmpID)
		rec := &store.ContainerRecord{
			ID:         tmpID,
			Name:       tmpID,
			Image:      baseRef,
			ImageID:    baseRef,
			Created:    time.Now(),
			Env:        append([]string{}, state.env...),
			Entrypoint: state.entrypoint,
			Cmd:        state.cmd,
		}
		if err := h.store.AddContainer(rec); err != nil {
			cleanupStageImages()
			fail("create temp build record: " + err.Error())
			return
		}

		cleanupTemp := func() {
			if !scratchBase {
				if st, _ := h.mgr.State(tmpID); st == "running" {
					_ = h.mgr.StopContainer(tmpID, 5*time.Second)
				}
				_ = h.mgr.RemoveContainer(tmpID)
			} else {
				_ = os.RemoveAll(filepath.Join(h.mgr.LXCPath(), tmpID))
			}
			_ = h.store.RemoveContainer(tmpID)
		}

		send(map[string]string{"stream": fmt.Sprintf("Step %d: cloning base image into temporary builder %s\n", step, tmpID)})
		step++
		if scratchBase {
			if err := os.MkdirAll(rootfs, 0o755); err != nil {
				_ = h.store.RemoveContainer(tmpID)
				cleanupStageImages()
				fail("create scratch builder rootfs: " + err.Error())
				return
			}
		} else {
			if err := h.mgr.CreateContainer(tmpID, baseRef, buildContainerConfigFromState(state)); err != nil {
				_ = h.store.RemoveContainer(tmpID)
				cleanupStageImages()
				fail("create temporary builder: " + err.Error())
				return
			}
		}
		for _, inst := range stage.instructions {
			send(map[string]string{"stream": fmt.Sprintf("Step %d: %s %s\n", step, inst.op, inst.args)})
			step++
			switch inst.op {
			case "FROM", "LABEL", "ARG", "USER", "STOPSIGNAL", "HEALTHCHECK", "VOLUME", "SHELL":
				continue
			case "WORKDIR":
				dst := resolveContainerPath(state.workdir, inst.args)
				if err := os.MkdirAll(filepath.Join(rootfs, strings.TrimPrefix(dst, "/")), 0o755); err != nil {
					cleanupTemp()
					cleanupStageImages()
					fail("WORKDIR: " + err.Error())
					return
				}
				state.workdir = dst
			case "ENV":
				state.env = mergeEnv(state.env, parseEnvInstruction(inst.args))
			case "COPY", "ADD":
				if err := h.applyCopyInstruction(ctxDir, rootfs, state.workdir, inst.args, stageRefs, send); err != nil {
					cleanupTemp()
					cleanupStageImages()
					fail(inst.op + ": " + err.Error())
					return
				}
			case "RUN":
				script := inst.args
				if state.workdir != "" {
					script = fmt.Sprintf("mkdir -p %q && cd %q && %s", state.workdir, state.workdir, inst.args)
				}
				shellArgs := runShell(state.shell)
				args := buildRunArgs(rootfs, shellArgs, state.user, script)
				cmd := exec.Command("chroot", args...)
				cmd.Env = append(os.Environ(), state.env...)
				out, err := cmd.CombinedOutput()
				if len(out) > 0 {
					send(map[string]string{"stream": string(out)})
				}
				if err != nil {
					cleanupTemp()
					cleanupStageImages()
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
				cleanupStageImages()
				fail(fmt.Sprintf("unsupported Dockerfile instruction %q on line %d", inst.op, inst.line))
				return
			}
		}

		outRef := normalizeImageRef(tag)
		if idx != targetIdx {
			outRef = normalizeImageRef(fmt.Sprintf("dld-build-stage-%s-%d:latest", buildNonce, idx))
			tempRefs = append(tempRefs, outRef)
		}
		send(map[string]string{"stream": fmt.Sprintf("Step %d: finalizing stage %s\n", step, outRef)})
		step++
		if err := h.finalizeBuiltImage(tmpID, outRef, state); err != nil {
			cleanupTemp()
			cleanupStageImages()
			fail("finalize image: " + err.Error())
			return
		}
		stageRefs[strconv.Itoa(idx)] = outRef
		if stage.name != "" {
			stageRefs[stage.name] = outRef
		}
	}

	cleanupStageImages()
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

func evaluateBuildStage(stage dockerfileStage) (buildState, error) {
	state := buildState{
		workdir: "/",
		env:     []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		labels:  map[string]string{},
	}
	state.baseRef = stage.baseRef
	for _, inst := range stage.instructions {
		switch inst.op {
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
		case "VOLUME":
			for _, v := range parseVolumeInstruction(inst.args) {
				state.volumes = append(state.volumes, v)
			}
		case "SHELL":
			// SHELL only accepts the exec form, e.g. SHELL ["/bin/bash","-c"].
			state.shell = parseCommandInstruction(inst.args)
		}
	}
	if state.baseRef == "" {
		return state, fmt.Errorf("Dockerfile must contain FROM")
	}
	return state, nil
}

func splitDockerfileStages(instrs []dockerfileInstruction) ([]dockerfileStage, error) {
	stages := []dockerfileStage{}
	current := dockerfileStage{}
	seenFrom := false
	for _, inst := range instrs {
		if inst.op == "FROM" {
			if seenFrom {
				stages = append(stages, current)
			}
			baseRef, name, err := parseFromInstruction(inst.args)
			if err != nil {
				return nil, fmt.Errorf("parse FROM on line %d: %w", inst.line, err)
			}
			current = dockerfileStage{
				baseRef:      baseRef,
				name:         name,
				instructions: []dockerfileInstruction{inst},
			}
			seenFrom = true
			continue
		}
		if !seenFrom {
			if inst.op == "ARG" {
				continue
			}
			return nil, fmt.Errorf("Dockerfile must contain FROM before %s on line %d", inst.op, inst.line)
		}
		current.instructions = append(current.instructions, inst)
	}
	if seenFrom {
		stages = append(stages, current)
	}
	if len(stages) == 0 {
		return nil, fmt.Errorf("Dockerfile must contain FROM")
	}
	return stages, nil
}

func parseFromInstruction(args string) (baseRef, name string, err error) {
	tokens := splitShellTokens(args)
	clean := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		if strings.HasPrefix(tokens[i], "--") {
			if tokens[i] == "--platform" && i+1 < len(tokens) {
				i++
			}
			continue
		}
		clean = append(clean, tokens[i])
	}
	if len(clean) == 0 {
		return "", "", fmt.Errorf("missing base image")
	}
	baseRef = clean[0]
	if len(clean) >= 3 && strings.EqualFold(clean[1], "AS") {
		name = clean[2]
	}
	return baseRef, name, nil
}

func selectBuildTarget(stages []dockerfileStage, target string) (int, error) {
	if len(stages) == 0 {
		return -1, fmt.Errorf("no build stages found")
	}
	if strings.TrimSpace(target) == "" {
		return len(stages) - 1, nil
	}
	for idx, stage := range stages {
		if stage.name == target || strconv.Itoa(idx) == target {
			return idx, nil
		}
	}
	return -1, fmt.Errorf("target stage %q not found", target)
}

func resolveStageBaseRef(baseRef string, stageRefs map[string]string) string {
	if ref, ok := stageRefs[baseRef]; ok {
		return ref
	}
	return baseRef
}

func isScratchBuildRef(ref string) bool {
	return strings.EqualFold(strings.TrimSpace(ref), "scratch")
}

// parseHealthcheckInstruction handles the two HEALTHCHECK forms:
//
//	HEALTHCHECK NONE                               → disable
//	HEALTHCHECK [OPTIONS] CMD <cmd>                → shell or exec cmd
//
// Options are --interval=<duration>, --timeout=<duration>,
// --start-period=<duration>, --retries=<n>. Durations accept Go's time.
// ParseDuration syntax (e.g. "30s", "1m30s").
func parseHealthcheckInstruction(args string) (*store.HealthcheckConfig, error) {
	raw := strings.TrimSpace(args)
	if strings.EqualFold(raw, "NONE") {
		return &store.HealthcheckConfig{Test: []string{"NONE"}}, nil
	}
	out := &store.HealthcheckConfig{}
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

// runShell returns the shell + flags a RUN instruction should invoke.
// Defaults to /bin/sh -lc (matching Docker) when the Dockerfile didn't
// declare SHELL. SHELL entries take the place of the shell path; we
// append -c so the script string arrives as a single argument.
func runShell(declared []string) []string {
	if len(declared) == 0 {
		return []string{"/bin/sh", "-lc"}
	}
	out := append([]string{}, declared...)
	needsC := true
	for _, a := range declared[1:] {
		if a == "-c" || a == "-lc" {
			needsC = false
			break
		}
	}
	if needsC {
		out = append(out, "-c")
	}
	return out
}

func buildRunArgs(rootfs string, shellArgs []string, user, script string) []string {
	args := []string{}
	if user = strings.TrimSpace(user); user != "" {
		args = append(args, "--userspec", user)
	}
	args = append(args, rootfs)
	args = append(args, shellArgs...)
	args = append(args, script)
	return args
}

// parseVolumeInstruction parses a Dockerfile VOLUME directive. Accepts
// both the JSON array form (VOLUME ["/data","/logs"]) and the whitespace-
// separated form (VOLUME /data /logs). Returns the declared paths.
func parseVolumeInstruction(args string) []string {
	args = strings.TrimSpace(args)
	if strings.HasPrefix(args, "[") {
		// Delegate to the command parser which already decodes JSON arrays.
		return parseCommandInstruction(args)
	}
	return splitShellTokens(args)
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

func parseCopyInstruction(args string) (from string, sources []string, dest string, err error) {
	tokens := splitShellTokens(args)
	clean := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "--") {
			clean = append(clean, tok)
			continue
		}
		switch {
		case strings.HasPrefix(tok, "--from="):
			from = strings.TrimPrefix(tok, "--from=")
		case tok == "--from" && i+1 < len(tokens):
			i++
			from = tokens[i]
		default:
			if !strings.Contains(tok, "=") && i+1 < len(tokens) && (tok == "--chmod" || tok == "--chown") {
				i++
			}
		}
	}
	if len(clean) < 2 {
		return "", nil, "", fmt.Errorf("COPY/ADD requires at least one source and a destination")
	}
	return from, clean[:len(clean)-1], clean[len(clean)-1], nil
}

func (h *Handler) applyCopyInstruction(ctxDir, rootfs, workdir, args string, stageRefs map[string]string, send func(any)) error {
	from, srcs, dest, err := parseCopyInstruction(args)
	if err != nil {
		return fmt.Errorf("COPY/ADD requires at least one source and a destination")
	}
	dest = resolveContainerPath(workdir, dest)
	destAbs := filepath.Join(rootfs, strings.TrimPrefix(dest, "/"))
	if len(srcs) > 1 {
		if err := os.MkdirAll(destAbs, 0o755); err != nil {
			return err
		}
	}
	sourceRoot := ctxDir
	cleanup := func() {}
	if from != "" {
		resolvedFrom := resolveStageBaseRef(from, stageRefs)
		normRef := normalizeImageRef(resolvedFrom)
		if h.store.GetImage(normRef) == nil {
			if err := h.ensureBuildBaseImage(normRef, send); err != nil {
				return err
			}
		}
		var err error
		sourceRoot, cleanup, err = h.openImageRootfs(h.store.GetImage(normRef))
		if err != nil {
			return err
		}
	}
	defer cleanup()
	for _, src := range srcs {
		srcPath := src
		if from != "" {
			srcPath = resolveContainerPath("/", src)
		}
		srcAbs, err := safeJoin(sourceRoot, srcPath)
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
		storage := h.mgr.PVEStorage()
		if storage == "" {
			storage = "large"
		}
		sourceDS := fmt.Sprintf("%s/lxc-%s", storage, tmpID)
		targetDS := fmt.Sprintf("%s/dld-templates/%s", storage, strings.TrimPrefix(safeTemplateName(ref), "__template_build_"))
		if err := exec.Command("zfs", "list", "-H", "-o", "name", sourceDS).Run(); err == nil {
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
				OCIHealthcheck:  state.healthcheck,
				OCIVolumes:      append([]string{}, state.volumes...),
				OCIShell:        append([]string{}, state.shell...),
			})
		}
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
		OCIHealthcheck: state.healthcheck,
		OCIVolumes:     append([]string{}, state.volumes...),
		OCIShell:       append([]string{}, state.shell...),
	})
}
