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
default route.

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
