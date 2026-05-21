BINARY     := docker-lxc-daemon
BUILD_DIR  := bin
CMD_PATH   := ./cmd/docker-lxc-daemon
GO_TEST    := go test

.PHONY: all build install uninstall deps clean test test-unit test-build test-integration

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
