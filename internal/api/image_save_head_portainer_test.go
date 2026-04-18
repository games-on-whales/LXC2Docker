package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/games-on-whales/LXC2Docker/internal/store"
	"github.com/gorilla/mux"
)

func TestHeadSaveImageSkipsTarStreamingForPortainer(t *testing.T) {
	t.Parallel()

	st, err := store.NewAt(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := st.AddImage(&store.ImageRecord{
		ID:      "img1",
		Ref:     normalizeImageRef("docker.io/library/alpine:latest"),
		Created: time.Unix(10, 0),
	}); err != nil {
		t.Fatalf("add image: %v", err)
	}

	h := &Handler{store: st}
	req := httptest.NewRequest(http.MethodHead, "/v1.45/images/alpine/get", nil)
	req = mux.SetURLVars(req, map[string]string{"name": "alpine"})
	rr := httptest.NewRecorder()
	h.saveImage(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("expected tar content type, got %q", rr.Header().Get("Content-Type"))
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("expected HEAD save to skip body streaming, got %d bytes", rr.Body.Len())
	}
}
