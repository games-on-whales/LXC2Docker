package api

import (
	"bufio"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/gorilla/mux"
	"golang.org/x/sys/unix"
)

func (h *Handler) containerChanges(w http.ResponseWriter, r *http.Request) {
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

	rootfs := h.mgr.RootfsPath(id)
	if rootfs == "" {
		jsonResponse(w, http.StatusOK, []ChangeResponse{})
		return
	}
	base, cleanup, err := h.resolveImageBaseDir(rec.Image)
	if err != nil || base == "" {
		// No base to diff against — return an empty change list rather
		// than a 500 so Portainer's Filesystem tab renders cleanly.
		if cleanup != nil {
			cleanup()
		}
		jsonResponse(w, http.StatusOK, []ChangeResponse{})
		return
	}
	defer cleanup()

	changes, err := diffRootfs(base, rootfs)
	if err != nil {
		errResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, changes)
}

// resolveImageBaseDir picks a directory containing the image's rootfs the
// caller can diff against. Tries the legacy template path first (cheap),
// then falls back to the ZFS @tmpl snapshot via the .zfs/snapshot/tmpl
// accessor — exactly the approach openImageRootfs takes for image save.
func (h *Handler) resolveImageBaseDir(ref string) (string, func(), error) {
	noop := func() {}
	if path := h.mgr.ImageRootfsPath(normalizeImageRef(ref)); path != "" {
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			return path, noop, nil
		}
	}
	rec := h.store.GetImage(normalizeImageRef(ref))
	if rec == nil {
		return "", noop, nil
	}
	if rec.TemplateDataset == "" {
		return "", noop, nil
	}
	mp, err := zfsMountpoint(rec.TemplateDataset)
	if err != nil {
		return "", noop, err
	}
	snap := filepath.Join(mp, ".zfs", "snapshot", "tmpl")
	if fi, err := os.Stat(snap); err != nil || !fi.IsDir() {
		return "", noop, nil
	}
	return snap, noop, nil
}

// readBlkioStats parses cgroup v2's io.stat format into Docker's BlkioStats
// shape. io.stat has one line per device:
//
//	8:0 rbytes=123 wbytes=456 rios=10 wios=20 dbytes=0 dios=0
//
// Docker's legacy shape is a map of named arrays. We populate
// io_service_bytes_recursive (Read/Write/Total) and io_serviced_recursive
// (Read/Write/Total), which is what Portainer's disk-IO chart reads.
func readBlkioStats(path string) map[string][]any {
	out := map[string][]any{
		"io_service_bytes_recursive": []any{},
		"io_serviced_recursive":      []any{},
		"io_queue_recursive":         []any{},
		"io_service_time_recursive":  []any{},
		"io_wait_time_recursive":     []any{},
		"io_merged_recursive":        []any{},
		"io_time_recursive":          []any{},
		"sectors_recursive":          []any{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		majorMinor := strings.SplitN(fields[0], ":", 2)
		if len(majorMinor) != 2 {
			continue
		}
		major, _ := strconv.ParseUint(majorMinor[0], 10, 64)
		minor, _ := strconv.ParseUint(majorMinor[1], 10, 64)
		vals := map[string]uint64{}
		for _, kv := range fields[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			vals[k] = parseUint64(v)
		}
		out["io_service_bytes_recursive"] = append(out["io_service_bytes_recursive"],
			blkioEntry(major, minor, "Read", vals["rbytes"]),
			blkioEntry(major, minor, "Write", vals["wbytes"]),
			blkioEntry(major, minor, "Total", vals["rbytes"]+vals["wbytes"]),
		)
		out["io_serviced_recursive"] = append(out["io_serviced_recursive"],
			blkioEntry(major, minor, "Read", vals["rios"]),
			blkioEntry(major, minor, "Write", vals["wios"]),
			blkioEntry(major, minor, "Total", vals["rios"]+vals["wios"]),
		)
	}
	return out
}

func parseUint64(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return n
}

func blkioEntry(major, minor uint64, op string, value uint64) map[string]any {
	return map[string]any{
		"major": major,
		"minor": minor,
		"op":    op,
		"value": value,
	}
}

// maxOrFallback picks the larger of peak (read from memory.peak) and the
// current usage; cgroup v2's memory.peak is only present on kernels ≥ 5.19,
// so older hosts return 0 and we fall back to the current usage.
func maxOrFallback(peak, cur uint64) uint64 {
	if peak > cur {
		return peak
	}
	return cur
}

// readMemoryEventsOOM parses cgroup v2's memory.events (one "key value"
// pair per line) and returns the cumulative `oom` counter, mapped onto
// Docker's MemoryStats.Failcnt field.
func readMemoryEventsOOM(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, " ")
		if !ok || k != "oom" {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// readMemoryStat parses cgroup v2's memory.stat (one "key value" pair per
// line) and also synthesises the cgroup v1 keys Docker's UI — and therefore
// Portainer's memory chart — historically reads: "cache" maps to v2's
// "file", "rss" maps to "anon".
func readMemoryStat(path string) map[string]any {
	stats := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		return stats
	}
	raw := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		raw[k] = n
		stats[k] = n
	}
	// Docker's classic field names — keep them present so older clients
	// (including Portainer's memory chart) don't show zeros.
	if _, ok := stats["cache"]; !ok {
		stats["cache"] = raw["file"]
	}
	if _, ok := stats["rss"]; !ok {
		stats["rss"] = raw["anon"]
	}
	return stats
}

func mountTypeForSource(st *store.Store, source string) string {
	if source == "" {
		return "bind"
	}
	for _, v := range st.ListVolumes() {
		if v.Mountpoint == source {
			return "volume"
		}
	}
	return "bind"
}

func volumeNameForSource(st *store.Store, source string) string {
	for _, v := range st.ListVolumes() {
		if v.Mountpoint == source {
			return v.Name
		}
	}
	return ""
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func (h *Handler) publishEvent(kind, action, id string, attrs map[string]string) {
	if h == nil || h.events == nil {
		return
	}
	if attrs == nil {
		attrs = map[string]string{}
	}
	attrs = h.normalizePublishedEventAttrs(kind, id, attrs)
	now := time.Now()
	from := publishedEventFrom(kind, id, attrs)
	h.events.publish(Event{
		Type:   kind,
		Action: action,
		Actor: EventActor{
			ID:         id,
			Attributes: attrs,
		},
		Scope:    "local",
		Time:     now.Unix(),
		TimeNano: now.UnixNano(),
		ID:       id,
		Status:   action,
		From:     from,
	})
}

func (h *Handler) normalizePublishedEventAttrs(kind, id string, attrs map[string]string) map[string]string {
	out := map[string]string{
		"daemon": localEventDaemon,
	}
	for k, v := range attrs {
		out[k] = v
	}
	switch kind {
	case "container":
		out["type"] = "container"
		out["container"] = id
		if rec := h.store.GetContainer(id); rec != nil {
			if out["name"] == "" {
				out["name"] = rec.Name
			}
			if out["image"] == "" {
				out["image"] = rec.Image
			}
			if out["imageID"] == "" {
				out["imageID"] = rec.ImageID
			}
			if out["exitCode"] == "" && rec.ExitCode != 0 {
				out["exitCode"] = strconv.Itoa(rec.ExitCode)
			}
		}
	case "image":
		out["type"] = "image"
		if out["image"] == "" {
			out["image"] = id
		}
		if out["name"] == "" {
			out["name"] = id
		}
	case "network":
		driver := out["driver"]
		if driver == "" && out["type"] != "" && out["type"] != "network" {
			driver = out["type"]
		}
		if driver == "" {
			driver = "bridge"
		}
		out["type"] = "network"
		out["driver"] = driver
		out["scope"] = "local"
		if out["network"] == "" {
			if out["name"] != "" {
				out["network"] = out["name"]
			} else {
				out["network"] = id
			}
		}
	case "volume":
		out["type"] = "volume"
		if out["driver"] == "" {
			out["driver"] = "local"
		}
		if out["name"] == "" {
			out["name"] = id
		}
	}
	return out
}

func publishedEventFrom(kind, id string, attrs map[string]string) string {
	switch kind {
	case "container", "image":
		if attrs["image"] != "" {
			return attrs["image"]
		}
		if attrs["name"] != "" {
			return attrs["name"]
		}
		return id
	case "network", "volume":
		if attrs["name"] != "" {
			return attrs["name"]
		}
		if kind == "network" && attrs["network"] != "" {
			return attrs["network"]
		}
		return id
	default:
		return id
	}
}

func (h *Handler) ensureVolume(name string) (*store.VolumeRecord, error) {
	if name == "" {
		return nil, fmt.Errorf("volume name is required")
	}
	if existing := h.store.GetVolume(name); existing != nil {
		if err := os.MkdirAll(existing.Mountpoint, 0o755); err != nil {
			return nil, err
		}
		return existing, nil
	}
	mp := filepath.Join(h.store.RootDir(), "volumes", name)
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return nil, err
	}
	v := &store.VolumeRecord{
		Name:       name,
		Driver:     "local",
		Mountpoint: mp,
		CreatedAt:  time.Now().UTC(),
		Labels:     map[string]string{},
		Options:    map[string]string{},
	}
	if err := h.store.AddVolume(v); err != nil {
		return nil, err
	}
	h.publishEvent("volume", "create", name, map[string]string{"name": name, "driver": "local"})
	return v, nil
}

func defaultContainerNetworks(rec *store.ContainerRecord) map[string]store.NetworkAttachment {
	return map[string]store.NetworkAttachment{
		"gow": {
			NetworkID:  "gow",
			IPAddress:  rec.IPAddress,
			Gateway:    "10.100.0.1",
			EndpointID: endpointID(rec.ID, "gow"),
		},
	}
}

func canonicalNetworkName(name string) string {
	switch strings.TrimSpace(name) {
	case "", "default", "bridge":
		return "gow"
	default:
		return name
	}
}

func attachRequestedNetworks(st *store.Store, rec *store.ContainerRecord, cfg NetworkingConfig) error {
	if len(cfg.EndpointsConfig) == 0 {
		return nil
	}
	if rec.Networks == nil {
		rec.Networks = defaultContainerNetworks(rec)
	}
	for name, ep := range cfg.EndpointsConfig {
		networkName := canonicalNetworkName(name)
		networkID := networkName
		if networkName != "gow" {
			n := st.GetNetwork(name)
			if n == nil {
				return fmt.Errorf("network %q not found", name)
			}
			networkName = n.Name
			networkID = n.ID
		}
		rec.Networks[networkName] = store.NetworkAttachment{
			NetworkID:  networkID,
			IPAddress:  orDefault(ep.IPAddress, rec.IPAddress),
			Gateway:    orDefault(ep.Gateway, "10.100.0.1"),
			MacAddress: ep.MacAddress,
			EndpointID: endpointID(rec.ID, networkName),
			Aliases:    append([]string{}, ep.Aliases...),
			Links:      append([]string{}, ep.Links...),
			DriverOpts: copyStringMap(ep.DriverOpts),
			IPAMConfig: endpointIPAMToStore(ep.IPAMConfig),
		}
	}
	return nil
}

// endpointIPAMToStore copies the API-level EndpointIPAMConfig into its
// store counterpart. Returns nil when nothing meaningful was set so older
// records keep deserialising untouched.
func endpointIPAMToStore(in *EndpointIPAMConfig) *store.EndpointIPAMConfig {
	if in == nil || (in.IPv4Address == "" && in.IPv6Address == "" && len(in.LinkLocalIPs) == 0) {
		return nil
	}
	return &store.EndpointIPAMConfig{
		IPv4Address:  in.IPv4Address,
		IPv6Address:  in.IPv6Address,
		LinkLocalIPs: append([]string{}, in.LinkLocalIPs...),
	}
}

// endpointIPAMFromStore is the inverse of endpointIPAMToStore.
func endpointIPAMFromStore(in *store.EndpointIPAMConfig) *EndpointIPAMConfig {
	if in == nil {
		return nil
	}
	return &EndpointIPAMConfig{
		IPv4Address:  in.IPv4Address,
		IPv6Address:  in.IPv6Address,
		LinkLocalIPs: append([]string{}, in.LinkLocalIPs...),
	}
}

// copyStringMap returns a defensive copy of src so persisted store records
// don't share a slice/map instance with the request body.
func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func buildContainerEndpoints(rec *store.ContainerRecord) map[string]EndpointSettings {
	attachments := rec.Networks
	if len(attachments) == 0 {
		if rec.IPAddress == "" {
			return map[string]EndpointSettings{}
		}
		attachments = defaultContainerNetworks(rec)
	}
	out := make(map[string]EndpointSettings, len(attachments))
	for name, attached := range attachments {
		ipAddress := attached.IPAddress
		if ipAddress == "" {
			ipAddress = rec.IPAddress
		}
		out[name] = EndpointSettings{
			IPAddress:  ipAddress,
			Gateway:    attached.Gateway,
			MacAddress: attached.MacAddress,
			NetworkID:  attached.NetworkID,
			EndpointID: attached.EndpointID,
			Aliases:    append([]string{}, attached.Aliases...),
			Links:      append([]string{}, attached.Links...),
			DriverOpts: copyStringMap(attached.DriverOpts),
			IPAMConfig: endpointIPAMFromStore(attached.IPAMConfig),
		}
	}
	return out
}

func endpointID(containerID, networkName string) string {
	id := containerID
	if len(id) > 12 {
		id = id[:12]
	}
	suffix := strings.ReplaceAll(networkName, "/", "_")
	if len(suffix) > 12 {
		suffix = suffix[:12]
	}
	return id + "-" + suffix
}

func diffRootfs(baseRoot, currentRoot string) ([]ChangeResponse, error) {
	baseEntries, err := walkRootfs(baseRoot)
	if err != nil {
		return nil, err
	}
	currentEntries, err := walkRootfs(currentRoot)
	if err != nil {
		return nil, err
	}

	changes := make([]ChangeResponse, 0)
	for path, base := range baseEntries {
		current, ok := currentEntries[path]
		if !ok {
			changes = append(changes, ChangeResponse{Path: path, Kind: 2})
			continue
		}
		if fileChanged(base, current) {
			changes = append(changes, ChangeResponse{Path: path, Kind: 0})
		}
	}
	for path := range currentEntries {
		if _, ok := baseEntries[path]; !ok {
			changes = append(changes, ChangeResponse{Path: path, Kind: 1})
		}
	}
	return changes, nil
}

type fileSnapshot struct {
	mode    fs.FileMode
	size    int64
	modTime time.Time
	link    string
}

func walkRootfs(root string) (map[string]fileSnapshot, error) {
	out := map[string]fileSnapshot{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := "/" + filepath.ToSlash(rel)
		snap := fileSnapshot{
			mode:    info.Mode(),
			size:    info.Size(),
			modTime: info.ModTime().UTC().Truncate(time.Second),
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err == nil {
				snap.link = target
			}
		}
		out[key] = snap
		return nil
	})
	return out, err
}

func fileChanged(a, b fileSnapshot) bool {
	if a.mode.Type() != b.mode.Type() {
		return true
	}
	if a.mode.Perm() != b.mode.Perm() {
		return true
	}
	if a.mode&os.ModeSymlink != 0 {
		return a.link != b.link
	}
	if a.mode.IsRegular() && (a.size != b.size || !a.modTime.Equal(b.modTime)) {
		return true
	}
	return false
}

func readUint64(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(data))
	if s == "" || s == "max" {
		return ^uint64(0)
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func readCPUStat(path string) (total, user, system uint64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "usage_usec":
			total = v * 1000
		case "user_usec":
			user = v * 1000
		case "system_usec":
			system = v * 1000
		}
	}
	return total, user, system
}

func readSystemCPUUsage() uint64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var total uint64
		for _, field := range fields {
			v, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				continue
			}
			total += v
		}
		ticks := uint64(100)
		return total * uint64(time.Second) / ticks
	}
	return 0
}

func systemMemTotal() uint64 {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0
	}
	return uint64(si.Totalram) * uint64(si.Unit)
}
