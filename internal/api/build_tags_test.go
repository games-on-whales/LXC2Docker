package api

import (
	"reflect"
	"testing"

	"github.com/games-on-whales/LXC2Docker/internal/store"
)

func TestCollectBuildTags(t *testing.T) {
	t.Parallel()
	if got := collectBuildTags(nil); len(got) != 0 {
		t.Errorf("nil = %v, want empty", got)
	}
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"a:1"}, []string{"a:1"}},
		{[]string{"a:1", "b:2"}, []string{"a:1", "b:2"}},       // repeated params
		{[]string{"a:1,b:2"}, []string{"a:1", "b:2"}},          // comma-joined
		{[]string{"a:1", "a:1"}, []string{"a:1"}},              // de-duped
		{[]string{" a:1 ", "", "b:2"}, []string{"a:1", "b:2"}}, // trimmed, empties skipped
	}
	for _, tc := range cases {
		if got := collectBuildTags(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("collectBuildTags(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestTagBuiltImage(t *testing.T) {
	t.Parallel()
	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddImage(&store.ImageRecord{ID: "build_app", Ref: "app:1", TemplateName: "tmpl-app"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st, events: newEventBroker()}

	norm, err := h.tagBuiltImage("app:1", "app:latest")
	if err != nil {
		t.Fatalf("tagBuiltImage: %v", err)
	}
	if norm != "app:latest" {
		t.Errorf("returned ref = %q, want app:latest", norm)
	}
	tagged := st.GetImage("app:latest")
	if tagged == nil {
		t.Fatal("app:latest not created")
	}
	// Shares the built image's ID and backing (like docker tag).
	if tagged.ID != "build_app" || tagged.TemplateName != "tmpl-app" {
		t.Errorf("tagged = %+v, want same ID/backing as source", tagged)
	}

	// An UNTAGGED extra ref (docker build -t app-alias) must be normalized to
	// :latest, so it's resolvable by a later GetImage — otherwise the tag is
	// unreachable and the daemon would try to pull it.
	norm2, err := h.tagBuiltImage("app:1", "app-alias")
	if err != nil {
		t.Fatalf("tagBuiltImage(untagged): %v", err)
	}
	if norm2 != "app-alias:latest" {
		t.Errorf("untagged extra ref = %q, want app-alias:latest", norm2)
	}
	if st.GetImage(normalizeImageRef("app-alias")) == nil {
		t.Error("app-alias:latest not resolvable after tagging an untagged ref")
	}

	// Tagging a missing image errors.
	if _, err := h.tagBuiltImage("ghost:1", "x:1"); err == nil {
		t.Error("tagging a nonexistent built image should error")
	}
}
