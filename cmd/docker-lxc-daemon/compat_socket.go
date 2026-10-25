package main

import (
	"log"
	"net"
	"os"
	"os/user"
	"strconv"
	"time"
)

// compatSocketPath is where stock Docker clients and compose files look for the
// daemon. When --socket points elsewhere the daemon links this path to it.
const compatSocketPath = "/var/run/docker.sock"

// linkCompatSocket points compat at socket so stock Docker clients find the
// daemon, and reports whether it created the link. It never clobbers a socket
// someone else is serving: a real dockerd, or another docker-lxc-daemon
// instance, keeps its path and this daemon logs where it is listening instead.
// A missing path, a dangling link (left by a daemon that exited without
// cleaning up) or a dead socket file is taken over. Anything else (a regular
// file or a directory) is left alone.
func linkCompatSocket(compat, socket string) bool {
	if compat == socket {
		return false
	}
	if fi, err := os.Lstat(compat); err == nil {
		if target, err := os.Readlink(compat); err == nil && target == socket {
			return true // already ours, from a previous run
		}
		isLink := fi.Mode()&os.ModeSymlink != 0
		if !isLink && fi.Mode()&os.ModeSocket == 0 {
			log.Printf("warning: leaving %s alone: not a socket or symlink; Docker clients must use %s", compat, socket)
			return false
		}
		if socketAnswers(compat) {
			log.Printf("warning: leaving %s alone: another daemon is serving it; Docker clients must use %s", compat, socket)
			return false
		}
		if err := os.Remove(compat); err != nil {
			log.Printf("warning: remove stale %s: %v", compat, err)
			return false
		}
	}
	if err := os.Symlink(socket, compat); err != nil {
		log.Printf("warning: symlink %s → %s: %v", compat, socket, err)
		return false
	}
	return true
}

// unlinkCompatSocket removes compat if it is still the link this daemon
// created, so a stopped daemon does not leave stock Docker clients pointed at a
// dead socket. A link someone replaced in the meantime is left alone.
func unlinkCompatSocket(compat, socket string) {
	if target, err := os.Readlink(compat); err == nil && target == socket {
		if err := os.Remove(compat); err != nil {
			log.Printf("warning: remove %s: %v", compat, err)
		}
	}
}

func socketAnswers(path string) bool {
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// chownSocketToDockerGroup gives the socket to the docker group when one
// exists, so docker-group users can connect as with the real Docker daemon.
// The daemon does this itself, after listening: a unit ExecStartPost runs as
// soon as the process forks, before the socket exists.
func chownSocketToDockerGroup(socket string) {
	g, err := user.LookupGroup("docker")
	if err != nil {
		return
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return
	}
	if err := os.Chown(socket, 0, gid); err != nil {
		log.Printf("warning: chown %s to group docker: %v", socket, err)
	}
}
