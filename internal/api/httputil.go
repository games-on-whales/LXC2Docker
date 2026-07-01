package api

import (
	"net/http"
	"strings"
)

// boolValue parses a boolean query parameter with the same lenient semantics as
// the real Docker daemon's api/server/httputils.BoolValue: the value is true
// unless it is empty or one of "0", "no", "false", "none" (case-insensitive).
//
// Docker clients almost always send "1" or "true", but the Engine accepts any
// other non-empty token (force=True, v=yes, all=on, ...). Parsing these as
// false — as the previous `== "1" || == "true"` checks did — silently dropped
// the flag and diverged from Docker.
func boolValue(r *http.Request, key string) bool {
	s := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key)))
	return s != "" && s != "0" && s != "no" && s != "false" && s != "none"
}

// boolValueDefault is boolValue but returns def when the parameter is absent
// (empty). Used for parameters that default to true, e.g.
// GET /containers/{id}/stats?stream, where omitting the param means "stream".
func boolValueDefault(r *http.Request, key string, def bool) bool {
	if strings.TrimSpace(r.URL.Query().Get(key)) == "" {
		return def
	}
	return boolValue(r, key)
}
