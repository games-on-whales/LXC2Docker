// Package oci handles pulling and unpacking OCI/Docker images using skopeo
// and umoci. This is the fallback path for images that are not known distro
// or app images in the built-in registry.
package oci

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ImageConfig holds the fields extracted from an OCI image configuration
// that are needed to run a container.
type ImageConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	Ports      []string          // e.g. ["80/tcp", "443/tcp"]
	Labels     map[string]string // OCI image labels (rendered in Portainer's image detail)
}

// ProgressEvent represents a single line of pull progress emitted to the
// Docker API client. The fields mirror what `docker pull` streams so the
// real Docker CLI, Portainer's pull modal, and compose renderers all work
// without special-casing.
type ProgressEvent struct {
	Status  string // e.g. "Pulling fs layer", "Downloading"
	ID      string // layer digest (short form) when applicable
	Current int64  // bytes transferred for the layer
	Total   int64  // total bytes for the layer (0 if unknown)
}

// PullOpts configures a Pull. Credentials, when non-empty, is passed to
// skopeo as --src-creds so private registries work. OnEvent receives
// structured progress events; callers that only want a status message
// can wrap their simple callback.
type PullOpts struct {
	Credentials string              // "user:password" form
	OnEvent     func(ProgressEvent) // structured progress (may be nil)
	OnStatus    func(string)        // plain status lines (for backward compat)
}

// Pull downloads an OCI image from a registry and unpacks it to a rootfs
// directory. It returns the extracted image config and the path to the rootfs.
//
// storeDir is the base directory for OCI storage (e.g. /var/lib/oci).
// ref is the image reference (e.g. "nginx:latest", "ghcr.io/org/app:v1").
func Pull(storeDir, ref string, opts PullOpts) (*ImageConfig, string, error) {
	emitStatus := func(s string) {
		if opts.OnStatus != nil {
			opts.OnStatus(s)
		}
	}
	emitEvent := func(e ProgressEvent) {
		if opts.OnEvent != nil {
			opts.OnEvent(e)
		}
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("oci: mkdir %s: %w", storeDir, err)
	}

	// Normalize ref: add docker.io/library/ prefix if bare name.
	dockerRef := normalizeDockerRef(ref)
	safeName := SafeDirName(ref)

	ociDir := filepath.Join(storeDir, "images", safeName)
	bundleDir := filepath.Join(storeDir, "bundles", safeName)
	tag := tagFromRef(ref)

	// 1. Pull image via skopeo.
	emitStatus(fmt.Sprintf("Pulling OCI image %s", dockerRef))
	if err := os.MkdirAll(filepath.Dir(ociDir), 0o755); err != nil {
		return nil, "", err
	}
	// Remove stale OCI dir if exists (re-pull).
	os.RemoveAll(ociDir)

	skopeoArgs := []string{"copy"}
	if opts.Credentials != "" {
		// Credentials only apply to the source (the registry we're pulling
		// from). The OCI layout destination is local. Using --src-creds on
		// anonymous pulls is harmless; skopeo ignores it when the registry
		// doesn't require auth.
		skopeoArgs = append(skopeoArgs, "--src-creds", opts.Credentials)
	}
	skopeoArgs = append(skopeoArgs,
		"docker://"+dockerRef,
		"oci:"+ociDir+":"+tag,
	)

	// Stream skopeo's stderr so we can report layer progress in real time.
	// skopeo prints one line per blob: "Copying blob <digest>" followed by
	// updates like "123.4 MB / 456.7 MB". Parsing these into ProgressEvents
	// lets Portainer's pull modal render per-layer bars instead of sitting
	// at 0% for minutes.
	cmd := exec.Command("skopeo", skopeoArgs...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", fmt.Errorf("oci: pipe stderr: %w", err)
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("oci: skopeo start: %w", err)
	}
	var stderrBuf strings.Builder
	go parseSkopeoProgress(stderr, &stderrBuf, emitEvent)
	if err := cmd.Wait(); err != nil {
		return nil, "", fmt.Errorf("oci: skopeo copy: %s: %w", stderrBuf.String(), err)
	}

	// 2. Parse image config from OCI layout.
	emitStatus("Extracting image config")
	cfg, err := parseImageConfig(ociDir, tag)
	if err != nil {
		return nil, "", fmt.Errorf("oci: parse config: %w", err)
	}

	// 3. Unpack to rootfs via umoci.
	emitStatus("Unpacking image layers")
	os.RemoveAll(bundleDir)
	umociArgs := []string{
		"unpack",
		"--image", ociDir + ":" + tag,
		bundleDir,
	}
	out, err := exec.Command("umoci", umociArgs...).CombinedOutput()
	if err != nil {
		return nil, "", fmt.Errorf("oci: umoci unpack: %s: %w", out, err)
	}

	rootfs := filepath.Join(bundleDir, "rootfs")
	if _, err := os.Stat(rootfs); err != nil {
		return nil, "", fmt.Errorf("oci: rootfs not found at %s", rootfs)
	}

	return cfg, rootfs, nil
}

// parseSkopeoProgress reads skopeo's stderr line-by-line, translates the
// most useful lines into ProgressEvents, and mirrors the raw text into tee
// so the caller can surface the full error output when the command fails.
//
// skopeo's stderr format (v1.13+):
//
//	Getting image source signatures
//	Copying blob sha256:abc123 123.4 MB / 456.7 MB [========>  ]  27.03%
//	Copying blob sha256:abc123 done
//	Copying config sha256:def456
//	Writing manifest to image destination
//	Storing signatures
//
// The size line is updated in-place with \r for real skopeo; we still see
// both forms depending on TTY detection. Be permissive.
func parseSkopeoProgress(r io.Reader, tee *strings.Builder, emit func(ProgressEvent)) {
	scanner := bufio.NewScanner(r)
	// Handle both \n and \r (progress updates) as delimiters so the
	// percent-complete refreshes land as individual lines.
	scanner.Split(scanLinesCR)
	re := regexp.MustCompile(`^Copying blob\s+(sha256:[0-9a-f]+)(?:\s+(\S+)\s+(\S+)\s+/\s+(\S+)\s+(\S+))?`)
	for scanner.Scan() {
		line := scanner.Text()
		tee.WriteString(line)
		tee.WriteByte('\n')
		switch {
		case strings.HasPrefix(line, "Copying blob") && strings.Contains(line, " done"):
			// Final "Copying blob <digest> done"
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				emit(ProgressEvent{
					Status: "Download complete",
					ID:     shortDigest(parts[2]),
				})
			}
		case strings.HasPrefix(line, "Copying blob"):
			if m := re.FindStringSubmatch(line); m != nil {
				ev := ProgressEvent{
					Status: "Downloading",
					ID:     shortDigest(m[1]),
				}
				if m[2] != "" {
					ev.Current = parseSize(m[2], m[3])
					ev.Total = parseSize(m[4], m[5])
				}
				emit(ev)
			}
		case strings.HasPrefix(line, "Copying config"):
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				emit(ProgressEvent{
					Status: "Pulling image config",
					ID:     shortDigest(parts[2]),
				})
			}
		case strings.HasPrefix(line, "Writing manifest"):
			emit(ProgressEvent{Status: "Writing manifest"})
		}
	}
}

// scanLinesCR is a bufio.SplitFunc that treats either \n or \r as the line
// terminator. skopeo rewrites progress in-place with \r so we have to break
// on either to capture each update.
func scanLinesCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// shortDigest returns the first 12 hex chars after the "sha256:" prefix,
// matching the IDs that Docker emits in its pull-progress stream.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// parseSize converts a ("123.4", "MB") pair to bytes. skopeo uses
// go-humanize-compatible suffixes, so we only need the common SI set.
func parseSize(num, unit string) int64 {
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(strings.TrimSuffix(unit, ",")) {
	case "B":
		return int64(v)
	case "KB":
		return int64(v * 1000)
	case "KIB":
		return int64(v * 1024)
	case "MB":
		return int64(v * 1000 * 1000)
	case "MIB":
		return int64(v * 1024 * 1024)
	case "GB":
		return int64(v * 1000 * 1000 * 1000)
	case "GIB":
		return int64(v * 1024 * 1024 * 1024)
	}
	return 0
}

// Cleanup removes the OCI layout and bundle for a given image ref.
func Cleanup(storeDir, ref string) {
	safeName := SafeDirName(ref)
	os.RemoveAll(filepath.Join(storeDir, "images", safeName))
	os.RemoveAll(filepath.Join(storeDir, "bundles", safeName))
}

// parseImageConfig reads the OCI image layout and extracts the image config.
func parseImageConfig(ociDir, tag string) (*ImageConfig, error) {
	// Read index.json to find the manifest.
	indexPath := filepath.Join(ociDir, "index.json")
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, fmt.Errorf("read index.json: %w", err)
	}

	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			MediaType   string            `json:"mediaType"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexData, &index); err != nil {
		return nil, fmt.Errorf("parse index.json: %w", err)
	}

	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("no manifests in index.json")
	}

	// Find manifest matching the tag, or use the first one.
	manifestDigest := index.Manifests[0].Digest
	for _, m := range index.Manifests {
		if m.Annotations["org.opencontainers.image.ref.name"] == tag {
			manifestDigest = m.Digest
			break
		}
	}

	// Read the manifest to find the config digest.
	manifestPath := filepath.Join(ociDir, "blobs", digestToPath(manifestDigest))
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	// Read the image config.
	configPath := filepath.Join(ociDir, "blobs", digestToPath(manifest.Config.Digest))
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var imgCfg struct {
		Config struct {
			Entrypoint   []string            `json:"Entrypoint"`
			Cmd          []string            `json:"Cmd"`
			Env          []string            `json:"Env"`
			WorkingDir   string              `json:"WorkingDir"`
			ExposedPorts map[string]struct{} `json:"ExposedPorts"`
			Labels       map[string]string   `json:"Labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(configData, &imgCfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	var ports []string
	for p := range imgCfg.Config.ExposedPorts {
		ports = append(ports, p)
	}

	return &ImageConfig{
		Entrypoint: imgCfg.Config.Entrypoint,
		Cmd:        imgCfg.Config.Cmd,
		Env:        imgCfg.Config.Env,
		WorkingDir: imgCfg.Config.WorkingDir,
		Ports:      ports,
		Labels:     imgCfg.Config.Labels,
	}, nil
}

// digestToPath converts "sha256:abc123..." to "sha256/abc123...".
func digestToPath(digest string) string {
	return strings.Replace(digest, ":", "/", 1)
}

// normalizeDockerRef adds docker.io/library/ prefix for bare image names.
// "nginx:latest" → "docker.io/library/nginx:latest"
// "ghcr.io/org/app:v1" → "ghcr.io/org/app:v1" (unchanged)
func normalizeDockerRef(ref string) string {
	// If ref contains no slashes, it's a Docker Hub library image.
	name, _, _ := strings.Cut(ref, ":")
	if !strings.Contains(name, "/") {
		return "docker.io/library/" + ref
	}
	// If the first component has no dots (e.g. "myorg/myapp"), it's Docker Hub.
	parts := strings.SplitN(name, "/", 2)
	if !strings.Contains(parts[0], ".") {
		return "docker.io/" + ref
	}
	return ref
}

// tagFromRef extracts the tag from an image reference, defaulting to "latest".
func tagFromRef(ref string) string {
	if _, tag, ok := strings.Cut(ref, ":"); ok {
		return tag
	}
	return "latest"
}

// SafeDirName converts an image ref to a safe directory name.
func SafeDirName(ref string) string {
	r := strings.NewReplacer("/", "_", ":", "_", ".", "_")
	return r.Replace(ref)
}
