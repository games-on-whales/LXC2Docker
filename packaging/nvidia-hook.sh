#!/bin/sh
# LXC mount hook: inject the host's NVIDIA userspace driver into a container that
# requested GPU access, using the NVIDIA container toolkit.
#
# docker-lxc-daemon wires this up (via lxc.hook.mount) for any container that
# opts into the GPU the games-on-whales / `--gpus` way: by setting
# NVIDIA_VISIBLE_DEVICES. Passing the /dev/dri and /dev/nvidia* device nodes
# alone is NOT enough — the driver *userspace* (libcuda, libnvidia-encode, the
# EGL/GLX vendor libraries, GLVND configs) must be injected too, or CUDA/EGL
# initialisation fails and GPU apps (e.g. Wolf's Wayland compositor) get a black
# screen / "no video".
#
# Rather than hardcode a driver-version-specific file list, defer to
# nvidia-container-cli — the same tool Docker's nvidia-container-runtime uses.
# liblxc runs this hook after the container rootfs is mounted, in the host mount
# namespace, with:
#   LXC_ROOTFS_MOUNT  path to the mounted container rootfs
#   LXC_CONFIG_FILE   path to the container's LXC config
# We read the NVIDIA_* selection from that config (defaulting to "all") and run
# `nvidia-container-cli configure`, which copies in the matching driver
# libraries + device nodes, runs ldconfig to create the SONAME symlinks, and
# installs the GLVND/EGL vendor configs — all matched to the running host driver,
# so it keeps working across driver upgrades with no daemon change.
#
# This hook is deliberately fault-tolerant: a GPU-injection problem must never
# stop the container from starting (it would just come up without the GPU), so
# we do not use `set -e` and we swallow the toolkit's expected ldcache failure.
set -u

rootfs="${LXC_ROOTFS_MOUNT:-}"
if [ -z "$rootfs" ]; then
	echo "nvidia-hook: LXC_ROOTFS_MOUNT unset, skipping GPU injection" >&2
	exit 0
fi

# The toolkit is required on NVIDIA hosts. If it is absent, skip rather than
# abort container start — the container simply comes up without the GPU.
if ! command -v nvidia-container-cli >/dev/null 2>&1; then
	echo "nvidia-hook: nvidia-container-cli not found, skipping GPU injection" >&2
	exit 0
fi

# Resolve the device selection and driver capabilities from the container's
# NVIDIA_* env, as recorded in the LXC config. Fall back to "all" — the hook is
# only invoked for containers that opted in, so injecting everything is the
# correct default when the values cannot be read.
devices="all"
caps="all"
conf="${LXC_CONFIG_FILE:-}"
if [ -n "$conf" ] && [ -r "$conf" ]; then
	# Matches both raw-LXC ("lxc.environment = K=V") and Proxmox
	# ("lxc.environment: K=V") config syntaxes.
	v=$(sed -n 's/^[[:space:]]*lxc\.environment[[:space:]]*[:=][[:space:]]*NVIDIA_VISIBLE_DEVICES=//p' "$conf" | tail -n1)
	[ -n "$v" ] && devices="$v"
	c=$(sed -n 's/^[[:space:]]*lxc\.environment[[:space:]]*[:=][[:space:]]*NVIDIA_DRIVER_CAPABILITIES=//p' "$conf" | tail -n1)
	[ -n "$c" ] && caps="$c"
fi

# "void" disables injection, matching the container toolkit's own semantics.
if [ "$devices" = "void" ]; then
	exit 0
fi

# Map NVIDIA_DRIVER_CAPABILITIES to nvidia-container-cli capability flags.
# "all" expands to the core set Wolf and most GPU apps need: compute (CUDA),
# utility (NVML / nvidia-smi), video (NVENC/NVDEC), graphics (EGL/GL/GLX). We
# deliberately keep this set conservative so it is supported across toolkit
# versions; explicit capability lists are passed through verbatim.
set --
if [ "$caps" = "all" ]; then
	set -- --compute --utility --video --graphics
else
	OLDIFS=$IFS
	IFS=,
	for cap in $caps; do
		[ -n "$cap" ] && set -- "$@" "--$cap"
	done
	IFS=$OLDIFS
fi

# Inject the driver libraries, device nodes, and base symlinks into the rootfs.
#
# --no-cgroups: the container's device cgroup is managed by LXC (privileged
# containers already allow all devices), so the toolkit only touches the rootfs.
#
# We intentionally do NOT let this command's exit status abort the hook. Its
# final "ldcache" step runs ldconfig through a privileged mount-based chroot that
# fails with EPERM inside an lxc.hook.mount ("mount operation failed: /:
# permission denied") — but only AFTER the libraries and device nodes are already
# in place. So it returns non-zero while having done the part we need; aborting
# here (set -e / exec) would brick the container in a start loop.
nvidia-container-cli --load-kmods configure \
	--ldconfig=@/sbin/ldconfig \
	--no-cgroups \
	--device="$devices" \
	"$@" \
	"$rootfs" || echo "nvidia-hook: nvidia-container-cli returned non-zero (ldcache step expected to fail in a mount hook); continuing" >&2

# Build the SONAME symlinks (e.g. libcuda.so.1 -> libcuda.so.NNN) and ld.so.cache
# ourselves. `ldconfig -r` chroots into the rootfs with a plain chroot(2) — it
# does NOT need the privileged mount the toolkit's own ldcache step uses, so it
# succeeds here where that step could not. Without these symlinks dlopen of the
# SONAMEs (libcuda.so.1, libnvidia-encode.so.1, ...) fails and CUDA/NVENC/EGL go
# unused.
ldconfig -r "$rootfs" || echo "nvidia-hook: ldconfig -r failed" >&2

exit 0
