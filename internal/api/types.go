// Package api implements the Docker Engine HTTP API surface that is consumed
// by the raw docker CLI and GoW tooling.
package api

import (
	"os"
	"time"
)

// --- Container Create ---

// ContainerCreateRequest mirrors the relevant subset of the Docker Engine
// POST /containers/create body. Fields the daemon doesn't act on are still
// declared here so they round-trip through create → inspect — Portainer's
// "Duplicate/Edit container" flow re-posts whatever inspect returned, so
// dropping fields on the floor corrupts the resulting container.
type ContainerCreateRequest struct {
	Hostname         string              `json:"Hostname"`
	Domainname       string              `json:"Domainname"`
	User             string              `json:"User"`
	AttachStdin      bool                `json:"AttachStdin"`
	AttachStdout     bool                `json:"AttachStdout"`
	AttachStderr     bool                `json:"AttachStderr"`
	Tty              bool                `json:"Tty"`
	OpenStdin        bool                `json:"OpenStdin"`
	StdinOnce        bool                `json:"StdinOnce"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts"`
	Image            string              `json:"Image"`
	Cmd              []string            `json:"Cmd"`
	Entrypoint       []string            `json:"Entrypoint"`
	Env              []string            `json:"Env"`
	Labels           map[string]string   `json:"Labels"`
	WorkingDir       string              `json:"WorkingDir"`
	Volumes          map[string]struct{} `json:"Volumes"`
	StopSignal       string              `json:"StopSignal"`
	StopTimeout      *int                `json:"StopTimeout,omitempty"`
	Healthcheck      *Healthcheck        `json:"Healthcheck,omitempty"`
	HostConfig       HostConfig          `json:"HostConfig"`
	NetworkingConfig *NetworkingConfig   `json:"NetworkingConfig,omitempty"`
}

// Healthcheck is Docker's Config.Healthcheck. LXC has no equivalent so we
// just persist and echo it.
type Healthcheck struct {
	Test        []string `json:"Test"`
	Interval    int64    `json:"Interval"`
	Timeout     int64    `json:"Timeout"`
	Retries     int      `json:"Retries"`
	StartPeriod int64    `json:"StartPeriod"`
}

// NetworkingConfig is an optional top-level Create body field. Portainer uses
// it to pre-attach a container to a named network. We accept and ignore.
type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointSettings `json:"EndpointsConfig"`
}

// HostConfig holds the host-level container options. Most fields below are
// accepted-and-echoed rather than enforced; the comments call out which ones
// actually shape the LXC config. Portainer's create/edit forms POST all of
// them, so silently dropping would surface as lost settings on "Duplicate".
type HostConfig struct {
	// Mounts & storage
	Binds          []string          `json:"Binds"` // "host:container[:ro]"
	Mounts         []MountSpec       `json:"Mounts,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	VolumesFrom    []string          `json:"VolumesFrom,omitempty"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	ShmSize        int64             `json:"ShmSize"`
	// Devices & capabilities
	Devices           []DeviceMapping   `json:"Devices"`
	DeviceCgroupRules []string          `json:"DeviceCgroupRules"`
	CapAdd            []string          `json:"CapAdd"`
	CapDrop           []string          `json:"CapDrop"`
	Privileged        bool              `json:"Privileged"`
	SecurityOpt       []string          `json:"SecurityOpt"`
	GroupAdd          []string          `json:"GroupAdd"`
	Sysctls           map[string]string `json:"Sysctls,omitempty"`
	// Resource limits
	Memory            int64  `json:"Memory"` // bytes, 0=unlimited
	MemoryReservation int64  `json:"MemoryReservation"`
	MemorySwap        int64  `json:"MemorySwap"`
	CPUShares         int64  `json:"CpuShares"`
	CPUQuota          int64  `json:"CpuQuota"`
	CPUPeriod         int64  `json:"CpuPeriod"`
	CpusetCpus        string `json:"CpusetCpus"`
	CpusetMems        string `json:"CpusetMems"`
	NanoCPUs          int64  `json:"NanoCpus"`
	PidsLimit         int64  `json:"PidsLimit"`
	OomKillDisable    bool   `json:"OomKillDisable"`
	OomScoreAdj       int    `json:"OomScoreAdj"`
	// Networking / DNS
	NetworkMode     string                   `json:"NetworkMode"`
	PortBindings    map[string][]PortBinding `json:"PortBindings"`
	DNS             []string                 `json:"Dns,omitempty"`
	DNSOptions      []string                 `json:"DnsOptions,omitempty"`
	DNSSearch       []string                 `json:"DnsSearch,omitempty"`
	ExtraHosts      []string                 `json:"ExtraHosts,omitempty"`
	PublishAllPorts bool                     `json:"PublishAllPorts"`
	// Namespaces
	IpcMode      string `json:"IpcMode"`
	PidMode      string `json:"PidMode"`
	UTSMode      string `json:"UTSMode"`
	UsernsMode   string `json:"UsernsMode"`
	CgroupnsMode string `json:"CgroupnsMode"`
	// Lifecycle
	RestartPolicy RestartPolicy `json:"RestartPolicy"`
	AutoRemove    bool          `json:"AutoRemove"`
	Init          *bool         `json:"Init,omitempty"`
	Runtime       string        `json:"Runtime"`
	// Misc
	Ulimits     []Ulimit          `json:"Ulimits,omitempty"`
	LogConfig   *LogConfig        `json:"LogConfig,omitempty"`
	Annotations map[string]string `json:"Annotations,omitempty"`
}

// MountSpec is the new-style mount declaration Portainer prefers over Binds.
type MountSpec struct {
	Type          string         `json:"Type"` // "bind", "volume", "tmpfs"
	Source        string         `json:"Source"`
	Target        string         `json:"Target"`
	ReadOnly      bool           `json:"ReadOnly"`
	Consistency   string         `json:"Consistency,omitempty"`
	BindOptions   map[string]any `json:"BindOptions,omitempty"`
	VolumeOptions map[string]any `json:"VolumeOptions,omitempty"`
	TmpfsOptions  map[string]any `json:"TmpfsOptions,omitempty"`
}

// Ulimit is an entry in HostConfig.Ulimits.
type Ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

// LogConfig mirrors Docker's HostConfig.LogConfig. LXC writes console output
// to a file regardless of what's requested here; we echo it back unchanged.
type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
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

// ContainerJSON is the body returned by GET /containers/{id}/json. Fields
// marked "Portainer" are read by the UI and rendered when present; missing
// ones show as blanks rather than breaking the inspect view.
type ContainerJSON struct {
	ID              string           `json:"Id"`
	Created         string           `json:"Created"`
	Path            string           `json:"Path"`
	Args            []string         `json:"Args"`
	Name            string           `json:"Name"`
	State           ContainerState   `json:"State"`
	Image           string           `json:"Image"`
	ResolvConfPath  string           `json:"ResolvConfPath"`
	HostnamePath    string           `json:"HostnamePath"`
	LogPath         string           `json:"LogPath"`
	RestartCount    int              `json:"RestartCount"`
	Driver          string           `json:"Driver"`
	Platform        string           `json:"Platform"`
	GraphDriver     GraphDriver      `json:"GraphDriver"`
	SizeRw          int64            `json:"SizeRw,omitempty"`
	SizeRootFs      int64            `json:"SizeRootFs,omitempty"`
	Config          *ContainerConfig `json:"Config"`
	HostConfig      *HostConfig      `json:"HostConfig"`
	Mounts          []MountJSON      `json:"Mounts"`
	NetworkSettings NetworkSettings  `json:"NetworkSettings"`
}

// GraphDriver mirrors Docker's storage-driver block. LXC doesn't use one, so
// we report a fixed "lxc" driver with no data. Portainer displays the driver
// name on the container detail page.
type GraphDriver struct {
	Name string            `json:"Name"`
	Data map[string]string `json:"Data"`
}

// MountJSON represents a mount in the inspect response.
type MountJSON struct {
	Type        string `json:"Type"`
	Name        string `json:"Name,omitempty"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver,omitempty"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
	Propagation string `json:"Propagation"`
}

// ContainerState holds the runtime state of a container.
type ContainerState struct {
	Status     string       `json:"Status"` // "running", "exited", "created", "paused"
	Running    bool         `json:"Running"`
	Paused     bool         `json:"Paused"`
	Restarting bool         `json:"Restarting"`
	OOMKilled  bool         `json:"OOMKilled"`
	Dead       bool         `json:"Dead"`
	Pid        int          `json:"Pid"`
	ExitCode   int          `json:"ExitCode"`
	Error      string       `json:"Error"`
	StartedAt  string       `json:"StartedAt"`
	FinishedAt string       `json:"FinishedAt"`
	Health     *HealthState `json:"Health,omitempty"`
}

// HealthState renders under State.Health on inspect. Portainer's container
// detail page shows the Status badge and the last few log entries.
type HealthState struct {
	Status        string                 `json:"Status"` // "starting"|"healthy"|"unhealthy"
	FailingStreak int                    `json:"FailingStreak"`
	Log           []ContainerHealthEntry `json:"Log"`
}

// ContainerHealthEntry is a single healthcheck result in State.Health.Log.
type ContainerHealthEntry struct {
	Start    string `json:"Start"`
	End      string `json:"End"`
	ExitCode int    `json:"ExitCode"`
	Output   string `json:"Output"`
}

// ContainerConfig is the image-level config embedded in ContainerJSON. The
// TTY/stdio flags aren't used by the runtime (LXC provides its own console)
// but Portainer reads them to populate the "Console" form on container
// recreate — omitting them produces a blank form.
type ContainerConfig struct {
	Hostname     string              `json:"Hostname"`
	Domainname   string              `json:"Domainname"`
	User         string              `json:"User"`
	AttachStdin  bool                `json:"AttachStdin"`
	AttachStdout bool                `json:"AttachStdout"`
	AttachStderr bool                `json:"AttachStderr"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	Tty          bool                `json:"Tty"`
	OpenStdin    bool                `json:"OpenStdin"`
	StdinOnce    bool                `json:"StdinOnce"`
	Image        string              `json:"Image"`
	Volumes      map[string]struct{} `json:"Volumes"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	Env          []string            `json:"Env"`
	Labels       map[string]string   `json:"Labels"`
	WorkingDir   string              `json:"WorkingDir"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	StopTimeout  *int                `json:"StopTimeout,omitempty"`
	Healthcheck  *Healthcheck        `json:"Healthcheck,omitempty"`
}

// NetworkSettings holds the IP and network info for a container. Portainer
// reads the top-level Bridge/Ports/IPAddress fields and the per-network
// Networks map; `docker inspect` output duplicates IPAddress at both levels.
type NetworkSettings struct {
	Bridge                 string                      `json:"Bridge"`
	SandboxID              string                      `json:"SandboxID"`
	SandboxKey             string                      `json:"SandboxKey"`
	HairpinMode            bool                        `json:"HairpinMode"`
	LinkLocalIPv6Address   string                      `json:"LinkLocalIPv6Address"`
	LinkLocalIPv6PrefixLen int                         `json:"LinkLocalIPv6PrefixLen"`
	Ports                  map[string][]PortBinding    `json:"Ports"`
	Gateway                string                      `json:"Gateway"`
	GlobalIPv6Address      string                      `json:"GlobalIPv6Address"`
	GlobalIPv6PrefixLen    int                         `json:"GlobalIPv6PrefixLen"`
	IPAddress              string                      `json:"IPAddress"`
	IPPrefixLen            int                         `json:"IPPrefixLen"`
	IPv6Gateway            string                      `json:"IPv6Gateway"`
	MacAddress             string                      `json:"MacAddress"`
	Networks               map[string]EndpointSettings `json:"Networks"`
}

// EndpointSettings is a per-network settings block. The NetworkID/EndpointID
// pair is how Portainer cross-references the container to the Networks tab;
// keep them populated so the "View network" link on a container works.
type EndpointSettings struct {
	IPAMConfig          any      `json:"IPAMConfig"`
	Links               []string `json:"Links"`
	Aliases             []string `json:"Aliases"`
	NetworkID           string   `json:"NetworkID"`
	EndpointID          string   `json:"EndpointID"`
	Gateway             string   `json:"Gateway"`
	IPAddress           string   `json:"IPAddress"`
	IPPrefixLen         int      `json:"IPPrefixLen"`
	IPv6Gateway         string   `json:"IPv6Gateway"`
	GlobalIPv6Address   string   `json:"GlobalIPv6Address"`
	GlobalIPv6PrefixLen int      `json:"GlobalIPv6PrefixLen"`
	MacAddress          string   `json:"MacAddress"`
	DriverOpts          any      `json:"DriverOpts"`
}

// --- Container List ---

// ContainerSummary is a single item in the GET /containers/json response.
type ContainerSummary struct {
	ID              string                       `json:"Id"`
	Names           []string                     `json:"Names"`
	Image           string                       `json:"Image"`
	ImageID         string                       `json:"ImageID"`
	Command         string                       `json:"Command"`
	Created         int64                        `json:"Created"` // Unix timestamp
	Status          string                       `json:"Status"`
	State           string                       `json:"State"`
	Ports           []Port                       `json:"Ports"`
	Labels          map[string]string            `json:"Labels"`
	SizeRw          int64                        `json:"SizeRw,omitempty"`
	SizeRootFs      int64                        `json:"SizeRootFs,omitempty"`
	HostConfig      *ContainerSummaryHostConfig  `json:"HostConfig,omitempty"`
	NetworkSettings *ContainerSummaryNetSettings `json:"NetworkSettings,omitempty"`
	Mounts          []MountJSON                  `json:"Mounts"`
}

// ContainerSummaryHostConfig is the slim HostConfig embedded in /containers/json.
type ContainerSummaryHostConfig struct {
	NetworkMode string `json:"NetworkMode"`
}

// ContainerSummaryNetSettings carries just enough network info for Portainer's
// list view to render a container's IP and attached networks without an
// extra /containers/{id}/json round-trip per row.
type ContainerSummaryNetSettings struct {
	Networks map[string]EndpointSettings `json:"Networks"`
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
	SharedSize  int64             `json:"SharedSize"`
	VirtualSize int64             `json:"VirtualSize"`
	Labels      map[string]string `json:"Labels"`
	// Containers is the count of containers using the image. Docker returns
	// -1 when the engine hasn't computed this (cheap); so do we.
	Containers int `json:"Containers"`
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
	ContainerConfig *ContainerConfig  `json:"ContainerConfig"`
	DockerVersion   string            `json:"DockerVersion"`
	Author          string            `json:"Author"`
	Config          *ContainerConfig  `json:"Config"`
	Architecture    string            `json:"Architecture"`
	Variant         string            `json:"Variant,omitempty"`
	Os              string            `json:"Os"`
	OsVersion       string            `json:"OsVersion,omitempty"`
	Size            int64             `json:"Size"`
	VirtualSize     int64             `json:"VirtualSize"`
	GraphDriver     GraphDriver       `json:"GraphDriver"`
	RootFS          ImageRootFS       `json:"RootFS"`
	Metadata        ImageMetadata     `json:"Metadata"`
	Labels          map[string]string `json:"Labels"`
}

// ImageRootFS describes an image's layer chain. LXC templates are flat so we
// report a single synthetic layer.
type ImageRootFS struct {
	Type   string   `json:"Type"`
	Layers []string `json:"Layers"`
}

// ImageMetadata carries timestamps the Portainer UI shows under "Last used".
type ImageMetadata struct {
	LastTagTime string `json:"LastTagTime"`
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
	OpenStdin     bool              `json:"OpenStdin"`
	OpenStderr    bool              `json:"OpenStderr"`
	OpenStdout    bool              `json:"OpenStdout"`
	CanRemove     bool              `json:"CanRemove"`
	DetachKeys    string            `json:"DetachKeys"`
	Pid           int               `json:"Pid"`
}

// ExecProcessConfig holds the command run via exec.
type ExecProcessConfig struct {
	Tty        bool     `json:"tty"`
	Entrypoint string   `json:"entrypoint"`
	Arguments  []string `json:"arguments"`
}

// --- System ---

// VersionResponse is the body of GET /version. Modern Docker clients
// (including Portainer's engine info panel) read Components for a per-engine
// breakdown; without it the panel shows just a single line.
type VersionResponse struct {
	Platform      map[string]string  `json:"Platform,omitempty"`
	Components    []VersionComponent `json:"Components,omitempty"`
	Version       string             `json:"Version"`
	APIVersion    string             `json:"ApiVersion"`
	MinAPIVersion string             `json:"MinAPIVersion"`
	GitCommit     string             `json:"GitCommit"`
	GoVersion     string             `json:"GoVersion"`
	Os            string             `json:"Os"`
	Arch          string             `json:"Arch"`
	KernelVersion string             `json:"KernelVersion"`
	Experimental  bool               `json:"Experimental"`
	BuildTime     string             `json:"BuildTime"`
}

// VersionComponent is one engine-component entry in /version.
type VersionComponent struct {
	Name    string            `json:"Name"`
	Version string            `json:"Version"`
	Details map[string]string `json:"Details,omitempty"`
}

// InfoResponse is a trimmed body for GET /info. It includes the fields
// Portainer and other Docker UIs probe when deciding whether the engine is
// healthy enough to display.
type InfoResponse struct {
	ID                 string            `json:"ID"`
	Containers         int               `json:"Containers"`
	ContainersRunning  int               `json:"ContainersRunning"`
	ContainersPaused   int               `json:"ContainersPaused"`
	ContainersStopped  int               `json:"ContainersStopped"`
	Images             int               `json:"Images"`
	Driver             string            `json:"Driver"`
	MemoryLimit        bool              `json:"MemoryLimit"`
	SwapLimit          bool              `json:"SwapLimit"`
	KernelVersion      string            `json:"KernelVersion"`
	OperatingSystem    string            `json:"OperatingSystem"`
	OSVersion          string            `json:"OSVersion"`
	OSType             string            `json:"OSType"`
	Architecture       string            `json:"Architecture"`
	NCPU               int               `json:"NCPU"`
	MemTotal           int64             `json:"MemTotal"`
	DockerRootDir      string            `json:"DockerRootDir"`
	ServerVersion      string            `json:"ServerVersion"`
	Name               string            `json:"Name"`
	IndexServerAddress string            `json:"IndexServerAddress"`
	RegistryConfig     RegistryConfig    `json:"RegistryConfig"`
	Swarm              SwarmInfo         `json:"Swarm"`
	Plugins            PluginsInfo       `json:"Plugins"`
	DefaultRuntime     string            `json:"DefaultRuntime"`
	Runtimes           map[string]any    `json:"Runtimes"`
	LiveRestoreEnabled bool              `json:"LiveRestoreEnabled"`
	Isolation          string            `json:"Isolation"`
	CgroupDriver       string            `json:"CgroupDriver"`
	CgroupVersion      string            `json:"CgroupVersion"`
	SystemTime         string            `json:"SystemTime"`
	Labels             []string          `json:"Labels"`
	ExperimentalBuild  bool              `json:"ExperimentalBuild"`
	HTTPProxy          string            `json:"HttpProxy"`
	HTTPSProxy         string            `json:"HttpsProxy"`
	NoProxy            string            `json:"NoProxy"`
	SecurityOptions    []string          `json:"SecurityOptions"`
	Warnings           []string          `json:"Warnings"`
	ClientInfo         map[string]string `json:"ClientInfo,omitempty"`
}

// RegistryConfig is the nested RegistryConfig object in /info.
type RegistryConfig struct {
	AllowNondistributableArtifactsCIDRs     []string       `json:"AllowNondistributableArtifactsCIDRs"`
	AllowNondistributableArtifactsHostnames []string       `json:"AllowNondistributableArtifactsHostnames"`
	InsecureRegistryCIDRs                   []string       `json:"InsecureRegistryCIDRs"`
	IndexConfigs                            map[string]any `json:"IndexConfigs"`
	Mirrors                                 []string       `json:"Mirrors"`
}

// SwarmInfo is the nested Swarm object in /info. We always report inactive.
type SwarmInfo struct {
	NodeID           string   `json:"NodeID"`
	NodeAddr         string   `json:"NodeAddr"`
	LocalNodeState   string   `json:"LocalNodeState"`
	ControlAvailable bool     `json:"ControlAvailable"`
	Error            string   `json:"Error"`
	RemoteManagers   []string `json:"RemoteManagers"`
}

// PluginsInfo is the nested Plugins object in /info. Portainer reads the
// volume/network lists to populate dropdowns.
type PluginsInfo struct {
	Volume        []string `json:"Volume"`
	Network       []string `json:"Network"`
	Authorization []string `json:"Authorization"`
	Log           []string `json:"Log"`
}

// ErrorResponse is the standard Docker API error body.
type ErrorResponse struct {
	Message string `json:"message"`
}

// execRecord tracks an in-flight or completed exec instance.
// Pty is set for Tty=true execs while the stream is active so resize
// requests can forward TIOCSWINSZ to the live terminal.
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
	Pty         *os.File
}
