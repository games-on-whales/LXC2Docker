package api

import (
	"strings"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

// ImageRecord.ConfigDigest is documented as "the 64-hex value; callers prefix
// sha256:", and every reader does exactly that. Build and commit honour it,
// storing hex.EncodeToString output. Pull does not: it stores
// manifest.Config.Digest straight from the OCI manifest, which is already
// "sha256:<hex>". So a pulled image's ID came back doubled:
//
//	docker image inspect ghcr.io/ggml-org/llama.cpp:server-rocm
//	  Id = "sha256:sha256:a40ffec0460df2942..."
//
// That breaks any caller comparing an image ID to a container's, which is the
// standard way to assert a deploy actually replaced the running container.
func TestImageDisplayIDIsBareHexWhateverTheWriterStored(t *testing.T) {
	const hex = "a40ffec0460df2942bc4da5dbc5d7ba1b89bafed0aafec4615ac99bb9a96ebeb"

	cases := []struct {
		name string
		rec  *store.ImageRecord
	}{
		{"pull stores a prefixed manifest digest", &store.ImageRecord{
			ID: "oci_ghcr.io_ggml-org_llama.cpp_server-rocm", ConfigDigest: "sha256:" + hex}},
		{"build stores bare hex", &store.ImageRecord{
			ID: "oci_ghcr.io_ggml-org_llama.cpp_server-rocm", ConfigDigest: hex}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageDisplayID(tc.rec)
			if got != hex {
				t.Errorf("imageDisplayID() = %q, want bare hex %q", got, hex)
			}
			if full := "sha256:" + got; strings.Count(full, "sha256:") != 1 {
				t.Errorf("caller-prefixed ID = %q, want exactly one sha256: prefix", full)
			}
		})
	}
}

// A legacy record with no ConfigDigest still falls back to the tag-derived ID,
// and that must not be mangled by the normalisation.
func TestImageDisplayIDLeavesLegacyPseudoIDAlone(t *testing.T) {
	rec := &store.ImageRecord{ID: "oci_ghcr.io_ggml-org_llama.cpp_server-rocm"}
	if got := imageDisplayID(rec); got != rec.ID {
		t.Errorf("imageDisplayID() = %q, want %q", got, rec.ID)
	}
}

// Docker's ContainerJSON has two distinct image fields and they are not the
// same thing:
//
//	.Image        the image ID   ("sha256:<hex>")
//	.Config.Image the reference the user asked for ("ghcr.io/org/app:tag")
//
// This daemon returned the reference in BOTH, so the standard deploy assertion
//
//	WANT=$(docker image inspect $TAG       --format '{{.Id}}')
//	GOT=$( docker inspect      $CONTAINER  --format '{{.Image}}')
//	[ "$WANT" = "$GOT" ] || abort
//
// could never pass: it compared a digest against a tag string. A verification
// step that can only ever fail gets deleted or ignored, which is worse than not
// having one.
func TestContainerImageFieldIsAnIDNotARef(t *testing.T) {
	const hex = "a40ffec0460df2942bc4da5dbc5d7ba1b89bafed0aafec4615ac99bb9a96ebeb"
	const ref = "ghcr.io/ggml-org/llama.cpp:server-rocm"

	img := &store.ImageRecord{ID: "oci_ghcr.io_ggml-org_llama.cpp_server-rocm",
		Ref: ref, ConfigDigest: "sha256:" + hex}

	got := containerImageID(img, ref)
	if want := "sha256:" + hex; got != want {
		t.Errorf("containerImageID() = %q, want %q", got, want)
	}
	if strings.Count(got, "sha256:") != 1 {
		t.Errorf("containerImageID() = %q, want exactly one sha256: prefix", got)
	}
}

// No image record (legacy container, or the image was removed): fall back to
// the reference rather than inventing a digest or returning empty.
func TestContainerImageFieldFallsBackToRef(t *testing.T) {
	const ref = "ghcr.io/ggml-org/llama.cpp:server-rocm"
	if got := containerImageID(nil, ref); got != ref {
		t.Errorf("containerImageID(nil) = %q, want %q", got, ref)
	}
}
