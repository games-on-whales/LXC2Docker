package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
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
	rec.RawHostConfig = rawHC
	rec.RestartPolicy = hc.RestartPolicy.Name
	rec.RestartMaxRetry = hc.RestartPolicy.MaximumRetryCount
	rec.AutoRemove = hc.AutoRemove
	rec.OomScoreAdj = hc.OomScoreAdj
	if err := h.store.AddContainer(rec); err != nil {
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
		if err := h.mgr.RemoveContainer(rec.ID); err != nil {
			continue
		}
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
		for _, img := range h.store.ListImages() {
			if _, used := inUse[img.Ref]; used {
				continue
			}
			if !pruneEligible(img.Created, img.OCILabels, filters, until) {
				continue
			}
			if err := h.mgr.RemoveImage(img.Ref); err != nil {
				continue
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
// We only manage user-defined networks in the store; the built-in "gow" is
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
		if n.Name == "gow" {
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
func (h *Handler) auth(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	jsonResponse(w, http.StatusOK, map[string]string{
		"Status":        "Login Succeeded",
		"IdentityToken": "",
	})
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
	dup.Ref = ref
	dup.Created = time.Now()
	dup.OCIEntrypoint = committedStringSlice(rec.Entrypoint, src.OCIEntrypoint)
	dup.OCICmd = committedStringSlice(rec.Cmd, src.OCICmd)
	dup.OCIEnv = committedStringSlice(rec.Env, src.OCIEnv)
	dup.OCIWorkingDir = committedString(rec.WorkingDir, src.OCIWorkingDir)
	dup.OCIPorts = committedSetKeys(rec.ExposedPorts, src.OCIPorts)
	dup.OCILabels = committedLabels(rec.Labels, src.OCILabels)
	dup.OCIUser = committedString(rec.User, src.OCIUser)
	dup.OCIStopSignal = committedString(rec.StopSignal, src.OCIStopSignal)
	dup.OCIHealthcheck = committedHealthcheck(rec, src.OCIHealthcheck)
	dup.OCIVolumes = committedSetKeys(rec.Volumes, src.OCIVolumes)
	if err := h.store.AddImage(&dup); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.publishEvent("image", "create", ref, map[string]string{"name": ref})
	jsonResponse(w, http.StatusCreated, map[string]string{
		"Id": "sha256:" + dup.ID,
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

func committedSetKeys(values map[string]struct{}, fallback []string) []string {
	if len(values) == 0 {
		return append([]string{}, fallback...)
	}
	return append([]string{}, mapKeys(values)...)
}

func committedLabels(values, fallback map[string]string) map[string]string {
	if len(values) == 0 {
		return copyLabels(fallback)
	}
	return copyLabels(values)
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
