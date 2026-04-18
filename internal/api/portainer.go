// Portainer-centric handlers. These endpoints don't change the daemon's
// underlying model — they round out the Docker API surface that web UIs
// (Portainer, LazyDocker, etc.) poll on top of the core container endpoints.
package api

import (
	"net/http"
	"os"
	"strconv"

	"github.com/gorilla/mux"
)

 

// GET /images/{name}/history
//
// LXC templates are a single flattened rootfs, so "history" is one virtual
// layer. Return a single entry that reflects the image metadata the UI has.
func (h *Handler) imageHistory(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	rec := h.store.GetImage(normalizeImageRef(name))
	if rec == nil {
		errResponse(w, http.StatusNotFound, "No such image: "+name)
		return
	}
	jsonResponse(w, http.StatusOK, []map[string]any{
		{
			"Id":        "sha256:" + rec.ID,
			"Created":   rec.Created.Unix(),
			"CreatedBy": "lxc-template " + rec.TemplateName,
			"Tags":      []string{rec.Ref},
			"Size":      0,
			"Comment":   "Flattened LXC rootfs",
		},
	})
}

// POST /images/{name}/tag?repo=<repo>&tag=<tag>
//
// Portainer's image detail "Tag" button adds an additional ref to an existing
// image. The daemon's store keys images by ref, so we add a second entry
// pointing at the same template. Missing source returns 404, duplicate
// target is silently treated as idempotent.
func (h *Handler) tagImage(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	src := h.store.GetImage(normalizeImageRef(name))
	if src == nil {
		errResponse(w, http.StatusNotFound, "No such image: "+name)
		return
	}
	repo := r.URL.Query().Get("repo")
	tag := r.URL.Query().Get("tag")
	if repo == "" {
		errResponse(w, http.StatusBadRequest, "repo is required")
		return
	}
	if tag == "" {
		tag = "latest"
	}
	newRef := repo + ":" + tag

	// Copy the record under the new ref. The template container is shared,
	// so subsequent container creates can use either ref interchangeably.
	cp := *src
	cp.Ref = newRef
	if err := h.store.AddImage(&cp); err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.emitImage("tag", newRef)
	w.WriteHeader(http.StatusCreated)
}

// POST /containers/{id}/update
//
// Portainer exposes a "Resource limits" modal that POSTs updated Memory,
// CPU, and restart policy fields. We persist the new values so inspect
// reflects them, and — when the container is running — push the memory /
// CPU changes into the live cgroup. Changes that require a restart
// (capabilities, namespaces) apply on next start.
// applyLiveLimits writes Memory/CPU limits to the container's cgroup so
// Portainer's "update resource limits" modal takes effect without a
// restart. Returns a non-empty warning string when any write failed.
func applyLiveLimits(id string, hc HostConfig) string {
	cg := resolveCgroupPath(id, containerPID(id))
	if cg == "" {
		return "cgroup not found; limits applied on next start"
	}
	writes := map[string]string{}
	if hc.Memory > 0 {
		writes["memory.max"] = strconv.FormatInt(hc.Memory, 10)
	}
	if hc.MemorySwap > 0 {
		swap := hc.MemorySwap - hc.Memory
		if swap < 0 {
			swap = 0
		}
		writes["memory.swap.max"] = strconv.FormatInt(swap, 10)
	}
	if hc.MemoryReservation > 0 {
		writes["memory.low"] = strconv.FormatInt(hc.MemoryReservation, 10)
	}
	if hc.CpusetCpus != "" {
		writes["cpuset.cpus"] = hc.CpusetCpus
	}
	if hc.CpusetMems != "" {
		writes["cpuset.mems"] = hc.CpusetMems
	}
	if hc.PidsLimit > 0 {
		writes["pids.max"] = strconv.FormatInt(hc.PidsLimit, 10)
	}
	if hc.BlkioWeight > 0 {
		writes["io.weight"] = "default " + strconv.FormatUint(uint64(hc.BlkioWeight), 10)
	}
	if hc.CPUShares > 0 {
		// Docker shares (1–1024) → cgroup v2 weight (1–10000).
		weight := (hc.CPUShares * 10000) / 1024
		if weight < 1 {
			weight = 1
		}
		writes["cpu.weight"] = strconv.FormatInt(weight, 10)
	}
	if hc.CPUQuota > 0 {
		period := hc.CPUPeriod
		if period <= 0 {
			period = 100000
		}
		writes["cpu.max"] = strconv.FormatInt(hc.CPUQuota, 10) + " " + strconv.FormatInt(period, 10)
	} else if hc.NanoCPUs > 0 {
		// 1 CPU = 1e9 NanoCPUs → quota µs at 100 ms period.
		quota := hc.NanoCPUs * 100000 / 1_000_000_000
		if quota < 1000 {
			quota = 1000
		}
		writes["cpu.max"] = strconv.FormatInt(quota, 10) + " 100000"
	}
	for file, val := range writes {
		if err := os.WriteFile(cg+"/"+file, []byte(val), 0o644); err != nil {
			return "some limits could not be applied live: " + err.Error()
		}
	}
	return ""
}

// GET /distribution/{name}/json
//
// Portainer's "Pull image" modal probes this to decide whether a pull is
// needed and what architectures a remote manifest offers. We don't hit the
// registry ourselves; a minimal response with a single linux/amd64 platform
// entry is enough to pass the UI's "does this image exist remotely?" check.
func (h *Handler) distributionInspect(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"Descriptor": map[string]any{
			"MediaType": "application/vnd.docker.distribution.manifest.v2+json",
			"Digest":    "",
			"Size":      0,
			"URLs":      []string{},
		},
		"Platforms": []map[string]string{
			{"architecture": "amd64", "os": "linux"},
		},
	})
}
