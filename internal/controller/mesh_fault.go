package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	chaosv1alpha1 "github.com/sghaida/fi-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// INBOUNDDirection Direction constants
	INBOUNDDirection = "INBOUND"
	// OUTBOUNDDirection Direction constants
	OUTBOUNDDirection = "OUTBOUND"
	// HTTPLatency  Action type constants
	HTTPLatency = "HTTP_LATENCY"
	// HTTPAbort  Action type constants
	HTTPAbort = "HTTP_ABORT"
)

// vsDesired describes the computed desired state for a VirtualService target.
//
// Rules are the injected HTTP route rules to be prepended into the target's .spec.http.
// Hosts applies only to managed outbound VirtualServices and is used to build the VS spec.
// Managed indicates whether the controller is allowed to create/delete the VirtualService.
type vsDesired struct {
	Rules   []map[string]any
	Hosts   []string
	Managed bool
}

// buildDesiredByVSTarget groups all desired injected HTTP rules by VirtualService target.
//
// It returns:
//   - byTarget: map keyed by "<namespace>/<virtualservice-name>" containing desired rules and metadata
//   - managed: set of keys for managed outbound VirtualServices that should exist for this spec
//
// For each FaultInjection action:
//   - INBOUND: the target is fi.Namespace/<VirtualServiceRef.Name>
//   - OUTBOUND: the target is fi.Namespace/<managedOutboundVSName(...)>, marked Managed=true,
//     and Hosts accumulates DestinationHosts.
//
// Rules are sorted stably by rule name to ensure deterministic application.
func (r *FaultInjectionReconciler) buildDesiredByVSTarget(fi *chaosv1alpha1.FaultInjection) (map[string]vsDesired, map[string]struct{}) {
	byTarget := map[string]vsDesired{}
	managed := map[string]struct{}{}

	for _, a := range fi.Spec.Actions.MeshFaults {
		switch a.Direction {
		case INBOUNDDirection:
			vsName := a.HTTP.VirtualServiceRef.Name
			key := joinKey(fi.Namespace, vsName)

			d := byTarget[key]
			rule := buildInjectedHTTPRule(fi, &a) // no route yet; will be filled from base route during patch
			d.Rules = append(d.Rules, rule)
			byTarget[key] = d

		case OUTBOUNDDirection:
			vsName := managedOutboundVSName(fi, &a)
			key := joinKey(fi.Namespace, vsName)

			d := byTarget[key]
			d.Managed = true
			d.Hosts = uniqueAppend(d.Hosts, a.HTTP.DestinationHosts)
			rule := buildInjectedHTTPRule(fi, &a)
			d.Rules = append(d.Rules, rule)
			byTarget[key] = d
			managed[key] = struct{}{}
		}
	}

	for k, d := range byTarget {
		sort.SliceStable(d.Rules, func(i, j int) bool {
			ni := getString(d.Rules[i], "name")
			nj := getString(d.Rules[j], "name")
			return ni < nj
		})
		byTarget[k] = d
	}

	return byTarget, managed
}

// getOrCreateVirtualService returns the target VirtualService as an Unstructured object.
//
// Behavior:
//   - If the VirtualService exists, it is returned with created=false.
//   - If it does not exist and desired.Managed is false, an error is returned.
//     (INBOUND targets must already exist.)
//   - If it does not exist and desired.Managed is true, a new VirtualService is created
//     with labels and ownerRef pointing to the FaultInjection, and created=true is returned.
//
// Managed outbound VirtualServices are created with a minimal valid spec containing:
//   - spec.hosts populated from desired.Hosts
//   - spec.http containing a single default route rule, so that Istio validation passes
//     even before injected rules are prepended.
func (r *FaultInjectionReconciler) getOrCreateVirtualService(
	ctx context.Context,
	fi *chaosv1alpha1.FaultInjection,
	vsNS, vsName string,
	desired vsDesired,
) (*unstructured.Unstructured, bool, error) {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(virtualServiceGVK())

	err := r.Get(ctx, types.NamespacedName{Namespace: vsNS, Name: vsName}, vs)
	if err == nil {
		return vs, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	if !desired.Managed {
		return nil, false, fmt.Errorf("referenced VirtualService %s/%s not found", vsNS, vsName)
	}

	newVS := &unstructured.Unstructured{}
	newVS.SetGroupVersionKind(virtualServiceGVK())
	newVS.SetNamespace(vsNS)
	newVS.SetName(vsName)
	newVS.SetLabels(map[string]string{
		"managed-by":          "fi-operator",
		"chaos.sghaida.io/fi": fi.Name,
	})

	// IMPORTANT: managed VS must be valid even before prepending faults:
	// Create with one default route rule so Istio validation always passes.
	spec := map[string]any{
		"hosts":    toAnySlice(desired.Hosts),
		"gateways": []any{"mesh"}, // ensures outbound VS is attached to the sidecar mesh gateway
		"http": []any{
			map[string]any{
				"name":  fmt.Sprintf("default-%s", fi.Name),
				"match": []any{map[string]any{"uri": map[string]any{"prefix": "/"}}},
				"route": buildManagedOutboundDefaultRoute(desired.Hosts),
			},
		},
	}
	newVS.Object["spec"] = spec

	if err := controllerutil.SetControllerReference(fi, newVS, r.Scheme); err != nil {
		return nil, false, err
	}
	if err := r.Create(ctx, newVS); err != nil {
		return nil, false, err
	}

	return newVS, true, nil
}

// ensureManagedOutboundVSGatewaysMesh ensures managed outbound VirtualServices attach to the mesh gateway.
//
// For managed VS, set spec.gateways=["mesh"] if it is missing.
// For non-managed VS, do nothing.
//
// It returns changed=true if it modified the object in-memory.
func ensureManagedOutboundVSGatewaysMesh(vs *unstructured.Unstructured, managed bool) bool {
	if !managed {
		return false
	}

	spec, ok := vs.Object["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		vs.Object["spec"] = spec
	}

	if gw, ok := spec["gateways"]; ok {
		// If already includes mesh, do nothing.
		if arr, ok := gw.([]any); ok {
			for _, v := range arr {
				if s, ok := v.(string); ok && s == "mesh" {
					return false
				}
			}
		}
	}

	spec["gateways"] = []any{"mesh"}
	vs.Object["spec"] = spec
	return true
}

// virtualServiceGVK returns the GroupVersionKind for Istio VirtualService objects
// reconciled by this controller.
//
// This controller targets networking.istio.io/v1beta1 VirtualService.
func virtualServiceGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1beta1",
		Kind:    "VirtualService",
	}
}

// patchVirtualServiceHTTP updates vs.spec.http to contain desiredRules prepended,
// after removing any previously injected rules whose name begins with injectedPrefix.
//
// The function is purely in-memory; callers must persist changes by calling Update.
//
// It returns changed=true when the effective .spec.http list differs from the current
// value. Non-map items inside the http list are preserved as-is.
func patchVirtualServiceHTTP(vs *unstructured.Unstructured, injectedPrefix string, desiredRules []map[string]any) bool {
	spec, ok := vs.Object["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
	}

	var existing []any
	if v, ok := spec["http"]; ok {
		if arr, ok := v.([]any); ok {
			existing = arr
		}
	}

	filtered := make([]any, 0, len(existing))
	for _, item := range existing {
		rule, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		name, _ := rule["name"].(string)
		if strings.HasPrefix(name, injectedPrefix) {
			continue
		}
		filtered = append(filtered, item)
	}

	newHTTP := make([]any, 0, len(desiredRules)+len(filtered))
	for _, rr := range desiredRules {
		newHTTP = append(newHTTP, rr)
	}
	newHTTP = append(newHTTP, filtered...)

	changed := !deepEqualHTTP(existing, newHTTP)
	spec["http"] = newHTTP
	vs.Object["spec"] = spec
	return changed
}

// buildInjectedHTTPRule constructs an Istio VirtualService HTTP rule that injects a fault.
//
// Important: the rule is built WITHOUT a "route" field. The route must be attached later
// during reconciliation using ensureHTTPRuleHasRoute, by cloning a base route from the
// existing VirtualService (INBOUND) or a generated default route (OUTBOUND).
//
// The resulting rule includes:
//   - name: a stable identifier derived from FaultInjection and action name
//   - match: one or more matchers derived from action HTTP routes (uri prefix/exact,
//     optional exact-match headers, optional sourceLabels for OUTBOUND)
//   - fault: either delay or abort, with percentage
//   - timeout: optional top-level timeout for the HTTP rule (if configured)
//
// Supported action types:
//   - HTTP_LATENCY: maps to spec.http[*].fault.delay.fixedDelay + percentage.value
//   - HTTP_ABORT: maps to spec.http[*].fault.abort.httpStatus + percentage.value
func buildInjectedHTTPRule(fi *chaosv1alpha1.FaultInjection, a *chaosv1alpha1.MeshFaultAction) map[string]any {
	ruleName := injectedRuleName(fi, a)

	matches := make([]any, 0, len(a.HTTP.Routes))
	for _, rt := range a.HTTP.Routes {
		m := rt.Match
		match := map[string]any{}

		if strings.TrimSpace(m.URIPrefix) != "" {
			match["uri"] = map[string]any{"prefix": m.URIPrefix}
		} else if strings.TrimSpace(m.URIExact) != "" {
			match["uri"] = map[string]any{"exact": m.URIExact}
		}

		if len(m.Headers) > 0 {
			h := map[string]any{}
			for k, v := range m.Headers {
				h[k] = map[string]any{"exact": v}
			}
			match["headers"] = h
		}

		if a.Direction == OUTBOUNDDirection && a.HTTP.SourceSelector != nil && len(a.HTTP.SourceSelector.MatchLabels) > 0 {
			match["sourceLabels"] = a.HTTP.SourceSelector.MatchLabels
		}

		matches = append(matches, match)
	}

	fault := map[string]any{}
	switch a.Type {
	case "HTTP_LATENCY":
		fault["delay"] = map[string]any{
			"fixedDelay": fmt.Sprintf("%ds", a.HTTP.Delay.FixedDelaySeconds),
			"percentage": map[string]any{"value": a.Percent},
		}
	case "HTTP_ABORT":
		fault["abort"] = map[string]any{
			"httpStatus": a.HTTP.Abort.HTTPStatus,
			"percentage": map[string]any{"value": a.Percent},
		}
	}

	rule := map[string]any{
		"name":  ruleName,
		"match": matches,
		"fault": fault,
	}

	// timeout is a top-level field of the http route rule
	if a.HTTP.Timeout != nil && a.HTTP.Timeout.TimeoutSeconds > 0 {
		rule["timeout"] = fmt.Sprintf("%ds", a.HTTP.Timeout.TimeoutSeconds)
	}

	return rule
}

// --- route handling helpers ---

// findAnyExistingRoute scans vs.spec.http and returns the first "route" list it finds.
//
// This is used to ensure injected HTTP rules remain valid according to Istio schema:
// an HTTP rule typically must contain a route/redirect/direct_response.
//
// The returned slice is a cloned []any to avoid sharing mutable backing arrays.
func findAnyExistingRoute(vs *unstructured.Unstructured) ([]any, bool) {
	spec, ok := vs.Object["spec"].(map[string]any)
	if !ok {
		return nil, false
	}

	httpList, ok := spec["http"].([]any)
	if !ok {
		return nil, false
	}

	for _, item := range httpList {
		rule, ok := item.(map[string]any)
		if !ok {
			continue
		}

		// route
		if rt, ok := rule["route"].([]any); ok && len(rt) > 0 {
			return cloneAnySlice(rt), true
		}

		// redirect / direct_response are valid too, but harder to reuse generally.
		// If you want: handle these as well by returning a sentinel.
	}

	return nil, false
}

// ensureHTTPRuleHasRoute guarantees rule contains a routable action.
//
// If rule already defines one of:
//   - route
//   - redirect
//   - direct_response
//
// it is left unchanged.
//
// Otherwise, it sets rule["route"] to a clone of baseRoute.
func ensureHTTPRuleHasRoute(rule map[string]any, baseRoute []any) {
	if _, ok := rule["route"]; ok {
		return
	}
	if _, ok := rule["redirect"]; ok {
		return
	}
	if _, ok := rule["direct_response"]; ok {
		return
	}

	// clone as we should not share mutable backing slices
	rule["route"] = cloneAnySlice(baseRoute)
}

// buildManagedOutboundDefaultRoute builds a minimal default route list for managed OUTBOUND VirtualServices.
//
// The route points to the first host in hosts. The VirtualService .spec.hosts already
// scopes the host selection; setting destination.host to one host is sufficient for a
// default catch-all route.
//
// Port is omitted intentionally, as external services often rely on ServiceEntry +
// DestinationRule for port and traffic policy.
//
// If hosts is empty, destination.host will be the empty string; callers should ensure
// hosts is non-empty via validation or desired state construction.
func buildManagedOutboundDefaultRoute(hosts []string) []any {
	dstHost := ""
	if len(hosts) > 0 {
		dstHost = hosts[0]
	}

	// Port omitted: for external services, you often rely on ServiceEntry + DR.
	return []any{
		map[string]any{
			"destination": map[string]any{
				"host": dstHost,
			},
		},
	}
}

// ensureVSHasAtLeastOneDefaultRouteRule ensures a managed VirtualService is schema-valid.
//
// If vs.spec.http is missing or empty, it inserts a single catch-all default route rule.
// This is mainly defensive: managed VS should be created with a default rule, but this
// helper guarantees validity if the resource was modified externally or created empty.
func ensureVSHasAtLeastOneDefaultRouteRule(vs *unstructured.Unstructured, hosts []string) {
	spec, ok := vs.Object["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
	}

	httpList, _ := spec["http"].([]any)
	if len(httpList) > 0 {
		return
	}

	spec["http"] = []any{
		map[string]any{
			"name":  "default",
			"match": []any{map[string]any{"uri": map[string]any{"prefix": "/"}}},
			"route": buildManagedOutboundDefaultRoute(hosts),
		},
	}
	vs.Object["spec"] = spec
}

// cleanupOrphanedManagedVS deletes managed outbound VirtualServices that are no longer desired.
//
// Inputs:
//   - stillWanted: set of "<namespace>/<vs-name>" keys that should exist after reconciliation.
//
// Behavior:
//   - Lists VirtualServices in fi.Namespace labeled:
//   - managed-by=fi-operator
//   - chaos.sghaida.io/fi=<fi.Name>
//   - Deletes any listed VS not found in stillWanted.
//   - Ignores NotFound on delete.
//
// Return:
//   - error on list failure or delete failures (except NotFound).
func (r *FaultInjectionReconciler) cleanupOrphanedManagedVS(ctx context.Context, fi *chaosv1alpha1.FaultInjection, stillWanted map[string]struct{}) error {
	var vsList unstructured.UnstructuredList
	vsList.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1beta1", Kind: "VirtualServiceList"})

	if err := r.List(
		ctx,
		&vsList,
		client.InNamespace(fi.Namespace),
		client.MatchingLabels{
			"managed-by":          "fi-operator",
			"chaos.sghaida.io/fi": fi.Name,
		},
	); err != nil {
		return err
	}

	for _, vs := range vsList.Items {
		key := joinKey(vs.GetNamespace(), vs.GetName())
		if _, ok := stillWanted[key]; ok {
			continue
		}

		item := vs.DeepCopy()
		if err := r.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		r.eventf(
			fi, "Normal", "OrphanVirtualServiceDeleted", "cleanup-orphans",
			"Deleted orphaned managed VirtualService %s/%s", item.GetNamespace(), item.GetName(),
		)
	}

	return nil
}

// injectedRuleName returns the stable injected HTTP rule name for an action.
//
// The name includes the FaultInjection prefix, the action direction, and a sanitized action name.
// It must start with fiRuleNamePrefix(fi) so patchVirtualServiceHTTP can remove it.
func injectedRuleName(fi *chaosv1alpha1.FaultInjection, a *chaosv1alpha1.MeshFaultAction) string {
	// Must start with fiRuleNamePrefix(fi) so patchVirtualServiceHTTP can remove it.
	return fmt.Sprintf("%s%s-%s",
		fiRuleNamePrefix(fi),
		strings.ToLower(a.Direction),
		sanitizeName(a.Name),
	)
}

// managedOutboundVSName returns the name of the managed outbound VirtualService for an OUTBOUND action.
//
// The name is derived from the FaultInjection name and the sanitized action name.
// This provides isolated resources per action and allows safe create/delete under FI ownership.

func managedOutboundVSName(fi *chaosv1alpha1.FaultInjection, a *chaosv1alpha1.MeshFaultAction) string {
	return fmt.Sprintf("fi-%s-%s", fi.Name, sanitizeName(a.Name))
}

// deepEqualHTTP performs a conservative equality check on two VirtualService .spec.http lists.
//
// Implementation:
//   - Compares fmt.Sprintf("%v", slice) values.
//   - This is sufficient for deterministic objects where ordering is controlled,
//     but is not a semantic deep-equal.
//
// Notes:
//   - If stricter comparisons are needed, replace with canonical JSON serialization
//     or a deterministic deep-equal.
func deepEqualHTTP(a, b []any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}
