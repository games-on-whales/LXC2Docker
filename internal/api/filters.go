package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// filters is the decoded form of the Docker Engine `?filters=<json>` query
// parameter. The wire format is `{"label":["foo=bar"],"status":["running"]}` —
// each key holds a list of OR-ed values; different keys are AND-ed together.
//
// Portainer relies on this for its Stacks view (label-filtering by compose
// project), its image tag filter, and several other UI components. If we
// silently ignore the parameter, compose stacks render as "unstacked"
// containers and tag searches show the full image list.
type filters map[string][]string

// parseFilters reads the query string's `filters` param and returns a map.
// Returns an empty map when the param is absent or malformed — filters are
// additive on top of the base list, so empty == no filtering.
func parseFilters(r *http.Request) filters {
	raw := r.URL.Query().Get("filters")
	if raw == "" {
		return filters{}
	}
	out := filters{}
	// Most modern clients send the map-of-arrays shape. Docker historically
	// also accepted map-of-bool-sets (`{"label":{"foo=bar":true}}`); handle
	// both by trying the array form first, then falling back.
	if err := json.Unmarshal([]byte(raw), &out); err == nil {
		return out
	}
	legacy := map[string]map[string]bool{}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return filters{}
	}
	for k, set := range legacy {
		for v, on := range set {
			if on {
				out[k] = append(out[k], v)
			}
		}
	}
	return out
}

// has returns whether the filter map contains key with at least one value.
func (f filters) has(key string) bool {
	return len(f[key]) > 0
}

// matchAny returns true if any of f[key]'s values equals val. Used for
// simple keys like "status" where equality is enough.
func (f filters) matchAny(key, val string) bool {
	if !f.has(key) {
		return true
	}
	for _, v := range f[key] {
		if v == val {
			return true
		}
	}
	return false
}

// matchLabel evaluates a set of "label" filters against a container's label
// map. Supports both "key" (key must exist) and "key=value" (exact match)
// forms, matching the Docker Engine behavior.
func (f filters) matchLabel(labels map[string]string) bool {
	vals := f["label"]
	if len(vals) == 0 {
		return true
	}
	for _, want := range vals {
		k, v, hasEq := strings.Cut(want, "=")
		got, present := labels[k]
		switch {
		case !hasEq:
			if !present {
				return false
			}
		default:
			if got != v {
				return false
			}
		}
	}
	return true
}

// matchNamePrefix tests a container's names against the "name" filter. Docker
// treats name filters as substring matches (`docker ps -f name=web` matches
// "web", "web-1", "my-web"). Portainer uses the leading-slash form.
func (f filters) matchNamePrefix(names []string) bool {
	vals := f["name"]
	if len(vals) == 0 {
		return true
	}
	for _, want := range vals {
		want = strings.TrimPrefix(want, "/")
		for _, n := range names {
			if strings.Contains(strings.TrimPrefix(n, "/"), want) {
				return true
			}
		}
	}
	return false
}

// matchID tests whether the id starts with any value in the "id" filter.
func (f filters) matchID(id string) bool {
	vals := f["id"]
	if len(vals) == 0 {
		return true
	}
	for _, want := range vals {
		if strings.HasPrefix(id, want) {
			return true
		}
	}
	return false
}

// matchAncestor tests the "ancestor" filter (image name or image ID) against
// a container's image. Portainer's image detail page sends this to list
// containers using the image.
func (f filters) matchAncestor(image, imageID string) bool {
	vals := f["ancestor"]
	if len(vals) == 0 {
		return true
	}
	for _, want := range vals {
		if want == image || want == imageID || strings.HasPrefix(imageID, want) {
			return true
		}
	}
	return false
}

// matchImageReference tests the "reference" filter used on /images/json.
// The wire value is an image name glob (e.g. "ubuntu:22.04", "nginx"). We
// accept exact matches and bare-name prefix matches.
func (f filters) matchImageReference(ref string) bool {
	vals := f["reference"]
	if len(vals) == 0 {
		return true
	}
	for _, want := range vals {
		if want == ref {
			return true
		}
		// Bare name match ("nginx" matches "nginx:latest").
		if !strings.Contains(want, ":") && strings.HasPrefix(ref, want+":") {
			return true
		}
	}
	return false
}
