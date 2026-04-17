// Package api implements the Docker Engine HTTP API surface that is consumed
// by the raw docker CLI and GoW tooling.
package api

import (
	"os"
	"time"
)

// --- Container Create ---

// ContainerCreateRequest mirrors the relevant subset of the Docker Engine
// POST /containers/create body.
type ContainerCreateRequest struct {
	Image            string              `json:"Image"`
	Cmd              []string            `json:"Cmd"`
	Entrypoint       []string            `json:"Entrypoint"`
	Env              []string            `json:"Env"`
	Labels           map[string]string   `json:"Labels"`
	WorkingDir       string              `json:"WorkingDir"`
	User             string              `json:"User"`
	Domainname       string              `json:"Domainname"`
	Hostname         string              `json:"Hostname"`
	Mounts           []MountRequest      `json:"Mounts"`
	Volumes          map[string]struct{} `json:"Volumes"`
	NetworkingConfig NetworkingConfig    `json:"NetworkingConfig"`
	HostConfig       HostConfig          `json:"HostConfig"`
	Healthcheck      *HealthConfig       `json:"Healthcheck,omitempty"`
	StopSignal       string              `json:"StopSignal,omitempty"`
	Tty              bool                `json:"Tty"`
	OpenStdin        bool                `json:"OpenStdin"`
	StdinOnce        bool                `json:"StdinOnce"`
}

// HealthConfig is Docker's healthcheck configuration block. We don't
// actually execute healthchecks, but we roundtrip the config through
// inspect so Portainer's UI can render what the user submitted.
type HealthConfig struct {
	Test          []string `json:"Test,omitempty"`
	Interval      int64    `json:"Interval,omitempty"`      // nanoseconds
	Timeout       int64    `json:"Timeout,omitempty"`       // nanoseconds
	StartPeriod   int64    `json:"StartPeriod,omitempty"`   // nanoseconds
	StartInterval int64    `json:"StartInterval,omitempty"` // nanoseconds
	Retries       int      `json:"Retries,omitempty"`
}

// HostConfig holds the host-level container options.
type HostConfig struct {
	Binds             []string                 `json:"Binds"` // "host:container[:ro]"
	Devices           []DeviceMapping          `json:"Devices"`
	DeviceCgroupRules []string                 `json:"DeviceCgroupRules"`
	Memory            int64                    `json:"Memory"` // bytes, 0=unlimited
	CPUShares         int64                    `json:"CpuShares"`
	NanoCPUs          int64                    `json:"NanoCpus"`
	NetworkMode       string                   `json:"NetworkMode"`
	IpcMode           string                   `json:"IpcMode"` // "host" or "" (private)
	PortBindings      map[string][]PortBinding `json:"PortBindings"`
	RestartPolicy     RestartPolicy            `json:"RestartPolicy"`
	Privileged        bool                     `json:"Privileged,omitempty"`
	CapAdd            []string                 `json:"CapAdd,omitempty"`
	CapDrop           []string                 `json:"CapDrop,omitempty"`
	ExtraHosts        []string                 `json:"ExtraHosts,omitempty"`
	Dns               []string                 `json:"Dns,omitempty"`
	DnsSearch         []string                 `json:"DnsSearch,omitempty"`
	DnsOptions        []string                 `json:"DnsOptions,omitempty"`
	// AutoRemove mirrors Docker's --rm flag. When true, the daemon creates
	// the container as ephemeral (no PVE UI presence; reaped by GC after
	// it exits). Default false → permanent PVE CT in PVE mode.
	AutoRemove bool `json:"AutoRemove"`
}

// DeviceMapping is a single host→container device mapping.
type DeviceMapping struct {
	PathOnHost        string `json:"PathOnHost"`
	PathInContainer   string `json:"PathInContainer"`
	CgroupPermissions string `json:"CgroupPermissions"`
}

// PortBinding maps a container port to a host port.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy mirrors Docker's restart policy field.
type RestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount"`
}

// ContainerCreateResponse is the body returned by POST /containers/create.
type ContainerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// --- Container Inspect ---

// ContainerJSON is the body returned by GET /containers/{id}/json.
type ContainerJSON struct {
	ID              string           `json:"Id"`
	Created         string           `json:"Created"`
	Path            string           `json:"Path"`
	Args            []string         `json:"Args"`
	Name            string           `json:"Name"`
	RestartCount    int              `json:"RestartCount"`
	Driver          string           `json:"Driver"`
	Platform        string           `json:"Platform"`
	State           ContainerState   `json:"State"`
	Image           string           `json:"Image"`
	ImageID         string           `json:"ImageID,omitempty"`
	LogPath         string           `json:"LogPath"`
	ResolvConfPath  string           `json:"ResolvConfPath"`
	HostnamePath    string           `json:"HostnamePath"`
	HostsPath       string           `json:"HostsPath"`
	SizeRw          *int64           `json:"SizeRw,omitempty"`
	SizeRootFs      *int64           `json:"SizeRootFs,omitempty"`
	Config          *ContainerConfig `json:"Config"`
	HostConfig      *HostConfig      `json:"HostConfig"`
	Mounts          []MountJSON      `json:"Mounts"`
	NetworkSettings NetworkSettings  `json:"NetworkSettings"`
}

// MountJSON represents a mount in the inspect response.
type MountJSON struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}

// ContainerState holds the runtime state of a container.
type ContainerState struct {
	Status     string           `json:"Status"` // "running", "exited", "created"
	Running    bool             `json:"Running"`
	Paused     bool             `json:"Paused"`
	Restarting bool             `json:"Restarting"`
	OOMKilled  bool             `json:"OOMKilled"`
	Dead       bool             `json:"Dead"`
	Pid        int              `json:"Pid"`
	ExitCode   int              `json:"ExitCode"`
	Error      string           `json:"Error"`
	StartedAt  string           `json:"StartedAt"`
	FinishedAt string           `json:"FinishedAt"`
	Health     *ContainerHealth `json:"Health,omitempty"`
}

// ContainerHealth is the /health subblock of ContainerState. We don't run
// healthchecks today, but Portainer's container list uses the presence
// and Status of this field to decide which badge to render.
type ContainerHealth struct {
	Status        string                 `json:"Status"`
	FailingStreak int                    `json:"FailingStreak"`
	Log           []ContainerHealthEntry `json:"Log"`
}

// ContainerHealthEntry is a single execution record inside Health.Log.
type ContainerHealthEntry struct {
	Start    string `json:"Start"`
	End      string `json:"End"`
	ExitCode int    `json:"ExitCode"`
	Output   string `json:"Output"`
}

// ContainerConfig is the image-level config embedded in ContainerJSON.
type ContainerConfig struct {
	Hostname     string              `json:"Hostname"`
	Domainname   string              `json:"Domainname,omitempty"`
	User         string              `json:"User,omitempty"`
	Image        string              `json:"Image"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	WorkingDir   string              `json:"WorkingDir"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	Healthcheck  *HealthConfig       `json:"Healthcheck,omitempty"`
	Tty          bool                `json:"Tty"`
	OpenStdin    bool                `json:"OpenStdin"`
	StdinOnce    bool                `json:"StdinOnce"`
	AttachStdin  bool                `json:"AttachStdin"`
	AttachStdout bool                `json:"AttachStdout"`
	AttachStderr bool                `json:"AttachStderr"`
}

// NetworkSettings holds the IP and network info for a container.
type NetworkSettings struct {
	IPAddress string                      `json:"IPAddress"`
	Networks  map[string]EndpointSettings `json:"Networks"`
	Ports     map[string][]PortBinding    `json:"Ports,omitempty"`
}

// EndpointSettings is a per-network settings block.
type EndpointSettings struct {
	IPAddress  string            `json:"IPAddress"`
	Gateway    string            `json:"Gateway"`
	MacAddress string            `json:"MacAddress"`
	NetworkID  string            `json:"NetworkID"`
	EndpointID string            `json:"EndpointID,omitempty"`
	Aliases    []string          `json:"Aliases,omitempty"`
	Links      []string          `json:"Links,omitempty"`
	DriverOpts map[string]string `json:"DriverOpts,omitempty"`
}

// --- Container List ---

// ContainerSummary is a single item in the GET /containers/json response.
type ContainerSummary struct {
	ID              string                 `json:"Id"`
	Names           []string               `json:"Names"`
	Image           string                 `json:"Image"`
	ImageID         string                 `json:"ImageID"`
	Command         string                 `json:"Command"`
	Created         int64                  `json:"Created"` // Unix timestamp
	Status          string                 `json:"Status"`
	State           string                 `json:"State"`
	Ports           []Port                 `json:"Ports"`
	Labels          map[string]string      `json:"Labels"`
	SizeRw          int64                  `json:"SizeRw,omitempty"`
	SizeRootFs      int64                  `json:"SizeRootFs,omitempty"`
	Mounts          []MountJSON            `json:"Mounts"`
	NetworkSettings *SummaryNetworkSetting `json:"NetworkSettings,omitempty"`
	HostConfig      *SummaryHostConfig     `json:"HostConfig,omitempty"`
}

// SummaryNetworkSetting is the shape Portainer expects for
// ContainerSummary.NetworkSettings — just a Networks map, not the full
// per-container NetworkSettings returned by inspect.
type SummaryNetworkSetting struct {
	Networks map[string]EndpointSettings `json:"Networks"`
}

// SummaryHostConfig is the minimal HostConfig block Docker includes in the
// container-list response (NetworkMode is the only field Portainer reads).
type SummaryHostConfig struct {
	NetworkMode string `json:"NetworkMode"`
}

// Port describes a mapped port.
type Port struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort uint16 `json:"PrivatePort"`
	PublicPort  uint16 `json:"PublicPort,omitempty"`
	Type        string `json:"Type"`
}

// --- Images ---

// ImageSummary is a single item in GET /images/json.
type ImageSummary struct {
	ID          string            `json:"Id"`
	ParentID    string            `json:"ParentId"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	VirtualSize int64             `json:"VirtualSize"`
	Labels      map[string]string `json:"Labels"`
}

// ImageInspect is the body returned by GET /images/{name}/json.
type ImageInspect struct {
	ID              string            `json:"Id"`
	RepoTags        []string          `json:"RepoTags"`
	RepoDigests     []string          `json:"RepoDigests"`
	Parent          string            `json:"Parent"`
	Comment         string            `json:"Comment"`
	Created         string            `json:"Created"`
	Container       string            `json:"Container"`
	DockerVersion   string            `json:"DockerVersion"`
	Author          string            `json:"Author"`
	Architecture    string            `json:"Architecture"`
	Os              string            `json:"Os"`
	Size            int64             `json:"Size"`
	VirtualSize     int64             `json:"VirtualSize"`
	Labels          map[string]string `json:"Labels"`
	Config          *ImageConfig      `json:"Config"`
	ContainerConfig *ImageConfig      `json:"ContainerConfig"`
	RootFS          ImageRootFS       `json:"RootFS"`
	GraphDriver     ImageGraphDriver  `json:"GraphDriver"`
}

// ImageConfig mirrors the OCI image config block Portainer reads for the
// image detail page (Entrypoint/Cmd/Env tabs, Exposed ports, WorkingDir).
type ImageConfig struct {
	Hostname     string              `json:"Hostname"`
	Image        string              `json:"Image"`
	Env          []string            `json:"Env"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	WorkingDir   string              `json:"WorkingDir"`
	Labels       map[string]string   `json:"Labels"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	Volumes      map[string]struct{} `json:"Volumes,omitempty"`
	User         string              `json:"User,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	Healthcheck  *HealthConfig       `json:"Healthcheck,omitempty"`
}

// ImageRootFS describes an image's layer list. We do not track layer
// checksums, so we emit a single synthetic layer pointing at the image ID —
// enough for Portainer to render the RootFS tab.
type ImageRootFS struct {
	Type   string   `json:"Type"`
	Layers []string `json:"Layers"`
}

// ImageGraphDriver describes the storage backend of an image. Portainer
// displays Name on the image detail page.
type ImageGraphDriver struct {
	Name string            `json:"Name"`
	Data map[string]string `json:"Data"`
}

// MountRequest is a mount entry in the Docker container-create request body.
type MountRequest struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
}

// NetworkingConfig mirrors the Docker container-create networking block.
type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointSettings `json:"EndpointsConfig"`
}

// --- Exec ---

// ExecCreateRequest is the body of POST /containers/{id}/exec.
type ExecCreateRequest struct {
	Cmd          []string `json:"Cmd"`
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Env          []string `json:"Env"`
	WorkingDir   string   `json:"WorkingDir"`
	User         string   `json:"User"`
	Privileged   bool     `json:"Privileged"`
}

// ExecCreateResponse is the body returned by POST /containers/{id}/exec.
type ExecCreateResponse struct {
	ID string `json:"Id"`
}

// ExecStartRequest is the body of POST /exec/{id}/start.
type ExecStartRequest struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

// ExecInspect is the body returned by GET /exec/{id}/json.
type ExecInspect struct {
	ID            string            `json:"ID"`
	ContainerID   string            `json:"ContainerID"`
	Running       bool              `json:"Running"`
	ExitCode      int               `json:"ExitCode"`
	ProcessConfig ExecProcessConfig `json:"ProcessConfig"`
}

// ExecProcessConfig holds the command run via exec.
type ExecProcessConfig struct {
	Tty        bool     `json:"tty"`
	Entrypoint string   `json:"entrypoint"`
	Arguments  []string `json:"arguments"`
	User       string   `json:"user,omitempty"`
	Privileged bool     `json:"privileged,omitempty"`
}

// --- System ---

// VersionResponse is the body of GET /version.
type VersionResponse struct {
	Version       string             `json:"Version"`
	APIVersion    string             `json:"ApiVersion"`
	MinAPIVersion string             `json:"MinAPIVersion"`
	GitCommit     string             `json:"GitCommit"`
	GoVersion     string             `json:"GoVersion"`
	Os            string             `json:"Os"`
	Arch          string             `json:"Arch"`
	KernelVersion string             `json:"KernelVersion"`
	BuildTime     string             `json:"BuildTime"`
	Platform      VersionPlatform    `json:"Platform"`
	Components    []VersionComponent `json:"Components"`
}

// VersionPlatform is the Platform subfield of /version, shown by
// Portainer's "Platform" label.
type VersionPlatform struct {
	Name string `json:"Name"`
}

// VersionComponent is an entry in /version Components. Portainer reads the
// Engine component and displays the version/runtime on the host details
// page.
type VersionComponent struct {
	Name    string            `json:"Name"`
	Version string            `json:"Version"`
	Details map[string]string `json:"Details,omitempty"`
}

// InfoResponse is a trimmed body for GET /info.
type InfoResponse struct {
	ID                 string           `json:"ID"`
	Name               string           `json:"Name"`
	Containers         int              `json:"Containers"`
	ContainersRunning  int              `json:"ContainersRunning"`
	ContainersPaused   int              `json:"ContainersPaused"`
	ContainersStopped  int              `json:"ContainersStopped"`
	Images             int              `json:"Images"`
	Driver             string           `json:"Driver"`
	MemoryLimit        bool             `json:"MemoryLimit"`
	SwapLimit          bool             `json:"SwapLimit"`
	KernelVersion      string           `json:"KernelVersion"`
	OperatingSystem    string           `json:"OperatingSystem"`
	OSVersion          string           `json:"OSVersion"`
	OSType             string           `json:"OSType"`
	Architecture       string           `json:"Architecture"`
	NCPU               int              `json:"NCPU"`
	MemTotal           int64            `json:"MemTotal"`
	DockerRootDir      string           `json:"DockerRootDir"`
	ServerVersion      string           `json:"ServerVersion"`
	CgroupDriver       string           `json:"CgroupDriver"`
	CgroupVersion      string           `json:"CgroupVersion"`
	DefaultRuntime     string           `json:"DefaultRuntime"`
	Runtimes           map[string]any   `json:"Runtimes"`
	Plugins            InfoPlugins      `json:"Plugins"`
	Labels             []string         `json:"Labels"`
	ExperimentalBuild  bool             `json:"ExperimentalBuild"`
	SystemTime         string           `json:"SystemTime"`
	LiveRestoreEnabled bool             `json:"LiveRestoreEnabled"`
	IndexServerAddress string           `json:"IndexServerAddress"`
	RegistryConfig     map[string]any   `json:"RegistryConfig"`
	Warnings           []string         `json:"Warnings"`
	SecurityOptions    []string         `json:"SecurityOptions"`
	ContainerdCommit   VersionComponent `json:"ContainerdCommit"`
	RuncCommit         VersionComponent `json:"RuncCommit"`
	InitCommit         VersionComponent `json:"InitCommit"`
}

// InfoPlugins mirrors the Plugins block from Docker's /info. Portainer reads
// Volume and Network lists to show storage/network drivers available.
type InfoPlugins struct {
	Volume        []string `json:"Volume"`
	Network       []string `json:"Network"`
	Authorization []string `json:"Authorization"`
	Log           []string `json:"Log"`
}

// ChangeResponse is a single filesystem change entry.
type ChangeResponse struct {
	Path string `json:"Path"`
	Kind int    `json:"Kind"`
}

// ContainerStats is a trimmed Docker-compatible stats payload.
type ContainerStats struct {
	Read         string              `json:"read"`
	PreRead      string              `json:"preread"`
	PidsStats    PidsStats           `json:"pids_stats"`
	BlkioStats   map[string][]any    `json:"blkio_stats"`
	NumProcs     int                 `json:"num_procs"`
	StorageStats map[string]any      `json:"storage_stats"`
	CPUStats     CPUStats            `json:"cpu_stats"`
	PreCPUStats  CPUStats            `json:"precpu_stats"`
	MemoryStats  MemoryStats         `json:"memory_stats"`
	Networks     map[string]NetStats `json:"networks,omitempty"`
}

type PidsStats struct {
	Current int `json:"current"`
}

type CPUStats struct {
	CPUUsage       CPUUsage `json:"cpu_usage"`
	SystemCPUUsage uint64   `json:"system_cpu_usage"`
	OnlineCPUs     int      `json:"online_cpus"`
}

type CPUUsage struct {
	TotalUsage        uint64   `json:"total_usage"`
	PercpuUsage       []uint64 `json:"percpu_usage"`
	UsageInKernelmode uint64   `json:"usage_in_kernelmode"`
	UsageInUsermode   uint64   `json:"usage_in_usermode"`
}

type MemoryStats struct {
	Usage    uint64         `json:"usage"`
	MaxUsage uint64         `json:"max_usage"`
	Limit    uint64         `json:"limit"`
	Stats    map[string]any `json:"stats"`
}

type NetStats struct {
	RxBytes   uint64 `json:"rx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	RxErrors  uint64 `json:"rx_errors"`
	RxDropped uint64 `json:"rx_dropped"`
	TxBytes   uint64 `json:"tx_bytes"`
	TxPackets uint64 `json:"tx_packets"`
	TxErrors  uint64 `json:"tx_errors"`
	TxDropped uint64 `json:"tx_dropped"`
}

// SystemDiskUsage is the response body for GET /system/df.
type SystemDiskUsage struct {
	LayersSize int64            `json:"LayersSize"`
	Images     []ImageUsage     `json:"Images"`
	Containers []ContainerUsage `json:"Containers"`
	Volumes    []VolumeUsage    `json:"Volumes"`
	BuildCache []any            `json:"BuildCache"`
}

type ImageUsage struct {
	ID           string   `json:"Id"`
	Repository   string   `json:"Repository"`
	Tag          string   `json:"Tag"`
	CreatedSince string   `json:"CreatedSince,omitempty"`
	CreatedAt    string   `json:"CreatedAt"`
	Size         int64    `json:"Size"`
	SharedSize   int64    `json:"SharedSize"`
	Containers   int      `json:"Containers"`
	RepoTags     []string `json:"RepoTags,omitempty"`
}

type ContainerUsage struct {
	ID         string   `json:"Id"`
	Names      []string `json:"Names"`
	Image      string   `json:"Image"`
	ImageID    string   `json:"ImageID"`
	Command    string   `json:"Command"`
	Created    int64    `json:"Created"`
	State      string   `json:"State"`
	Status     string   `json:"Status"`
	SizeRw     int64    `json:"SizeRw"`
	SizeRootFs int64    `json:"SizeRootFs"`
}

type VolumeUsage struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
	Scope      string            `json:"Scope"`
	UsageData  VolumeUsageData   `json:"UsageData"`
}

type VolumeUsageData struct {
	RefCount int   `json:"RefCount"`
	Size     int64 `json:"Size"`
}

type VolumeListResponse struct {
	Volumes  []VolumeUsage `json:"Volumes"`
	Warnings []string      `json:"Warnings"`
}

type VolumeCreateResponse struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Options    map[string]string `json:"Options"`
	Scope      string            `json:"Scope"`
}

type VolumeCreateRequest struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Labels     map[string]string `json:"Labels"`
	DriverOpts map[string]string `json:"DriverOpts"`
}

type NetworkResource struct {
	Name       string                     `json:"Name"`
	ID         string                     `json:"Id"`
	Created    string                     `json:"Created"`
	Scope      string                     `json:"Scope"`
	Driver     string                     `json:"Driver"`
	EnableIPv4 bool                       `json:"EnableIPv4"`
	EnableIPv6 bool                       `json:"EnableIPv6"`
	Internal   bool                       `json:"Internal"`
	Attachable bool                       `json:"Attachable"`
	Ingress    bool                       `json:"Ingress"`
	IPAM       map[string]any             `json:"IPAM"`
	Options    map[string]string          `json:"Options"`
	Labels     map[string]string          `json:"Labels"`
	Containers map[string]NetworkEndpoint `json:"Containers,omitempty"`
}

type NetworkEndpoint struct {
	Name        string `json:"Name"`
	EndpointID  string `json:"EndpointID"`
	MacAddress  string `json:"MacAddress"`
	IPv4Address string `json:"IPv4Address"`
	IPv6Address string `json:"IPv6Address"`
}

type NetworkCreateRequest struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	IPAM       *IPAMRequest      `json:"IPAM"`
}

// IPAMRequest is the subset of Docker's IPAM block we honour on create.
type IPAMRequest struct {
	Driver  string            `json:"Driver"`
	Config  []IPAMConfigEntry `json:"Config"`
	Options map[string]string `json:"Options"`
}

// IPAMConfigEntry mirrors a single IPAM.Config entry. Roundtripped verbatim
// through inspect — the daemon doesn't use it for address allocation today.
type IPAMConfigEntry struct {
	Subnet     string            `json:"Subnet,omitempty"`
	IPRange    string            `json:"IPRange,omitempty"`
	Gateway    string            `json:"Gateway,omitempty"`
	AuxAddress map[string]string `json:"AuxiliaryAddresses,omitempty"`
}

type NetworkCreateResponse struct {
	ID      string `json:"Id"`
	Warning string `json:"Warning"`
}

type NetworkConnectRequest struct {
	Container      string           `json:"Container"`
	EndpointConfig EndpointSettings `json:"EndpointConfig"`
}

type ImageHistoryItem struct {
	ID        string   `json:"Id"`
	Created   int64    `json:"Created"`
	CreatedBy string   `json:"CreatedBy"`
	Tags      []string `json:"Tags"`
	Size      int64    `json:"Size"`
	Comment   string   `json:"Comment"`
}

type ImageSearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	StarCount   int    `json:"star_count"`
	IsOfficial  bool   `json:"is_official"`
	IsAutomated bool   `json:"is_automated"`
}

type EventMessage struct {
	Type     string     `json:"Type"`
	Action   string     `json:"Action"`
	Actor    EventActor `json:"Actor"`
	Scope    string     `json:"scope"`
	Time     int64      `json:"time"`
	TimeNano int64      `json:"timeNano"`
}

type EventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

// ErrorResponse is the standard Docker API error body.
type ErrorResponse struct {
	Message string `json:"message"`
}

// execRecord tracks an in-flight or completed exec instance.
type execRecord struct {
	ID          string
	ContainerID string
	Cmd         []string
	Tty         bool
	Env         []string
	WorkingDir  string
	User        string
	ExitCode    int
	Running     bool
	StartedAt   time.Time
	// pty is the PTY master for a running TTY exec, used by /exec/{id}/resize.
	// nil when the exec is not TTY or has not started yet.
	pty *os.File
}
