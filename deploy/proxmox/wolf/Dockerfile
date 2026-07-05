# Proxmox Wolf image: upstream Games-on-Whales Wolf plus the Wolf Den management
# UI on :8080, mirroring the SmoothNAS plugin image. Wolf itself already carries
# the server, config bootstrap, and entrypoint, so this image stays thin.
#
#   docker compose build   (or the workflow_dispatch build in .github/workflows)

ARG WOLF_BASE=ghcr.io/games-on-whales/wolf:stable
ARG WOLF_DEN_IMAGE=ghcr.io/games-on-whales/wolf-den:stable

FROM ${WOLF_DEN_IMAGE} AS wolfden

FROM ${WOLF_BASE}

ENV HOST_APPS_STATE_FOLDER=/etc/wolf \
    XDG_RUNTIME_DIR=/run/wolf \
    WOLF_DOCKER_SOCKET=/var/run/docker.sock

# --- Wolf Den (management UI on :8080) ------------------------------------
# Upstream Wolf runs /wolf/wolf as PID 1 via /entrypoint.sh and ships no s6
# supervisor, so Wolf Den can't be an /etc/services.d unit. A wrapper entrypoint
# launches Wolf Den in the background and execs Wolf's real entrypoint, keeping
# Wolf as PID 1. Pull the published app + the .NET runtime it was built against
# straight from the upstream wolf-den image.
COPY --from=wolfden /app /opt/wolf-den
COPY --from=wolfden /usr/share/dotnet /usr/share/dotnet
# socat (Wolf Den local API proxy) + .NET native deps (libicu). wolf:stable is
# Debian (apt); wolf:vulkan is Fedora (dnf) — install with whichever is present.
RUN set -e; \
    ln -sf /usr/share/dotnet/dotnet /usr/local/bin/dotnet; \
    if command -v apt-get >/dev/null 2>&1; then \
        apt-get update; \
        apt-get install -y --no-install-recommends socat libicu72 || \
            apt-get install -y --no-install-recommends socat libicu; \
        rm -rf /var/lib/apt/lists/*; \
    elif command -v dnf >/dev/null 2>&1; then \
        dnf install -y socat libicu findutils; dnf clean all; \
    elif command -v microdnf >/dev/null 2>&1; then \
        microdnf install -y socat libicu findutils; microdnf clean all; \
    else \
        echo "no supported package manager to install socat/libicu" >&2; exit 1; \
    fi; \
    dotnet --info >/dev/null
COPY --chmod=755 wolf-den-launch.sh /opt/wolf-den/launch.sh
COPY --chmod=755 wolf-den-entrypoint.sh /opt/wolf-den/entrypoint.sh
ENTRYPOINT ["/opt/wolf-den/entrypoint.sh"]
EXPOSE 8080

LABEL org.opencontainers.image.source="https://github.com/games-on-whales/LXC2Docker"
LABEL org.opencontainers.image.description="Proxmox Wolf image (Games-on-Whales Wolf + Wolf Den UI on :8080)"
