// Package store persists metadata for containers and images managed by this
// daemon. LXC itself has no concept of Docker-specific metadata (image name,
// environment variables, port bindings, etc.), so we maintain a JSON file
// alongside the LXC state directory.
package store

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultPath = "/var/lib/docker-lxc-daemon"

// ContainerRecord holds Docker-layer metadata for a single container.
type ContainerRecord struct {
	ID           string                       `json:"id"`       // Docker hex ID (API-facing)
	VMID         int                          `json:"vmid"`     // Proxmox CT VMID (0 = legacy direct LXC)
	Name         string                       `json:"name"`     // Docker-style name (no leading slash)
	Image        string                       `json:"image"`    // Original image:tag as requested
	ImageID      string                       `json:"image_id"` // Resolved image identifier
	Created      time.Time                    `json:"created"`
	Entrypoint   []string                     `json:"entrypoint"`
	Cmd          []string                     `json:"cmd"`
	Env          []string                     `json:"env"`
	Labels       map[string]string            `json:"labels"`
	IPAddress    string                       `json:"ip_address"`
	PortBindings []PortBinding                `json:"port_bindings,omitempty"`
	Mounts       []MountSpec                  `json:"mounts"`
	Networks     map[string]NetworkAttachment `json:"networks,omitempty"`
	StartedAt    *time.Time                   `json:"started_at,omitempty"`  // nil until first start; distinguishes "created" from "exited"
	FinishedAt   *time.Time                   `json:"finished_at,omitempty"` // nil while running or before first exit
	ExitCode     int                          `json:"exit_code,omitempty"`
	// Ephemeral is true only for daemon-created raw-LXC containers that the
	// GC is permitted to reap. Permanent Proxmox CTs (visible in PVE UI) and
	// any pre-existing records that lack this flag are left strictly alone.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Storage records the PVE storage pool this container's rootfs lives
	// on. Used by RemoveContainer for ephemeral containers (the ZFS clone
	// dataset path includes the pool name) and for diagnostics on PVE CTs.
	Storage string `json:"storage,omitempty"`
	// RestartPolicy is echoed back through /containers/{id}/json so
	// Portainer's container detail shows the policy the user selected at
	// create time. The daemon does not currently enforce it — containers
	// that exit stay exited until the user restarts them.
	RestartPolicy *RestartPolicy `json:"restart_policy,omitempty"`
	// RestartCount tracks how many times the user has restarted the
	// container via /containers/{id}/restart. Surfaces on inspect.
	RestartCount int `json:"restart_count,omitempty"`
	// Healthcheck is echoed back on inspect so Portainer's detail panel
	// and duplicate/edit dialog reflect the user's input. Like
	// RestartPolicy, the daemon does not actually execute the check.
	Healthcheck *HealthcheckConfig `json:"healthcheck,omitempty"`
	// StopSignal mirrors Docker's Config.StopSignal field (e.g. "SIGINT").
	// We don't map it into lxc-stop, but Portainer reads it from inspect.
	StopSignal string `json:"stop_signal,omitempty"`
	// WorkingDir mirrors Docker's Config.WorkingDir so inspect echoes
	// what the user picked instead of always reporting empty.
	WorkingDir string `json:"working_dir,omitempty"`
	// User/Domainname/Hostname mirror their Config counterparts. The
	// daemon does not actually drop privileges to User today.
	User       string `json:"user,omitempty"`
	Domainname string `json:"domainname,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	// Tty/OpenStdin/StdinOnce mirror the Config flags; currently not
	// propagated to lxc-start but roundtrip through inspect.
	Tty       bool `json:"tty,omitempty"`
	OpenStdin bool `json:"open_stdin,omitempty"`
	StdinOnce bool `json:"stdin_once,omitempty"`
	// RequestedVolumes captures Config.Volumes from the create body so
	// inspect can echo it back alongside the image-declared volumes.
	// We do not auto-create anonymous volumes for these paths today; the
	// field is purely roundtripped.
	RequestedVolumes []string `json:"requested_volumes,omitempty"`
	// HostConfigExtras holds the HostConfig fields we roundtrip through
	// inspect without enforcing (Privileged, caps, DNS overrides,
	// resource limits). Portainer's Host Config tab renders these.
	HostConfigExtras *HostConfigExtras `json:"host_config_extras,omitempty"`
}

// HostConfigExtras is the persisted subset of Docker's HostConfig we
// echo on inspect. Each field maps 1:1 to its Docker counterpart.
type HostConfigExtras struct {
	Privileged        bool              `json:"privileged,omitempty"`
	CapAdd            []string          `json:"cap_add,omitempty"`
	CapDrop           []string          `json:"cap_drop,omitempty"`
	ExtraHosts        []string          `json:"extra_hosts,omitempty"`
	Dns               []string          `json:"dns,omitempty"`
	DnsSearch         []string          `json:"dns_search,omitempty"`
	DnsOptions        []string          `json:"dns_options,omitempty"`
	Memory            int64             `json:"memory,omitempty"`
	CPUShares         int64             `json:"cpu_shares,omitempty"`
	NanoCPUs          int64             `json:"nano_cpus,omitempty"`
	Tmpfs             map[string]string `json:"tmpfs,omitempty"`
	ReadonlyRootfs    bool              `json:"readonly_rootfs,omitempty"`
	PidMode           string            `json:"pid_mode,omitempty"`
	UTSMode           string            `json:"uts_mode,omitempty"`
	Devices           []DeviceMapping   `json:"devices,omitempty"`
	DeviceCgroupRules []string          `json:"device_cgroup_rules,omitempty"`
	UsernsMode        string            `json:"userns_mode,omitempty"`
	GroupAdd          []string          `json:"group_add,omitempty"`
	SecurityOpt       []string          `json:"security_opt,omitempty"`
	Sysctls           map[string]string `json:"sysctls,omitempty"`
	PidsLimit         int64             `json:"pids_limit,omitempty"`
	OomScoreAdj       int               `json:"oom_score_adj,omitempty"`
	LogDriver         string            `json:"log_driver,omitempty"`
	LogOptions        map[string]string `json:"log_options,omitempty"`
}

// DeviceMapping mirrors Docker's HostConfig.Devices entries.
type DeviceMapping struct {
	PathOnHost        string `json:"path_on_host"`
	PathInContainer   string `json:"path_in_container,omitempty"`
	CgroupPermissions string `json:"cgroup_permissions,omitempty"`
}

// HealthcheckConfig mirrors Docker's Config.Healthcheck. Stored verbatim;
// the daemon currently does not execute healthchecks (all fields are
// echoed back on inspect for UI purposes only).
type HealthcheckConfig struct {
	Test          []string `json:"test,omitempty"`
	Interval      int64    `json:"interval,omitempty"`
	Timeout       int64    `json:"timeout,omitempty"`
	StartPeriod   int64    `json:"start_period,omitempty"`
	StartInterval int64    `json:"start_interval,omitempty"`
	Retries       int      `json:"retries,omitempty"`
}

// RestartPolicy mirrors Docker's HostConfig.RestartPolicy block. Kept as a
// pointer on ContainerRecord so zero-value state doesn't clobber persisted
// records written before this field existed.
type RestartPolicy struct {
	Name              string `json:"name"`
	MaximumRetryCount int    `json:"maximum_retry_count,omitempty"`
}

// NetworkAttachment records a container's membership in a Docker-style network.
type NetworkAttachment struct {
	NetworkID  string             `json:"network_id"`
	IPAddress  string             `json:"ip_address,omitempty"`
	Gateway    string             `json:"gateway,omitempty"`
	MacAddress string             `json:"mac_address,omitempty"`
	EndpointID string             `json:"endpoint_id,omitempty"`
	Aliases    []string           `json:"aliases,omitempty"`
	Links      []string           `json:"links,omitempty"`
	DriverOpts map[string]string  `json:"driver_opts,omitempty"`
	IPAMConfig *EndpointIPAMConfig `json:"ipam_config,omitempty"`
}

// EndpointIPAMConfig persists the per-endpoint IPAM block. The daemon
// does not pin static addresses today; the field is roundtripped so
// clients can read back what they submitted.
type EndpointIPAMConfig struct {
	IPv4Address  string   `json:"ipv4_address,omitempty"`
	IPv6Address  string   `json:"ipv6_address,omitempty"`
	LinkLocalIPs []string `json:"link_local_ips,omitempty"`
}

// PortBinding records a single host→container port mapping.
type PortBinding struct {
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Proto         string `json:"proto"` // "tcp" or "udp"
}

// MountSpec mirrors the relevant fields of a Docker bind mount.
type MountSpec struct {
	Type        string `json:"type,omitempty"` // "bind" or "volume"
	Name        string `json:"name,omitempty"` // volume name when Type=="volume"
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
}

// VolumeRecord holds metadata for a Docker-style named volume.
type VolumeRecord struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	CreatedAt  time.Time         `json:"created_at"`
	Labels     map[string]string `json:"labels,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
}

// NetworkRecord holds metadata for a Docker-style network object.
type NetworkRecord struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Scope      string            `json:"scope"`
	CreatedAt  time.Time         `json:"created_at"`
	Labels     map[string]string `json:"labels,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	Attachable bool              `json:"attachable,omitempty"`
	// IPAM is roundtripped verbatim on inspect. The daemon does not use
	// these addresses for allocation — containers still get addresses
	// from the single gow bridge — but Portainer's network detail page
	// displays the configured Subnet/Gateway/IPRange.
	IPAM *NetworkIPAM `json:"ipam,omitempty"`
}

// NetworkIPAM mirrors Docker's IPAM block.
type NetworkIPAM struct {
	Driver  string              `json:"driver,omitempty"`
	Options map[string]string   `json:"options,omitempty"`
	Config  []NetworkIPAMConfig `json:"config,omitempty"`
}

// NetworkIPAMConfig is one entry inside NetworkIPAM.Config.
type NetworkIPAMConfig struct {
	Subnet     string            `json:"subnet,omitempty"`
	IPRange    string            `json:"ip_range,omitempty"`
	Gateway    string            `json:"gateway,omitempty"`
	AuxAddress map[string]string `json:"aux_address,omitempty"`
}

// ImageRecord holds metadata for a pulled image. Templates can be backed
// by a raw ZFS dataset (preferred — invisible to the PVE UI), a Proxmox
// CT template VMID (legacy from pre-Apr-2026 daemons; cluttered the UI),
// or a directory-based LXC template container (legacy mode without PVE).
type ImageRecord struct {
	ID              string    `json:"id"`               // e.g. "ubuntu_22.04"
	Ref             string    `json:"ref"`              // original "ubuntu:22.04"
	Distro          string    `json:"distro"`           // "ubuntu"
	Release         string    `json:"release"`          // "jammy"
	Arch            string    `json:"arch"`             // "amd64"
	TemplateName    string    `json:"template_name"`    // LXC container used as clone source (legacy)
	TemplateVMID    int       `json:"template_vmid"`    // Proxmox CT VMID of the template (legacy; cluttered PVE UI)
	TemplateDataset string    `json:"template_dataset"` // ZFS dataset path of the template (preferred for new pulls; e.g. "large/dld-templates/nginx-alpine")
	Created         time.Time `json:"created"`
	// OCI image metadata (populated only for OCI-pulled images).
	OCIEntrypoint  []string           `json:"oci_entrypoint,omitempty"`
	OCICmd         []string           `json:"oci_cmd,omitempty"`
	OCIEnv         []string           `json:"oci_env,omitempty"`
	OCIWorkingDir  string             `json:"oci_working_dir,omitempty"`
	OCIPorts       []string           `json:"oci_ports,omitempty"`
	OCILabels      map[string]string  `json:"oci_labels,omitempty"`
	OCIUser        string             `json:"oci_user,omitempty"`
	OCIStopSignal  string             `json:"oci_stop_signal,omitempty"`
	OCIHealthcheck *HealthcheckConfig `json:"oci_healthcheck,omitempty"`
	OCIVolumes     []string           `json:"oci_volumes,omitempty"`
	OCIShell       []string           `json:"oci_shell,omitempty"`
	OCIDigest      string             `json:"oci_digest,omitempty"`
}

type state struct {
	Containers map[string]*ContainerRecord `json:"containers"` // keyed by ID
	Images     map[string]*ImageRecord     `json:"images"`     // keyed by Ref (e.g. "ubuntu:22.04")
	Volumes    map[string]*VolumeRecord    `json:"volumes"`    // keyed by volume name
	Networks   map[string]*NetworkRecord   `json:"networks"`   // keyed by network id
	NextIP     int                         `json:"next_ip"`    // last octet of 10.100.0.x, starts at 2
	FreeIPs    []int                       `json:"free_ips"`   // last octets of freed IPs available for reuse
}

// Store is a thread-safe, file-backed metadata store.
type Store struct {
	mu   sync.RWMutex
	path string
	data state
}

// New opens (or creates) the store at the default path.
func New() (*Store, error) {
	return NewAt(defaultPath)
}

// NewAt opens (or creates) the store rooted at dir.
func NewAt(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
	}

	s := &Store{
		path: filepath.Join(dir, "state.json"),
		data: state{
			Containers: make(map[string]*ContainerRecord),
			Images:     make(map[string]*ImageRecord),
			Volumes:    make(map[string]*VolumeRecord),
			Networks:   make(map[string]*NetworkRecord),
			NextIP:     2,
		},
	}

	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("store: load: %w", err)
	}
	if s.data.Containers == nil {
		s.data.Containers = make(map[string]*ContainerRecord)
	}
	if s.data.Images == nil {
		s.data.Images = make(map[string]*ImageRecord)
	}
	if s.data.Volumes == nil {
		s.data.Volumes = make(map[string]*VolumeRecord)
	}
	if s.data.Networks == nil {
		s.data.Networks = make(map[string]*NetworkRecord)
	}
	return s, nil
}

// AllocateIP returns the next available IP in the 10.100.0.0/24 range.
// Freed IPs are reused first; otherwise the counter advances.
func (s *Store) AllocateIP() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var octet int
	if len(s.data.FreeIPs) > 0 {
		octet = s.data.FreeIPs[0]
		s.data.FreeIPs = s.data.FreeIPs[1:]
	} else {
		if s.data.NextIP > 254 {
			return "", fmt.Errorf("store: IP space exhausted")
		}
		octet = s.data.NextIP
		s.data.NextIP++
	}
	ip := fmt.Sprintf("10.100.0.%d", octet)
	return ip, s.save()
}

// FreeIP returns an IP address to the pool for reuse.
func (s *Store) FreeIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		if octet, err := strconv.Atoi(parts[3]); err == nil && octet >= 2 {
			s.data.FreeIPs = append(s.data.FreeIPs, octet)
			s.save()
		}
	}
}

// AddContainer persists a new container record.
func (s *Store) AddContainer(r *ContainerRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Containers[r.ID] = r
	return s.save()
}

// RemoveContainer deletes a container record by ID and frees its IP address.
func (s *Store) RemoveContainer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.data.Containers[id]; ok && rec.IPAddress != "" {
		if ip := net.ParseIP(rec.IPAddress); ip != nil {
			parts := strings.Split(rec.IPAddress, ".")
			if len(parts) == 4 {
				if octet, err := strconv.Atoi(parts[3]); err == nil && octet >= 2 {
					s.data.FreeIPs = append(s.data.FreeIPs, octet)
				}
			}
		}
	}

	delete(s.data.Containers, id)
	return s.save()
}

// GetContainer returns the record for id, or nil if not found.
func (s *Store) GetContainer(id string) *ContainerRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Containers[id]
}

// FindContainerByName returns the first container whose Name matches.
func (s *Store) FindContainerByName(name string) *ContainerRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.data.Containers {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// ListContainers returns all container records.
func (s *Store) ListContainers() []*ContainerRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ContainerRecord, 0, len(s.data.Containers))
	for _, r := range s.data.Containers {
		out = append(out, r)
	}
	return out
}

// AddVolume persists a named volume record.
func (s *Store) AddVolume(v *VolumeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Volumes[v.Name] = v
	return s.save()
}

// GetVolume returns the named volume, or nil if not found.
func (s *Store) GetVolume(name string) *VolumeRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Volumes[name]
}

// ListVolumes returns all known named volumes.
func (s *Store) ListVolumes() []*VolumeRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*VolumeRecord, 0, len(s.data.Volumes))
	for _, v := range s.data.Volumes {
		out = append(out, v)
	}
	return out
}

// RemoveVolume deletes a named volume record.
func (s *Store) RemoveVolume(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Volumes, name)
	return s.save()
}

// AddNetwork persists a network record.
func (s *Store) AddNetwork(n *NetworkRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Networks[n.ID] = n
	return s.save()
}

// GetNetwork returns a network by id or name, or nil if not found.
func (s *Store) GetNetwork(idOrName string) *NetworkRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n, ok := s.data.Networks[idOrName]; ok {
		return n
	}
	for _, n := range s.data.Networks {
		if n.Name == idOrName {
			return n
		}
	}
	return nil
}

// ListNetworks returns all known networks.
func (s *Store) ListNetworks() []*NetworkRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*NetworkRecord, 0, len(s.data.Networks))
	for _, n := range s.data.Networks {
		out = append(out, n)
	}
	return out
}

// RemoveNetwork deletes a network by id.
func (s *Store) RemoveNetwork(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Networks, id)
	return s.save()
}

// AddImage persists a new image record keyed by its Ref.
func (s *Store) AddImage(r *ImageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Images[r.Ref] = r
	return s.save()
}

// RemoveImage deletes an image record by Ref.
func (s *Store) RemoveImage(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Images, ref)
	return s.save()
}

// GetImage returns the image record for ref, or nil if not found.
// It tries an exact match first, then falls back to matching after
// stripping registry and "library/" prefixes (e.g. "nginx:latest"
// matches "docker.io/library/nginx:latest").
func (s *Store) GetImage(ref string) *ImageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.data.Images[ref]; ok {
		return r
	}
	// Fuzzy match: strip prefixes from both sides and compare.
	bare := bareImageRef(ref)
	for key, r := range s.data.Images {
		if bareImageRef(key) == bare {
			return r
		}
	}
	return nil
}

// bareImageRef strips registry and "library/" prefixes from an image ref.
// "docker.io/library/nginx:latest" → "nginx:latest"
// "nginx:latest" → "nginx:latest"
func bareImageRef(ref string) string {
	// Strip registry (anything with a dot before the first slash).
	if i := strings.Index(ref, "/"); i != -1 {
		prefix := ref[:i]
		if strings.Contains(prefix, ".") || strings.Contains(prefix, ":") {
			ref = ref[i+1:]
		}
	}
	// Strip "library/" prefix (Docker Hub default namespace).
	ref = strings.TrimPrefix(ref, "library/")
	return ref
}

// ListImages returns all image records.
func (s *Store) ListImages() []*ImageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ImageRecord, 0, len(s.data.Images))
	for _, r := range s.data.Images {
		out = append(out, r)
	}
	return out
}

// ResolveID resolves a partial or full container ID or name to a full ID.
// Returns "" if not found.
func (s *Store) ResolveID(idOrName string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Exact ID match
	if _, ok := s.data.Containers[idOrName]; ok {
		return idOrName
	}
	// Prefix match on ID
	for id := range s.data.Containers {
		if len(idOrName) >= 4 && len(id) >= len(idOrName) && id[:len(idOrName)] == idOrName {
			return id
		}
	}
	// Name match
	for id, r := range s.data.Containers {
		if r.Name == idOrName {
			return id
		}
	}
	return ""
}

func (s *Store) load() error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(&s.data)
}

func (s *Store) save() error {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&s.data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// RootDir returns the directory containing the store state.
func (s *Store) RootDir() string {
	return filepath.Dir(s.path)
}
