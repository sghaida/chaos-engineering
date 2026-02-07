package controller

import (
	"maps"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
)

// sanitizeName normalizes an arbitrary string into a DNS-like, lowercase, dash-separated name.
//
// It is used anywhere user-provided values (for example, action names) must be converted
// into stable, Kubernetes/Istio-safe identifiers such as:
//
//   - managed VirtualService names (e.g., fi-<fi-name>-<action>)
//   - injected HTTP rule names (used for prefix-based cleanup and idempotent patching)
//   - labels/keys that must remain deterministic across reconciles
//
// Normalization rules:
//   - Lowercases and trims surrounding whitespace.
//   - Replaces underscores and spaces with '-'.
//   - Replaces any rune outside [a-z0-9-] with '-'.
//   - Trims leading/trailing '-'.
//   - Returns "x" if the result is empty.
//
// These rules intentionally bias toward safety and determinism over preserving the
// exact original string.
func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, s)
	s = strings.Trim(s, "-")
	if s == "" {
		return "x"
	}
	return s
}

// joinKey builds a stable map/set key in the form "<namespace>/<name>".
//
// The controller uses this format to represent VirtualService targets and other
// namespaced resources in maps/sets (e.g., desiredByVSTarget, stillWanted).
//
// This is a convenience helper to ensure the key format is consistent everywhere.
func joinKey(ns, name string) string {
	return ns + "/" + name
}

// splitKey splits a "<namespace>/<name>" key into namespace and name.
//
// If the key does not contain a '/', it returns ("", key). Callers typically treat
// this as malformed input and either ignore it or let downstream lookups fail.
func splitKey(k string) (string, string) {
	parts := strings.SplitN(k, "/", 2)
	if len(parts) != 2 {
		return "", k
	}
	return parts[0], parts[1]
}

// toAnySlice converts a []string into a []any.
//
// This is primarily used when constructing unstructured objects (e.g., VirtualService
// specs) where fields are represented as map[string]any and list values are []any.
func toAnySlice(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

// uniqueAppend merges add into base, removes duplicates, and returns base sorted.
//
// Deterministic ordering is important for:
//   - stable diffs in GitOps workflows,
//   - predictable reconciliation results,
//   - avoiding spurious updates caused by non-deterministic slice order.
//
// If base already contains a value from add, it is not appended again.
func uniqueAppend(base []string, add []string) []string {
	set := map[string]struct{}{}
	for _, b := range base {
		set[b] = struct{}{}
	}
	for _, a := range add {
		if _, ok := set[a]; ok {
			continue
		}
		base = append(base, a)
		set[a] = struct{}{}
	}
	sort.Strings(base)
	return base
}

// getString retrieves a string value from a map[string]any.
//
// It returns "" when the key is missing or when the value is not a string.
// This helper keeps unstructured field access concise and avoids repetitive type asserts.
func getString(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// cloneAnySlice returns a shallow copy of a []any.
//
// This prevents sharing backing arrays between "desired" generated state and "existing"
// state pulled from the API server, which can cause accidental in-place mutations.
//
// Notes:
//   - Elements are not deep-copied. If elements include nested maps/slices and you need
//     full isolation, introduce a recursive deep-clone.
func cloneAnySlice(in []any) []any {
	// shallow clone elements; for our purpose it’s enough (dest maps remain shared).
	// If you want deep clone, we can recursively copy.
	return append([]any(nil), in...)
}

// cloneMap returns a shallow copy of a map[string]any.
//
// This is used when taking a base rule/template and modifying it to produce a desired
// rule without mutating the original input map.
//
// Notes:
//   - Nested maps/slices are not deep-copied.
func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

// event emits a Kubernetes Event using the controller's Recorder.
//
// Parameters:
//   - regarding: the object the event is about (typically *FaultInjection).
//   - eventType: usually "Normal" or "Warning".
//   - reason: a short, stable, CamelCase-ish reason string for filtering/grouping.
//   - action: an optional action/category string (used as a secondary classifier).
//   - note: a human-readable message.
//
// If Recorder is nil, event does nothing.
//
//nolint:unparam // eventType kept for future extensibility ("Normal"/"Warning")
func (r *FaultInjectionReconciler) event(
	regarding runtime.Object,
	eventType, reason, action, note string,
) {
	if r.Recorder == nil {
		return
	}
	// new events API: (regarding, related, type, reason, action, noteFmt, ...)
	r.Recorder.Eventf(regarding, nil, eventType, reason, action, "%s", note)
}

// eventf emits a formatted Kubernetes Event using the controller's Recorder.
//
// This is a printf-style variant of event, preferred when callers already have
// dynamic details to include in the message.
//
// If Recorder is nil, eventf does nothing.
func (r *FaultInjectionReconciler) eventf(
	regarding runtime.Object,
	eventType, reason, action, noteFmt string,
	args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(regarding, nil, eventType, reason, action, noteFmt, args...)
}
