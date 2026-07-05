# Wolf on Proxmox (LXC2Docker)

Run [Games-on-Whales Wolf](https://github.com/games-on-whales/wolf) — stream
virtual desktops and applications to Moonlight clients — on a Proxmox VE host,
backed by `docker-lxc-daemon`. This is the Proxmox counterpart of the SmoothNAS
Wolf plugin: the same container shape, expressed as a Compose file against the
host daemon, with the Wolf Den management UI bundled on `:8080`.

## How it works

`docker-lxc-daemon` on the PVE host serves the Docker Engine API but backs every
container with an LXC / Proxmox CT. Wolf runs as one such CT with the host socket
bind-mounted in, so each session it starts (a desktop, an app, or a game) is
launched as a **sibling Proxmox CT** through the same daemon — not a nested
container.

The Compose file reproduces the SmoothNAS `wolf-runtime` + `gpu-*` profiles:

| SmoothNAS plugin | Here (Compose → LXC2Docker) |
| --- | --- |
| `wolf-runtime` socket mount | `-v /var/run/docker.sock:/var/run/docker.sock` |
| `wolf-runtime` devices/caps | `devices:` + `cap_add:` + `device_cgroup_rules:` |
| `gpu-nvidia` profile | `docker-compose.nvidia.yml` (`NVIDIA_VISIBLE_DEVICES` → CDI) |
| `gpu-intel` / `gpu-amd` | base file (`/dev/dri`) |
| tier-bound `state` volume | `-v /etc/wolf:/etc/wolf` |
| identity-mapped runtime vol | `-v ${WOLF_RUNTIME_DIR}:${WOLF_RUNTIME_DIR}` |
| bundled Wolf Den (`:8080`) | `Dockerfile` (Wolf + Wolf Den wrapper entrypoint) |
| `hostExpose` Moonlight ports | `gow.lan` static IP on the CT |
| SmoothNAS LXC runtime | `docker-lxc-daemon` on the PVE host |
| _(new for Proxmox)_ | `gow.pve=true` → sessions are Proxmox CTs |

## Prerequisites

1. **docker-lxc-daemon on the PVE host**:
   ```sh
   bash -c "$(curl -fsSL https://raw.githubusercontent.com/games-on-whales/LXC2Docker/main/scripts/install-on-pve.sh)"
   systemctl enable --now docker-lxc-daemon
   ```
   Configure its `--lan-*` bridge so CTs can take a static LAN IP.
2. **GPU drivers loaded on the host** (Intel/AMD `/dev/dri`, or NVIDIA with
   `nvidia-drm modeset=1`). Confirm with `ls /dev/dri` / `nvidia-smi`.
3. **Virtual-input udev rules** on the host:
   ```sh
   install -m 0644 85-wolf-virtual-inputs.rules /etc/udev/rules.d/
   modprobe uinput
   udevadm control --reload-rules && udevadm trigger
   ```
4. **A free static LAN IP** for the Wolf CT (Moonlight discovery relies on
   mDNS/multicast reaching the LAN).

## Bring up

```sh
cp .env.example .env      # set WOLF_LAN_IP, DLD_STORAGE, WOLF_RENDER_NODE, ...
docker compose build      # build the Wolf + Wolf Den image (or pull WOLF_IMAGE)
docker compose up -d                                             # Intel / AMD
docker compose -f docker-compose.yml -f docker-compose.nvidia.yml up -d   # NVIDIA
```

- **Wolf Den UI:** `http://<WOLF_LAN_IP>:8080`
- **Pairing:** point Moonlight at `<WOLF_LAN_IP>`, then open
  `http://<WOLF_LAN_IP>:47989` to enter the PIN and pick a desktop/app.

## Ports (Moonlight)

`47984/tcp` `47989/tcp` `47999/udp` `48010/tcp` `48100/udp` `48200/udp` (+ Wolf
Den `8080/tcp`). With the `gow.lan` static IP these are reachable directly on the
CT. On segmented LANs you may need a multicast querier on the bridge for
discovery.

## Publishing the image

`docker compose build` builds locally. To publish a shared
`ghcr.io/games-on-whales/wolf-proxmox` image, run the
`.github/workflows/wolf-proxmox-image.yml` workflow (manual dispatch), then set
`WOLF_IMAGE` to the pinned tag/digest.

## Configuration

On first start the bundled `startup-app.sh` seeds `/etc/wolf/config.toml` with a
fresh host uuid and the default app profile set (`config/default-config.toml` —
Wolf UI, desktops, Steam, RetroArch, …), pins the GOW app images to
`WOLF_IMAGE_TAG`, and installs `fake-udev`. Edit `/etc/wolf/config.toml`
afterwards (or use Wolf Den) to customise apps; it is preserved across restarts.

## Status

Draft mirroring the production SmoothNAS plugin — pending verification on a live
PVE host with a GPU (the GPU/CDI path and the Docker-API options Wolf uses to
create session CTs).
