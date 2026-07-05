#!/usr/bin/env bash
# SmoothNAS bundles Wolf Den (the WolfLeash management UI) in the same
# container as Wolf — SmoothNAS plugins are single-container. The upstream
# Wolf image starts /wolf/wolf as PID 1 via /entrypoint.sh and ships no s6
# supervisor, so an /etc/services.d unit is never run. Launch Wolf Den in the
# background here, then hand PID 1 to Wolf's real entrypoint unchanged.
set -u
/opt/wolf-den/launch.sh &
if [ -x /entrypoint.sh ]; then
  exec /entrypoint.sh "$@"
fi
exec /wolf/wolf "$@"
