// Package api implements the Docker Engine HTTP API surface that is consumed
// by the raw docker CLI and GoW tooling.
package api

import "time"

// --- Container Create ---

// ContainerCreateRequest mirrors the relevant subset of the Docker Engine
// POST /containers/create body.
type ContainerCreateRequest struct {
	Image            string            `json:"Image"`
	Cmd              []string          `json:"Cmd"`
	Entrypoint       []string          `json:"Entrypoint"`
	Env              []string          `json:"Env"`
	Labels           map[string]string `json:"Labels"`
	WorkingDir       string            `json:"WorkingDir"`
	Mounts           []MountRequest    `json:"Mounts"`
	NetworkingConfig NetworkingConfig  `json:"NetworkingConfig"`
	HostConfig       HostConfig        `json:"HostConfig"`
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
	Name            string           `json:"Name"`
	State           ContainerState   `json:"State"`
	Image           string           `json:"Image"`
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
	Status     string `json:"Status"` // "running", "exited", "created"
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	Dead       bool   `json:"Dead"`
	Pid        int    `json:"Pid"`
	ExitCode   int    `json:"ExitCode"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

// ContainerConfig is the image-level config embedded in ContainerJSON.
type ContainerConfig struct {
	Hostname   string            `json:"Hostname"`
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Entrypoint []string          `json:"Entrypoint"`
	Env        []string          `json:"Env"`
	Labels     map[string]string `json:"Labels"`
	WorkingDir string            `json:"WorkingDir"`
}

// NetworkSettings holds the IP and network info for a container.
type NetworkSettings struct {
	IPAddress string                      `json:"IPAddress"`
	Networks  map[string]EndpointSettings `json:"Networks"`
}

// EndpointSettings is a per-network settings block.
type EndpointSettings struct {
	IPAddress  string `json:"IPAddress"`
	Gateway    string `json:"Gateway"`
	MacAddress string `json:"MacAddress"`
	NetworkID  string `json:"NetworkID"`
	EndpointID string `json:"EndpointID,omitempty"`
}

// --- Container List ---

// ContainerSummary is a single item in the GET /containers/json response.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"` // Unix timestamp
	Status  string            `json:"Status"`
	State   string            `json:"State"`
	Ports   []Port            `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
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
	Labels      map[string]string `json:"Labels"`
}

// ImageInspect is the body returned by GET /images/{name}/json.
type ImageInspect struct {
	ID           string            `json:"Id"`
	RepoTags     []string          `json:"RepoTags"`
	Created      string            `json:"Created"`
	Architecture string            `json:"Architecture"`
	Os           string            `json:"Os"`
	Labels       map[string]string `json:"Labels"`
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
}

// --- System ---

// VersionResponse is the body of GET /version.
type VersionResponse struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	GitCommit     string `json:"GitCommit"`
	GoVersion     string `json:"GoVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
	KernelVersion string `json:"KernelVersion"`
	BuildTime     string `json:"BuildTime"`
}

// InfoResponse is a trimmed body for GET /info.
type InfoResponse struct {
	ID                string `json:"ID"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
	Driver            string `json:"Driver"`
	MemoryLimit       bool   `json:"MemoryLimit"`
	SwapLimit         bool   `json:"SwapLimit"`
	KernelVersion     string `json:"KernelVersion"`
	OperatingSystem   string `json:"OperatingSystem"`
	OSType            string `json:"OSType"`
	Architecture      string `json:"Architecture"`
	NCPU              int    `json:"NCPU"`
	MemTotal          int64  `json:"MemTotal"`
	DockerRootDir     string `json:"DockerRootDir"`
	ServerVersion     string `json:"ServerVersion"`
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
	Name    string            `json:"Name"`
	Driver  string            `json:"Driver"`
	Options map[string]string `json:"Options"`
	Labels  map[string]string `json:"Labels"`
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
	ExitCode    int
	Running     bool
	StartedAt   time.Time
}
