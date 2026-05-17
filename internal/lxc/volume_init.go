package lxc

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type pathOwnership struct {
	uid  int
	gid  int
	mode os.FileMode
}

func initializeMountSources(rootfs string, cfg ContainerConfig) {
	for _, mount := range cfg.Mounts {
		if !mount.Initialize {
			continue
		}
		if err := initializeMountSource(rootfs, cfg.User, mount); err != nil {
			log.Printf("prepareRootfs: warning: initialize mount %s -> %s: %v",
				mount.Source, mount.Destination, err)
		}
	}
}

func initializeMountSource(rootfs, userSpec string, mount MountSpec) error {
	sourceInfo, err := os.Stat(mount.Source)
	if err != nil {
		return err
	}
	if !sourceInfo.IsDir() {
		return nil
	}
	empty, err := dirIsEmpty(mount.Source)
	if err != nil || !empty {
		return err
	}

	destRel, err := resolveInRootfs(rootfs, mount.Destination)
	if err != nil {
		destRel = strings.TrimPrefix(filepath.Clean(mount.Destination), "/")
	}
	dest := filepath.Join(rootfs, destRel)

	owner, destInfo, ok := ownershipFromPath(dest)
	if !ok {
		owner = ownershipFromUser(rootfs, userSpec)
	} else if !destInfo.IsDir() {
		owner.mode = 0o755
	}
	if err := os.Chmod(mount.Source, owner.mode.Perm()); err != nil {
		return err
	}
	if err := os.Chown(mount.Source, owner.uid, owner.gid); err != nil {
		return err
	}
	if ok && destInfo.IsDir() {
		return copyDirContents(dest, mount.Source)
	}
	return nil
}

func dirIsEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

func ownershipFromPath(path string) (pathOwnership, os.FileInfo, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return pathOwnership{}, nil, false
	}
	uid, gid, ok := fileOwner(info)
	if !ok {
		return pathOwnership{}, nil, false
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}
	return pathOwnership{uid: uid, gid: gid, mode: mode}, info, true
}

func ownershipFromUser(rootfs, spec string) pathOwnership {
	owner := pathOwnership{uid: 0, gid: 0, mode: 0o755}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return owner
	}
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	uid, gid, err := resolveUserSpec(rootfs, userPart)
	if err != nil {
		return owner
	}
	if hasGroup && groupPart != "" {
		if resolved, err := resolveGroupSpec(rootfs, groupPart); err == nil {
			gid = resolved
		}
	}
	if parsedUID, err := strconv.Atoi(uid); err == nil {
		owner.uid = parsedUID
	}
	if parsedGID, err := strconv.Atoi(gid); err == nil {
		owner.gid = parsedGID
	}
	return owner
}

func copyDirContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyPath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		if uid, gid, ok := fileOwner(info); ok {
			_ = os.Lchown(dst, uid, gid)
		}
		return nil
	case info.IsDir():
		if err := os.MkdirAll(dst, mode.Perm()); err != nil {
			return err
		}
		applyFileOwnership(dst, info)
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	case mode.IsRegular():
		return copyRegularFile(src, dst, info)
	default:
		log.Printf("prepareRootfs: skipping unsupported image volume entry %s (%s)", src, mode.Type())
		return nil
	}
}

func copyRegularFile(src, dst string, info os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	applyFileOwnership(dst, info)
	return os.Chmod(dst, info.Mode().Perm())
}

func applyFileOwnership(path string, info os.FileInfo) {
	if uid, gid, ok := fileOwner(info); ok {
		_ = os.Chown(path, uid, gid)
	}
	_ = os.Chmod(path, info.Mode().Perm())
}

func fileOwner(info os.FileInfo) (int, int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
