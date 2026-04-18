package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/api"
	"github.com/games-on-whales/LXC2Docker/internal/lxc"
	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// bridgeFlag accumulates repeated --bridge values into a slice.
type bridgeFlag []string

func (b *bridgeFlag) String() string     { return strings.Join(*b, ",") }
func (b *bridgeFlag) Set(v string) error { *b = append(*b, v); return nil }

func main() {
	socketPath := flag.String("socket", "/run/docker-lxc-daemon/docker.sock", "Unix socket path to listen on")
	lxcPath := flag.String("lxcpath", "/var/lib/lxc", "LXC container storage path (legacy direct-LXC mode)")
	pveStorage := flag.String("pve-storage", "", "Default Proxmox storage name for CT rootfs (e.g. 'large'); enables Proxmox CT mode. Per-container override via 'dld.storage' label.")
	statePath := flag.String("statepath", "/var/lib/docker-lxc-daemon", "Daemon state directory")

	// Multi-bridge configuration. Repeatable; first value is the default
	// bridge used when a container requests LAN networking without naming
	// a specific bridge via the 'dld.bridge' label.
	var bridges bridgeFlag
	flag.Var(&bridges, "bridge",
		"LAN bridge spec 'name=prefix/subnet:gateway' (e.g. 'vmbr0=192.168.1/23:192.168.1.1'); repeatable. The first value is the default.")

	// Legacy single-bridge flags (backwards compatible). When --bridge is
	// not used, these populate the single default bridge entry.
	lanBridge := flag.String("lan-bridge", "", "DEPRECATED: use --bridge. Default LAN bridge name.")
	lanPrefix := flag.String("lan-prefix", "", "DEPRECATED: use --bridge. Default LAN IP prefix.")
	lanGateway := flag.String("lan-gateway", "", "DEPRECATED: use --bridge. Default LAN gateway.")
	lanSubnet := flag.Int("lan-subnet", 24, "DEPRECATED: use --bridge. Default LAN subnet prefix length.")
	flag.Parse()

	if os.Geteuid() != 0 {
		log.Fatal("docker-lxc-daemon must run as root")
	}

	st, err := store.NewAt(*statePath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	lan, err := buildLANConfig(bridges, *lanBridge, *lanPrefix, *lanGateway, *lanSubnet)
	if err != nil {
		log.Fatalf("bridge config: %v", err)
	}
	mgr, err := lxc.NewManager(*lxcPath, *pveStorage, lan, st)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}

	handler, healthEmit, restartEmit := api.NewHandlerWithHooks(mgr, st)

	// Ensure socket directory exists.
	socketDir := filepath.Dir(*socketPath)
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		log.Fatalf("mkdir socket dir %s: %v", socketDir, err)
	}

	// Remove stale socket if present.
	os.Remove(*socketPath)

	l, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("listen %s: %v", *socketPath, err)
	}
	// Docker clients expect the socket to be world-writable (group docker
	// restricts access in production; for GoW we keep it simple).
	if err := os.Chmod(*socketPath, 0o666); err != nil {
		log.Printf("warning: chmod socket: %v", err)
	}

	// Create a compatibility symlink at /var/run/docker.sock so Docker
	// clients and compose files work without modification.
	compatSocket := "/var/run/docker.sock"
	if *socketPath != compatSocket {
		os.Remove(compatSocket)
		if err := os.Symlink(*socketPath, compatSocket); err != nil {
			log.Printf("warning: symlink %s → %s: %v", compatSocket, *socketPath, err)
		}
	}

	srv := &http.Server{Handler: handler}

	// Graceful shutdown on SIGTERM/SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Start background GC that removes stopped ephemeral containers.
	mgr.StartGC(ctx)
	// Start the restart-policy / AutoRemove watcher.
	mgr.StartRestartWatcherWithEmitter(ctx, restartEmit)
	// Start the HEALTHCHECK runner so Portainer's health badge updates.
	mgr.StartHealthWatcher(ctx, healthEmit)

	go func() {
		<-ctx.Done()
		log.Println("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		lxc.TeardownBridge()
	}()

	fmt.Printf("docker-lxc-daemon listening on %s\n", *socketPath)
	if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}

// buildLANConfig resolves a single LAN bridge. In the current manager
// implementation only one bridge is supported, so this function accepts at most
// one `--bridge` entry and the legacy single-bridge flags as fallback.
func buildLANConfig(specs []string, legacyName, legacyPrefix, legacyGateway string, legacySubnet int) (lxc.LANConfig, error) {
	if len(specs) > 1 {
		return lxc.LANConfig{}, fmt.Errorf("multiple --bridge values are unsupported")
	}
	if len(specs) == 1 {
		spec, err := parseBridgeSpec(specs[0])
		if err != nil {
			return lxc.LANConfig{}, err
		}
		return lxc.LANConfig{
			Bridge:  spec.Name,
			Prefix:  spec.Prefix,
			Gateway: spec.Gateway,
			Subnet:  spec.Subnet,
		}, nil
	}
	if legacyName != "" {
		return lxc.LANConfig{
			Bridge:  legacyName,
			Prefix:  legacyPrefix,
			Gateway: legacyGateway,
			Subnet:  legacySubnet,
		}, nil
	}
	return lxc.LANConfig{}, nil
}

type bridgeSpec struct {
	Name    string
	Prefix  string
	Gateway string
	Subnet  int
}

// parseBridgeSpec parses 'name=prefix/subnet:gateway' into a bridgeSpec.
// Example: 'vmbr0=192.168.1/23:192.168.1.1'.
func parseBridgeSpec(raw string) (bridgeSpec, error) {
	nameRest := strings.SplitN(raw, "=", 2)
	if len(nameRest) != 2 || nameRest[0] == "" {
		return bridgeSpec{}, fmt.Errorf("bridge spec %q: expected 'name=prefix/subnet:gateway'", raw)
	}
	name := nameRest[0]
	netGw := strings.SplitN(nameRest[1], ":", 2)
	if len(netGw) != 2 {
		return bridgeSpec{}, fmt.Errorf("bridge spec %q: missing ':gateway'", raw)
	}
	prefSub := strings.SplitN(netGw[0], "/", 2)
	if len(prefSub) != 2 {
		return bridgeSpec{}, fmt.Errorf("bridge spec %q: prefix must be 'prefix/subnet'", raw)
	}
	subnet, err := strconv.Atoi(prefSub[1])
	if err != nil {
		return bridgeSpec{}, fmt.Errorf("bridge spec %q: invalid subnet: %w", raw, err)
	}
	return bridgeSpec{
		Name:    name,
		Prefix:  prefSub[0],
		Gateway: netGw[1],
		Subnet:  subnet,
	}, nil
}
