package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// ResolvedImage is the result of resolving a Docker image reference.
type ResolvedImage struct {
	// Ref is the canonical input reference, e.g. "ubuntu:22.04".
	Ref string
	// Kind is either KindDistro or KindApp.
	Kind Kind
	// Distro is the LXC download-template distro name (e.g. "ubuntu").
	// For KindApp this is the base distro.
	Distro string
	// Release is the LXC download-template release (e.g. "jammy").
	Release string
	// Arch is the target architecture (e.g. "amd64").
	Arch string
	// App is populated only for KindApp; it describes the packages to install.
	App *AppDef
	// BaseRef is populated only for KindApp; it is the base image reference
	// that must be resolved and pulled before this image can be built.
	BaseRef string
	// TemplateContainerName is the LXC container name used as the clone
	// source for this image, e.g. "__template_ubuntu_22.04".
	TemplateContainerName string
}

// Kind classifies a resolved image.
type Kind int

const (
	KindDistro Kind = iota // pure OS image — resolved directly from LXC download template
	KindApp                // application image — base distro + package install
	KindOCI                // arbitrary OCI/Docker image — pulled via skopeo + umoci
)

// Resolve parses a Docker image reference and returns a ResolvedImage.
// arch should be "amd64" or "arm64".
//
// Faithful-Docker semantics: a standard ref like "alpine", "alpine:3.19" or
// "docker.io/library/alpine" pulls the real Docker image as OCI — exactly as
// dockerd would. The LXC distro-template shortcut is NOT a silent override of
// those names; it is opt-in via the linuxcontainers image server host, e.g.
// "images.linuxcontainers.org/alpine[:3.19]" (the same source `lxc-create -t
// download` uses). This keeps LXC2Docker a faithful Docker engine while still
// exposing native LXC distro templates for callers that explicitly ask.
//
// When preferOCI is true, the built-in app shortcut registry is also skipped:
// any reference becomes KindOCI and is pulled from a Docker registry. This is
// what callers running in Proxmox-CT mode use, so names like "nginx:alpine"
// carry standard Docker semantics (real image, suitable for permanent CTs)
// rather than the GoW-specific app-template shortcut (always ephemeral).
func Resolve(ref, arch string, preferOCI bool) (*ResolvedImage, error) {
	if arch == "" {
		arch = "amd64"
	}

	name, tag := parseRef(ref)

	// 1. LXC distro template — OPT-IN only, requested via the linuxcontainers
	// image server host (images.linuxcontainers.org/<distro>[:<release>]). A
	// bare or docker.io ref ("alpine") instead pulls the real Docker image as
	// OCI below, so the distro shortcut never silently shadows a Hub image.
	if isLinuxcontainersHost(registryHost(ref)) && isKnownDistro(name) {
		distro, release := resolveDistro(name, tag)
		if distro == "" {
			return nil, fmt.Errorf("image: unknown release %q for distro %q", tag, name)
		}
		return &ResolvedImage{
			Ref:                   ref,
			Kind:                  KindDistro,
			Distro:                distro,
			Release:               release,
			Arch:                  arch,
			TemplateContainerName: templateName(distro, release),
		}, nil
	}

	// 2. Try known app image. Skipped when the caller prefers OCI.
	if !preferOCI {
		if def, ok := lookupApp(name); ok {
			baseName, baseTag := parseRef(def.Base)
			baseDistro, baseRelease := resolveDistro(baseName, baseTag)
			if baseDistro == "" {
				return nil, fmt.Errorf("image: app %q has unknown base %q", name, def.Base)
			}
			appDef := def // copy
			return &ResolvedImage{
				Ref:                   ref,
				Kind:                  KindApp,
				Distro:                baseDistro,
				Release:               baseRelease,
				Arch:                  arch,
				App:                   &appDef,
				BaseRef:               def.Base,
				TemplateContainerName: appTemplateName(name, tag),
			}, nil
		}
	}

	// 3. Fall through to OCI image — will be pulled via skopeo + umoci.
	_, tag = parseRef(ref)
	return &ResolvedImage{
		Ref:                   ref,
		Kind:                  KindOCI,
		Arch:                  arch,
		TemplateContainerName: ociTemplateName(ref),
	}, nil
}

// registryHost returns the registry host of a ref (the part before the first
// "/" when it looks like a host — contains "." or ":", or is "localhost"), or
// "" for a bare/Docker-Hub ref. Mirrors Docker's own ref-parsing rule.
func registryHost(ref string) string {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	prefix := parts[0]
	if strings.Contains(prefix, ".") || strings.Contains(prefix, ":") || prefix == "localhost" {
		return prefix
	}
	return ""
}

// isLinuxcontainersHost reports whether a registry host selects the LXC distro
// image server (images.linuxcontainers.org) — the explicit opt-in for an LXC
// distro template instead of an OCI Docker image. A port suffix is tolerated.
func isLinuxcontainersHost(host string) bool {
	if i := strings.IndexByte(host, ':'); i != -1 {
		host = host[:i]
	}
	return host == "images.linuxcontainers.org" || strings.HasSuffix(host, ".linuxcontainers.org") || host == "linuxcontainers.org"
}

// parseRef splits "name:tag", "name" (defaults tag to "latest"),
// and "registry/name:tag" (strips registry prefix for lookup purposes).
func parseRef(ref string) (name, tag string) {
	// Strip registry prefix (anything with a dot or colon before the first slash)
	parts := strings.SplitN(ref, "/", 2)
	bare := ref
	if len(parts) == 2 {
		prefix := parts[0]
		if strings.Contains(prefix, ".") || strings.Contains(prefix, ":") {
			bare = parts[1]
		}
	}

	if idx := strings.LastIndex(bare, ":"); idx != -1 {
		name = bare[:idx]
		tag = bare[idx+1:]
	} else {
		name = bare
		tag = "latest"
	}
	// Strip any remaining path component for lookup (e.g. "library/ubuntu" → "ubuntu")
	if idx := strings.LastIndex(name, "/"); idx != -1 {
		name = name[idx+1:]
	}
	return
}

// templateName returns the LXC container name used as the clone source for a
// distro image, e.g. "__template_ubuntu_jammy".
func templateName(distro, release string) string {
	return fmt.Sprintf("__template_%s_%s", distro, sanitize(release))
}

// appTemplateName returns the LXC container name for an app image template.
func appTemplateName(app, tag string) string {
	return fmt.Sprintf("__template_app_%s_%s", sanitize(app), sanitize(tag))
}

// ociTemplateName returns the LXC container name for an OCI image template.
// LXC also writes this value to lxc.uts.name; keep it within the Linux
// hostname limit instead of embedding an arbitrarily long registry ref.
func ociTemplateName(ref string) string {
	const (
		prefix    = "__template_oci_"
		maxLength = 63
		hashChars = 12
	)

	safe := sanitizeOCIRef(ref)
	if len(prefix)+len(safe) <= maxLength {
		return prefix + safe
	}

	sum := sha256.Sum256([]byte(ref))
	digest := hex.EncodeToString(sum[:])[:hashChars]
	headLen := maxLength - len(prefix) - len(digest) - 1
	if headLen < 1 {
		headLen = 1
	}
	if len(safe) > headLen {
		safe = safe[:headLen]
	}
	safe = strings.Trim(safe, "._-")
	if safe == "" {
		safe = "image"
	}
	return prefix + safe + "_" + digest
}

// sanitize replaces characters that are not safe in an LXC container name.
func sanitize(s string) string {
	return strings.NewReplacer(
		":", "_",
		"/", "_",
		" ", "_",
	).Replace(s)
}

func sanitizeOCIRef(s string) string {
	return strings.NewReplacer(
		":", "_",
		"/", "_",
		".", "_",
		" ", "_",
	).Replace(s)
}
