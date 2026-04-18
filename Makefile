BINARY     := docker-lxc-daemon
BUILD_DIR  := bin
CMD_PATH   := ./cmd/docker-lxc-daemon
GO_TEST    := go test

CGO_CFLAGS  := $(shell pkg-config --cflags lxc 2>/dev/null)
CGO_LDFLAGS := $(shell pkg-config --libs lxc 2>/dev/null || echo "-llxc")

.PHONY: all build install uninstall deps clean test test-unit test-build test-integration

all: build

## Download Go module dependencies and generate go.sum.
deps:
	go mod tidy

## Build the daemon binary. Requires liblxc-dev.
build:
	CGO_ENABLED=1 \
	CGO_CFLAGS="$(CGO_CFLAGS)" \
	CGO_LDFLAGS="$(CGO_LDFLAGS)" \
	go build -o $(BUILD_DIR)/$(BINARY) $(CMD_PATH)

## Compile all packages and verify tests are buildable.
test-build:
	CGO_ENABLED=1 \
	CGO_CFLAGS="$(CGO_CFLAGS)" \
	CGO_LDFLAGS="$(CGO_LDFLAGS)" \
	$(GO_TEST) -run '^$' ./...

## Run all available unit tests.
test-unit:
	CGO_ENABLED=1 \
	CGO_CFLAGS="$(CGO_CFLAGS)" \
	CGO_LDFLAGS="$(CGO_LDFLAGS)" \
	$(GO_TEST) ./...

## Run integration tests with the integration build tag.
test-integration:
	CGO_ENABLED=1 \
	CGO_CFLAGS="$(CGO_CFLAGS)" \
	CGO_LDFLAGS="$(CGO_LDFLAGS)" \
	$(GO_TEST) -tags=integration ./...

## Run the full test matrix used by CI.
test: test-build test-unit test-integration

## Install binary and systemd unit.
install: build
	install -m 0755 $(BUILD_DIR)/$(BINARY) /usr/local/bin/$(BINARY)
	install -m 0644 systemd/$(BINARY).service /etc/systemd/system/
	systemctl daemon-reload
	@echo "Run 'systemctl enable --now docker-lxc-daemon' to start."

## Remove binary and systemd unit.
uninstall:
	systemctl stop $(BINARY) || true
	systemctl disable $(BINARY) || true
	rm -f /usr/local/bin/$(BINARY)
	rm -f /etc/systemd/system/$(BINARY).service
	systemctl daemon-reload

clean:
	rm -rf $(BUILD_DIR)
