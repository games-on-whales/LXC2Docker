# Proposal: Windows support — WSL2 + Hyper-V with a Docker-Desktop-style install

Status: **draft (rev 4, all-in, two roundtable passes)** · Owner: TBD · Target: `docker-lxc-daemon` (LXC2Docker)

> **Rev 4** is the *all-in* scope — this one document commits to **both**
> backends (WSL2 **and** Hyper-V) **and** the Docker-Desktop-style installer +
> tray, with full design for each — and folds in **two** roundtable review
> passes (pass 1: five seats on the whole draft; pass 2: three seats on the new
> Hyper-V + installer designs). Delivery is still **sequenced** (WSL2 ships first
> to de-risk the bridge) and now every phase past 0 is **spike-gated** the same
> way Phase 1 is. Streaming / display / input passthrough remains a **non-goal**
> (§15). Review log: §16.
>
> **Fixes that landed in rev 4** (all blocking/major from pass 2): virtiofs is
> impossible on Hyper-V → replaced with SMB/CIFS (§4.6, §8); the installer must
> **not** touch the shared `~/.docker/config.json` → per-app `DOCKER_CONFIG`
> isolation (§7); Authenticode signatures need **RFC3161 timestamping** + HSM/
> cloud signing (§11); NoCloud needs the `cidata` label + Secure-Boot-off Gen2
> (§6); GPU-P needs Win11 22H2 + manual driver-store copy (§6, §8); external
> vSwitch is wired-only + needs macvlan for container multicast (§8); §14
> criteria are now phase-annotated; Phases 2/3 are spike-gated (§9); delta-VHDX
> and full appliance reproducibility are demoted to spike-gated aspirations
> (§7, §11, §12).

---

## 1. Summary

Run the Linux `docker-lxc-daemon` inside a managed Linux VM on Windows and bridge
the Windows Docker socket into it — the Docker Desktop model — across **two
committed backends**:

- **WSL2** (default; works on Windows Home): a registered WSL distro carrying the
  daemon + LXC userland; the fast, low-friction path for dev/OCI workloads.
- **Hyper-V** (full-fidelity; Pro/Enterprise/Education): a curated Debian
  appliance VM with a **ZFS data disk**, so the copy-on-write fast-clone path and
  (optional PVE variant) `pct`-style CTs work for real; GPU partitioning and
  LAN-bridged networking are available here.

Both sit behind one **signed installer + system-tray control app** that mirrors
Docker Desktop: click through, get `docker` + `docker compose` + `docker build`
on the Windows command line via a **dedicated, isolated CLI config** (not the
user's shared `~/.docker`), a tray to start/stop/switch backends and manage
settings, and clean coexistence with an existing Docker Desktop install.

The daemon is Linux-only by nature (it speaks the Docker Engine API on top of
**LXC** and orchestrates ~20 host tools — `lxc-*`, `pct`, `pvesm`, `zfs`, `nft`,
`skopeo`, `umoci`, `ip`, `nsenter`, …). So "Windows support" = engine VM + socket
bridge + Windows control plane, exactly like Docker Desktop. The daemon stays
**generic** (no downstream-product specifics baked in). **Non-goals** (§15):
Windows containers (WCOW); game-streaming / display / audio / input passthrough;
Kubernetes; arm64 Hyper-V.

## 2. Motivation

- Today the only supported install is a Debian `.deb` on an LXC/Proxmox host;
  Windows users can't run the daemon at all.
- Docker Desktop set the bar for "Docker on Windows": one signed installer, a
  tray icon, `docker` on PATH, WSL2 integration, a Hyper-V option. Matching that
  end-to-end is what makes the daemon usable by anyone on Windows.
- The two backends serve genuinely different users: WSL2 for dev laptops /
  Windows Home; Hyper-V for the full-fidelity engine (ZFS CoW clones, `pct`,
  GPU-P, LAN bridging) that the WSL2 kernel structurally cannot provide.

## 3. Background: how the daemon binds today (constraints)

From `cmd/docker-lxc-daemon/main.go` and the systemd unit:

- Listens on a **unix socket** (`--socket`), `chmod 0666`, and unconditionally
  symlinks `/var/run/docker.sock` (`main.go:104`). **No TLS, no auth, no TCP.**
- **Must run as root** (`os.Geteuid() != 0` → fatal, `main.go:53`).
- On startup `reconcile()` creates a managed bridge and re-applies nftables DNAT
  for published ports.
- Pure Go, no cgo — cross-compiles cleanly to `linux/{amd64,arm64}`. But the
  binary is half the runtime: it **shells out to ~20 external tools** and expects
  LXC (plus ZFS + PVE for the fast paths). `github.com/Microsoft/go-winio` is
  already an indirect dep; the Windows binaries promote it to direct.

**Honest implication:** the *binary* ports unchanged; the *runtime userland* must
be rebuilt inside each engine VM. **`pct`/`pvesm`/`zfs` cannot exist in a stock
WSL2 distro** — ZFS/Proxmox are structurally absent on WSL2 and only real on the
Hyper-V appliance. The daemon's unconditional `/var/run/docker.sock` symlink and
root-hardfail are fine inside a VM but must be guarded from leaking into a user's
other WSL distros (§7 coexistence).

---

## 4. Shared architecture (both backends)

```
  Windows host (NT)                         │  Linux engine VM (WSL2 distro | Hyper-V appliance)
                                            │
  docker.exe / compose / buildx             │
  (bundled; own DOCKER_CONFIG, context lxc) │
        │  npipe \\.\pipe\docker_lxc_engine  │
        ▼                                    │
  ┌──────────────────────────┐               │  ┌───────────────────────────┐
  │ docker-lxc-desktop.exe    │   transport   │  │ docker-lxc-daemon (Linux) │
  │ • tray + control service  │   per backend │  │  unix: docker.sock (root) │
  │ • npipe listener (SDDL)   │──────────────►│  │  LXC / nftables           │
  │ • published-port forwarder│  WSL: stdio    │  │  (+ ZFS on Hyper-V)       │
  │ • Backend lifecycle       │  HV : hvsock   │  │  + in-VM relay shim       │
  └──────────────────────────┘  (token-auth'd)│  └───────────────────────────┘
```

### 4.1 The socket bridge — corrected transports

Windows Docker clients connect to a **product-unique** pipe
`\\.\pipe\docker_lxc_engine` (**not** `docker_engine` — that is Docker Desktop's
own pipe; §7 coexistence). A Windows-side listener (`Microsoft/go-winio`) accepts
npipe connections and forwards the byte stream to the daemon's unix socket:

- **WSL2 — default: `wsl.exe -d <distro> exec <relay>` stdio relay.** Spawns a
  tiny in-distro relay that `connect()`s the unix socket and pipes it over
  stdin/stdout. No TCP, no ACL gap. *(The rev-1 "AF_UNIX over
  `\\wsl.localhost\<distro>\...`" option is **deleted** — that UNC path is 9P
  file access and cannot `connect()` a Linux AF_UNIX socket.)*
- **WSL2 — fallback: in-VM `127.0.0.1:<port>`** via WSL2's `localhostForwarding`
  relay (**TCP-only**). A loopback port has **no per-user ACL**, so it is gated
  behind a per-session token handshake (§4.5). Used only if the stdio relay is
  unavailable.
- **Hyper-V — `hvsock`/`AF_VSOCK`.** A registered guest-communication service
  GUID (`{PORT:00000000}-facb-11e6-bd58-64006a7986d3`, registered host-side under
  `HKLM\...\Virtualization\GuestCommunicationServices`) gives a hostless channel;
  the appliance kernel is built with `CONFIG_VSOCKETS` + `CONFIG_HYPERV_VSOCKETS`
  (`hv_sock`). **Peer identity note:** from the guest, *every* host process
  presents `VMADDR_CID_HOST (2)`, so the CID cannot authenticate which host
  user/process connected — the in-VM shim applies the **same per-session token
  handshake** as the loopback path, not CID filtering (§4.5).

**Half-close is a first-class hazard.** Windows named pipes have **no native
`shutdown(SHUT_WR)`**; the hijack half-close that `exec -it` / `attach` /
`logs -f` rely on must be *emulated* — map npipe `CloseWrite`/`FlushFileBuffers`
to a unix-side `SHUT_WR` and propagate EOF both directions. §10's conformance
suite targets this exact mapping.

### 4.2 Backend abstraction

```
type Backend interface {
    EnsureInstalled(ctx) error   // feature enablement, VM/distro registration
    Start(ctx) error             // boot, wait for daemon healthy
    Stop(ctx) error
    Status(ctx) (State, error)
    DialEngine(ctx) (net.Conn, error)   // a conn to the unix socket
    Capabilities() CapSet               // drives §8
    ResourceLimits() LimitCaps          // what CPU/RAM/disk knobs are honorable
}
```

`WSLBackend` and `HyperVBackend` both implement it. The bridge, tray, and
port-forwarder are backend-agnostic; only lifecycle, `DialEngine`, and the
resource-limit surface differ. `Capabilities()` drives the matrix (§8) and makes
the tray grey out unsupported features per backend.

### 4.5 Security & trust model (was the weakest area)

The engine is **root-equivalent**: anyone who reaches it can `docker run
--privileged -v C:\:/host …` and own the host. Access control is the design.

- **Named-pipe ACL.** `\\.\pipe\docker_lxc_engine` is created with an explicit
  restrictive SDDL: owner SYSTEM/Administrators, connect granted **only** to a
  dedicated `docker-lxc-users` local group; the listener re-checks the peer SID
  at accept. The installer provisions the group and documents it as
  admin-equivalent.
- **Per-backend boundary, honestly.** **WSL2 is a convenience boundary, not a
  security one** — `\\wsl$`/9P, `/mnt/c` drvfs, loopback mirroring, interop mean
  root-in-WSL2 ≈ the Windows user's context (and, via 9P, their files).
  **Hyper-V is a genuine hardware-virt boundary.** Docs state this; the two are
  not equivalent.
- **Channel auth.** Neither the hvsock GUID nor the loopback port is a secret,
  and (per §4.1) the hvsock CID cannot identify the peer; the in-VM shim
  therefore requires a **per-session token** minted at bridge start (over an
  authenticated local handshake) on both the loopback and hvsock paths.
- **Host-path stance.** WSL2 auto-bridges `/mnt/c`, so a container could
  bind-mount Windows files. Host-path bind mounts are **allowlist-gated, opt-in**
  (§4.6); `C:\` is never auto-exposed to privileged containers; privileged
  capabilities default off on WSL2.
- **Supply chain + updates.** All artifacts (rootfs, VHDX, bundled Docker CLI,
  appliance kernel + out-of-tree ZFS module) are **digest-pinned, checksummed,
  and Authenticode/detached-signed with RFC3161 timestamps** (§11), shipped with
  SBOM + provenance (SLSA-style); **reproducible builds are a goal, not a claimed
  property** of the whole appliance. The updater verifies signature + timestamp +
  checksum over TLS **before** swapping the in-VM binary; rollback protection
  included. **Signing is a release gate, not optional** (§11).

### 4.6 Windows filesystem sharing & path translation (table-stakes)

`docker run -v C:\Users\me\src:/src` and Compose `volumes:` are the #1 dev-loop
feature:

- **CLI-side path translation:** rewrite `C:\Users\me\src` → the engine's Linux
  mount path before the create call.
- **WSL2:** ride the auto-mounted `/mnt/c` drvfs; document the real limits (slower,
  broken `inotify`, permission/`chmod` mangling). Paths outside shared drives
  fail with an explicit documented error, never a silent empty mount.
- **Hyper-V — corrected transport.** **virtiofs is not available on Hyper-V**
  (Hyper-V presents VMBus/synthetic devices, not a virtio bus — there is no
  virtio-fs), so host↔guest file sharing uses **SMB/CIFS**: the Windows host
  serves a share and the appliance mounts it over `cifs` (this is exactly what
  Docker's Hyper-V "Shared Drives" did, with the well-known credential prompt +
  permission/`inotify` caveats). A **host-run 9P/FUSE server exposed over
  hvsock** is a possible future alternative but requires shipping our own
  host-side server (Hyper-V, unlike WSL2, provides no built-in Plan 9 transport).
  The File-Sharing pane manages the allowlist for both backends.

### 4.7 Published-port reachability

Docker Desktop makes `-p 8080:80` reachable at `localhost:8080` from Windows on
**all** editions via a **host-side userland forwarder**, not via WSL mirrored
networking. The tray/control process **watches the engine API for published
ports and opens matching Windows loopback listeners** that forward over the same
transport. Default on both backends; Win11 22H2+ mirrored networking is an
*optimization*, not a requirement. (Default WSL2 NAT is **TCP-only**, so
UDP/multicast can't traverse — a capability limit, §8.)

### 4.8 Build (BuildKit / buildx)

The daemon already vendors `moby/buildkit`; both images ship buildx so `docker
build` / `docker buildx build` work over the bridge. §10 adds a build smoke test.

---

## 5. Backend A — WSL2 (full design)

- **Image:** a rootfs tarball registered via `wsl --import docker-lxc-engine
  <dir> <rootfs.tar> --version 2`. Contents: `linux/amd64` daemon, full `lxc`
  runtime, `skopeo`/`umoci`, `nftables`, buildx, `/etc/wsl.conf` with
  `systemd=true`, a systemd unit starting the daemon + relay. Built from a
  digest-pinned Debian base (reproducibility a goal, §12).
- **Lifecycle:** `EnsureInstalled` enables `Microsoft-Windows-Subsystem-Linux` +
  `VirtualMachinePlatform` (via the installer's elevated phase, §7), imports the
  distro; `Start` runs `wsl -d docker-lxc-engine` and waits for the daemon health
  endpoint; `Stop` = `wsl --terminate`.
- **Transport:** stdio relay (default) / loopback-TCP + token (fallback), §4.1.
- **State:** containers/images/volumes in the distro's `ext4.vhdx` (grows, never
  auto-shrinks → reclaim tooling in the tray, §7).
- **Resource limits:** CPU/RAM come from the **global** `.wslconfig` (applies to
  ALL the user's distros — including an installed Docker Desktop's — and needs
  `wsl --shutdown` to apply). The tray surfaces this honestly and points users to
  Hyper-V for true per-engine limits.
- **Kernel constraints (Phase 0 spike gate, §9):** stock WSL2 kernel must provide
  cgroup v2 unified-hierarchy delegation (needs `systemd=true` + a recent WSL app)
  and `CONFIG_NF_TABLES` (older WSL kernels were iptables-only); ZFS is
  structurally absent. No `pct`. Dir-mode LXC only.

## 6. Backend B — Hyper-V (full design; direction proposed, ratified by the Phase-3a spike §9)

- **VM management model — proposed direction.** Use the Hyper-V **WMI provider**
  (`root\virtualization\v2`) / PowerShell Hyper-V module to create and drive a
  **normal, inspectable Gen2 VM**, *not* HCS/`hcsshim`. Rationale: HCS manages
  hidden/ephemeral utility VMs and is under-documented and awkward for a full
  appliance that needs a ZFS data disk, an external vSwitch, and GPU
  partitioning; a WMI-managed VM is debuggable (visible in Hyper-V Manager),
  supports DDA/GPU-P and multiple vNICs cleanly, and is well-documented.
  (Historical note: the *WMI-managed, inspectable* VM was old Docker-for-Windows'
  **MobyLinuxVM**; the *HCS-hidden* LinuxKit VM was the later **DockerDesktopVM**
  — we are choosing the MobyLinuxVM-style model.)
- **Gen2 + Secure Boot.** The VM is Gen2 (UEFI). Because we ship a **custom
  appliance kernel** *and* an **out-of-tree OpenZFS module**, both of which
  Secure Boot's default "Microsoft Windows" template would refuse/blocks under
  kernel lockdown, the appliance VM is provisioned **`-SecureBootEnabled Off`**.
  (This is fully in our control since the installer creates the VM, and it
  simultaneously unblocks the custom kernel, the Linux loader, and unsigned
  module loading. The alternative — MOK enrollment — needs an interactive
  MokManager prompt we can't drive from an automated first boot.)
- **Appliance image:** a prebuilt, signed **VHDX** — Debian + `lxc` +
  **OpenZFS** (prebuilt module against the pinned appliance kernel, à la Proxmox)
  + `nftables` + buildx + the daemon; a **second, fixed-size data VHDX formatted
  ZFS** carries the CoW template datasets (`dld-tmpl-*`) and container rootfs
  (fixed, not dynamic, to avoid stacking ZFS CoW on NTFS-hosted dynamic-VHDX CoW;
  guest disk write-cache disabled for integrity). An optional **PVE-based
  appliance variant** additionally provides `pct` mode; the default appliance is
  plain Debian (keeps the daemon generic).
- **First boot — NoCloud datasource.** Hyper-V has no standard cloud-init
  datasource, so the installer attaches a small config VHD **labeled `cidata`**
  (FAT/ISO with `user-data`/`meta-data` — the label is mandatory or NoCloud
  silently no-ops) carrying hostname, the hvsock service GUID, data-disk import,
  and initial daemon flags. Standard, offline, no network.
- **Transport:** hvsock/AF_VSOCK with in-VM per-session-token auth (§4.1, §4.5).
- **Lifecycle:** `EnsureInstalled` enables `Microsoft-Hyper-V` +
  `HypervisorPlatform` (installer elevated phase), copies the VHDX, registers the
  VM (Secure-Boot-off) + data disk + hvsock GUID; `Start`/`Stop` via WMI; health
  via the bridge.
- **Networking:** default internal NAT switch + the host-side port forwarder
  (§4.7). Optionally an **external vSwitch** bridged to the physical LAN, with two
  honest limits: it true-L2-bridges only over **wired Ethernet** (Hyper-V cannot
  bridge Wi-Fi — it falls back to an ARP/NAT proxy that breaks multicast, and
  most dev laptops are on Wi-Fi), and putting the *guest* on the LAN does not by
  itself give a *container* LAN reachability — the daemon's `reconcile()` NAT
  bridge sits in front, so container-level LAN/multicast needs a **macvlan**
  attach to the bridged vNIC. Both are tray-gated with these caveats surfaced.
- **GPU — corrected.** Ship **GPU-P (GPU Partitioning)** for **shared** CUDA/
  DirectX only, with two buildability caveats: it requires **Windows 11 22H2+**
  (`Add-VMGpuPartitionAdapter` on general VMs is a 22H2 capability; earlier it was
  confined to Windows Sandbox/WSLg), and GPU-P does **not** auto-provision the
  guest driver — the installer must **copy the host GPU driver-store files into
  the guest and keep them version-matched on every host driver update** (the
  partition surfaces as `/dev/dxg`; CUDA uses the WSL CUDA runtime stack). **DDA
  (Discrete Device Assignment) is a Windows *Server* feature**, unsupported on
  client Hyper-V (community-only), exclusive, and dismounts the GPU from the host
  (single-GPU workstations lose their display) — DDA/display/streaming are a §15
  non-goal.
- **Resource limits:** true per-VM CPU/RAM/disk via WMI (the reason to offer
  Hyper-V for users who need real limits).

---

## 7. Install & control plane (Docker-Desktop parity, full design; spike-gated §9)

- **Coexistence with Docker Desktop is mandatory** (most target users have it),
  and the key insight from review is **do not mutate the user's shared
  `~/.docker`**:
  - **Isolated CLI config.** The bundled Docker CLI runs with its own
    `DOCKER_CONFIG` (e.g. `%LOCALAPPDATA%\DockerLXC\.docker`) holding *its own*
    `config.json`, `contexts/`, and `cli-plugins/` (compose, buildx). This one
    move avoids clobbering Docker Desktop's `credsStore`/`currentContext` and
    plugin dirs. Documented tradeoff: the `lxc` context is not visible to the
    user's *existing* on-PATH `docker` — acceptable, and far cleaner than
    mutating shared state.
  - **Never** write the global `credsStore` or `currentContext` into the shared
    `~/.docker/config.json` (both would break an installed Docker Desktop, whose
    config.json is the same file). Registry auth uses a credential helper
    (`docker-credential-wincred`) configured **only inside the isolated
    `DOCKER_CONFIG`**, via additive per-registry `credHelpers` where sharing is
    unavoidable — not the global key.
  - Distinct pipe (`docker_lxc_engine`), a distinct Windows **service name**, and
    a distinct WSL distro (`docker-lxc-engine`) — none collide with Docker
    Desktop's `com.docker.*` services or `docker-desktop`/`-data` distros. The
    shared `.wslconfig` (§5) is a known shared surface — call it out in-product.
- **Installer** = a **WiX Burn bootstrapper** wrapping per-scope MSIs (MSI custom
  actions can't cleanly do "enable feature → reboot → resume"; Burn exists for
  exactly this prereq/elevation/reboot-resume chain, as Docker Desktop does):
  1. Prereq/edition/virtualization gate. Distinguish **"we can fix"** (enable
     WSL/Hyper-V features via DISM + one managed reboot) from **"you must fix"**
     (virtualization disabled in **BIOS/UEFI** — detect, message, never attempt).
  2. Minimal **elevated** chain: DISM feature enablement, service install, pipe
     `docker-lxc-users` group + SDDL. Everything after runs as the normal user.
  3. Install `docker-lxc-desktop.exe` (tray + control service) + the chosen
     backend image + a **digest-pinned upstream Docker CLI + compose/buildx
     plugins** (Apache-2.0; ship NOTICE/LICENSE) into the isolated `DOCKER_CONFIG`
     tree. Register the WSL distro and/or the Hyper-V VM. Add Start-menu + tray
     entries.
- **Tray/control app** (`docker-lxc-desktop.exe`): start/stop/restart; live
  health; **backend switch (WSL⇄Hyper-V) with a state-destructive warning
  dialog** (state does NOT migrate — §14.3); **resource limits** (Hyper-V
  per-VM; WSL2 with the global-`.wslconfig` caveat surfaced); **File Sharing**
  allowlist; **Proxies** (HTTP/HTTPS/no-proxy — critical in enterprises);
  **DNS**; **WSL-integration** distro list; **daemon.json / engine config** pane
  (registry mirrors, insecure registries, default logging); **disk-image
  relocation** (move `ext4.vhdx` / the data VHDX off C:); **registry sign-in**
  UI; **reclaim disk** (`wsl --manage --set-sparse` / `Optimize-VHD`); **collect
  support bundle** (bridge logs + in-VM `journalctl` + versions); **factory
  reset**; update check + log viewer.
- **Service posture:** the control service is **delayed-auto-start** with
  configured recovery/restart actions; the **VM/distro itself starts on demand**
  (booting WSL2/Hyper-V at every login is expensive and surprising); once the
  pipe SDDL is in place, on-demand engine start is the default so the
  root-equivalent pipe isn't idle-exposed.
- **Updates:** separate **engine-binary** updates (in-place, signed +
  timestamped, verified, rollback-protected) from **appliance/image** updates
  (full signed-image replacement in v1, preserving the separate data VHDX +
  container state; **binary-delta of a whole signed VHDX is a future
  optimization requiring its own spike** — not committed for v1). A dedicated
  updater process performs swaps (an MSI can't cleanly update its own running
  service). **MSIX, if pursued, is UI-only** — it cannot install/run a Windows
  service, run elevated, do DISM, or provision a machine-wide pipe group, so the
  service + elevated setup stay MSI/Burn permanently; `winget` distributes the
  Burn bundle.
- **Uninstall (complete teardown):** `wsl --unregister` the distro; **Remove-VM**
  + **Remove-VMSwitch** (external vSwitch) + delete the VHDX/data-VHDX/NoCloud
  VHD; **unregister the hvsock service GUID**; **remove the `docker-lxc-users`
  group**; **delete the isolated `DOCKER_CONFIG` tree** (nothing to restore in
  the shared config since we never wrote it); remove any `netsh advfirewall`
  rules added for LAN bridging; clean `%LOCALAPPDATA%`. Reverse DISM features
  only on explicit opt-in. "Leaves no residue" is a success criterion (§14.6).

---

## 8. Capability matrix (corrected)

| Capability | Linux host (.deb) | Hyper-V appliance | WSL2 distro |
| --- | --- | --- | --- |
| Docker API, OCI images (skopeo/umoci) | ✅ | ✅ | ✅ |
| `docker build` / buildx | ✅ | ✅ | ✅ |
| LXC containers, exec/logs/attach | ✅ | ✅ | ✅ (dir mode) |
| Host-path bind mounts (`-v C:\…`) | ✅ native | ⚠️ SMB/CIFS (allowlist; cred prompt, perm caveats) | ⚠️ `/mnt/c` drvfs (slow, inotify/perm caveats) |
| `-p` reachable from Windows host | ✅ | ✅ host-side forwarder | ✅ host-side forwarder |
| ZFS CoW fast clone (`dld-tmpl-*`) | ✅ | ✅ (fixed ZFS data VHDX) | ❌ structurally absent (OpenZFS out-of-tree) |
| Proxmox `pct` mode / UI | ✅ | ⚠️ only in the optional PVE appliance variant | ❌ (`pct`/`pvesm` absent) |
| GPU compute (CUDA/DirectX) | ✅ CDI | ⚠️ **GPU-P**, Win11 22H2+, manual driver-store copy | ⚠️ WSL **GPU-PV** (shared, `/dev/dxg`) |
| GPU exclusive passthrough | ✅ | ❌ **DDA is Server-only**; exclusive = host loses GPU → non-goal | ❌ |
| Guest-on-LAN bridging | ✅ | ⚠️ external vSwitch, **wired Ethernet only** (Wi-Fi can't L2-bridge) | ⚠️ NAT (TCP-only) |
| Container LAN multicast / service discovery | ✅ | ⚠️ needs **macvlan** onto the bridged vNIC (wired only) | ❌ (NAT is TCP-only) |
| Display / audio / input passthrough, streaming | (product concern) | ❌ non-goal (§15) | ❌ non-goal (§15) |

## 9. Rollout phases (all committed; each phase past 0 spike-gated)

Everything below is committed; ordering ships WSL2 first because it proves the
bridge before the heavier Hyper-V appliance. **Each phase has a go/no-go spike
gate**, mirroring Phase 0 → Phase 1, so "committed" never means "assumed."

- **Phase 0 — foundations & spike.** Cross-compile daemon for `linux/amd64`;
  define + document the bridge byte protocol and the npipe↔unix half-close
  mapping; `//go:build windows` module + `Backend` interface. **Gate → Phase 1:**
  stock WSL2 kernel runs LXC dir mode (cgroup v2 delegation + `CONFIG_NF_TABLES`
  + veth/bridge/nftables-DNAT + `-p` reachability).
- **Phase 1 — WSL2 MVP (manual install).** Reproducible WSL rootfs; systemd unit;
  standalone `docker-lxc-bridge.exe` (ACL'd pipe + published-port forwarder);
  isolated `DOCKER_CONFIG` + `lxc` context; documented `wsl --import` flow.
  **Milestone:** §14.2 checks pass on Windows Home.
- **Phase 2 — installer + tray (committed; gated by an installer/coexistence
  spike).** Burn bootstrapper, tray/control service, feature enablement, isolated
  CLI + credential-helper wiring, all settings panes, updates, uninstall,
  diagnostics (§7). **Spike gate:** prove clean coexistence with an installed
  Docker Desktop (no shared-config mutation), Burn reboot-resume, and full
  teardown, before committing the MSI surface. **Milestone:** §14.1a.
- **Phase 3 — Hyper-V backend (committed; gated by a Phase-3a spike).**
  - **3a spike/gate:** WMI drive-through of a Gen2 Secure-Boot-off VM, NoCloud
    (`cidata`) first boot, hvsock transport, and GPU-P on a 22H2 host — go/no-go
    before 3b.
  - **3b:** the ZFS/`pct` appliance + fixed data VHDX + external vSwitch (the real
    lift — prebuilt OpenZFS against the pinned kernel, ZFS-in-VHDX, optional PVE
    variant). **Milestone:** §14.3 ZFS CoW clone; §14.1b Hyper-V default on Pro.
- **Phase 4 — GPU compute.** WSL GPU-PV and Hyper-V GPU-P for CUDA/DirectX only.
  Display/audio/input/streaming stay a §15 non-goal.

**Where the real risk lives:** concentrated early — the bridge hijack/half-close,
Windows-path volume translation, `-p` reachability (all Phase 1) — plus Phase
3b's ZFS/PVE appliance. The gates reflect that, not a smooth ramp.

## 10. Testing & CI

- **Per-PR CI (proportionate to today's 4 ubuntu jobs):** one **`GOOS=windows go
  build`** job for the bridge/relay; **bridge conformance tests on the existing
  ubuntu runner** (bidirectional streaming, **half-close/`CloseWrite`**, large
  payloads, hijack semantics via a npipe-interface stand-in → unix socket). Keep
  the linux build/unit/integration/deb jobs unchanged; drop the vestigial
  `liblxc-dev`/`pkg-config` install (daemon is pure Go).
- **Release automation (not per-PR):** WSL-image build/smoke, VHDX build, and
  code signing (signing runs against a **cloud HSM / Azure Trusted Signing** — no
  signing key as a per-PR secret; CA/B rules require keys on FIPS-140 hardware,
  §11).
- **E2E reality check.** Stock `windows-latest` GitHub runners **cannot** boot a
  WSL2 *v2* distro or nested Hyper-V (nested virt disabled; WSL1 only). Real E2E
  (`wsl --import` + `docker run`/`compose up`/bind mount; and the Hyper-V VM boot)
  runs on **self-hosted bare-metal or nested-virt-capable larger/Azure runners**.
  Only host-agnostic bridge conformance is safe on `windows-latest`.
- **Capability guards:** integration tests **skip** (not fail) capability-gated
  features per `Capabilities()` and assert the matrix/docs match.

## 11. Packaging, artifacts & supply chain

Per release, alongside today's `.deb`:

- `docker-lxc-engine-wsl-<ver>.tar` (WSL rootfs).
- `docker-lxc-engine-hyperv-<ver>.vhdx(.zip)` + the fixed ZFS data VHDX template.
- `DockerLXCDesktop-Setup.exe` (Burn bundle, amd64), later MSIX (UI-only) +
  `winget` manifest.

**Signing (release gate, mandatory):**
- Authenticode-sign the installer and **all** shipped PE binaries **and every
  updater-delivered payload**, each with an **RFC3161 timestamp
  countersignature** (`signtool /tr <rfc3161-url> /td sha256 /fd sha256`) — without
  it, already-shipped installers become invalid the day the cert expires.
  Timestamp presence is part of the gate.
- Signing keys live on a **FIPS-140 HSM / cloud signing service** (CA/B Forum
  June-2023 baseline requires this for OV *and* EV). EV accelerates SmartScreen
  reputation but no longer grants instant clearance.
- No kernel-mode Windows driver is shipped (GPU-P, hvsock, and CIFS ride inbox
  Windows drivers); if one were ever added it needs EV + Partner Center
  attestation signing — a separate pipeline. State this so the assumption is
  auditable.
- Inputs digest-pinned; outputs checksummed + signed; SBOM + provenance
  published. **Reproducible builds are a goal**, not an asserted property of the
  full appliance.

## 12. Ongoing maintenance cost (be honest)

The all-in scope ships/updates a Debian WSL rootfs, a Hyper-V VHDX appliance (+
fixed ZFS data VHDX + a **pinned appliance kernel with a prebuilt out-of-tree
OpenZFS module**), a Burn/MSI installer, and a pinned bundled Docker CLI — a
**several-fold** increase in release + CVE-patching surface versus one `.deb`
(dominated by the two kernels and the signing pipeline). The sharp, recurring
hazards:

- **Kernel ↔ OpenZFS patch-lag treadmill.** A *pinned* kernel + *out-of-tree*
  ZFS means every kernel security bump forces an OpenZFS-compat rebuild, and
  OpenZFS lags new kernels by weeks-to-months — so "pin" and "patch promptly" are
  in tension. Policy decision required: either accept a **bounded patch-lag
  window**, or **track a distro kernel + DKMS** (e.g. the Proxmox/Debian kernel)
  instead of pinning a bespoke one. Recommendation: DKMS-against-distro-kernel to
  stay off the treadmill.
- **Signing-cert custody.** The HSM/cloud-signing cert is a standing operational
  + monetary cost with a real **bus-factor**; document custody, renewal, and a
  backup signer.
- **GPU-P driver-store sync.** Every host GPU-driver update requires re-copying
  version-matched driver files into the appliance (§6).

Mitigations baked in: build the WSL rootfs and the appliance from the **same
debootstrap world** where possible; keep the PVE appliance variant optional so
the default stays minimal; automate signing/SBOM/timestamping in release CI.

## 13. Risks & open questions

- **Every phase past 0 is spike-gated** (§9); "committed" is not "assumed."
- **WSL2 is not a security boundary** — trust model, pipe ACL, host-path
  allowlist land *with* Phase 1.
- **Docker Desktop coexistence** rests on isolated `DOCKER_CONFIG` — never
  mutate the shared config (§7).
- **Hyper-V appliance is the heavy lift** — Secure-Boot-off Gen2, OpenZFS vs
  kernel, ZFS-in-VHDX, NoCloud (`cidata`), CIFS sharing, external-vSwitch
  wired-only, GPU-P 22H2 + driver sync; 3b carries the risk.
- **`.wslconfig` is global** (shared with Docker Desktop); **`ext4.vhdx` never
  auto-shrinks** (ship reclaim).
- **arm64** is WSL2-only + experimental at most; **arm64 Hyper-V is a non-goal**.
- **Offline install & OS floor** — DISM/`wsl --update`/CLI fetch may need network;
  define an offline-media path + minimum Windows build (Hyper-V GPU-P needs Win11
  22H2+).
- **Telemetry** — explicit decision (default: none).

## 14. Success criteria (measurable, phase-annotated)

Each criterion lists the **earliest phase** that can satisfy it, so ~half being
Hyper-V-terminal is explicit, not hidden.

| # | Criterion | Gated by |
| --- | --- | --- |
| 14.1a | **One-shot install, WSL2 backend** on clean Win11 22H2+ (Home *and* Pro) with virtualization on: signed installer + defaults → working `docker version` (client **and** server) from a fresh PowerShell in **≤ 8 min, ≤ 1 reboot**, zero manual `wsl`/`dism`/`docker context`. Automated E2E. | Phase 2 |
| 14.1b | **One-shot install, Hyper-V default on Pro** — same budget, Hyper-V backend selected. | Phase 3b |
| 14.2 | **Core workflows** (each exit 0 as CI assertions, WSL2, Win Home 22H2): `docker run --rm hello-world`; `docker run -d -p 8080:80 nginx` then `Invoke-WebRequest http://localhost:8080` → HTTP 200 **from the Windows host**; `docker exec -it <c> sh -c 'echo hi'` → `hi` over a real TTY; `docker logs -f <c>` streams and exits cleanly on Ctrl-C; `docker compose up -d` (2-service) reaches `healthy`; `docker build` of a 2-stage Dockerfile; a shared-Windows-path bind mount is read/writable. | Phase 1 |
| 14.3 | **Backend switch + Hyper-V ZFS:** selecting Hyper-V + launching an app CT does a ZFS CoW clone in **≤ 3 s** (via `zfs list -t snapshot`), same pipe, no `DOCKER_HOST`. State does **not** migrate; switching is **state-destructive**; the tray shows a confirmation dialog and a test asserts it. | Phase 3b |
| 14.4 | **Behavioral parity:** the same integration suite (minus capability-gated cases) passes on `.deb` and WSL2 (Phase 1) and **Hyper-V** (Phase 3b); all Windows-only code stays in the bridge/lifecycle/relay, no `//go:build windows` container logic. | Phase 1 → 3b |
| 14.5 | **Coexistence:** installs + runs with Docker Desktop present; both independently usable; **shared `~/.docker` untouched** (isolated `DOCKER_CONFIG`), script-verified. | Phase 2 |
| 14.6 | **Clean uninstall:** no distro/VM/VHDX/vSwitch/hvsock-GUID/group/context/`%LOCALAPPDATA%`/firewall-rule residue, script-verified. | Phase 2 |
| 14.7 | **Volume honesty:** a Windows-path bind mount either works or fails with an explicit, documented error — never a silent empty mount. | Phase 1 |

## 15. Non-goals / out of scope

- **Windows containers (WCOW / process isolation).** Linux containers via LXC
  only.
- **Kubernetes.** Docker Desktop ships single-node k8s; we do not. Stated so
  users don't assume it.
- **Game streaming / display / audio / input passthrough on Windows.**
  Speculative research, not a phase: DDA is exclusive (single-GPU hosts lose
  their display), the driver + synthetic-display + `uinput`/`/dev/input` + audio
  + multicast-discovery stack is brittle even on bare metal, and it drags
  downstream product specifics into a generic daemon. GPU **compute** (CUDA/DX)
  is the only GPU claim. Note: §6's external-vSwitch ships **raw L2 LAN
  bridging** only (wired, macvlan for containers); any app-level discovery or
  streaming built atop it remains out of scope here.
- **arm64 Hyper-V** (unproven; arm64 is WSL2-only + experimental at most).
- **Baking any specific downstream product** into the daemon — it stays generic
  and config/env-driven.

## 16. Roundtable review log (condensed)

**Pass 1** (five seats on rev 1): pipe collision + `DOCKER_HOST` hijack;
`\\wsl.localhost` AF_UNIX transport can't work; no auth/ACL on pipe or vsock +
"VM is the boundary" unsound for WSL2; Windows-path bind mounts / `-p` / build
missing; over-scoped + GoW/Wolf leakage + Phase-4 streaming fantasy;
`windows-latest` can't run WSL2-v2/nested-Hyper-V; HCS≠Hyper-V-WMI; DDA
Server-only → GPU-P; vibes success criteria. → resolved in rev 2/3 (unique pipe +
context, stdio relay, §4.5 trust model, §4.6–4.8, generic daemon, §15 non-goals,
§10 runners, §6 WMI + GPU-P, measurable §14).

**Pass 2** (three seats on the rev-3 Hyper-V + installer designs):
- **virtiofs impossible on Hyper-V** (no virtio bus) → SMB/CIFS (§4.6, §8).
- **`credsStore`/`currentContext` writes break Docker Desktop** (shared
  config.json) → per-app `DOCKER_CONFIG` isolation, never mutate shared config
  (§7, §14.5).
- **Authenticode without RFC3161 timestamp** invalidates shipped installers on
  cert expiry; CA/B requires HSM/cloud signing → §11.
- **NoCloud needs the `cidata` label; Gen2 Secure Boot blocks a custom kernel +
  out-of-tree ZFS** → Secure-Boot-off Gen2 (§6).
- **GPU-P needs Win11 22H2 + manual driver-store copy**; **external vSwitch is
  wired-only + needs macvlan for container multicast** (§6, §8).
- **§14 criteria silently Phase-3-terminal; Phases 2/3 not spike-gated;
  delta-VHDX + full-appliance reproducibility over-claimed; kernel↔OpenZFS
  patch-lag treadmill understated** → phase-annotated §14, spike-gated §9,
  demoted claims (§7, §11), honest §12.
- Minor: MobyLinuxVM/DockerDesktopVM attribution fixed (§6); Burn bootstrapper +
  MSIX-UI-only (§7); Kubernetes non-goal (§15); daemon.json/disk-relocation/
  sign-in tray panes + delayed-auto-start/on-demand-VM service posture (§7).
