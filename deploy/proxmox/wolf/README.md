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
5. **Bind-mount source dirs on the host** — Wolf's `/etc/wolf` (state) and
   `/run/wolf` (runtime) are bind-mounted from the host, so they must exist
   before it starts (`/run` is tmpfs, so recreate `/run/wolf` on boot):
   ```sh
   install -m 0644 tmpfiles.d/wolf-proxmox.conf /etc/tmpfiles.d/
   systemd-tmpfiles --create /etc/tmpfiles.d/wolf-proxmox.conf
   ```

## Bring up

```sh
cp .env.example .env      # set WOLF_LAN_IP, DLD_STORAGE, WOLF_RENDER_NODE, ...
# The image is published by the CI workflow; pull it (or `docker compose build`).
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

## Controllers

Wolf builds virtual gamepads on the host via `uinput` and injects them into each
session, so controllers work in the Wolf UI, Steam Big Picture, and games.

**Known caveat — Steam Input:** for games launched through Steam with *Steam
Input* enabled, the controller may not register in-game. Steam Input grabs the
physical pad and builds its own virtual gamepad via `/dev/uinput` *inside the
session container*, which isn't passed through to sessions yet (only Wolf itself
gets `uinput`). Until that lands, **disable Steam Input** so the game reads
Wolf's pad directly:

- Per game: *game → ⚙ → Controller → Override for … → Disable Steam Input*, or
- Globally: *Steam → Settings → Controller → turn off the desktop/BPM toggles*.

Menus (Wolf UI, Big Picture) are unaffected — they read the pad over evdev.

## Status

Draft mirroring the production SmoothNAS plugin — pending verification on a live
PVE host with a GPU (the GPU/CDI path and the Docker-API options Wolf uses to
create session CTs).
