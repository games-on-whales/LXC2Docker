#!/usr/bin/env bash
# install-on-pve.sh — install docker-lxc-daemon (LXC2Docker) directly on a
# Proxmox VE host, so `docker` / `docker compose` create first-class Proxmox
# CTs. This is the full-feature install path (Proxmox CT mode, GPU passthrough);
# the community-scripts CT install runs the daemon inside an LXC in plain-LXC
# mode instead.
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/games-on-whales/LXC2Docker/main/scripts/install-on-pve.sh)"
#
set -eEuo pipefail

REPO="games-on-whales/LXC2Docker"
BL="\033[36m"; GN="\033[1;92m"; RD="\033[01;31m"; YW="\033[33m"; CL="\033[m"
msg()  { echo -e " ${GN}✔${CL} $*"; }
info() { echo -e " ${BL}➜${CL} $*"; }
die()  { echo -e " ${RD}✗${CL} $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root on the Proxmox VE host"
command -v pveversion >/dev/null 2>&1 || die "this does not look like a Proxmox VE host (pveversion not found)"
command -v curl >/dev/null 2>&1 || die "curl is required"

info "Resolving latest ${REPO} release"
asset_url="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep -oE '"browser_download_url": *"[^"]*_amd64\.deb"' \
  | head -1 | cut -d'"' -f4)"
[ -n "$asset_url" ] || die "no .deb asset found on the latest release"

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
deb="$tmp/${asset_url##*/}"
info "Downloading ${asset_url##*/}"
curl -fsSL -o "$deb" "$asset_url"

info "Installing the .deb (pulls nftables + lxc-pve)"
apt-get update -qq
apt-get install -y "$deb"

cat <<EOF

$(echo -e "${GN}docker-lxc-daemon installed on this Proxmox host.${CL}")

To back containers with Proxmox CTs, point the service at a PVE storage by
editing the systemd unit's ExecStart (add --pve-storage=<storage>), e.g.:

  systemctl edit docker-lxc-daemon        # add: --pve-storage=local-lvm
  systemctl enable --now docker-lxc-daemon

Note: the daemon takes over /var/run/docker.sock, replacing the real Docker
daemon on this host. See ${YW}https://github.com/${REPO}${CL} for all flags.
EOF
