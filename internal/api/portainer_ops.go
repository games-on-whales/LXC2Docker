package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
	"github.com/games-on-whales/LXC2Docker/internal/lxc"
	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/gorilla/mux"
)

// setAttachPTY records (or clears) the PTY master for an attach session.
// Portainer's browser terminal calls /containers/{id}/resize while the
// attach is live, and resizeContainer forwards the ioctl to this PTY.
func (h *Handler) setAttachPTY(id string, p *os.File) {
	h.attachMu.Lock()
	defer h.attachMu.Unlock()
	if p == nil {
		delete(h.attachPTYs, id)
		return
	}
	h.attachPTYs[id] = p
}

func (h *Handler) getAttachPTY(id string) *os.File {
	h.attachMu.Lock()
	defer h.attachMu.Unlock()
	return h.attachPTYs[id]
}

func parseResize(r *http.Request) (rows, cols uint16, ok bool) {
	h, err1 := strconv.Atoi(r.URL.Query().Get("h"))
	w, err2 := strconv.Atoi(r.URL.Query().Get("w"))
	if err1 != nil || err2 != nil || h <= 0 || w <= 0 {
		return 0, 0, false
	}
	return uint16(h), uint16(w), true
}

// POST /containers/{id}/resize
func (h *Handler) resizeContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	rows, cols, ok := parseResize(r)
	if !ok {
		errResponse(w, http.StatusBadRequest, "invalid h/w query params")
		return
	}
	if p := h.getAttachPTY(id); p != nil {
		_ = pty.Setsize(p, &pty.Winsize{Rows: rows, Cols: cols})
	}
	w.WriteHeader(http.StatusOK)
}

// POST /exec/{id}/resize
func (h *Handler) resizeExec(w http.ResponseWriter, r *http.Request) {
	rec := h.execs.get(mux.Vars(r)["id"])
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such exec instance")
		return
	}
	rows, cols, ok := parseResize(r)
	if !ok {
		errResponse(w, http.StatusBadRequest, "invalid h/w query params")
		return
	}
	if rec.Pty != nil {
		_ = pty.Setsize(rec.Pty, &pty.Winsize{Rows: rows, Cols: cols})
	}
	w.WriteHeader(http.StatusOK)
}

// POST /containers/{id}/pause
// LXC freeze requires the freezer cgroup, which is not available for
// unprivileged containers on modern kernels. Return a clear 409 so Portainer
// surfaces a real message instead of a mystery 404.
func (h *Handler) pauseContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	errResponse(w, http.StatusConflict, "pause is not supported by docker-lxc-daemon")
}

// POST /containers/{id}/unpause
func (h *Handler) unpauseContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	errResponse(w, http.StatusConflict, "unpause is not supported by docker-lxc-daemon")
}

// POST /containers/{id}/update
// Docker's update endpoint accepts a partial HostConfig body. Portainer uses
// it to edit resource limits and restart policy in-place, so we merge the
// provided keys into the stored HostConfig, persist the typed lifecycle
// fields the daemon actively enforces, and best-effort apply live cgroup
// changes when the container is currently running.
func (h *Handler) updateContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	rec := h.store.GetContainer(id)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}

	patch := map[string]json.RawMessage{}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil && err != io.EOF {
		errResponse(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	hc := buildHostConfig(rec)
	if err := mergeContainerUpdate(hc, patch); err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	normalizeHostConfig(hc)

	rawHC, err := json.Marshal(hc)
	if err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := h.store.UpdateContainer(id, func(current *store.ContainerRecord) {
		current.RawHostConfig = rawHC
		current.RestartPolicy = hc.RestartPolicy.Name
		current.RestartMaxRetry = hc.RestartPolicy.MaximumRetryCount
		current.AutoRemove = hc.AutoRemove
		current.OomScoreAdj = hc.OomScoreAdj
	}); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	warnings := []string{}
	if pid := containerPID(id); pid > 0 {
		if warning := applyLiveLimits(id, *hc); warning != "" {
			warnings = append(warnings, warning)
		}
		if _, ok := patch["OomScoreAdj"]; ok {
			if err := os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid),
				[]byte(strconv.Itoa(hc.OomScoreAdj)), 0o644); err != nil {
				warnings = append(warnings, "failed to apply oom_score_adj live: "+err.Error())
			}
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"Warnings": warnings})
}

func mergeContainerUpdate(hc *HostConfig, patch map[string]json.RawMessage) error {
	for key, raw := range patch {
		switch key {
		case "Memory":
			if err := json.Unmarshal(raw, &hc.Memory); err != nil {
				return fmt.Errorf("invalid Memory: %w", err)
			}
		case "MemoryReservation":
			if err := json.Unmarshal(raw, &hc.MemoryReservation); err != nil {
				return fmt.Errorf("invalid MemoryReservation: %w", err)
			}
		case "MemorySwap":
			if err := json.Unmarshal(raw, &hc.MemorySwap); err != nil {
				return fmt.Errorf("invalid MemorySwap: %w", err)
			}
		case "CpuShares":
			if err := json.Unmarshal(raw, &hc.CPUShares); err != nil {
				return fmt.Errorf("invalid CpuShares: %w", err)
			}
		case "CpuQuota":
			if err := json.Unmarshal(raw, &hc.CPUQuota); err != nil {
				return fmt.Errorf("invalid CpuQuota: %w", err)
			}
		case "CpuPeriod":
			if err := json.Unmarshal(raw, &hc.CPUPeriod); err != nil {
				return fmt.Errorf("invalid CpuPeriod: %w", err)
			}
		case "NanoCpus":
			if err := json.Unmarshal(raw, &hc.NanoCPUs); err != nil {
				return fmt.Errorf("invalid NanoCpus: %w", err)
			}
		case "CpusetCpus":
			if err := json.Unmarshal(raw, &hc.CpusetCpus); err != nil {
				return fmt.Errorf("invalid CpusetCpus: %w", err)
			}
		case "CpusetMems":
			if err := json.Unmarshal(raw, &hc.CpusetMems); err != nil {
				return fmt.Errorf("invalid CpusetMems: %w", err)
			}
		case "PidsLimit":
			if err := json.Unmarshal(raw, &hc.PidsLimit); err != nil {
				return fmt.Errorf("invalid PidsLimit: %w", err)
			}
		case "BlkioWeight":
			if err := json.Unmarshal(raw, &hc.BlkioWeight); err != nil {
				return fmt.Errorf("invalid BlkioWeight: %w", err)
			}
		case "OomScoreAdj":
			if err := json.Unmarshal(raw, &hc.OomScoreAdj); err != nil {
				return fmt.Errorf("invalid OomScoreAdj: %w", err)
			}
		case "RestartPolicy":
			if err := json.Unmarshal(raw, &hc.RestartPolicy); err != nil {
				return fmt.Errorf("invalid RestartPolicy: %w", err)
			}
		case "Ulimits":
			if err := json.Unmarshal(raw, &hc.Ulimits); err != nil {
				return fmt.Errorf("invalid Ulimits: %w", err)
			}
		}
	}
	return nil
}

// POST /containers/prune
func (h *Handler) pruneContainers(w http.ResponseWriter, r *http.Request) {
	filters, err := parseListFilters(r.URL.Query().Get("filters"))
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	until, err := parsePruneUntil(filters)
	if err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	deleted := []string{}
	var reclaimed int64
	for _, rec := range h.store.ListContainers() {
		state, _ := h.mgr.State(rec.ID)
		if state == "running" {
			continue
		}
		if !pruneEligible(rec.Created, rec.Labels, filters, until) {
			continue
		}
		// Measure the writable rootfs before destroying it so SpaceReclaimed
		// reports the freed bytes, consistent with the SizeRw the daemon
		// reports for containers in /system/df. Docker returns 0 only when
		// nothing was pruned.
		size := rootfsSize(h.mgr.RootfsPath(rec.ID))
		if err := h.mgr.RemoveContainer(rec.ID); err != nil {
			continue
		}
		reclaimed += size
		h.publishEvent("container", "destroy", rec.ID, map[string]string{
			"name":  rec.Name,
			"image": normalizeImageRef(rec.Image),
		})
		deleted = append(deleted, rec.ID)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ContainersDeleted": deleted,
		"SpaceReclaimed":    reclaimed,
	})
}

// POST /images/prune
// Portainer's prune includes a `filters` JSON blob. With
// dangling=["true"] (Docker's default) only dangling images are removed —
// we have no dangling state, so we delete nothing. With dangling=["false"]
// every image not currently referenced by a container is removed, subject
// to the label and until filters Docker also honours on this endpoint.
func (h *Handler) pruneImages(w http.ResponseWriter, r *http.Request) {
	filters, err := parseListFilters(r.URL.Query().Get("filters"))
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	onlyDangling := true
	if vals := filters["dangling"]; len(vals) > 0 {
		for _, v := range vals {
			if v == "false" || v == "0" {
				onlyDangling = false
			}
		}
	}
	until, err := parsePruneUntil(filters)
	if err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	deleted := []map[string]string{}
	var reclaimed int64
	if !onlyDangling {
		inUse := map[string]struct{}{}
		for _, c := range h.store.ListContainers() {
			inUse[normalizeImageRef(c.Image)] = struct{}{}
		}
		// Count each removed image's size once per underlying image ID: several
		// refs (e.g. `docker tag`) can share one template, and counting per-ref
		// would over-report — consistent with the per-ID sizing /system/df uses.
		countedIDs := map[string]bool{}
		for _, img := range h.store.ListImages() {
			if _, used := inUse[img.Ref]; used {
				continue
			}
			if !pruneEligible(img.Created, img.OCILabels, filters, until) {
				continue
			}
			var size int64
			if !countedIDs[img.ID] {
				size = imageSize(h.mgr.LXCPath(), h.mgr.PVEStorage(), img)
			}
			if err := h.mgr.RemoveImage(img.Ref); err != nil {
				continue
			}
			if !countedIDs[img.ID] {
				countedIDs[img.ID] = true
				reclaimed += size
			}
			h.publishEvent("image", "delete", img.Ref, map[string]string{"name": img.Ref})
			deleted = append(deleted, map[string]string{"Untagged": img.Ref})
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ImagesDeleted":  deleted,
		"SpaceReclaimed": reclaimed,
	})
}

// POST /networks/prune
// We only manage user-defined networks in the store; the built-in bridge is
// treated as system and never pruned. A network is considered unused when no
// container in the store attaches to it.
func (h *Handler) pruneNetworks(w http.ResponseWriter, r *http.Request) {
	filters, err := parseListFilters(r.URL.Query().Get("filters"))
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid filters: "+err.Error())
		return
	}
	until, err := parsePruneUntil(filters)
	if err != nil {
		errResponse(w, http.StatusBadRequest, err.Error())
		return
	}
	inUse := map[string]struct{}{}
	for _, c := range h.store.ListContainers() {
		for netID := range c.Networks {
			inUse[netID] = struct{}{}
		}
	}
	deleted := []string{}
	for _, n := range h.store.ListNetworks() {
		if canonicalNetworkName(n.Name) == lxc.DefaultNetworkName {
			continue
		}
		if _, used := inUse[n.ID]; used {
			continue
		}
		if _, used := inUse[n.Name]; used {
			continue
		}
		if !pruneEligible(n.CreatedAt, n.Labels, filters, until) {
			continue
		}
		if err := h.store.RemoveNetwork(n.ID); err != nil {
			continue
		}
		h.publishEvent("network", "destroy", n.ID, map[string]string{
			"name": n.Name, "type": n.Driver,
		})
		deleted = append(deleted, n.Name)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"NetworksDeleted": deleted,
	})
}

// POST /build/prune
// We don't maintain a build cache — builds run straight against rootfs — so
// there is nothing to reclaim. Return an empty response so Portainer's cache
// cleanup button reports success instead of failing.
func (h *Handler) pruneBuildCache(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"CachesDeleted":  []string{},
		"SpaceReclaimed": 0,
	})
}

// POST /auth
// Portainer calls /auth when the user configures a registry credential. We
// don't authenticate against registries ourselves — pulls go through whatever
// the host has set up — so accept any payload and return the shape Docker's
// "login succeeded" response uses.
// authConfig mirrors the credentials POST /auth carries in its BODY (an
// AuthConfig JSON object) — NOT the X-Registry-Auth header the pull path uses.
type authConfig struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Serveraddress string `json:"serveraddress"`
	IdentityToken string `json:"identitytoken"`
}

func (h *Handler) auth(w http.ResponseWriter, r *http.Request) {
	var body authConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		errResponse(w, http.StatusBadRequest, "invalid auth body: "+err.Error())
		return
	}
	// Clients (Portainer) probe with empty credentials or an identity token;
	// don't reject those — acknowledge, matching the prior stub.
	if body.Username == "" || body.Password == "" {
		jsonResponse(w, http.StatusOK, map[string]string{"Status": "Login Succeeded", "IdentityToken": ""})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	host := registryHostFromAuth(body.Serveraddress)
	if err := probeRegistryLogin(ctx, host, body.Username, body.Password); err != nil {
		var authErr *registryAuthError
		if errors.As(err, &authErr) {
			// Genuine bad credentials → 401, like Docker.
			errResponse(w, http.StatusUnauthorized, "unauthorized: incorrect username or password")
			return
		}
		// Transport/registry-unreachable error → 500 (not a credential verdict).
		errResponse(w, http.StatusInternalServerError, "auth check failed: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"Status": "Login Succeeded", "IdentityToken": ""})
}

// registryHostFromAuth resolves the registry host for a login probe. Empty or
// the legacy Docker Hub index URL/host map to docker.io. A scheme and any path
// are stripped so "https://index.docker.io/v1/" → "docker.io".
func registryHostFromAuth(serveraddress string) string {
	s := strings.TrimSpace(serveraddress)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	switch s {
	case "", "index.docker.io", "registry-1.docker.io", "docker.io":
		return "docker.io"
	}
	return s
}

// registryAuthError marks a genuine authentication failure (bad credentials),
// as opposed to a transport/unreachable error.
type registryAuthError struct{ msg string }

func (e *registryAuthError) Error() string { return e.msg }

// probeRegistryLogin runs `skopeo login` as a stateless credential check
// (password over stdin, throwaway authfile). Returns nil on success, a
// *registryAuthError on a genuine auth failure, or a plain error on a
// transport/other failure — so the caller can map to 401 vs 500.
func probeRegistryLogin(ctx context.Context, host, user, pass string) error {
	// A host that looks like a flag would be mis-parsed by skopeo as an option
	// (worst case: `--help` exits 0 → a false "Login Succeeded"). Reject it, and
	// also pass "--" before the positional as defense in depth.
	if host == "" || strings.HasPrefix(host, "-") {
		return fmt.Errorf("invalid registry host %q", host)
	}
	f, err := os.CreateTemp("", "dld-authcheck-*.json")
	if err != nil {
		return err
	}
	authfile := f.Name()
	f.Close()
	defer os.Remove(authfile)

	cmd := exec.CommandContext(ctx, "skopeo", "login",
		"--username", user, "--password-stdin",
		"--authfile", authfile, "--tls-verify=true", "--", host)
	cmd.Stdin = strings.NewReader(pass)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		// Only treat a genuine auth failure as 401; anything else is a transport
		// error → 500. "unauthorized" already covers "401 Unauthorized", so we
		// avoid a bare "401" substring (it would false-match a host/port
		// containing 401).
		se := strings.ToLower(stderr.String())
		if strings.Contains(se, "unauthorized") ||
			strings.Contains(se, "invalid username or password") ||
			strings.Contains(se, "authentication required") {
			return &registryAuthError{msg: "invalid username/password"}
		}
		return fmt.Errorf("skopeo login: %s: %w", strings.TrimSpace(stderr.String()), runErr)
	}
	return nil
}

// GET /plugins
func (h *Handler) listPlugins(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, []Plugin{})
}

// POST /plugins/create
func (h *Handler) createPlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotImplemented, "plugins are not supported by docker-lxc-daemon")
}

// GET /plugins/privileges
// Portainer probes this before plugin installation to discover what elevated
// permissions a plugin would request. We don't support Docker plugins, so the
// daemon reports an empty set rather than 404ing the route.
func (h *Handler) pluginPrivileges(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, []PluginPrivilege{})
}

// GET /plugins/{name}/json
func (h *Handler) inspectPlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotFound, "plugin not found")
}

// GET /plugins/{name}/yaml
func (h *Handler) pluginYAML(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotFound, "plugin not found")
}

// POST /plugins/pull
func (h *Handler) pullPlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotImplemented, "plugins are not supported by docker-lxc-daemon")
}

// POST /plugins/{name}/enable
func (h *Handler) enablePlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotFound, "plugin not found")
}

// POST /plugins/{name}/disable
func (h *Handler) disablePlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotFound, "plugin not found")
}

// POST /plugins/{name}/push
func (h *Handler) pushPlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotImplemented, "plugins are not supported by docker-lxc-daemon")
}

// POST /plugins/{name}/set
func (h *Handler) setPlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotImplemented, "plugins are not supported by docker-lxc-daemon")
}

// POST /plugins/{name}/upgrade
func (h *Handler) upgradePlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotImplemented, "plugins are not supported by docker-lxc-daemon")
}

// DELETE /plugins/{name}
func (h *Handler) removePlugin(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusNotFound, "plugin not found")
}

// swarmUnavailable is shared by all swarm-mode endpoints. Docker returns 503
// with the exact message below when swarm isn't initialised; Portainer keys
// off both the status code and the message text.
func (h *Handler) swarmUnavailable(w http.ResponseWriter, r *http.Request) {
	errResponse(w, http.StatusServiceUnavailable,
		"This node is not a swarm manager. Use \"docker swarm init\" or \"docker swarm join\" to connect this node to swarm and try again.")
}

// GET /distribution/{name}/json
// Portainer calls this before pulling so it can show manifest details. We
// don't have registry access of our own, so we synthesise a minimal response
// advertising amd64/linux — Portainer's pull UI stays happy and the
// subsequent /images/create pull path does the real work.
func (h *Handler) inspectDistribution(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	ref := normalizeImageRef(name)
	digest := ""
	if rec := h.store.GetImage(ref); rec != nil {
		digest = "sha256:" + rec.ID
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"Descriptor": map[string]any{
			"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"digest":    digest,
			"size":      0,
		},
		"Platforms": []map[string]any{
			{"architecture": "amd64", "os": "linux"},
		},
	})
}

// POST /images/{name}/push
// Portainer exposes a "push image" action from the image detail view. We
// don't implement registry pushes yet, but Docker clients expect a streamed
// JSON response from this route rather than a hard 404.
func (h *Handler) pushImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	ref := normalizeImageRef(name)
	if tag != "" && !strings.Contains(name, ":") {
		ref = normalizeImageRef(name + ":" + tag)
	}
	if h.store.GetImage(ref) == nil {
		errResponse(w, http.StatusNotFound, "No such image: "+name)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	_ = enc.Encode(map[string]string{
		"status": fmt.Sprintf("The push refers to repository [%s]", ref),
	})
	_ = enc.Encode(map[string]any{
		"error": "image push is not supported by docker-lxc-daemon",
		"errorDetail": map[string]string{
			"message": "image push is not supported by docker-lxc-daemon",
		},
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// GET /containers/{id}/export
// Streams the container rootfs as an uncompressed tar. Portainer's "export
// container" button and `docker export` both consume this.
func (h *Handler) exportContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	rootfs := h.mgr.RootfsPath(id)
	if rootfs == "" {
		errResponse(w, http.StatusConflict, "container rootfs unavailable")
		return
	}
	if _, err := os.Stat(rootfs); err != nil {
		errResponse(w, http.StatusNotFound, "container rootfs not found")
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)

	cmd := exec.CommandContext(r.Context(), "tar", "-cf", "-", "-C", rootfs, ".")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	_, _ = io.Copy(w, stdout)
	_ = cmd.Wait()
}

// POST /commit
// Portainer's "duplicate/edit" flow snapshots a container into an image using
// this endpoint. We approximate it by creating a new image record that points
// at the source container's image — no squash, no layer history, but enough
// for Portainer to surface a new tag that can be used to recreate the
// container with the edited settings.
func (h *Handler) commitContainer(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	containerParam := q.Get("container")
	if containerParam == "" {
		errResponse(w, http.StatusBadRequest, "container query parameter is required")
		return
	}
	cfg, err := decodeCommitConfig(r)
	if err != nil {
		errResponse(w, http.StatusBadRequest, "invalid commit config: "+err.Error())
		return
	}
	id := h.resolveID(containerParam)
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container: "+containerParam)
		return
	}
	repo := strings.TrimSpace(q.Get("repo"))
	tag := strings.TrimSpace(q.Get("tag"))
	if repo == "" {
		errResponse(w, http.StatusBadRequest, "repo is required")
		return
	}
	if tag == "" {
		tag = "latest"
	}
	ref := repo
	if !strings.Contains(repo, ":") {
		ref = repo + ":" + tag
	}
	ref = normalizeImageRef(ref)

	rec := h.store.GetContainer(id)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container: "+containerParam)
		return
	}
	src := h.store.GetImage(normalizeImageRef(rec.Image))
	if src == nil {
		errResponse(w, http.StatusConflict,
			fmt.Sprintf("container %s references unknown image %s", id, rec.Image))
		return
	}

	dup := *src
	dup.ID = "commit_" + generateID()[:12]
	dup.Ref = ref
	dup.Created = time.Now()
	dup.OCIAuthor = committedString(q.Get("author"), src.OCIAuthor)
	dup.OCIComment = committedString(q.Get("comment"), src.OCIComment)
	dup.OCIContainer = committedString(id, src.OCIContainer)
	dup.OCIDockerVersion = committedString("24.0.0-lxc", src.OCIDockerVersion)
	dup.OCIVariant = committedString(src.OCIVariant, "")
	dup.OCIEntrypoint = committedStringSlice(rec.Entrypoint, src.OCIEntrypoint)
	dup.OCICmd = committedStringSlice(rec.Cmd, src.OCICmd)
	dup.OCIEnv = committedStringSlice(rec.Env, src.OCIEnv)
	dup.OCIWorkingDir = committedString(rec.WorkingDir, src.OCIWorkingDir)
	dup.OCIPorts = committedSetKeys(rec.ExposedPorts, src.OCIPorts)
	dup.OCILabels = committedLabels(rec.Labels, src.OCILabels)
	dup.OCIHostname = committedString(rec.Hostname, src.OCIHostname)
	dup.OCIDomainname = committedString(rec.Domainname, src.OCIDomainname)
	dup.OCIUser = committedString(rec.User, src.OCIUser)
	dup.OCIAttachStdin = committedBool(rec.AttachStdin, src.OCIAttachStdin)
	dup.OCIAttachStdout = committedBool(rec.AttachStdout, src.OCIAttachStdout)
	dup.OCIAttachStderr = committedBool(rec.AttachStderr, src.OCIAttachStderr)
	dup.OCITty = committedBool(rec.Tty, src.OCITty)
	dup.OCIOpenStdin = committedBool(rec.OpenStdin, src.OCIOpenStdin)
	dup.OCIStdinOnce = committedBool(rec.StdinOnce, src.OCIStdinOnce)
	dup.OCIStopSignal = committedString(rec.StopSignal, src.OCIStopSignal)
	dup.OCIStopTimeout = committedInt(rec.StopTimeout, src.OCIStopTimeout)
	dup.OCIHealthcheck = committedHealthcheck(rec, src.OCIHealthcheck)
	dup.OCIVolumes = committedSetKeys(rec.Volumes, src.OCIVolumes)
	applyCommitConfig(&dup, cfg)
	if err := applyCommitChanges(&dup, r.URL.Query()["changes"]); err != nil {
		errResponse(w, http.StatusBadRequest, "invalid commit change: "+err.Error())
		return
	}
	if h.mgr != nil {
		rootfs := h.mgr.RootfsPath(id)
		if rootfs == "" {
			errResponse(w, http.StatusConflict, "container rootfs unavailable")
			return
		}
		if _, err := os.Stat(rootfs); err != nil {
			errResponse(w, http.StatusNotFound, "container rootfs not found")
			return
		}
		// Docker pauses the container during commit (default true) for a
		// consistent snapshot. Best-effort: the freezer cgroup is often
		// unavailable for unprivileged CTs, so on failure we log and snapshot
		// live rather than failing the commit. This uses the manager's freeze
		// primitive directly; the public pause/unpause endpoints keep their
		// deliberate 409 policy.
		if boolValueDefault(r, "pause", true) {
			if state, _ := h.mgr.State(id); state == "running" {
				if err := h.mgr.PauseContainer(id); err == nil {
					defer h.mgr.UnpauseContainer(id)
				} else {
					log.Printf("commit: pause %s failed (%v); snapshotting live", id, err)
				}
			}
		}
		templateName, err := snapshotCommittedImageRootfs(h.mgr.LXCPath(), ref, rootfs)
		if err != nil {
			errResponse(w, http.StatusInternalServerError, "snapshot container rootfs: "+err.Error())
			return
		}
		dup.TemplateName = templateName
		dup.TemplateVMID = 0
		dup.TemplateDataset = ""
		// Compute Docker's real image ID (sha256 of the OCI config) over the
		// freshly-committed snapshot rootfs — the same rootfs a later
		// `docker save` reads — so the response Id, inspect, and save all agree.
		// Best-effort: fall back to the internal ID if the rootfs can't be
		// tarred.
		committedRootfs := filepath.Join(h.mgr.LXCPath(), templateName, "rootfs")
		if digest, derr := computeConfigDigestFromRootfs(&dup, committedRootfs); derr == nil && digest != "" {
			dup.ConfigDigest = digest
		} else if derr != nil {
			log.Printf("commit: config-digest for %s failed (%v); using internal id", ref, derr)
		}
	}
	if err := h.store.AddImage(&dup); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("image", "create", ref, map[string]string{"name": ref})
	jsonResponse(w, http.StatusCreated, map[string]string{
		"Id": "sha256:" + imageDisplayID(&dup),
	})
}

func committedString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func committedStringSlice(values, fallback []string) []string {
	if len(values) == 0 {
		return append([]string{}, fallback...)
	}
	return append([]string{}, values...)
}

func committedBool(value, fallback bool) bool {
	if value {
		return true
	}
	return fallback
}

func committedInt(value, fallback int) int {
	if value != 0 {
		return value
	}
	return fallback
}

func committedSetKeys(values map[string]struct{}, fallback []string) []string {
	if len(values) == 0 {
		return append([]string{}, fallback...)
	}
	return append([]string{}, mapKeys(values)...)
}

func committedLabels(values, fallback map[string]string) map[string]string {
	if len(values) == 0 && len(fallback) == 0 {
		return map[string]string{}
	}
	out := copyLabels(fallback)
	for k, v := range values {
		out[k] = v
	}
	return out
}

func committedHealthcheck(rec *store.ContainerRecord, fallback *store.HealthcheckConfig) *store.HealthcheckConfig {
	if len(rec.HealthcheckTest) > 0 {
		return &store.HealthcheckConfig{
			Test:        append([]string{}, rec.HealthcheckTest...),
			Interval:    rec.HealthcheckInterval,
			Timeout:     rec.HealthcheckTimeout,
			Retries:     rec.HealthcheckRetries,
			StartPeriod: rec.HealthcheckStartPeriod,
		}
	}
	if fallback == nil {
		return nil
	}
	return &store.HealthcheckConfig{
		Test:        append([]string{}, fallback.Test...),
		Interval:    fallback.Interval,
		Timeout:     fallback.Timeout,
		Retries:     fallback.Retries,
		StartPeriod: fallback.StartPeriod,
	}
}

func snapshotCommittedImageRootfs(lxcPath, ref, rootfs string) (string, error) {
	targetName := safeCommitTemplateName(ref)
	targetDir := filepath.Join(lxcPath, targetName)
	if err := os.RemoveAll(targetDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", err
	}
	if err := copyTree(rootfs, filepath.Join(targetDir, "rootfs")); err != nil {
		return "", err
	}
	minimalConfig := fmt.Sprintf("lxc.include = /usr/share/lxc/config/common.conf\nlxc.arch = linux64\nlxc.rootfs.path = dir:%s\nlxc.uts.name = %s\n",
		filepath.Join(targetDir, "rootfs"), targetName)
	if err := os.WriteFile(filepath.Join(targetDir, "config"), []byte(minimalConfig), 0o644); err != nil {
		return "", err
	}
	return targetName, nil
}

func safeCommitTemplateName(ref string) string {
	ref = normalizeImageRef(ref)
	ref = strings.NewReplacer(":", "_", "/", "_", ".", "_", " ", "_").Replace(ref)
	return "__template_commit_" + ref
}

type commitConfig struct {
	Cmd             []string            `json:"Cmd"`
	Entrypoint      []string            `json:"Entrypoint"`
	Env             []string            `json:"Env"`
	Labels          map[string]string   `json:"Labels"`
	Hostname        *string             `json:"Hostname"`
	Domainname      *string             `json:"Domainname"`
	MacAddress      *string             `json:"MacAddress"`
	User            *string             `json:"User"`
	AttachStdin     *bool               `json:"AttachStdin"`
	AttachStdout    *bool               `json:"AttachStdout"`
	AttachStderr    *bool               `json:"AttachStderr"`
	Tty             *bool               `json:"Tty"`
	OpenStdin       *bool               `json:"OpenStdin"`
	StdinOnce       *bool               `json:"StdinOnce"`
	NetworkDisabled *bool               `json:"NetworkDisabled"`
	ArgsEscaped     *bool               `json:"ArgsEscaped"`
	WorkingDir      *string             `json:"WorkingDir"`
	OnBuild         []string            `json:"OnBuild"`
	Shell           []string            `json:"Shell"`
	StopSignal      *string             `json:"StopSignal"`
	StopTimeout     *int                `json:"StopTimeout"`
	ExposedPorts    map[string]struct{} `json:"ExposedPorts"`
	Volumes         map[string]struct{} `json:"Volumes"`
	Healthcheck     *Healthcheck        `json:"Healthcheck"`
}

func decodeCommitConfig(r *http.Request) (*commitConfig, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	defer r.Body.Close()
	var cfg commitConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}
	return &cfg, nil
}

func applyCommitConfig(rec *store.ImageRecord, cfg *commitConfig) {
	if rec == nil || cfg == nil {
		return
	}
	if cfg.Cmd != nil {
		rec.OCICmd = append([]string{}, cfg.Cmd...)
	}
	if cfg.Entrypoint != nil {
		rec.OCIEntrypoint = append([]string{}, cfg.Entrypoint...)
	}
	if cfg.Env != nil {
		rec.OCIEnv = append([]string{}, cfg.Env...)
	}
	if cfg.Labels != nil {
		rec.OCILabels = copyLabels(cfg.Labels)
	}
	if cfg.Hostname != nil {
		rec.OCIHostname = *cfg.Hostname
	}
	if cfg.Domainname != nil {
		rec.OCIDomainname = *cfg.Domainname
	}
	if cfg.MacAddress != nil {
		rec.OCIMacAddress = *cfg.MacAddress
	}
	if cfg.User != nil {
		rec.OCIUser = *cfg.User
	}
	if cfg.AttachStdin != nil {
		rec.OCIAttachStdin = *cfg.AttachStdin
	}
	if cfg.AttachStdout != nil {
		rec.OCIAttachStdout = *cfg.AttachStdout
	}
	if cfg.AttachStderr != nil {
		rec.OCIAttachStderr = *cfg.AttachStderr
	}
	if cfg.Tty != nil {
		rec.OCITty = *cfg.Tty
	}
	if cfg.OpenStdin != nil {
		rec.OCIOpenStdin = *cfg.OpenStdin
	}
	if cfg.StdinOnce != nil {
		rec.OCIStdinOnce = *cfg.StdinOnce
	}
	if cfg.NetworkDisabled != nil {
		rec.OCINetworkDisabled = *cfg.NetworkDisabled
	}
	if cfg.ArgsEscaped != nil {
		rec.OCIArgsEscaped = *cfg.ArgsEscaped
	}
	if cfg.WorkingDir != nil {
		rec.OCIWorkingDir = *cfg.WorkingDir
	}
	if cfg.OnBuild != nil {
		rec.OCIOnBuild = append([]string{}, cfg.OnBuild...)
	}
	if cfg.StopSignal != nil {
		rec.OCIStopSignal = *cfg.StopSignal
	}
	if cfg.StopTimeout != nil {
		rec.OCIStopTimeout = *cfg.StopTimeout
	}
	if cfg.ExposedPorts != nil {
		rec.OCIPorts = mapKeys(cfg.ExposedPorts)
	}
	if cfg.Volumes != nil {
		rec.OCIVolumes = mapKeys(cfg.Volumes)
	}
	if cfg.Healthcheck != nil {
		rec.OCIHealthcheck = &store.HealthcheckConfig{
			Test:        append([]string{}, cfg.Healthcheck.Test...),
			Interval:    cfg.Healthcheck.Interval,
			Timeout:     cfg.Healthcheck.Timeout,
			Retries:     cfg.Healthcheck.Retries,
			StartPeriod: cfg.Healthcheck.StartPeriod,
		}
	}
	if cfg.Shell != nil {
		rec.OCIShell = append([]string{}, cfg.Shell...)
	}
}

func applyCommitChanges(rec *store.ImageRecord, changes []string) error {
	instrs, err := parseCommitChangeInstructions(changes)
	if err != nil {
		return err
	}
	for _, inst := range instrs {
		switch inst.op {
		case "ENV":
			rec.OCIEnv = mergeEnv(rec.OCIEnv, parseEnvInstruction(inst.args))
		case "LABEL":
			rec.OCILabels = committedLabels(parseLabelInstruction(inst.args), rec.OCILabels)
		case "USER":
			rec.OCIUser = strings.TrimSpace(inst.args)
		case "WORKDIR":
			rec.OCIWorkingDir = resolveContainerPath(rec.OCIWorkingDir, inst.args)
		case "CMD":
			rec.OCICmd = parseCommandInstruction(inst.args)
		case "ENTRYPOINT":
			rec.OCIEntrypoint = parseCommandInstruction(inst.args)
		case "EXPOSE":
			rec.OCIPorts = mergeCommittedStrings(rec.OCIPorts, strings.Fields(inst.args))
		case "VOLUME":
			rec.OCIVolumes = mergeCommittedStrings(rec.OCIVolumes, parseVolumeInstruction(inst.args))
		case "STOPSIGNAL":
			rec.OCIStopSignal = strings.TrimSpace(inst.args)
		case "HEALTHCHECK":
			hc, err := parseHealthcheckInstruction(inst.args)
			if err != nil {
				return err
			}
			rec.OCIHealthcheck = hc
		case "SHELL":
			rec.OCIShell = parseCommandInstruction(inst.args)
		case "":
			continue
		default:
			return fmt.Errorf("unsupported instruction %q", inst.op)
		}
	}
	return nil
}

func parseCommitChangeInstructions(changes []string) ([]dockerfileInstruction, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	joined := strings.Join(changes, "\n")
	return parseDockerfile(joined)
}

func mergeCommittedStrings(current, incoming []string) []string {
	if len(incoming) == 0 {
		return append([]string{}, current...)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(current)+len(incoming))
	for _, v := range current {
		if strings.TrimSpace(v) == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, v := range incoming {
		if strings.TrimSpace(v) == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
