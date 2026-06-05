# docker-lxc-daemon

A Docker-compatible API that backs containers with LXC (optionally Proxmox CTs).
Drop-in replacement for `/var/run/docker.sock` — use `docker`, `docker-compose`,
or any Docker SDK without modification.

## Install

The supported install is the prebuilt **`.deb`** — it depends only on the
*runtime* LXC command-line tools (`lxc-pve | lxc`, `nftables`), never a build
toolchain, so a host already running LXC/Proxmox has everything it needs:

```sh
# grab the .deb from the latest CI run / release, then:
sudo apt install ./docker-lxc-daemon_*.deb
sudo systemctl enable --now docker-lxc-daemon
```

The package installs the binary to `/usr/bin`, ships the systemd unit, creates
the `docker` group, and `Conflicts:` the real Docker packages — so it cleanly
replaces `docker.io` / `docker-ce` on the same socket.

### Build from source (maintainers / CI only)

The daemon is pure Go — building needs only Go 1.21+ (no cgo, no liblxc-dev);
containers are driven through the LXC command-line tools at runtime:

```sh
make build        # -> bin/docker-lxc-daemon
make deb          # -> bin/docker-lxc-daemon_<ver>_<arch>.deb  (what users install)
sudo make install # dev convenience: -> /usr/local/bin + systemd unit
```

## Testing

Run all checks:

```sh
make test
```

Individual targets:

```sh
make test-build       # compile check (`go test -run '^$' ./...`)
make test-unit        # unit tests
make test-integration # integration tests (`go test -tags=integration ./...`)
```

## Run

```sh
sudo systemctl enable --now docker-lxc-daemon
```

Or directly:

```sh
sudo docker-lxc-daemon --socket=/var/run/docker.sock
```

Useful flags:

| Flag | Purpose |
| --- | --- |
| `--socket` | Unix socket path (default `/run/docker-lxc-daemon/docker.sock`) |
| `--lxcpath` | LXC container storage (default `/var/lib/lxc`) |
| `--pve-storage` | Proxmox storage name — enables Proxmox CT mode |
| `--min-free-disk-gb` | Low-space guardrail (default 2): refuse creates onto, and warn when, a watched filesystem drops below this. `0` disables |
| `--cache-path` | Directory for bulky regenerable data (image tarballs, OCI unpacks, named volumes, build cache). Empty auto-defaults onto a ZFS `--pve-storage` pool, else the state dir — set it to keep bulk off the host root |
| `--default-memory` | RAM for PVE CTs without `--memory` (e.g. `16G`, `32G`; empty = host RAM / no cap). Per-container override via the `dld.memory` label |
| `--lan-bridge` / `--lan-prefix` / `--lan-gateway` / `--lan-subnet` | Dual-NIC LAN settings |

## Use

```sh
docker run -d --name web -p 8080:80 ubuntu:22.04
docker ps
docker exec -it web bash
```

Image refs map to LXC templates: `ubuntu:22.04`, `debian:bookworm`, `alpine:3.19`.
Arbitrary OCI images are pulled via `skopeo` + `umoci`.

See [deepdive.md](./deepdive.md) for the why and how.
