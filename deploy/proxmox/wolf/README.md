# Wolf on Proxmox — moved

The Wolf-on-Proxmox recipe (Compose files, Wolf + Wolf Den image, config seed,
udev rules, and image-build workflow) now lives in its own repository:

**https://github.com/games-on-whales/proxmox-community-script**

It streams virtual desktops and applications to Moonlight clients on a Proxmox VE
host, backed by this daemon (`docker-lxc-daemon`). Sessions launch as sibling
Proxmox CTs.

The daemon itself still installs from here — see
[`scripts/install-on-pve.sh`](../../../scripts/install-on-pve.sh), which the
recipe references as a prerequisite.
