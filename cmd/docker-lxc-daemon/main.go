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
	"syscall"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/api"
	"github.com/games-on-whales/docker-lxc-daemon/internal/lxc"
	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
)

func main() {
	socketPath := flag.String("socket", "/run/docker-lxc-daemon/docker.sock", "Unix socket path to listen on")
	lxcPath := flag.String("lxcpath", "/var/lib/lxc", "LXC container storage path (legacy direct-LXC mode)")
	pveStorage := flag.String("pve-storage", "", "Proxmox storage name for CT rootfs (e.g. 'large'); enables Proxmox CT mode")
	statePath := flag.String("statepath", "/var/lib/docker-lxc-daemon", "Daemon state directory")
	lanBridge := flag.String("lan-bridge", "", "Physical LAN bridge for dual-NIC containers (e.g. 'vmbr0')")
	lanPrefix := flag.String("lan-prefix", "", "LAN IP prefix; VMID becomes last octet (e.g. '192.168.1')")
	lanGateway := flag.String("lan-gateway", "", "LAN gateway (e.g. '192.168.1.1')")
	lanSubnet := flag.Int("lan-subnet", 24, "LAN subnet prefix length (e.g. 23 for /23)")
	flag.Parse()

	if os.Geteuid() != 0 {
		log.Fatal("docker-lxc-daemon must run as root")
	}

	st, err := store.NewAt(*statePath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	lan := lxc.LANConfig{
		Bridge:  *lanBridge,
		Prefix:  *lanPrefix,
		Gateway: *lanGateway,
		Subnet:  *lanSubnet,
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
