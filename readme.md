# docker-lxc-daemon

A Docker-compatible API that backs containers with LXC (optionally Proxmox CTs).
Drop-in replacement for `/var/run/docker.sock` — use `docker`, `docker-compose`,
or any Docker SDK without modification.

## Build

Requires Go 1.21+, `liblxc-dev`, `pkg-config`.

```sh
make build        # -> bin/docker-lxc-daemon
sudo make install # -> /usr/local/bin + systemd unit
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
