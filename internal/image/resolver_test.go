package image

import "testing"

func TestResolveKnownDistro(t *testing.T) {
	t.Parallel()

	got, err := Resolve("alpine:3.19.1", "arm64", false)
	if err != nil {
		t.Fatalf("Resolve() returned error: %v", err)
	}
	if got.Kind != KindDistro {
		t.Fatalf("expected KindDistro, got %d", got.Kind)
	}
	if got.Distro != "alpine" {
		t.Fatalf("expected distro alpine, got %q", got.Distro)
	}
	if got.Release != "3.19" {
		t.Fatalf("expected release 3.19, got %q", got.Release)
	}
	if got.Arch != "arm64" {
		t.Fatalf("expected arch arm64, got %q", got.Arch)
	}
	if got.TemplateContainerName != "__template_alpine_3.19" {
		t.Fatalf("expected template name __template_alpine_3.19, got %q", got.TemplateContainerName)
	}
}

func TestResolveKnownApp(t *testing.T) {
	t.Parallel()

	got, err := Resolve("nginx", "", false)
	if err != nil {
		t.Fatalf("Resolve() returned error: %v", err)
	}
	if got.Kind != KindApp {
		t.Fatalf("expected KindApp, got %d", got.Kind)
	}
	if got.App == nil {
		t.Fatal("expected App to be set for KindApp")
	}
	if got.TemplateContainerName != "__template_app_nginx_latest" {
		t.Fatalf("unexpected template name %q", got.TemplateContainerName)
	}
	if got.BaseRef != "debian:bookworm" {
		t.Fatalf("unexpected base ref %q", got.BaseRef)
	}
}

func TestResolvePreferOCIOversAppShortcut(t *testing.T) {
	t.Parallel()

	got, err := Resolve("nginx:alpine", "", true)
	if err != nil {
		t.Fatalf("Resolve() returned error: %v", err)
	}
	if got.Kind != KindOCI {
		t.Fatalf("expected KindOCI when preferOCI=true, got %d", got.Kind)
	}
	if got.TemplateContainerName != "__template_oci_nginx_alpine" {
		t.Fatalf("unexpected template name %q", got.TemplateContainerName)
	}
}

func TestResolveDefaultsToAMD64(t *testing.T) {
	t.Parallel()

	got, err := Resolve("custom/repo/image", "", false)
	if err != nil {
		t.Fatalf("Resolve() returned error: %v", err)
	}
	if got.Arch != "amd64" {
		t.Fatalf("expected default arch amd64, got %q", got.Arch)
	}
	if got.Kind != KindOCI {
		t.Fatalf("expected KindOCI for unknown image, got %d", got.Kind)
	}
}
