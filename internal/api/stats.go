package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

// dockerStats is the subset of the Docker /containers/{id}/stats response
// that Portainer and `docker stats` actually render. Fields we can't compute
// cheaply (storage, pids) are left zero — Portainer shows "—" for them
// rather than failing.
type dockerStats struct {
	Read         string              `json:"read"`
	Preread      string              `json:"preread"`
	Name         string              `json:"name"`
	ID           string              `json:"id"`
	NumProcs     uint32              `json:"num_procs"`
	PidsStats    pidsStats           `json:"pids_stats"`
	CPUStats     cpuStats            `json:"cpu_stats"`
	PreCPUStats  cpuStats            `json:"precpu_stats"`
	MemoryStats  memStats            `json:"memory_stats"`
	Networks     map[string]netStats `json:"networks"`
	BlkioStats   blkioStats          `json:"blkio_stats"`
	StorageStats map[string]any      `json:"storage_stats"`
}

type pidsStats struct {
	Current uint64 `json:"current"`
}

type cpuStats struct {
	CPUUsage       cpuUsage   `json:"cpu_usage"`
	SystemCPUUsage uint64     `json:"system_cpu_usage"`
	OnlineCPUs     uint32     `json:"online_cpus"`
	ThrottlingData throttling `json:"throttling_data"`
}

type cpuUsage struct {
	TotalUsage        uint64   `json:"total_usage"`
	PercpuUsage       []uint64 `json:"percpu_usage,omitempty"`
	UsageInKernelmode uint64   `json:"usage_in_kernelmode"`
	UsageInUsermode   uint64   `json:"usage_in_usermode"`
}

type throttling struct {
	Periods          uint64 `json:"periods"`
	ThrottledPeriods uint64 `json:"throttled_periods"`
	ThrottledTime    uint64 `json:"throttled_time"`
}

type memStats struct {
	Usage    uint64            `json:"usage"`
	MaxUsage uint64            `json:"max_usage"`
	Limit    uint64            `json:"limit"`
	Stats    map[string]uint64 `json:"stats"`
}

type netStats struct {
	RxBytes   uint64 `json:"rx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	RxErrors  uint64 `json:"rx_errors"`
	RxDropped uint64 `json:"rx_dropped"`
	TxBytes   uint64 `json:"tx_bytes"`
	TxPackets uint64 `json:"tx_packets"`
	TxErrors  uint64 `json:"tx_errors"`
	TxDropped uint64 `json:"tx_dropped"`
}

type blkioStats struct {
	IOServiceBytesRecursive []any `json:"io_service_bytes_recursive"`
	IOServicedRecursive     []any `json:"io_serviced_recursive"`
}

// GET /containers/{id}/stats
//
// When ?stream=0 (or one-shot=1) is passed, returns a single snapshot.
// Otherwise streams snapshots ~every second until the client disconnects.
// Portainer uses one-shot mode for the dashboard list and streaming mode on
// a container's detail page.
func (h *Handler) containerStats(w http.ResponseWriter, r *http.Request) {
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

	stream := r.URL.Query().Get("stream") != "0" && r.URL.Query().Get("stream") != "false"
	// `docker stats --no-stream` sends one-shot=1 on API >= 1.41.
	if r.URL.Query().Get("one-shot") == "1" || r.URL.Query().Get("one-shot") == "true" {
		stream = false
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	writeOne := func(prev *dockerStats) *dockerStats {
		s := h.sampleStats(id, rec.Name)
		if prev != nil {
			s.PreCPUStats = prev.CPUStats
			s.Preread = prev.Read
		} else {
			s.Preread = s.Read
			s.PreCPUStats = s.CPUStats
		}
		_ = enc.Encode(&s)
		if flusher != nil {
			flusher.Flush()
		}
		return &s
	}

	prev := writeOne(nil)
	if !stream {
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			prev = writeOne(prev)
		}
	}
}

// sampleStats collects a single snapshot of cgroup + network counters for the
// container. Works on cgroup v1 and v2. Missing files are treated as zeros
// rather than errors — a just-started container may not have all counters yet.
func (h *Handler) sampleStats(id, name string) dockerStats {
	now := time.Now()
	s := dockerStats{
		Read:         now.UTC().Format(time.RFC3339Nano),
		Preread:      "0001-01-01T00:00:00Z",
		Name:         "/" + name,
		ID:           id,
		Networks:     map[string]netStats{},
		StorageStats: map[string]any{},
		CPUStats: cpuStats{
			OnlineCPUs: uint32(onlineCPUs()),
		},
	}
	s.PreCPUStats = s.CPUStats

	pid := containerPID(id)
	cgPath := resolveCgroupPath(id, pid)

	// CPU
	cpu, user, sys := readCPUStats(cgPath)
	s.CPUStats.CPUUsage.TotalUsage = cpu
	s.CPUStats.CPUUsage.UsageInUsermode = user
	s.CPUStats.CPUUsage.UsageInKernelmode = sys
	s.CPUStats.SystemCPUUsage = readSystemCPUNanos()
	periods, throttled, throttledTime := readThrottling(cgPath)
	s.CPUStats.ThrottlingData.Periods = periods
	s.CPUStats.ThrottlingData.ThrottledPeriods = throttled
	s.CPUStats.ThrottlingData.ThrottledTime = throttledTime

	// Memory
	usage, limit, memstats := readMemoryStats(cgPath)
	s.MemoryStats.Usage = usage
	s.MemoryStats.Limit = limit
	s.MemoryStats.MaxUsage = usage
	s.MemoryStats.Stats = memstats

	// Process count (cgroup v2 pids.current, v1 pids.current or tasks).
	s.PidsStats.Current = readPidsCurrent(cgPath)
	s.NumProcs = uint32(s.PidsStats.Current)

	// Network (per-interface rx/tx from /proc/<pid>/net/dev).
	if pid > 0 {
		s.Networks = readNetStats(pid)
	}

	// Blkio — read cgroup v2 io.stat if present, fall back to empty arrays.
	// Portainer's stats chart plots IOServiceBytesRecursive; populating it
	// unlocks real disk I/O graphs instead of a flat line.
	s.BlkioStats.IOServiceBytesRecursive = readBlkioServiceBytes(cgPath)
	s.BlkioStats.IOServicedRecursive = readBlkioServiced(cgPath)

	return s
}

// readBlkioServiceBytes parses cgroup v2's io.stat (preferred) or the
// cgroup v1 blkio.throttle.io_service_bytes fallback into Docker's legacy
// per-device, per-op byte counters. Format v2 (one line per device):
//
//	MAJ:MIN rbytes=N wbytes=N rios=N wios=N dbytes=N dios=N
//
// Format v1:
//
//	MAJ:MIN Read N
//	MAJ:MIN Write N
//	...
//
// Portainer reads Read/Write to render the disk chart; missing file → empty.
func readBlkioServiceBytes(cg string) []any {
	out := []any{}
	if cg == "" {
		return out
	}
	// v2
	if data, err := os.ReadFile(filepath.Join(cg, "io.stat")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			major, minor := parseMajMin(fields[0])
			var rbytes, wbytes uint64
			for _, f := range fields[1:] {
				k, v, ok := strings.Cut(f, "=")
				if !ok {
					continue
				}
				n, _ := strconv.ParseUint(v, 10, 64)
				switch k {
				case "rbytes":
					rbytes = n
				case "wbytes":
					wbytes = n
				}
			}
			if rbytes > 0 || wbytes > 0 {
				out = append(out,
					map[string]any{"major": major, "minor": minor, "op": "Read", "value": rbytes},
					map[string]any{"major": major, "minor": minor, "op": "Write", "value": wbytes},
				)
			}
		}
		return out
	}
	// v1 fallback.
	return readBlkioV1(cg, "blkio.throttle.io_service_bytes")
}

// readBlkioServiced is the IOs-served counterpart to readBlkioServiceBytes.
func readBlkioServiced(cg string) []any {
	out := []any{}
	if cg == "" {
		return out
	}
	if data, err := os.ReadFile(filepath.Join(cg, "io.stat")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			major, minor := parseMajMin(fields[0])
			var rios, wios uint64
			for _, f := range fields[1:] {
				k, v, ok := strings.Cut(f, "=")
				if !ok {
					continue
				}
				n, _ := strconv.ParseUint(v, 10, 64)
				switch k {
				case "rios":
					rios = n
				case "wios":
					wios = n
				}
			}
			if rios > 0 || wios > 0 {
				out = append(out,
					map[string]any{"major": major, "minor": minor, "op": "Read", "value": rios},
					map[string]any{"major": major, "minor": minor, "op": "Write", "value": wios},
				)
			}
		}
		return out
	}
	return readBlkioV1(cg, "blkio.throttle.io_serviced")
}

// readBlkioV1 parses cgroup v1's blkio recursive counter files. They list
// `MAJ:MIN <op> <value>` lines plus a "Total" summary we skip. Only Read
// and Write op codes are emitted (Async/Sync duplicate counts).
func readBlkioV1(cg, filename string) []any {
	out := []any{}
	data, err := os.ReadFile(filepath.Join(cg, filename))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		op := fields[1]
		if op != "Read" && op != "Write" {
			continue
		}
		major, minor := parseMajMin(fields[0])
		n, _ := strconv.ParseUint(fields[2], 10, 64)
		if n == 0 {
			continue
		}
		out = append(out, map[string]any{
			"major": major, "minor": minor, "op": op, "value": n,
		})
	}
	return out
}

// parseMajMin splits "MAJ:MIN" into its two integers. Bad input → (0, 0).
func parseMajMin(s string) (uint64, uint64) {
	maj, min, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0
	}
	ma, _ := strconv.ParseUint(maj, 10, 64)
	mi, _ := strconv.ParseUint(min, 10, 64)
	return ma, mi
}

func onlineCPUs() int {
	data, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return 1
	}
	// Format: "0-3" or "0,2-3".
	count := 0
	for _, segment := range strings.Split(strings.TrimSpace(string(data)), ",") {
		if strings.Contains(segment, "-") {
			parts := strings.SplitN(segment, "-", 2)
			lo, _ := strconv.Atoi(parts[0])
			hi, _ := strconv.Atoi(parts[1])
			count += hi - lo + 1
		} else if segment != "" {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}

// containerPID looks up the container's init PID via lxc-info. Returns 0 if
// the container isn't running. This matches Docker's stats behavior (report
// zeros for stopped containers rather than an error).
func containerPID(id string) int {
	// Try the LXC manager path first.
	for _, lxcpath := range []string{"/var/lib/lxc", "/var/lib/docker-lxc-daemon"} {
		out, err := exec.Command("lxc-info", "-n", id, "--lxcpath", lxcpath, "-pH").Output()
		if err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
			if pid > 0 {
				return pid
			}
		}
	}
	// Try without an explicit path (covers default lxcpath).
	out, err := exec.Command("lxc-info", "-n", id, "-pH").Output()
	if err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		if pid > 0 {
			return pid
		}
	}
	return 0
}

// resolveCgroupPath returns the cgroup v2 / v1 directory for the container.
// For cgroup v2 we use /sys/fs/cgroup/<path-from-proc>; for v1 we walk up to
// find the lxc container slice. Reading /proc/<pid>/cgroup is the reliable
// portable way — lxc places containers under
// /lxc.payload.<name>/ on v2 or /lxc/<name>/ on v1.
func resolveCgroupPath(id string, pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return ""
	}
	// cgroup v2 unified line: "0::/lxc.payload.<id>/..."
	// cgroup v1 lines: "<id>:<controller>:<path>"
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		// Prefer the unified v2 line.
		if parts[0] == "0" && parts[1] == "" {
			return filepath.Join("/sys/fs/cgroup", parts[2])
		}
	}
	// v1 fallback: try the memory controller path.
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && strings.Contains(parts[1], "memory") {
			return filepath.Join("/sys/fs/cgroup/memory", parts[2])
		}
	}
	return ""
}

// readCPUStats returns total, user and system cpu ns from the cgroup.
// cgroup v2: cpu.stat; v1: cpuacct.usage and cpuacct.stat.
func readCPUStats(cg string) (total, user, sys uint64) {
	if cg == "" {
		return
	}
	// v2
	if data, err := os.ReadFile(filepath.Join(cg, "cpu.stat")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "usage_usec":
				total = v * 1000 // to ns
			case "user_usec":
				user = v * 1000
			case "system_usec":
				sys = v * 1000
			}
		}
		return
	}
	// v1 fallback.
	if data, err := os.ReadFile(filepath.Join(cg, "cpuacct.usage")); err == nil {
		total, _ = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	}
	if data, err := os.ReadFile(filepath.Join(cg, "cpuacct.stat")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			// cpuacct.stat reports USER_HZ; convert to ns (USER_HZ=100 on
			// most distros, so 1 tick = 10ms = 10,000,000 ns).
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "user":
				user = v * 10_000_000
			case "system":
				sys = v * 10_000_000
			}
		}
	}
	return
}

// readThrottling pulls CFS throttling numbers from cpu.stat. cgroup v2
// reports nr_periods / nr_throttled / throttled_usec; v1 uses throttled_time
// (ns). Docker emits them via ThrottlingData; Portainer's stats chart draws
// a separate line for throttled-time when non-zero, which is the main signal
// that a container's CPU quota is tight.
func readThrottling(cg string) (periods, throttled, throttledTime uint64) {
	if cg == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(cg, "cpu.stat"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "nr_periods":
			periods = v
		case "nr_throttled":
			throttled = v
		case "throttled_usec":
			throttledTime = v * 1000 // to ns
		case "throttled_time":
			throttledTime = v // v1 is already in ns
		}
	}
	return
}

// readSystemCPUNanos reads /proc/stat's "cpu" line and returns the host's
// total CPU time in nanoseconds. Docker computes per-container CPU % as
// (ct_total - ct_prev) / (sys_total - sys_prev) * online_cpus * 100.
func readSystemCPUNanos() uint64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		var total uint64
		for _, f := range fields[1:] {
			v, _ := strconv.ParseUint(f, 10, 64)
			total += v
		}
		// USER_HZ=100: 1 tick = 10ms = 10_000_000 ns.
		return total * 10_000_000
	}
	return 0
}

// readMemoryStats returns usage, limit, and a detail map from the cgroup.
func readMemoryStats(cg string) (usage, limit uint64, stats map[string]uint64) {
	stats = map[string]uint64{}
	if cg == "" {
		return
	}
	// v2
	if data, err := os.ReadFile(filepath.Join(cg, "memory.current")); err == nil {
		usage, _ = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	}
	if data, err := os.ReadFile(filepath.Join(cg, "memory.max")); err == nil {
		s := strings.TrimSpace(string(data))
		if s == "max" {
			limit = physicalMemory()
		} else {
			limit, _ = strconv.ParseUint(s, 10, 64)
		}
	}
	// v1 fallback
	if usage == 0 {
		if data, err := os.ReadFile(filepath.Join(cg, "memory.usage_in_bytes")); err == nil {
			usage, _ = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		}
	}
	if limit == 0 {
		if data, err := os.ReadFile(filepath.Join(cg, "memory.limit_in_bytes")); err == nil {
			limit, _ = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		}
	}
	// memory.stat is the same on v1 and v2 (k/v lines).
	if data, err := os.ReadFile(filepath.Join(cg, "memory.stat")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				continue
			}
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			stats[fields[0]] = v
		}
	}
	// Clamp absurd limits (on many kernels the "no limit" value is
	// 9223372036854771712); report host memory in that case so the UI
	// doesn't render a nonsense percentage.
	if limit > physicalMemory()*2 || limit == 0 {
		limit = physicalMemory()
	}
	return
}

// readPidsCurrent returns the count of processes/tasks in the container's
// cgroup. Portainer's stats card shows "PIDs: N". cgroup v2 exposes
// pids.current; v1 keeps it under pids.current or in a tasks file.
func readPidsCurrent(cg string) uint64 {
	if cg == "" {
		return 0
	}
	for _, name := range []string{"pids.current"} {
		if data, err := os.ReadFile(filepath.Join(cg, name)); err == nil {
			n, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if n > 0 {
				return n
			}
		}
	}
	// cgroup v1 fallback: count the lines of tasks.
	if data, err := os.ReadFile(filepath.Join(cg, "tasks")); err == nil {
		return uint64(strings.Count(string(data), "\n"))
	}
	return 0
}

func physicalMemory() uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, _ := strconv.ParseUint(fields[1], 10, 64)
		return kb * 1024
	}
	return 0
}

// readNetStats returns per-interface counters for a container by reading the
// container-side /proc/<pid>/net/dev (this file is namespaced, so we see the
// container's veth from inside its netns). The "lo" interface is skipped.
func readNetStats(pid int) map[string]netStats {
	out := map[string]netStats{}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/dev", pid))
	if err != nil {
		return out
	}
	for i, line := range strings.Split(string(data), "\n") {
		if i < 2 || line == "" {
			continue // skip header rows
		}
		fields := strings.Fields(line)
		if len(fields) < 17 {
			continue
		}
		iface := strings.TrimSuffix(fields[0], ":")
		if iface == "lo" {
			continue
		}
		rxB, _ := strconv.ParseUint(fields[1], 10, 64)
		rxP, _ := strconv.ParseUint(fields[2], 10, 64)
		rxE, _ := strconv.ParseUint(fields[3], 10, 64)
		rxD, _ := strconv.ParseUint(fields[4], 10, 64)
		txB, _ := strconv.ParseUint(fields[9], 10, 64)
		txP, _ := strconv.ParseUint(fields[10], 10, 64)
		txE, _ := strconv.ParseUint(fields[11], 10, 64)
		txD, _ := strconv.ParseUint(fields[12], 10, 64)
		out[iface] = netStats{
			RxBytes: rxB, RxPackets: rxP, RxErrors: rxE, RxDropped: rxD,
			TxBytes: txB, TxPackets: txP, TxErrors: txE, TxDropped: txD,
		}
	}
	return out
}
