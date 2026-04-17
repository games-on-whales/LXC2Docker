package api

import (
	"encoding/json"
	"strings"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
)

// listFilters is the decoded form of the `filters` query parameter used by
// Docker's list endpoints: a JSON object mapping filter key to a list of
// accepted values. An empty map means "no filter".
type listFilters map[string][]string

func parseListFilters(raw string) (listFilters, error) {
	if raw == "" {
		return listFilters{}, nil
	}
	// Docker accepts both shapes for historical reasons:
	//   {"status":["running"]}            — slice form
	//   {"status":{"running":true}}       — map form, values ignored
	// Try the slice form first, then fall back to decoding the map.
	var slice map[string][]string
	if err := json.Unmarshal([]byte(raw), &slice); err == nil {
		return listFilters(slice), nil
	}
	var asMap map[string]map[string]bool
	if err := json.Unmarshal([]byte(raw), &asMap); err != nil {
		return nil, err
	}
	out := listFilters{}
	for k, v := range asMap {
		for key := range v {
			out[k] = append(out[k], key)
		}
	}
	return out, nil
}

func (f listFilters) anyMatch(key string, candidates ...string) bool {
	vals := f[key]
	if len(vals) == 0 {
		return true
	}
	for _, v := range vals {
		for _, c := range candidates {
			if c == "" {
				continue
			}
			if v == c {
				return true
			}
		}
	}
	return false
}

// matchesContainerFilters applies Docker's /containers/json filter keys
// (status, id, name, label, ancestor) to a single container record.
func matchesContainerFilters(rec *store.ContainerRecord, state string, f listFilters) bool {
	if !f.anyMatch("status", state) {
		return false
	}
	if !f.anyMatch("id", rec.ID, rec.ID[:12]) {
		return false
	}
	if !f.anyMatch("name", rec.Name, "/"+rec.Name) {
		return false
	}
	// "ancestor" matches the container's image ref — exact match or with an
	// implicit ":latest" tag stripped on either side.
	if vals := f["ancestor"]; len(vals) > 0 {
		image := normalizeImageRef(rec.Image)
		matched := false
		for _, v := range vals {
			if normalizeImageRef(v) == image {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if !matchesLabelFilter(f["label"], rec.Labels) {
		return false
	}
	return true
}

// matchesLabelFilter implements Docker's label filter: each entry is either
// "key" (requires key to be present) or "key=value" (requires exact value).
// All entries must match.
func matchesLabelFilter(filters []string, labels map[string]string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, want := range filters {
		k, v, hasValue := strings.Cut(want, "=")
		got, present := labels[k]
		if !present {
			return false
		}
		if hasValue && got != v {
			return false
		}
	}
	return true
}
