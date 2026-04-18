package api

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/games-on-whales/docker-lxc-daemon/internal/store"
)

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// listFilters is the decoded form of the `filters` query parameter used by
// Docker's list endpoints: a JSON object mapping filter key to a list of
// accepted values. An empty map means "no filter".
type listFilters map[string][]string

func parseListFilters(raw string) (listFilters, error) {
	if raw == "" {
		return listFilters{}, nil
	}

	rawObj := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(raw), &rawObj); err != nil {
		return nil, err
	}

	out := listFilters{}
	for key, rawValue := range rawObj {
		var asSlice []string
		if err := json.Unmarshal(rawValue, &asSlice); err == nil {
			out[key] = append(out[key], asSlice...)
			continue
		}

		var asMap map[string]bool
		if err := json.Unmarshal(rawValue, &asMap); err != nil {
			return nil, err
		}

		for k, enabled := range asMap {
			if key == "label" || enabled {
				out[key] = append(out[key], k)
			}
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
	if !matchesContainerIDFilter(f["id"], rec.ID) {
		return false
	}
	if !f.anyMatch("name", rec.Name, "/"+rec.Name) {
		return false
	}
	// "ancestor" matches the container's image ref — exact match or with an
	// implicit ":latest" tag stripped on either side.
	if vals := f["ancestor"]; len(vals) > 0 {
		matched := false
		for _, v := range vals {
			if ancestorsMatch(rec.Image, v) {
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

func matchesContainerIDFilter(filters []string, id string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, want := range filters {
		if want == id || strings.HasPrefix(id, want) {
			return true
		}
	}
	return false
}

// ancestorsMatch compares an image reference coming from a container record against
// a user-provided filter value, accepting canonical and registry-stripped forms.
func ancestorsMatch(recordImage, filter string) bool {
	left := imageRefCandidates(recordImage)
	right := imageRefCandidates(filter)
	for l := range left {
		if _, ok := right[l]; ok {
			return true
		}
	}
	return false
}

func imageRefCandidates(ref string) map[string]struct{} {
	norm := normalizeImageRef(ref)
	cands := map[string]struct{}{
		norm: {},
	}
	short := shortenImageRef(norm)
	cands[short] = struct{}{}
	return cands
}

func shortenImageRef(ref string) string {
	// Strip registry if it looks like a host (contains '.' or ':').
	if i := strings.Index(ref, "/"); i != -1 {
		prefix := ref[:i]
		if strings.Contains(prefix, ".") || strings.Contains(prefix, ":") {
			ref = ref[i+1:]
		}
	}
	ref = strings.TrimPrefix(ref, "library/")
	return ref
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

// matchesNetworkFilters applies /networks filter keys (driver, id, name,
// scope, type, label) to a single network record.
func matchesNetworkFilters(n *store.NetworkRecord, f listFilters) bool {
	if !f.anyMatch("driver", n.Driver) {
		return false
	}
	if !f.anyMatch("id", n.ID) {
		return false
	}
	if !f.anyMatch("name", n.Name) {
		return false
	}
	if !f.anyMatch("scope", n.Scope) {
		return false
	}
	// Docker's "type" filter bucketises into "builtin" vs "custom". We don't
	// track a bucket, so custom networks always match "custom" and the
	// default gow network is "builtin".
	if vals := f["type"]; len(vals) > 0 {
		bucket := "custom"
		if n.Name == "gow" {
			bucket = "builtin"
		}
		if !f.anyMatch("type", bucket) {
			return false
		}
	}
	if !matchesLabelFilter(f["label"], n.Labels) {
		return false
	}
	return true
}

// parsePruneUntil decodes Docker's "until" prune filter — either an RFC3339
// timestamp or an epoch. Returns the latest creation time a resource can
// carry to remain eligible for pruning; nil means no constraint.
func parsePruneUntil(filters listFilters) (*time.Time, error) {
	vals := filters["until"]
	if len(vals) == 0 {
		return nil, nil
	}
	// Docker only supports one `until` value; keep the last one if
	// duplicates sneak in.
	raw := vals[len(vals)-1]
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, nil
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		t := time.Unix(secs, 0).UTC()
		return &t, nil
	}
	return nil, fmt.Errorf("invalid until value %q", raw)
}

// pruneEligible returns true when a resource with the given creation time
// and labels should be included in a prune sweep: its created time must be
// before `until` (if set) and every `label` entry must match.
func pruneEligible(created time.Time, labels map[string]string, filters listFilters, until *time.Time) bool {
	if until != nil && created.After(*until) {
		return false
	}
	if !matchesLabelFilter(filters["label"], labels) {
		return false
	}
	return true
}

// matchesVolumeFilters applies /volumes filter keys (driver, name, label,
// dangling) to a single volume record. dangling=true keeps volumes with no
// referencing container; refCount is supplied by the caller.
func matchesVolumeFilters(v *store.VolumeRecord, refCount int, f listFilters) bool {
	if !f.anyMatch("driver", orDefault(v.Driver, "local")) {
		return false
	}
	if !f.anyMatch("name", v.Name) {
		return false
	}
	if !matchesLabelFilter(f["label"], v.Labels) {
		return false
	}
	if vals := f["dangling"]; len(vals) > 0 {
		want := false
		for _, val := range vals {
			if val == "true" || val == "1" {
				want = true
			}
		}
		isDangling := refCount == 0
		if want && !isDangling {
			return false
		}
		if !want && isDangling {
			return false
		}
	}
	return true
}
