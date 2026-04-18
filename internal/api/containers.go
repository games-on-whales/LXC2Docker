package api

import (
	"archive/tar"
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/image"
	"github.com/games-on-whales/docker-lxc-daemon/internal/lxc"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
	"github.com/gorilla/mux"
)

// POST /containers/create
func (h *Handler) createContainer(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	name = strings.TrimPrefix(name, "/")
	if name != "" && !isValidContainerName(name) {
		errResponse(w, http.StatusBadRequest, fmt.Sprintf("Invalid container name (%s), only [a-zA-Z0-9][a-zA-Z0-9_.-]+ are allowed", name))
		return
	}

	var req ContainerCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResponse(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if reqJSON, err := json.Marshal(req); err == nil {
		log.Printf("createContainer: request body: %s", reqJSON)
	}

	if req.Image == "" {
		errResponse(w, http.StatusBadRequest, "Image is required")
		return
	}
	if !isValidRestartPolicy(req.HostConfig.RestartPolicy.Name) {
		errResponse(w, http.StatusBadRequest,
			fmt.Sprintf("invalid restart policy %q; expected one of no, always, unless-stopped, on-failure", req.HostConfig.RestartPolicy.Name))
		return
	}
	if req.HostConfig.RestartPolicy.MaximumRetryCount != 0 &&
		req.HostConfig.RestartPolicy.Name != "on-failure" {
		errResponse(w, http.StatusBadRequest,
			"maximum retry count cannot be used with restart policies other than on-failure")
		return
	}
	if req.HostConfig.MemorySwap > 0 &&
		req.HostConfig.Memory > 0 &&
		req.HostConfig.MemorySwap < req.HostConfig.Memory {
		errResponse(w, http.StatusBadRequest,
			"Minimum memoryswap limit should be larger than memory limit, see usage")
		return
	}
	if req.HostConfig.Memory < 0 {
		errResponse(w, http.StatusBadRequest, "Memory limit cannot be negative")
		return
	}
	if req.HostConfig.AutoRemove &&
		req.HostConfig.RestartPolicy.Name != "" &&
		req.HostConfig.RestartPolicy.Name != "no" {
		errResponse(w, http.StatusBadRequest,
			"can't create 'AutoRemove' container with restart policy")
		return
	}

	// Name conflict: Docker returns 409 rather than silently clobbering
	// the existing container's record. Portainer relies on this to detect
	// duplicate-create attempts (e.g. after a partially-failed deploy) and
	// surface a clear error to the user.
	if name != "" && h.store.FindContainerByName(name) != nil {
		errResponse(w, http.StatusConflict,
			fmt.Sprintf("Conflict. The container name %q is already in use.", "/"+name))
		return
	}

	// Auto-pull if image not present. Portainer's deploy flow POSTs
	// /containers/create with an image that hasn't been pulled yet; when
	// the registry is private the user's credentials arrive in
	// X-Registry-Auth on this request (same decoder as /images/create).
	if rec := h.store.GetImage(normalizeImageRef(req.Image)); rec == nil {
		creds := decodeRegistryAuth(r.Header.Get("X-Registry-Auth"))
		pullErr := h.mgr.PullImageWith(req.Image, "amd64", lxc.PullOpts{
			Credentials: creds,
		})
		if pullErr != nil {
			errResponse(w, http.StatusNotFound,
				fmt.Sprintf("No such image: %s — and pull failed: %s", req.Image, pullErr))
			return
		}
	}

	id := generateID()
	if name == "" {
		name = id[:12]
	}

	// Apply defaults from the image when no explicit Cmd/Entrypoint provided.
	entrypoint := req.Entrypoint
	cmd := req.Cmd
	env := req.Env
	if imgRec := h.store.GetImage(normalizeImageRef(req.Image)); imgRec != nil {
		// OCI image defaults.
		if len(entrypoint) == 0 && len(imgRec.OCIEntrypoint) > 0 {
			entrypoint = imgRec.OCIEntrypoint
		}
		if len(cmd) == 0 && len(imgRec.OCICmd) > 0 {
			cmd = imgRec.OCICmd
		}
		// Merge OCI image env with request env. Image vars provide
		// defaults; request vars override them (matching Docker behavior).
		if len(imgRec.OCIEnv) > 0 {
			env = mergeEnv(imgRec.OCIEnv, env)
		}
		// App registry defaults (if no OCI config and no user-provided cmd).
		if len(entrypoint) == 0 && len(cmd) == 0 {
			if resolved, err := image.Resolve(imgRec.Ref, "amd64"); err == nil && resolved.App != nil && resolved.App.DefaultCmd != "" {
				cmd = []string{"/bin/sh", "-c", resolved.App.DefaultCmd}
			}
		}
	}

	// Working directory: request overrides image default.
	workingDir := req.WorkingDir
	if workingDir == "" {
		if imgRec := h.store.GetImage(normalizeImageRef(req.Image)); imgRec != nil {
			workingDir = imgRec.OCIWorkingDir
		}
	}

	cfg := lxc.ContainerConfig{
		Entrypoint:        entrypoint,
		Cmd:               cmd,
		Env:               env,
		WorkingDir:        workingDir,
		DeviceCgroupRules: req.HostConfig.DeviceCgroupRules,
		NetworkMode:       req.HostConfig.NetworkMode,
		IpcMode:           req.HostConfig.IpcMode,
		MemoryBytes:       req.HostConfig.Memory,
		CPUShares:         req.HostConfig.CPUShares,
		CPUQuota:          req.HostConfig.CPUQuota,
		NanoCPUs:          req.HostConfig.NanoCPUs,
		CpusetCpus:        req.HostConfig.CpusetCpus,
		CpusetMems:        req.HostConfig.CpusetMems,
		PidsLimit:         req.HostConfig.PidsLimit,
		Ulimits:           apiToLXCUlimits(req.HostConfig.Ulimits),
		ShmSize:           req.HostConfig.ShmSize,
		BlkioWeight:       req.HostConfig.BlkioWeight,
		Privileged:        req.HostConfig.Privileged,
		CapAdd:            req.HostConfig.CapAdd,
		CapDrop:           req.HostConfig.CapDrop,
		SecurityOpt:       req.HostConfig.SecurityOpt,
		Sysctls:           req.HostConfig.Sysctls,
		Tmpfs:             req.HostConfig.Tmpfs,
		ExtraHosts:        req.HostConfig.ExtraHosts,
		DNS:               req.HostConfig.DNS,
		DNSSearch:         req.HostConfig.DNSSearch,
		DNSOptions:        req.HostConfig.DNSOptions,
		ProxmoxCT:         req.Labels["gow.pve"] == "true",
		LAN:               req.Labels["gow.lan"] == "true",
	}
	// LAN bridge replaces host networking: the container gets its own network
	// namespace with dual NICs (internal + LAN) instead of sharing the host's.
	if cfg.LAN && cfg.NetworkMode == "host" {
		cfg.NetworkMode = ""
	}

	// Mount collection. We assemble one list keyed by type so the store
	// record can echo the original semantic back on inspect (Portainer's
	// Mounts tab distinguishes bind/volume/tmpfs by a colored badge).
	var storeMounts []store.MountSpec
	// Parse legacy Binds ("host:container[:ro]").
	for _, bind := range req.HostConfig.Binds {
		parts := strings.SplitN(bind, ":", 3)
		if len(parts) < 2 {
			continue
		}
		m := lxc.MountSpec{
			Source:      parts[0],
			Destination: parts[1],
			ReadOnly:    len(parts) == 3 && parts[2] == "ro",
		}
		cfg.Mounts = append(cfg.Mounts, m)
		storeMounts = append(storeMounts, store.MountSpec{
			Type:        "bind",
			Source:      m.Source,
			Destination: m.Destination,
			ReadOnly:    m.ReadOnly,
		})
	}

	// Device mappings
	for _, d := range req.HostConfig.Devices {
		cfg.Devices = append(cfg.Devices, lxc.DeviceSpec{
			PathOnHost:      d.PathOnHost,
			PathInContainer: d.PathInContainer,
		})
	}

	// Anonymous volumes declared in Config.Volumes. Docker's runtime
	// creates a fresh named volume with a generated name per entry and
	// bind-mounts it at the requested path. Compose-style database
	// containers (`VOLUME /var/lib/postgresql/data` in the Dockerfile)
	// rely on this so state persists across container rebuilds even
	// without an explicit --mount flag.
	for path := range req.Volumes {
		if hasMountAt(storeMounts, path) {
			continue
		}
		volName := generateID()[:12] + "_anon"
		mountpoint, err := h.ensureVolumeOwned(volName, id, true)
		if err != nil {
			continue
		}
		storeMounts = append(storeMounts, store.MountSpec{
			Type:        "volume",
			Source:      mountpoint,
			Destination: path,
		})
		cfg.Mounts = append(cfg.Mounts, lxc.MountSpec{
			Source:      mountpoint,
			Destination: path,
		})
	}

	// New-style Mounts (Portainer emits these alongside or instead of Binds).
	// Bind mounts use the source path directly. Volume mounts resolve the
	// named volume to its backing directory (auto-creating it so anonymous
	// volumes and compose stacks with declared volumes both work). Tmpfs
	// mounts fall through to HostConfig.Tmpfs and are handled there.
	for _, m := range req.HostConfig.Mounts {
		mType := m.Type
		if mType == "" {
			mType = "bind"
		}
		source := m.Source
		if mType == "volume" {
			if resolved, err := h.ensureVolume(m.Source); err == nil {
				source = resolved
			} else {
				continue
			}
		}
		storeMounts = append(storeMounts, store.MountSpec{
			Type:        mType,
			Source:      source,
			Destination: m.Target,
			ReadOnly:    m.ReadOnly,
			Propagation: propagationFromBindOptions(m.BindOptions),
		})
		if mType == "tmpfs" {
			if cfg.Tmpfs == nil {
				cfg.Tmpfs = map[string]string{}
			}
			if _, already := cfg.Tmpfs[m.Target]; !already {
				cfg.Tmpfs[m.Target] = tmpfsOptionsString(m.TmpfsOptions, m.ReadOnly)
			}
			continue
		}
		cfg.Mounts = append(cfg.Mounts, lxc.MountSpec{
			Source:      source,
			Destination: m.Target,
			ReadOnly:    m.ReadOnly,
		})
	}

	// Preserve the full HostConfig as JSON so inspect can echo exactly what
	// the client posted, including fields the LXC runtime doesn't honor.
	rawHC, _ := json.Marshal(req.HostConfig)

	// Persist record before creating so the IP is allocated.
	rec := &store.ContainerRecord{
		ID:              id,
		Name:            name,
		Image:           req.Image,
		ImageID:         normalizeImageRef(req.Image),
		Created:         time.Now(),
		Entrypoint:      entrypoint,
		Cmd:             cmd,
		Env:             env,
		Labels:          req.Labels,
		Hostname:        req.Hostname,
		Domainname:      req.Domainname,
		User:            req.User,
		Tty:             req.Tty,
		OpenStdin:       req.OpenStdin,
		WorkingDir:      workingDir,
		StopSignal:      req.StopSignal,
		ExposedPorts:    req.ExposedPorts,
		Volumes:         req.Volumes,
		StopTimeout:     stopTimeoutValue(req.StopTimeout),
		OomScoreAdj:     req.HostConfig.OomScoreAdj,
		RawHostConfig:   rawHC,
		Mounts:          storeMounts,
		RestartPolicy:   req.HostConfig.RestartPolicy.Name,
		RestartMaxRetry: req.HostConfig.RestartPolicy.MaximumRetryCount,
		AutoRemove:      req.HostConfig.AutoRemove,
	}
	if hc := req.Healthcheck; hc != nil && len(hc.Test) > 0 {
		rec.HealthcheckTest = hc.Test
		rec.HealthcheckInterval = hc.Interval
		rec.HealthcheckTimeout = hc.Timeout
		rec.HealthcheckRetries = hc.Retries
		rec.HealthcheckStartPeriod = hc.StartPeriod
		rec.HealthStatus = "starting"
	}
	// Parse port bindings from HostConfig (e.g. "80/tcp" -> [{HostPort:8080, ContainerPort:80, Proto:"tcp"}])
	for containerPortProto, bindings := range req.HostConfig.PortBindings {
		parts := strings.SplitN(containerPortProto, "/", 2)
		cPort, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		proto := "tcp"
		if len(parts) == 2 && parts[1] != "" {
			proto = parts[1]
		}
		for _, b := range bindings {
			hPort, err := strconv.Atoi(b.HostPort)
			if err != nil {
				continue
			}
			rec.PortBindings = append(rec.PortBindings, store.PortBinding{
				HostPort:      hPort,
				ContainerPort: cPort,
				Proto:         proto,
			})
		}
	}

	if err := h.store.AddContainer(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	log.Printf("createContainer: creating LXC container %s (image=%s)", id[:12], req.Image)
	if err := h.mgr.CreateContainer(id, normalizeImageRef(req.Image), cfg); err != nil {
		log.Printf("createContainer: failed: %v", err)
		h.store.RemoveContainer(id)
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("createContainer: success %s", id[:12])

	h.emitContainer("create", h.store.GetContainer(id))

	jsonResponse(w, http.StatusCreated, ContainerCreateResponse{
		ID:       id,
		Warnings: []string{},
	})
}

// GET /containers/json
func (h *Handler) listContainers(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	withSize := r.URL.Query().Get("size") == "1" || r.URL.Query().Get("size") == "true"
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	filt := parseFilters(r)
	records := h.store.ListContainers()

	out := make([]ContainerSummary, 0, len(records))
	for _, rec := range records {
		state, _ := h.mgr.State(rec.ID)
		if state == "exited" && rec.StartedAt == nil {
			state = "created"
		}
		if !all && state != "running" {
			continue
		}
		// Filter evaluation. The "status" filter accepts the same state
		// strings returned in the list response. "label" supports both
		// existence ("com.docker.compose.project") and equality
		// ("com.docker.compose.project=foo") checks.
		if !filt.matchAny("status", state) {
			continue
		}
		if !filt.matchLabel(rec.Labels) {
			continue
		}
		if !filt.matchNamePrefix([]string{rec.Name, "/" + rec.Name}) {
			continue
		}
		if !filt.matchID(rec.ID) {
			continue
		}
		if !filt.matchAncestor(rec.Image, rec.ImageID) {
			continue
		}
		if filt.has("volume") && !containerUsesVolume(rec, h.store, filt["volume"]) {
			continue
		}
		if filt.has("network") && !containerOnNetwork(rec, filt["network"]) {
			continue
		}
		if filt.has("expose") && !containerExposes(h.exposedPortsFor(rec), filt["expose"]) {
			continue
		}
		// The "health" filter matches HealthStatus ("starting"/"healthy"/
		// "unhealthy") or the special value "none" for containers without
		// a configured healthcheck. Portainer's "Unhealthy" dashboard
		// widget relies on this.
		if filt.has("health") {
			hs := rec.HealthStatus
			if hs == "" {
				hs = "none"
			}
			if !filt.matchAny("health", hs) {
				continue
			}
		}
		cmd := strings.Join(append(rec.Entrypoint, rec.Cmd...), " ")
		ports := make([]Port, 0, len(rec.PortBindings))
		for _, pb := range rec.PortBindings {
			ports = append(ports, Port{
				IP:          "0.0.0.0",
				PrivatePort: uint16(pb.ContainerPort),
				PublicPort:  uint16(pb.HostPort),
				Type:        pb.Proto,
			})
		}
		mounts := make([]MountJSON, 0, len(rec.Mounts))
		for _, m := range rec.Mounts {
			mounts = append(mounts, h.mountJSONFrom(m))
		}
		sort.SliceStable(mounts, func(i, j int) bool {
			return mounts[i].Destination < mounts[j].Destination
		})
		summary := ContainerSummary{
			ID:      rec.ID,
			Names:   []string{"/" + rec.Name},
			Image:   normalizeImageRef(rec.Image),
			ImageID: rec.ImageID,
			Command: cmd,
			Created: rec.Created.Unix(),
			State:   state,
			Status:  stateToStatusFull(state, rec.Created, rec.HealthStatus, rec.ExitCode, rec.FinishedAt),
			Ports:   ports,
			Labels:  rec.Labels,
			Mounts:  mounts,
			HostConfig: &ContainerSummaryHostConfig{
				NetworkMode: networkModeFor(rec),
			},
			NetworkSettings: &ContainerSummaryNetSettings{
				Networks: networkSettingsFor(rec),
			},
		}
		if withSize {
			sz := rootfsSize(h.mgr.RootfsPath(rec.ID))
			summary.SizeRw = sz
			summary.SizeRootFs = sz
		}
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Created > out[j].Created
	})
	if before := filt["before"]; len(before) > 0 {
		out = trimBefore(out, before, h.store)
	}
	if since := filt["since"]; len(since) > 0 {
		out = trimSince(out, since, h.store)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	jsonResponse(w, http.StatusOK, out)
}

func trimBefore(list []ContainerSummary, refs []string, st *store.Store) []ContainerSummary {
	cutoff := refCutoff(refs, st)
	if cutoff == 0 {
		return list
	}
	out := list[:0]
	for _, c := range list {
		if c.Created < cutoff {
			out = append(out, c)
		}
	}
	return out
}

func trimSince(list []ContainerSummary, refs []string, st *store.Store) []ContainerSummary {
	cutoff := refCutoff(refs, st)
	if cutoff == 0 {
		return list
	}
	out := list[:0]
	for _, c := range list {
		if c.Created > cutoff {
			out = append(out, c)
		}
	}
	return out
}

func refCutoff(refs []string, st *store.Store) int64 {
	for _, r := range refs {
		id := st.ResolveID(r)
		if id == "" {
			continue
		}
		if rec := st.GetContainer(id); rec != nil {
			return rec.Created.Unix()
		}
	}
	return 0
}

// networkModeFor returns the NetworkMode string Portainer displays in the list
// view. Matches the resolution in the create handler.
func networkModeFor(rec *store.ContainerRecord) string {
	if rec.Labels["gow.lan"] == "true" {
		return "lan"
	}
	return "gow"
}

// networkSettingsFor builds the per-network endpoint map for a container.
// One entry per attached network ("gow" is the daemon's managed bridge).
func networkSettingsFor(rec *store.ContainerRecord) map[string]EndpointSettings {
	if rec.IPAddress == "" {
		return map[string]EndpointSettings{}
	}
	return map[string]EndpointSettings{
		"gow": {
			NetworkID:   "gow",
			EndpointID:  rec.ID,
			Gateway:     lxc.BridgeGW,
			IPAddress:   rec.IPAddress,
			IPPrefixLen: 24,
			Aliases:     []string{rec.Name},
		},
	}
}

// GET /containers/{id}/json
func (h *Handler) inspectContainer(w http.ResponseWriter, r *http.Request) {
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

	state, _ := h.mgr.State(id)
	running := state == "running"
	paused := state == "paused"
	if paused {
		running = true // Docker reports Running=true and Paused=true when frozen.
	}

	// A container that was never started should report "created", not "exited".
	// Docker uses "created" for containers that exist but have never been started.
	if state == "exited" && rec.StartedAt == nil {
		state = "created"
	}

	startedAt := "0001-01-01T00:00:00Z"
	if rec.StartedAt != nil {
		startedAt = rec.StartedAt.Format(time.RFC3339Nano)
	}
	finishedAt := "0001-01-01T00:00:00Z"
	if rec.FinishedAt != nil {
		finishedAt = rec.FinishedAt.Format(time.RFC3339Nano)
	}

	// Build Mounts array from stored mount specs.
	mounts := make([]MountJSON, 0, len(rec.Mounts))
	for _, m := range rec.Mounts {
		mounts = append(mounts, h.mountJSONFrom(m))
	}
	sort.SliceStable(mounts, func(i, j int) bool {
		return mounts[i].Destination < mounts[j].Destination
	})

	// Split entrypoint[0] as Path and the rest as Args, matching what Docker
	// does for `docker inspect`. Portainer renders these on the detail page.
	path := ""
	args := []string{}
	combined := append([]string{}, rec.Entrypoint...)
	combined = append(combined, rec.Cmd...)
	if len(combined) > 0 {
		path = combined[0]
		args = combined[1:]
	}

	ports := map[string][]PortBinding{}
	for _, pb := range rec.PortBindings {
		key := fmt.Sprintf("%d/%s", pb.ContainerPort, pb.Proto)
		ports[key] = append(ports[key], PortBinding{
			HostIP:   "0.0.0.0",
			HostPort: strconv.Itoa(pb.HostPort),
		})
	}

	hostname := rec.Hostname
	if hostname == "" {
		hostname = rec.ID[:12]
	}

	// Grab the container's init PID (0 if stopped) so Portainer can display it.
	pid := 0
	if running {
		pid = containerPID(id)
	}

	resp := ContainerJSON{
		ID:             rec.ID,
		Created:        rec.Created.Format(time.RFC3339Nano),
		Path:           path,
		Args:           args,
		Name:           "/" + rec.Name,
		ResolvConfPath: "",
		HostnamePath:   "",
		LogPath:        h.mgr.LogPath(id),
		RestartCount:   rec.RestartCount,
		Driver:         "lxc",
		Platform:       "linux",
		GraphDriver: GraphDriver{
			Name: "lxc",
			Data: map[string]string{},
		},
		State: ContainerState{
			Status:     state,
			Running:    running,
			Paused:     paused,
			Pid:        pid,
			ExitCode:   rec.ExitCode,
			Error:      rec.StartError,
			StartedAt:  startedAt,
			FinishedAt: finishedAt,
			Health:     healthStateFrom(rec),
		},
		Image:  rec.Image,
		Mounts: mounts,
		Config: &ContainerConfig{
			Hostname:     hostname,
			Domainname:   rec.Domainname,
			User:         rec.User,
			Tty:          rec.Tty,
			OpenStdin:    rec.OpenStdin,
			ExposedPorts: h.exposedPortsFor(rec),
			Image:        rec.Image,
			Volumes:      ensureStructMap(rec.Volumes),
			Cmd:          ensureSlice(rec.Cmd),
			Entrypoint:   ensureSlice(rec.Entrypoint),
			Env:          ensureSlice(rec.Env),
			Labels:       ensureMap(rec.Labels),
			WorkingDir:   rec.WorkingDir,
			StopSignal:   rec.StopSignal,
			StopTimeout:  stopTimeoutPtr(rec.StopTimeout),
			Healthcheck:  healthcheckFrom(rec),
		},
		HostConfig: buildHostConfig(rec),
		NetworkSettings: NetworkSettings{
			Bridge:      lxc.BridgeName,
			IPAddress:   rec.IPAddress,
			IPPrefixLen: 24,
			Gateway:     lxc.BridgeGW,
			Ports:       ports,
			Networks:    networkSettingsFor(rec),
		},
	}
	if r.URL.Query().Get("size") == "1" || r.URL.Query().Get("size") == "true" {
		sz := rootfsSize(h.mgr.RootfsPath(id))
		resp.SizeRw = sz
		resp.SizeRootFs = sz
	}
	jsonResponse(w, http.StatusOK, resp)
}

// POST /containers/{id}/start
func (h *Handler) startContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if s, _ := h.mgr.State(id); s == "running" {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if err := h.mgr.StartContainer(id); err != nil {
		// Persist the failure on the record so inspect shows it in
		// State.Error. Portainer's detail page renders this alongside
		// the red "failed to start" toast, and the user can see the
		// underlying reason without chasing daemon logs.
		if rec := h.store.GetContainer(id); rec != nil {
			rec.StartError = err.Error()
			h.store.AddContainer(rec)
		}
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Clear any stale error from a previous failed start.
	if rec := h.store.GetContainer(id); rec != nil && rec.StartError != "" {
		rec.StartError = ""
		h.store.AddContainer(rec)
	}

	// Refresh StartedAt (Docker updates it on every start, not just the
	// first), clear the user-stopped flag so the restart watcher enforces
	// the policy again on subsequent exits, and clear FinishedAt now that
	// the container is running again.
	if rec := h.store.GetContainer(id); rec != nil {
		now := time.Now()
		rec.StartedAt = &now
		rec.StoppedByUser = false
		rec.FinishedAt = nil
		h.store.AddContainer(rec)
	}

	if rec := h.store.GetContainer(id); rec != nil && rec.IPAddress != "" {
		for _, pb := range rec.PortBindings {
			if err := lxc.AddPortForward(rec.IPAddress, pb.HostPort, pb.ContainerPort, pb.Proto); err != nil {
				log.Printf("warning: port forward %d->%s:%d/%s failed: %v",
					pb.HostPort, rec.IPAddress, pb.ContainerPort, pb.Proto, err)
			}
		}
	}

	if rec := h.store.GetContainer(id); rec != nil && rec.OomScoreAdj != 0 {
		if pid := containerPID(id); pid > 0 {
			if err := os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid),
				[]byte(strconv.Itoa(rec.OomScoreAdj)), 0o644); err != nil {
				log.Printf("warning: set oom_score_adj for %s: %v", id[:12], err)
			}
		}
	}

	h.emitContainer("start", h.store.GetContainer(id))
	w.WriteHeader(http.StatusNoContent)
}

// POST /containers/{id}/stop
func (h *Handler) stopContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if s, _ := h.mgr.State(id); s != "running" && s != "paused" {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	stopSig := ""
	defaultTO := 0
	if rec := h.store.GetContainer(id); rec != nil {
		stopSig = rec.StopSignal
		defaultTO = rec.StopTimeout
	}
	if err := h.mgr.StopContainerWithSignal(id, stopTimeoutWithDefault(r, defaultTO), stopSig); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	rec := h.store.GetContainer(id)
	if rec != nil {
		// Mark as user-stopped so the restart watcher doesn't bring it
		// back when the policy is unless-stopped / always, and record
		// the finish time so inspect shows "Stopped X ago".
		rec.StoppedByUser = true
		now := time.Now()
		rec.FinishedAt = &now
		h.store.AddContainer(rec)
	}
	h.emitContainer("stop", rec)
	h.emitContainer("die", rec)
	w.WriteHeader(http.StatusNoContent)
}

// stopTimeout reads the Docker-standard `t` query param (seconds before
// SIGKILL). Missing / malformed falls back to 10 s, matching Docker's
// default. Negative values disable the timeout — we translate that to an
// effectively unbounded wait (1 hour) rather than block forever.
func stopTimeout(r *http.Request) time.Duration {
	return stopTimeoutWithDefault(r, 0)
}

func stopTimeoutWithDefault(r *http.Request, defaultSec int) time.Duration {
	v := r.URL.Query().Get("t")
	if v == "" {
		if defaultSec > 0 {
			return time.Duration(defaultSec) * time.Second
		}
		return 10 * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 10 * time.Second
	}
	if n < 0 {
		return time.Hour
	}
	return time.Duration(n) * time.Second
}

func stopTimeoutValue(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func stopTimeoutPtr(v int) *int {
	if v <= 0 {
		return nil
	}
	return &v
}

// POST /containers/{id}/wait
func (h *Handler) waitContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	condition := r.URL.Query().Get("condition")
	if condition == "" {
		condition = "not-running"
	}
	wasRunning := false
	if state, _ := h.mgr.State(id); state == "running" {
		wasRunning = true
	}
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		switch condition {
		case "removed":
			if h.store.GetContainer(id) == nil {
				jsonResponse(w, http.StatusOK, map[string]any{"StatusCode": 0, "Error": nil})
				return
			}
		case "next-exit":
			state, _ := h.mgr.State(id)
			if !wasRunning && state == "running" {
				wasRunning = true
			}
			if wasRunning && state != "running" {
				jsonResponse(w, http.StatusOK, map[string]any{"StatusCode": 0, "Error": nil})
				return
			}
		default:
			state, _ := h.mgr.State(id)
			if state != "running" {
				jsonResponse(w, http.StatusOK, map[string]any{"StatusCode": 0, "Error": nil})
				return
			}
		}
	}
}

// POST /containers/{id}/kill
func (h *Handler) killContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	signal := r.URL.Query().Get("signal")
	if signal != "" && !isValidSignal(signal) {
		errResponse(w, http.StatusBadRequest, fmt.Sprintf("Invalid signal: %s", signal))
		return
	}
	if s, _ := h.mgr.State(id); s != "running" && s != "paused" {
		errResponse(w, http.StatusConflict, "Container is not running")
		return
	}
	if err := h.mgr.KillContainer(id, signal); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	rec := h.store.GetContainer(id)
	if rec != nil {
		rec.StoppedByUser = true
		now := time.Now()
		rec.FinishedAt = &now
		h.store.AddContainer(rec)
	}
	h.emitContainer("kill", rec)
	h.emitContainer("die", rec)
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /containers/{id}
func (h *Handler) removeContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if r.URL.Query().Get("link") == "1" || r.URL.Query().Get("link") == "true" {
		errResponse(w, http.StatusBadRequest, "container links are not supported by docker-lxc-daemon")
		return
	}
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"

	if force {
		state, _ := h.mgr.State(id)
		if state == "paused" {
			h.mgr.UnpauseContainer(id)
			state, _ = h.mgr.State(id)
		}
		if state == "running" {
			h.mgr.StopContainer(id, 5*time.Second)
		}
	} else {
		if state, _ := h.mgr.State(id); state == "running" || state == "paused" {
			errResponse(w, http.StatusConflict,
				"You cannot remove a running container. Stop the container before attempting removal or use force")
			return
		}
	}

	// Snapshot the record before removing so the emitted event carries a name.
	rec := h.store.GetContainer(id)

	// Remove port forwarding rules before destroying the container.
	if rec != nil && rec.IPAddress != "" {
		if err := lxc.RemovePortForwards(rec.IPAddress); err != nil {
			log.Printf("warning: removing port forwards for %s: %v", rec.IPAddress, err)
		}
	}

	if err := h.mgr.RemoveContainer(id); err != nil {
		errResponse(w, http.StatusConflict, err.Error())
		return
	}
	os.Remove(h.mgr.LogPath(id))
	os.Remove(h.mgr.LogPath(id) + ".1")
	if r.URL.Query().Get("v") == "1" || r.URL.Query().Get("v") == "true" {
		h.removeAnonVolumesOf(id)
	}
	h.emitContainer("destroy", rec)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeAnonVolumesOf(id string) {
	for _, v := range h.store.ListVolumes() {
		if !v.Anonymous || v.OwnerID != id {
			continue
		}
		os.RemoveAll(v.Mountpoint)
		h.store.RemoveVolume(v.Name)
	}
}

// GET /containers/{id}/logs
//
// Query params honored (matching Docker Engine):
//   - stdout=1 / stderr=1  — which streams to return (default both)
//   - tail=N | tail=all    — last N lines of the pre-existing log
//   - since=<unix-ts>      — drop lines older than the timestamp
//   - until=<unix-ts>      — drop lines newer than the timestamp
//   - timestamps=1         — prefix each line with RFC3339Nano
//   - follow=1             — keep the connection open and stream new lines
//
// LXC writes a single interleaved console log (stdout+stderr merged), so we
// emit every line as frame type 1 (stdout) regardless of the stderr flag.
// Portainer accepts this gracefully; the real Docker daemon also can't
// separate the streams for containers started with a TTY.
func (h *Handler) containerLogs(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}

	q := r.URL.Query()
	stdout := q.Get("stdout") == "1" || q.Get("stdout") == "true"
	stderr := q.Get("stderr") == "1" || q.Get("stderr") == "true"
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"
	timestamps := q.Get("timestamps") == "1" || q.Get("timestamps") == "true"
	if !stdout && !stderr {
		stdout, stderr = true, true
	}
	tail := parseTail(q.Get("tail")) // -1 means "all"
	since := parseUnixTS(q.Get("since"))
	until := parseUnixTS(q.Get("until"))

	logPath := h.mgr.LogPath(id)
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)

	streamID := byte(1)
	if !stdout && stderr {
		streamID = 2
	}

	// When since/until are active we gate every line through the current
	// wall clock — the console.log has no per-line timestamps, so the best
	// we can do is accept lines when "now" is inside the window. For a
	// live-tailing session (follow=1) this filters out nothing initially
	// but correctly clips when until fires.
	inWindow := func() bool {
		t := time.Now()
		if !since.IsZero() && t.Before(since) {
			return false
		}
		if !until.IsZero() && t.After(until) {
			return false
		}
		return true
	}
	emit := func(line []byte) {
		if timestamps {
			prefix := time.Now().UTC().Format(time.RFC3339Nano) + " "
			line = append([]byte(prefix), line...)
		}
		writeLogFrame(w, streamID, line)
	}

	// Backfill phase. When tail is set we collect the last N lines into a
	// ring buffer instead of streaming everything — the default Portainer
	// log view requests tail=100 and otherwise the UI would download the
	// full console log for every open. If the live log was recently
	// rotated we may need to pull older lines from <log>.1 to satisfy the
	// request; otherwise tail=100 returns mostly-empty output right after
	// a rotation.
	if tail != 0 {
		var lines [][]byte
		liveLines, _ := readTail(f, tail)
		// Rotation preserves the previous tail as <log>.1. Read it when
		// the live file doesn't cover the requested window.
		if tail < 0 || len(liveLines) < tail {
			if older, err := readRotatedTail(logPath+".1", tail-len(liveLines)); err == nil {
				lines = append(lines, older...)
			}
		}
		lines = append(lines, liveLines...)
		for _, line := range lines {
			if !inWindow() {
				continue
			}
			emit(line)
		}
	}

	if !follow {
		return
	}

	// Seek to end so the tail loop only picks up new writes.
	f.Seek(0, io.SeekEnd)
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
		buf := make([]byte, 32*1024)
		n, err := f.Read(buf)
		if n > 0 && inWindow() {
			if timestamps {
				emit(buf[:n])
			} else {
				writeLogFrame(w, streamID, buf[:n])
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		if !until.IsZero() && time.Now().After(until) {
			return
		}
		if err == io.EOF {
			continue
		}
		if err != nil {
			return
		}
	}
}

// parseTail interprets the Docker `tail` query value. "all" (or empty) means
// stream the whole backlog, a positive integer caps the backfill, and 0
// suppresses backfill entirely. Returns -1 for "all".
func parseTail(v string) int {
	if v == "" || v == "all" {
		return -1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// parseUnixTS parses the Docker log `since`/`until` values. Accepts integer
// seconds; nanosecond or RFC3339 forms are uncommon from Portainer and
// aren't worth the extra handling.
func parseUnixTS(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(ts, 0)
	}
	if ts, err := strconv.ParseFloat(v, 64); err == nil {
		sec := int64(ts)
		nsec := int64((ts - float64(sec)) * 1e9)
		return time.Unix(sec, nsec)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// readRotatedTail opens a rotated log file (<log>.1) and returns up to n
// trailing lines. Missing file is not an error — rotation is lazy, so the
// rotated file is often absent. n < 0 requests all lines.
func readRotatedTail(path string, n int) ([][]byte, error) {
	if n == 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil // missing is fine
	}
	defer f.Close()
	return readTail(f, n)
}

// readTail returns up to n trailing lines of f. n < 0 returns all lines. The
// ring buffer keeps memory bounded to n; the file is read sequentially from
// the beginning (reading from the tail would need os.Seek + reverse scan,
// which bufio doesn't do well with multi-byte lines).
func readTail(f *os.File, n int) ([][]byte, error) {
	scanner := bufio.NewScanner(f)
	// Console log lines are usually short, but allow up to 1 MiB per line so
	// a long warning doesn't bufio.ErrTooLong us out.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if n < 0 {
		var all [][]byte
		for scanner.Scan() {
			line := append([]byte{}, scanner.Bytes()...)
			all = append(all, append(line, '\n'))
		}
		return all, scanner.Err()
	}
	if n == 0 {
		return nil, nil
	}
	ring := make([][]byte, 0, n)
	for scanner.Scan() {
		line := append([]byte{}, scanner.Bytes()...)
		line = append(line, '\n')
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}
	return ring, scanner.Err()
}

// POST /containers/{id}/restart
func (h *Handler) restartContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	state, _ := h.mgr.State(id)
	if state == "running" {
		defaultTO := 0
		if rec := h.store.GetContainer(id); rec != nil {
			defaultTO = rec.StopTimeout
		}
		if err := h.mgr.StopContainer(id, stopTimeoutWithDefault(r, defaultTO)); err != nil {
			errResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		rec := h.store.GetContainer(id)
		h.emitContainer("die", rec)
	}
	if err := h.mgr.StartContainer(id); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	rec := h.store.GetContainer(id)
	h.emitContainer("start", rec)
	h.emitContainer("restart", rec)
	w.WriteHeader(http.StatusNoContent)
}

// POST /containers/{id}/rename
func (h *Handler) renameContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	newName := r.URL.Query().Get("name")
	if newName == "" {
		errResponse(w, http.StatusBadRequest, "name is required")
		return
	}
	newName = strings.TrimSpace(strings.TrimPrefix(newName, "/"))
	if !isValidContainerName(newName) {
		errResponse(w, http.StatusBadRequest, fmt.Sprintf("Invalid container name (%s)", newName))
		return
	}
	rec := h.store.GetContainer(id)
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	if other := h.store.FindContainerByName(newName); other != nil && other.ID != id {
		errResponse(w, http.StatusConflict,
			fmt.Sprintf("Conflict. The container name %q is already in use.", "/"+newName))
		return
	}
	oldName := rec.Name
	rec.Name = newName
	if err := h.store.AddContainer(rec); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The rename event carries an oldName attribute so Portainer's event
	// feed can render "foo renamed to bar". Docker prefixes names with a
	// leading slash on the wire.
	h.emitContainerWithAttrs("rename", rec, map[string]string{
		"oldName": "/" + oldName,
	})
	w.WriteHeader(http.StatusNoContent)
}

// GET /containers/{id}/top
func (h *Handler) topContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	state, _ := h.mgr.State(id)
	if state != "running" {
		errResponse(w, http.StatusConflict, "container is not running")
		return
	}
	psArgs := r.URL.Query().Get("ps_args")
	if psArgs == "" {
		psArgs = "-ef"
	}
	psCmd := append([]string{"ps"}, strings.Fields(psArgs)...)
	cmd := h.mgr.Exec(id, psCmd, nil)
	out, err := cmd.Output()
	if err != nil {
		if titles, procs, ok := procTop(containerPID(id)); ok {
			jsonResponse(w, http.StatusOK, map[string]any{
				"Titles":    titles,
				"Processes": procs,
			})
			return
		}
		errResponse(w, http.StatusInternalServerError, fmt.Sprintf("ps: %v", err))
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		jsonResponse(w, http.StatusOK, map[string]any{
			"Titles":    []string{},
			"Processes": [][]string{},
		})
		return
	}
	titles := strings.Fields(lines[0])
	processes := make([][]string, 0, len(lines)-1)
	cols := len(titles)
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < cols {
			continue
		}
		row := append([]string{}, fields[:cols-1]...)
		row = append(row, strings.Join(fields[cols-1:], " "))
		processes = append(processes, row)
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"Titles":    titles,
		"Processes": processes,
	})
}

func procTop(initPID int) ([]string, [][]string, bool) {
	if initPID <= 0 {
		return nil, nil, false
	}
	pids := map[int]bool{initPID: true}
	pids = walkChildren(initPID, pids)
	titles := []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}
	var rows [][]string
	for pid := range pids {
		row := readProcRow(pid)
		if row != nil {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, nil, false
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i][1] < rows[j][1] })
	return titles, rows, true
}

func walkChildren(pid int, seen map[int]bool) map[int]bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", pid, pid))
	if err != nil {
		return seen
	}
	for _, f := range strings.Fields(string(data)) {
		child, err := strconv.Atoi(f)
		if err != nil || seen[child] {
			continue
		}
		seen[child] = true
		walkChildren(child, seen)
	}
	return seen
}

func readProcRow(pid int) []string {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return nil
	}
	var uid, ppid, name string
	for _, line := range strings.Split(string(status), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Uid":
			if fields := strings.Fields(v); len(fields) > 0 {
				uid = fields[0]
			}
		case "PPid":
			ppid = v
		case "Name":
			name = v
		}
	}
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	cmd := name
	if len(cmdline) > 0 {
		cmd = strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
	}
	return []string{uid, strconv.Itoa(pid), ppid, "0", "00:00", "?", "00:00:00", cmd}
}

// POST /containers/{id}/attach
func (h *Handler) attachContainer(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}

	q := r.URL.Query()
	logsFlag := q.Get("logs") == "1" || q.Get("logs") == "true"
	streamFlag := q.Get("stream") != "0" && q.Get("stream") != "false"

	hj, ok := w.(http.Hijacker)
	if !ok {
		errResponse(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	buf.WriteString("HTTP/1.1 101 UPGRADED\r\n")
	buf.WriteString("Content-Type: application/vnd.docker.raw-stream\r\n")
	buf.WriteString("Connection: Upgrade\r\n")
	buf.WriteString("Upgrade: tcp\r\n")
	buf.WriteString("\r\n")
	buf.Flush()

	logPath := h.mgr.LogPath(id)
	if logsFlag {
		if f, err := os.Open(logPath); err == nil {
			io.Copy(conn, f)
			f.Close()
		}
	}
	if !streamFlag {
		return
	}

	f, err := os.OpenFile(logPath, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)

	ctx := r.Context()
	buffer := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if s, _ := h.mgr.State(id); s != "running" && s != "paused" {
			return
		}
		n, readErr := f.Read(buffer)
		if n > 0 {
			if _, err := conn.Write(buffer[:n]); err != nil {
				return
			}
		}
		if readErr == io.EOF || n == 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if readErr != nil {
			return
		}
	}
}

// safeJoin joins base and untrusted path, returning an error if the result
// escapes base. Prevents path traversal attacks in docker cp.
func safeJoin(base, untrusted string) (string, error) {
	target := filepath.Join(base, filepath.Clean("/"+untrusted))
	if !strings.HasPrefix(target, filepath.Clean(base)+string(os.PathSeparator)) && target != filepath.Clean(base) {
		return "", fmt.Errorf("path %q escapes rootfs", untrusted)
	}
	return target, nil
}

// PUT /containers/{id}/archive — docker cp TO container
func (h *Handler) putArchive(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	destPath := r.URL.Query().Get("path")
	if destPath == "" {
		errResponse(w, http.StatusBadRequest, "path is required")
		return
	}
	rootfs := h.mgr.RootfsPath(id)
	dest, err := safeJoin(rootfs, destPath)
	if err != nil {
		errResponse(w, http.StatusForbidden, err.Error())
		return
	}

	tr := tar.NewReader(r.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			errResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Reject symlinks — they can be used to escape the rootfs.
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			continue
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			errResponse(w, http.StatusForbidden, err.Error())
			return
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				errResponse(w, http.StatusInternalServerError, err.Error())
				return
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				errResponse(w, http.StatusInternalServerError, err.Error())
				return
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				errResponse(w, http.StatusInternalServerError, err.Error())
				return
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				errResponse(w, http.StatusInternalServerError, err.Error())
				return
			}
			f.Close()
		}
	}
	w.WriteHeader(http.StatusOK)
}

// GET /containers/{id}/archive — docker cp FROM container
func (h *Handler) getArchive(w http.ResponseWriter, r *http.Request) {
	id := h.resolveID(mux.Vars(r)["id"])
	if id == "" {
		errResponse(w, http.StatusNotFound, "No such container")
		return
	}
	srcPath := r.URL.Query().Get("path")
	if srcPath == "" {
		errResponse(w, http.StatusBadRequest, "path is required")
		return
	}
	rootfs := h.mgr.RootfsPath(id)
	src, err := safeJoin(rootfs, srcPath)
	if err != nil {
		errResponse(w, http.StatusForbidden, err.Error())
		return
	}

	// Use Lstat to detect symlinks without following them.
	info, err := os.Lstat(src)
	if err != nil {
		errResponse(w, http.StatusNotFound, fmt.Sprintf("no such file: %s", srcPath))
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		errResponse(w, http.StatusForbidden, "refusing to follow symlink")
		return
	}

	// Docker CLI requires X-Docker-Container-Path-Stat header.
	stat := map[string]any{
		"name":       info.Name(),
		"size":       info.Size(),
		"mode":       info.Mode(),
		"mtime":      info.ModTime().Format(time.RFC3339),
		"linkTarget": "",
	}
	statJSON, _ := json.Marshal(stat)
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(statJSON))
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	tw := tar.NewWriter(w)
	defer tw.Close()

	if !info.IsDir() {
		tw.WriteHeader(&tar.Header{
			Name: filepath.Base(srcPath),
			Size: info.Size(),
			Mode: int64(info.Mode()),
		})
		f, err := os.Open(src)
		if err != nil {
			return
		}
		io.Copy(tw, f)
		f.Close()
		return
	}

	filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Skip symlinks to prevent escape.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		hdr, _ := tar.FileInfoHeader(fi, "")
		hdr.Name = rel
		tw.WriteHeader(hdr)
		if !d.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			io.Copy(tw, f)
			f.Close()
		}
		return nil
	})
}

// writeLogFrame writes a single Docker multiplexed stream frame.
// streamType: 1=stdout, 2=stderr.
func writeLogFrame(w io.Writer, streamType byte, data []byte) {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	w.Write(header)
	w.Write(data)
}

// stateToStatus returns a human-readable status string like Docker's "Up 2 hours".
func stateToStatus(state string, created time.Time) string {
	return stateToStatusFull(state, created, "", 0, nil)
}

func stateToStatusWithHealth(state string, created time.Time, health string) string {
	return stateToStatusFull(state, created, health, 0, nil)
}

func stateToStatusFull(state string, created time.Time, health string, exitCode int, finishedAt *time.Time) string {
	base := ""
	switch state {
	case "running":
		base = "Up " + humanDuration(time.Since(created))
	case "paused":
		base = "Up " + humanDuration(time.Since(created)) + " (Paused)"
	case "created":
		return "Created"
	case "exited":
		since := time.Since(created)
		if finishedAt != nil {
			since = time.Since(*finishedAt)
		}
		return fmt.Sprintf("Exited (%d) %s ago", exitCode, humanDuration(since))
	default:
		return state
	}
	switch health {
	case "healthy":
		return base + " (healthy)"
	case "unhealthy":
		return base + " (unhealthy)"
	case "starting":
		return base + " (health: starting)"
	}
	return base
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func (h *Handler) resolveID(idOrName string) string {
	return h.store.ResolveID(idOrName)
}

// buildHostConfig reconstructs a HostConfig from the stored container record.
// If the full create-time HostConfig was persisted, we replay that verbatim
// so Portainer's "Duplicate" flow sees the exact fields it posted; otherwise
// we synthesize the minimum set from typed record fields.
func buildHostConfig(rec *store.ContainerRecord) *HostConfig {
	hc := &HostConfig{}
	if len(rec.RawHostConfig) > 0 {
		if err := json.Unmarshal(rec.RawHostConfig, hc); err != nil {
			// Fall through to synthesized config on decode error.
			hc = &HostConfig{}
		}
	}
	if hc.NetworkMode == "" {
		hc.NetworkMode = networkModeFor(rec)
	}
	// Rebuild PortBindings/Binds from the typed record so that runtime state
	// (e.g. dynamically allocated ports) wins over the stored create body.
	if len(rec.PortBindings) > 0 {
		hc.PortBindings = make(map[string][]PortBinding)
		for _, pb := range rec.PortBindings {
			key := fmt.Sprintf("%d/%s", pb.ContainerPort, pb.Proto)
			hc.PortBindings[key] = append(hc.PortBindings[key], PortBinding{
				HostIP:   "0.0.0.0",
				HostPort: strconv.Itoa(pb.HostPort),
			})
		}
	}
	if len(rec.Mounts) > 0 {
		hc.Binds = hc.Binds[:0]
		for _, m := range rec.Mounts {
			bind := m.Source + ":" + m.Destination
			if m.ReadOnly {
				bind += ":ro"
			}
			hc.Binds = append(hc.Binds, bind)
		}
	}
	normalizeHostConfig(hc)
	return hc
}

func normalizeHostConfig(hc *HostConfig) {
	if hc.Binds == nil {
		hc.Binds = []string{}
	}
	if hc.Devices == nil {
		hc.Devices = []DeviceMapping{}
	}
	if hc.DeviceCgroupRules == nil {
		hc.DeviceCgroupRules = []string{}
	}
	if hc.CapAdd == nil {
		hc.CapAdd = []string{}
	}
	if hc.CapDrop == nil {
		hc.CapDrop = []string{}
	}
	if hc.SecurityOpt == nil {
		hc.SecurityOpt = []string{}
	}
	if hc.GroupAdd == nil {
		hc.GroupAdd = []string{}
	}
	if hc.PortBindings == nil {
		hc.PortBindings = map[string][]PortBinding{}
	}
}

func (h *Handler) exposedPortsFor(rec *store.ContainerRecord) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range rec.ExposedPorts {
		out[k] = struct{}{}
	}
	for _, pb := range rec.PortBindings {
		out[fmt.Sprintf("%d/%s", pb.ContainerPort, pb.Proto)] = struct{}{}
	}
	if img := h.store.GetImage(normalizeImageRef(rec.Image)); img != nil {
		for _, p := range img.OCIPorts {
			out[p] = struct{}{}
		}
	}
	return out
}

func tmpfsOptionsString(opts map[string]any, readOnly bool) string {
	flags := []string{"nosuid", "nodev"}
	if readOnly {
		flags = append(flags, "ro")
	} else {
		flags = append([]string{"rw"}, flags...)
	}
	if opts != nil {
		if v, ok := opts["SizeBytes"]; ok {
			switch n := v.(type) {
			case float64:
				if n > 0 {
					flags = append(flags, fmt.Sprintf("size=%d", int64(n)))
				}
			case int64:
				if n > 0 {
					flags = append(flags, fmt.Sprintf("size=%d", n))
				}
			case int:
				if n > 0 {
					flags = append(flags, fmt.Sprintf("size=%d", n))
				}
			}
		}
		if v, ok := opts["Mode"]; ok {
			if mode, ok := v.(float64); ok && mode > 0 {
				flags = append(flags, fmt.Sprintf("mode=0%o", int(mode)))
			}
		}
	}
	return strings.Join(flags, ",")
}

func propagationFromBindOptions(opts map[string]any) string {
	if opts == nil {
		return ""
	}
	v, ok := opts["Propagation"]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	switch s {
	case "rprivate", "private", "rshared", "shared", "rslave", "slave":
		return s
	}
	return ""
}

func apiToLXCUlimits(u []Ulimit) []lxc.Ulimit {
	if len(u) == 0 {
		return nil
	}
	out := make([]lxc.Ulimit, 0, len(u))
	for _, x := range u {
		out = append(out, lxc.Ulimit{Name: x.Name, Soft: x.Soft, Hard: x.Hard})
	}
	return out
}

func ensureSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func ensureMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func ensureStructMap(m map[string]struct{}) map[string]struct{} {
	if m == nil {
		return map[string]struct{}{}
	}
	return m
}

func containerExposes(exposed map[string]struct{}, wants []string) bool {
	if len(exposed) == 0 {
		return false
	}
	for _, want := range wants {
		if _, ok := exposed[want]; ok {
			return true
		}
		if !strings.Contains(want, "/") {
			if _, ok := exposed[want+"/tcp"]; ok {
				return true
			}
			if _, ok := exposed[want+"/udp"]; ok {
				return true
			}
		}
	}
	return false
}

func containerOnNetwork(rec *store.ContainerRecord, names []string) bool {
	attached := map[string]bool{networkModeFor(rec): true, "gow": true}
	for _, want := range names {
		if attached[want] {
			return true
		}
	}
	return false
}

func containerUsesVolume(rec *store.ContainerRecord, st *store.Store, names []string) bool {
	sources := map[string]bool{}
	for _, name := range names {
		if v := st.GetVolume(name); v != nil {
			sources[v.Mountpoint] = true
		}
		sources[name] = true
	}
	for _, m := range rec.Mounts {
		if sources[m.Source] {
			return true
		}
	}
	return false
}

func isValidSignal(s string) bool {
	if strings.ContainsAny(s, " \t\n") {
		return false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n >= 0 && n <= 64
	}
	bare := strings.TrimPrefix(strings.ToUpper(s), "SIG")
	if bare == "" {
		return false
	}
	for _, r := range bare {
		if !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '+' || r == '-') {
			return false
		}
	}
	return true
}

func isValidRestartPolicy(name string) bool {
	switch name {
	case "", "no", "always", "unless-stopped", "on-failure":
		return true
	}
	return false
}

func isValidContainerName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || r == '.' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			if i == 0 && (r == '_' || r == '.' || r == '-') {
				return false
			}
			continue
		}
		return false
	}
	return true
}

func healthcheckFrom(rec *store.ContainerRecord) *Healthcheck {
	if len(rec.HealthcheckTest) == 0 {
		return nil
	}
	return &Healthcheck{
		Test:        rec.HealthcheckTest,
		Interval:    rec.HealthcheckInterval,
		Timeout:     rec.HealthcheckTimeout,
		Retries:     rec.HealthcheckRetries,
		StartPeriod: rec.HealthcheckStartPeriod,
	}
}

// healthStateFrom renders the stored health fields as the State.Health
// object Portainer reads. Returns nil when no healthcheck is configured so
// inspect omits the key entirely (matching Docker's behavior).
func healthStateFrom(rec *store.ContainerRecord) *HealthState {
	if len(rec.HealthcheckTest) == 0 {
		return nil
	}
	status := rec.HealthStatus
	if status == "" {
		status = "starting"
	}
	log := make([]ContainerHealthEntry, 0, len(rec.HealthLog))
	for _, r := range rec.HealthLog {
		log = append(log, ContainerHealthEntry{
			Start:    r.Start.Format(time.RFC3339Nano),
			End:      r.End.Format(time.RFC3339Nano),
			ExitCode: r.ExitCode,
			Output:   r.Output,
		})
	}
	return &HealthState{
		Status:        status,
		FailingStreak: rec.HealthFailingStreak,
		Log:           log,
	}
}

// hasMountAt reports whether the collected mount list already targets the
// given in-container destination. Used to skip auto-creating anonymous
// volumes for paths that the user already bind-mounted.
func hasMountAt(mounts []store.MountSpec, dest string) bool {
	for _, m := range mounts {
		if m.Destination == dest {
			return true
		}
	}
	return false
}

// ensureVolume resolves a named volume to its backing directory, creating
// it on demand. Anonymous volumes and compose-declared-but-not-pre-created
// volumes both rely on auto-creation; this matches Docker's behavior.
func (h *Handler) ensureVolume(name string) (string, error) {
	return h.ensureVolumeOwned(name, "", false)
}

func (h *Handler) ensureVolumeOwned(name, owner string, anonymous bool) (string, error) {
	if v := h.store.GetVolume(name); v != nil {
		return v.Mountpoint, nil
	}
	mountpoint := filepath.Join(h.volumeRoot(), name)
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return "", err
	}
	rec := &store.VolumeRecord{
		Name:       name,
		Driver:     "local",
		Mountpoint: mountpoint,
		Created:    time.Now(),
		OwnerID:    owner,
		Anonymous:  anonymous,
	}
	if err := h.store.AddVolume(rec); err != nil {
		return "", err
	}
	return mountpoint, nil
}

// mountJSONFrom converts a store mount record to the Docker wire format.
// Records written before the Type field existed default to "bind" — the
// only mount type the LXC runtime actually mounts today.
func (h *Handler) mountJSONFrom(m store.MountSpec) MountJSON {
	mode := "rw"
	if m.ReadOnly {
		mode = "ro"
	}
	t := m.Type
	if t == "" {
		t = "bind"
	}
	propagation := m.Propagation
	if propagation == "" {
		propagation = "rprivate"
	}
	out := MountJSON{
		Type:        t,
		Source:      m.Source,
		Destination: m.Destination,
		Mode:        mode,
		RW:          !m.ReadOnly,
		Propagation: propagation,
	}
	if t == "volume" {
		if v := h.volumeByMountpoint(m.Source); v != nil {
			out.Name = v.Name
			out.Driver = v.Driver
		}
	}
	return out
}

func (h *Handler) volumeByMountpoint(path string) *store.VolumeRecord {
	for _, v := range h.store.ListVolumes() {
		if v.Mountpoint == path {
			return v
		}
	}
	return nil
}

// mergeEnv merges image-level env vars with request-level env vars.
// Request vars override image vars with the same key (KEY=value format).
func mergeEnv(imageEnv, requestEnv []string) []string {
	m := make(map[string]string, len(imageEnv)+len(requestEnv))
	order := make([]string, 0, len(imageEnv)+len(requestEnv))
	for _, e := range imageEnv {
		key, _, _ := strings.Cut(e, "=")
		m[key] = e
		order = append(order, key)
	}
	for _, e := range requestEnv {
		key, _, _ := strings.Cut(e, "=")
		if _, exists := m[key]; !exists {
			order = append(order, key)
		}
		m[key] = e
	}
	result := make([]string, 0, len(order))
	for _, key := range order {
		result = append(result, m[key])
	}
	return result
}
