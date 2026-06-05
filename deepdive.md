# Deep Dive

## Why

Games on Whales needs Docker ergonomics (images, `docker run`, compose files)
but LXC semantics (full-system containers, systemd inside, GPU/input device
passthrough, Proxmox integration). Running dockerd inside an LXC host is
fragile — nested cgroups, apparmor, and overlayfs pain. Instead of bending
Docker to LXC, we speak the Docker API on top of LXC directly.

The result: any Docker client works unmodified, but the runtime is LXC, so
containers are first-class on the host (visible in `lxc-ls`, in the Proxmox
UI when `--pve-storage` is set, and with real init).

## Architecture

```
 docker CLI / compose / SDK
            │  HTTP over unix socket
            ▼
   /var/run/docker.sock  ──►  cmd/docker-lxc-daemon
                                 │
                                 ├── internal/api     Docker Engine API router
                                 ├── internal/lxc     go-lxc + pct wrapper
                                 ├── internal/image   ref → LXC template
                                 ├── internal/oci     skopeo+umoci pull
                                 └── internal/store   JSON-on-disk metadata
```

### API layer (`internal/api`)

Implements the Docker Engine API subset that real clients actually hit:
containers, images, exec, logs, archive, events, networks (stubbed),
`/_ping`, `/version`, `/info`. Version-prefixed (`/v1.43/...`) and bare
paths are both routed. No TLS — unix socket only, `chown root:docker`
matches the real daemon's group convention.

### LXC manager (`internal/lxc`)

Two modes:

1. **Legacy** — raw `lxc-*` and go-lxc against `--lxcpath`.
2. **Proxmox CT** — `pct create/start/...` against a named PVE storage, so
   containers appear in the Proxmox UI with correct ZFS/LVM rootfs layout.

One LXC container == one Docker container. The LXC name doubles as the Docker
container ID. On startup, `reconcile()` walks the store, drops orphans whose
LXC dir is gone, and re-applies nftables port forwards for running
containers (nft state doesn't survive reboots).

A background GC sweeps stopped ephemeral (`--rm`) containers.

### Image resolution (`internal/image`)

Docker refs map to three kinds:

- **Distro** (`ubuntu:22.04`, `debian:bookworm`, `alpine:3.19`): resolved
  straight to LXC download-template args (`ubuntu/jammy/amd64`).
- **App** (distro + package overlay): base distro is pulled as a template
  container, cloned, packages installed.
- **OCI** (anything else): pulled via `skopeo copy` into an OCI layout,
  flattened with `umoci unpack`, rootfs imported into an LXC container.

Templates are cached as `__template_<distro>_<tag>` containers and cloned
for new instances — cheap and avoids re-downloading.

### Networking

A managed bridge is created on startup. Each container gets a veth on this
bridge; port publishes (`-p`) become nftables DNAT rules to the container
IP. Optional dual-NIC mode attaches a second interface to a physical LAN
bridge with a deterministic IP (`<prefix>.<vmid>`), making mDNS and
Moonlight discovery work on the LAN — the LAN NIC is `net.0` so it's the
default route. A Proxmox CT can't share the host's network namespace, so
`--network=host` can't be honored literally; when a LAN bridge is configured
(`--bridge`), host-mode containers (e.g. Wolf) are given this dual-NIC LAN
setup instead of an empty, unreachable namespace (issue #53).

### Disk sizing & fast cloning (PVE)

A container's rootfs size defaults to an image-derived estimate
(`tarballRootfsGB`: compressed-tarball-GB × 3 + 4, floored at 4G). That can be
too tight for apps that write a lot into the rootfs (e.g. Steam game/shader
data), so the size is configurable two ways, label winning over storage-opt:

- `docker run --storage-opt size=64G …`  (`HostConfig.StorageOpt["size"]`)
- the `dld.disksize` label (e.g. `dld.disksize=64G`, or bare `64` = GB)

`ParseDiskSizeGB` accepts a bare number (GB) or a K/M/G/T[i][B] suffix. The
request is floored at the image minimum so the unpacked rootfs always fits.

On **ZFS** storage, ephemeral containers (everything that isn't a UI-visible
`gow.pve` CT) skip per-launch tarball extraction entirely: the image tarball is
unpacked once into a cached template dataset (`<pool>/dld-tmpl-<image>` with an
`@base` snapshot) and each container is a copy-on-write `zfs clone` of it —
near-instant, which removes the bulk of Wolf's app-launch latency. The clone
inherits the pool's free space (thin) unless `DiskSizeGB` is set, in which case
it becomes a `refquota` cap. Any failure on this path falls back to the
storage-agnostic `pct create`, so non-ZFS storage and error cases still work.
`RemoveImage` drops the template dataset once no clones depend on it.

### Store (`internal/store`)

JSON under `--statepath` (default `/var/lib/docker-lxc-daemon`). Holds
container records (labels, env, port bindings, ephemeral flag, IP) and
image records. LXC is the source of truth for container existence; the
store is for Docker-semantic metadata that LXC doesn't track.

### Exec

`docker exec` is implemented via `lxc-attach`. Exec instances live in an
in-memory table pruned every 60s; the router's `execStart` streams stdio
back over the hijacked HTTP connection using the Docker frame protocol.

## Extending

- New distro? Add an entry to `knownDistros` in `internal/image/distro.go`.
- New API route? Register in `internal/api/router.go` and add the handler.
- New LXC config knob? `internal/lxc/config.go` builds the container config
  from the Docker create payload.
