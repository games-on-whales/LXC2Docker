package api

import (
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestShortID(t *testing.T) {
	t.Parallel()
	if got := shortID("0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("shortID long = %q, want 12 chars", got)
	}
	if got := shortID("short"); got != "short" {
		t.Errorf("shortID short = %q, want unchanged", got)
	}
}

func TestImageInUse(t *testing.T) {
	t.Parallel()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Image ID "oci_nginx" with two tags (as `docker tag` produces).
	for _, ref := range []string{"nginx:latest", "myreg/nginx:pinned"} {
		if err := st.AddImage(&store.ImageRecord{ID: "oci_nginx", Ref: ref}); err != nil {
			t.Fatal(err)
		}
	}
	// An unrelated image with no container.
	if err := st.AddImage(&store.ImageRecord{ID: "oci_redis", Ref: "redis:7"}); err != nil {
		t.Fatal(err)
	}
	// A container created from the *second* tag of the nginx image.
	if err := st.AddContainer(&store.ContainerRecord{ID: "cont123", Name: "web", Image: "myreg/nginx:pinned"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}

	nginx := st.GetImage("nginx:latest")
	if cid := h.imageInUse(nginx); cid != "cont123" {
		t.Errorf("imageInUse(nginx) = %q, want cont123 (in use via a sibling tag)", cid)
	}
	redis := st.GetImage("redis:7")
	if cid := h.imageInUse(redis); cid != "" {
		t.Errorf("imageInUse(redis) = %q, want empty (not in use)", cid)
	}
	if cid := h.imageInUse(nil); cid != "" {
		t.Errorf("imageInUse(nil) = %q, want empty", cid)
	}
}
