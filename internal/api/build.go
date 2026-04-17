package api

import (
	"encoding/json"
	"net/http"
)

// buildImage implements POST /build. We return a Docker-style streamed error
// instead of a bare 404 so Portainer can surface a clear unsupported-build
// failure.
func (h *Handler) buildImage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       "image build is not supported yet",
		"errorDetail": map[string]string{"message": "image build is not supported yet"},
	})
}
