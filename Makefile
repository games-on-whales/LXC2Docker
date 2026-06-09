BINARY     := docker-lxc-daemon
BUILD_DIR  := bin
CMD_PATH   := ./cmd/docker-lxc-daemon
GO_TEST    := go test

# Packaging — produces a .deb that end users install with apt. The daemon is
# pure Go now (no cgo / liblxc-dev), so the .deb depends only on the RUNTIME lxc
# CLI tools + nftables, and building it needs no build toolchain on the target.
# Debian versions must start with a digit, so we can't fall back to a bare
# commit SHA (git describe --always). Use the tag when there is one (stripping a
# leading "v" and flattening describe's "-N-gsha" suffix to dots so dpkg accepts
# it); otherwise synthesise a digit-led "0.0.0+git<sha>".
VERSION  ?= $(shell v=$$(git describe --tags --dirty 2>/dev/null | sed -e 's/^v//' -e 's/-/./g'); [ -n "$$v" ] || v="0.0.0+git$$(git rev-parse --short HEAD 2>/dev/null || echo 0)"; echo "$$v")
DEB_ARCH := $(shell dpkg --print-architecture 2>/dev/null || echo amd64)
DEB_PKG  := $(BUILD_DIR)/$(BINARY)_$(VERSION)_$(DEB_ARCH).deb

.PHONY: all build install uninstall deps clean test test-unit test-build test-integration deb

all: build

## Download Go module dependencies and generate go.sum.
deps:
	go mod tidy

## Build the daemon binary.
build:
	go build -o $(BUILD_DIR)/$(BINARY) $(CMD_PATH)

## Compile all packages and verify tests are buildable.
test-build:
	$(GO_TEST) -run '^$$' ./...

## Run all available unit tests.
test-unit:
	$(GO_TEST) ./...

## Run integration tests with the integration build tag.
test-integration:
	$(GO_TEST) -tags=integration ./...

## Run the full test matrix used by CI.
test: test-build test-unit test-integration

## Install binary and systemd unit.
install: build
	install -m 0755 $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)
	install -D -m 0755 packaging/nvidia-hook.sh /usr/libexec/$(BINARY)/nvidia-hook.sh
	install -m 0644 systemd/$(BINARY).service /etc/systemd/system/
	systemctl daemon-reload
	@echo "Run 'systemctl enable --now docker-lxc-daemon' to start."

## Build a .deb. End users install it with `apt install ./<pkg>.deb` — it pulls
## only the runtime lxc/liblxc + nftables, never the build toolchain. Building
## the .deb itself needs liblxc-dev (done once by the maintainer / CI).
deb: build
	@echo ">> packaging $(DEB_PKG)"
	rm -rf $(BUILD_DIR)/deb
	install -D -m 0755 $(BUILD_DIR)/$(BINARY) $(BUILD_DIR)/deb/usr/bin/$(BINARY)
	install -D -m 0644 systemd/$(BINARY).service $(BUILD_DIR)/deb/lib/systemd/system/$(BINARY).service
	# Package binary lives in /usr/bin, not the source /usr/local/bin.
	sed -i 's#/usr/local/bin/$(BINARY)#/usr/bin/$(BINARY)#' $(BUILD_DIR)/deb/lib/systemd/system/$(BINARY).service
	install -D -m 0755 packaging/nvidia-hook.sh $(BUILD_DIR)/deb/usr/libexec/$(BINARY)/nvidia-hook.sh
	install -D -m 0755 packaging/postinst $(BUILD_DIR)/deb/DEBIAN/postinst
	install -D -m 0755 packaging/prerm $(BUILD_DIR)/deb/DEBIAN/prerm
	printf 'Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Games on Whales <noreply@games-on-whales.github.io>\nSection: admin\nPriority: optional\nDepends: nftables, lxc-pve | lxc\nRecommends: skopeo, umoci, pve-container\nConflicts: docker.io, docker-ce\nHomepage: https://github.com/games-on-whales/docker-lxc-daemon\nDescription: Docker-compatible API daemon backed by LXC\n Speaks the Docker Engine API on top of LXC / Proxmox CTs, so docker,\n docker compose, and Docker SDKs work unmodified while containers run as\n first-class LXC containers. A drop-in replacement for the Docker daemon\n socket that needs only the runtime liblxc — no build toolchain.\n' \
		"$(BINARY)" "$(VERSION)" "$(DEB_ARCH)" > $(BUILD_DIR)/deb/DEBIAN/control
	dpkg-deb --root-owner-group --build $(BUILD_DIR)/deb $(DEB_PKG)
	@echo ">> built $(DEB_PKG)"
	@dpkg-deb -I $(DEB_PKG) | sed -n '/Package:/,/Description:/p'

## Remove binary and systemd unit.
uninstall:
	systemctl stop $(BINARY) || true
	systemctl disable $(BINARY) || true
	rm -f /usr/local/bin/$(BINARY)
	rm -rf /usr/libexec/$(BINARY)
	rm -f /etc/systemd/system/$(BINARY).service
	systemctl daemon-reload

clean:
	rm -rf $(BUILD_DIR)
