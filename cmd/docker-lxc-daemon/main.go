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

	"github.com/games-on-whales/docker-lxc-daemon/internal/api"
	"github.com/games-on-whales/docker-lxc-daemon/internal/lxc"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
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

	handler := api.NewHandler(mgr, st)

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

// buildLANConfig assembles the daemon's bridge catalog from the new
// repeatable --bridge flag and the legacy single-bridge flags. Each
// --bridge value parses as 'name=prefix/subnet:gateway'. The legacy
// flags, if any are set, become an additional entry; the first entry
// added becomes the default. Returns an empty config (no bridges) when
// neither form is provided — the daemon is then LAN-disabled and any
// container requesting LAN will fail at create time.
func buildLANConfig(specs []string, legacyName, legacyPrefix, legacyGateway string, legacySubnet int) (lxc.LANConfig, error) {
	cfg := lxc.LANConfig{Bridges: map[string]lxc.BridgeSpec{}}
	for _, raw := range specs {
		spec, err := parseBridgeSpec(raw)
		if err != nil {
			return cfg, err
		}
		if _, dup := cfg.Bridges[spec.Name]; dup {
			return cfg, fmt.Errorf("duplicate bridge %q", spec.Name)
		}
		cfg.Bridges[spec.Name] = spec
		if cfg.Default == "" {
			cfg.Default = spec.Name
		}
	}
	if legacyName != "" {
		if _, exists := cfg.Bridges[legacyName]; !exists {
			cfg.Bridges[legacyName] = lxc.BridgeSpec{
				Name:    legacyName,
				Prefix:  legacyPrefix,
				Gateway: legacyGateway,
				Subnet:  legacySubnet,
			}
			if cfg.Default == "" {
				cfg.Default = legacyName
			}
		}
	}
	return cfg, nil
}

// parseBridgeSpec parses 'name=prefix/subnet:gateway' into a BridgeSpec.
// Example: 'vmbr0=192.168.1/23:192.168.1.1'.
func parseBridgeSpec(raw string) (lxc.BridgeSpec, error) {
	nameRest := strings.SplitN(raw, "=", 2)
	if len(nameRest) != 2 || nameRest[0] == "" {
		return lxc.BridgeSpec{}, fmt.Errorf("bridge spec %q: expected 'name=prefix/subnet:gateway'", raw)
	}
	name := nameRest[0]
	netGw := strings.SplitN(nameRest[1], ":", 2)
	if len(netGw) != 2 {
		return lxc.BridgeSpec{}, fmt.Errorf("bridge spec %q: missing ':gateway'", raw)
	}
	prefSub := strings.SplitN(netGw[0], "/", 2)
	if len(prefSub) != 2 {
		return lxc.BridgeSpec{}, fmt.Errorf("bridge spec %q: prefix must be 'prefix/subnet'", raw)
	}
	subnet, err := strconv.Atoi(prefSub[1])
	if err != nil {
		return lxc.BridgeSpec{}, fmt.Errorf("bridge spec %q: invalid subnet: %w", raw, err)
	}
	return lxc.BridgeSpec{
		Name:    name,
		Prefix:  prefSub[0],
		Gateway: netGw[1],
		Subnet:  subnet,
	}, nil
}
