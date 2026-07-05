#!/usr/bin/env bash
# Wolf Den (WolfLeash) — web UI for managing Wolf on :8080. Started in the
# background by /opt/wolf-den/entrypoint.sh. Wolf Den runs inside this
# plugin's Wolf container (plugins are single-container), so it reaches Wolf's
# API socket locally. Wolf creates that socket at ${XDG_RUNTIME_DIR}/wolf.sock
# (api.cpp default); front it with a world-accessible socat proxy and point
# the .NET app at it, as the upstream wolf-den entrypoint does.
set -u

SOCK="${XDG_RUNTIME_DIR:-/run/user/wolf}/wolf.sock"
echo "[wolf-den] waiting for Wolf API socket at ${SOCK}"
for _ in $(seq 1 180); do [ -S "${SOCK}" ] && break; sleep 1; done
if [ ! -S "${SOCK}" ]; then
  echo "[wolf-den] Wolf API socket never appeared; not starting" >&2
  exit 1
fi

mkdir -p /run/wolf-den
socat "UNIX-LISTEN:/run/wolf-den/wolf.sock,mode=666,reuseaddr,fork" \
      "UNIX-CONNECT:${SOCK}" 2>/dev/null &

export WOLF_SOCKET_PATH="unix:///run/wolf-den/wolf.sock"
export ASPNETCORE_URLS="http://0.0.0.0:8080"
export DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1

cd /opt/wolf-den
echo "[wolf-den] starting WolfLeash on :8080"
exec dotnet /opt/wolf-den/WolfLeash.dll
