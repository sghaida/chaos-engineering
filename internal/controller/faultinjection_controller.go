// Package controller contains the Kubernetes controller-runtime reconciler that
// implements the FaultInjection custom resource for Istio/Envoy HTTP fault injection.
//
// # Overview
//
// The controller watches FaultInjection objects and reconciles them into concrete
// Istio VirtualService HTTP rules that inject delay/abort faults.
//
// Key behaviors:
//
//   - Lifecycle: The first reconcile sets Status.StartedAt and Status.ExpiresAt.
//     The controller requeues until ExpiresAt, then automatically cleans up injected
//     rules and marks the experiment Completed.
//
//   - Guardrails: The reconciler validates spec constraints (duration, traffic
//     percentage bounds, required fields per action type/direction, and optional
//     maxPodsAffected enforcement). Invalid specs fail closed: injected rules are
//     removed and the FaultInjection is moved to Error phase.
//
//   - Idempotent rule application: Desired injected rules are deterministically
//     generated and prepended to the target VirtualService .spec.http list. Any
//     previously injected rules (identified by name prefix) are removed first, so
//     re-applying produces stable outcomes.
//
//   - Targeting model:
//
//   - INBOUND actions patch an existing VirtualService referenced by name.
//
//   - OUTBOUND actions create/manage a dedicated VirtualService owned by the
//     FaultInjection CR (labels + ownerRef). This allows safe creation/deletion
//     without impacting unrelated resources.
//
//   - Safety around routing: Istio requires each HTTP rule to be valid (typically
//     containing "route", "redirect", or "direct_response"). Injected rules are
//     created without "route" and later populated by cloning an existing route
//     from the target VirtualService (INBOUND), or by generating a default route
//     for managed outbound VirtualServices (OUTBOUND). If an INBOUND target has
//     no routable rule anywhere, reconciliation fails closed to avoid creating an
//     invalid VirtualService.
//
// # Resource ownership and cleanup
//
// Managed outbound VirtualServices are labeled:
//   - managed-by=fi-operator
//   - chaos.sghaida.io/fi=<FaultInjection name>
//
// They also have an owner reference to the FaultInjection, so Kubernetes garbage
// collection can clean them up if the CR is deleted. Additionally, this controller
// removes injected rules on expiry/error and deletes orphaned managed VS that are
// no longer desired by the current spec.
package controller

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	chaosv1alpha1 "github.com/sghaida/fi-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// ErrorPhase indicates the FaultInjection is in an error state.
	ErrorPhase = "Error"
	// RunningPhase indicates the FaultInjection is currently running.
	RunningPhase = "Running"
	// CompletedPhase indicates the FaultInjection has completed successfully.
	CompletedPhase = "Completed"
	// INBOUNDDirection Direction constants
	INBOUNDDirection = "INBOUND"
	// OUTBOUNDDirection Direction constants
	OUTBOUNDDirection = "OUTBOUND"
	// HTTPLatency  Action type constants
	HTTPLatency = "HTTP_LATENCY"
	// HTTPAbort  Action type constants
	HTTPAbort = "HTTP_ABORT"
)

// FaultInjectionReconciler reconciles chaos.sghaida.io/v1alpha1 FaultInjection resources.
//
// It renders FaultInjection.Spec.Actions.MeshFaults into Istio VirtualService HTTP rules.
// For INBOUND actions, it patches an existing VirtualService referenced by the spec.
// For OUTBOUND actions, it creates and manages a dedicated VirtualService owned by the
// FaultInjection (labels + ownerRef) and patches that resource.
//
// The reconciler is designed to be:
//
//   - Safe-by-default: on invalid specs or unsafe targets, it cleans up injected rules
//     and reports Error without leaving partial state behind.
//
//   - Idempotent: repeated reconciles produce the same VirtualService state.
//
//   - Time-bounded: it automatically expires experiments at Status.ExpiresAt and
//     performs cleanup.
type FaultInjectionReconciler struct {
	// Client is a controller-runtime client used to read/write Kubernetes objects.
	client.Client

	// Scheme is the runtime scheme used to set controller references for managed resources.
	Scheme *runtime.Scheme
}

// Reconcile performs reconciliation for a single FaultInjection resource.
//
// RBAC markers for kubebuilder/controller-gen:
//
//   - FaultInjection CRD read/write + status updates + finalizers
//   - Istio VirtualService read/write for applying/creating fault rules
//   - Pod list access for enforcing blastRadius.maxPodsAffected
//   - Deployment list/delete for demo workload cleanup
//
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;delete
//
// Reconcile ensures the desired fault-injection rules are applied to the correct VirtualService targets and that experiments are automatically cleaned up after expiry.
//
// Reconciliation flow:
//
//  1. Fetch FaultInjection. If not found, exit.
//  2. Initialize lifecycle timestamps (Status.StartedAt, Status.ExpiresAt) on first run.
//  3. If expired (now > ExpiresAt): remove injected rules and delete managed VS,
//     then mark the experiment Completed.
//  4. Validate guardrails (duration, traffic caps, required fields, optional
//     maxPodsAffected). On failure, cleanup and mark Error.
//  5. Build desired injected rules grouped by VirtualService target.
//  6. For each target VirtualService:
//     - Get or create (only for managed outbound VS).
//     - Determine a base route to satisfy Istio validation.
//     - Attach that route to each injected rule (if missing).
//     - Patch the VS .spec.http list: remove prior injected rules and prepend desired.
//     - Update the VS if changed.
//  7. Delete orphaned managed outbound VirtualServices that are no longer desired.
//  8. Mark FaultInjection Running and requeue until expiry.
//
// Return value:
//   - On success, returns a Result that requeues after time.Until(ExpiresAt).
//   - On terminal conditions (Completed/Error), typically returns no requeue.
//
// Note: This controller intentionally stores injected rules by name prefix. It does not
// attempt to merge or diff arbitrary user-authored rules beyond removing prior injected
// entries and prepending desired rules.
func (r *FaultInjectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fi chaosv1alpha1.FaultInjection
	if err := r.Get(ctx, req.NamespacedName, &fi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1) Lifecycle timestamps
	now := time.Now()
	if fi.Status.StartedAt == nil {
		t := metav1.NewTime(now)
		fi.Status.StartedAt = &t
	}
	expiresAt := fi.Status.StartedAt.Add(time.Duration(fi.Spec.BlastRadius.DurationSeconds) * time.Second)
	t := metav1.NewTime(expiresAt)
	fi.Status.ExpiresAt = &t

	// Expired => cleanup
	if now.After(fi.Status.ExpiresAt.Time) {
		log.Info("experiment expired; cleaning up", "name", fi.Name, "ns", fi.Namespace)
		if err := r.cleanupAll(ctx, &fi); err != nil {
			return ctrl.Result{}, err
		}
		fi.Status.Phase = "Completed"
		fi.Status.Message = "Expired: cleaned up injected rules"
		_ = r.Status().Update(ctx, &fi)
		return ctrl.Result{}, nil
	}

	// 2) Guardrails
	if err := r.validateSpec(ctx, &fi); err != nil {
		_ = r.cleanupAll(ctx, &fi)
		fi.Status.Phase = ErrorPhase
		fi.Status.Message = err.Error()
		_ = r.Status().Update(ctx, &fi)
		return ctrl.Result{}, nil
	}

	// 3) Desired rules grouped by VS
	desiredByVSTarget, managedVSNames := r.buildDesiredByVSTarget(&fi)

	// 4) Apply patches (prepend injected rules)
	for targetKey, desired := range desiredByVSTarget {
		vsNS, vsName := splitKey(targetKey)

		vs, created, err := r.getOrCreateVirtualService(ctx, &fi, vsNS, vsName, desired)
		if err != nil {
			fi.Status.Phase = ErrorPhase
			fi.Status.Message = fmt.Sprintf("failed getting/creating VirtualService %s/%s: %v", vsNS, vsName, err)
			_ = r.Status().Update(ctx, &fi)
			return ctrl.Result{}, err
		}

		// IMPORTANT:
		// For managed OUTBOUND VirtualServices, explicitly attach to the mesh gateway.
		// This avoids ambiguity where a VirtualService with hosts+http rules exists but is not applied to sidecar egress traffic.
		meshGatewayChanged := ensureManagedOutboundVSGatewaysMesh(vs, desired.Managed)

		// IMPORTANT:
		// We must ensure each injected HTTP rule has a valid "route" (or redirect/direct_response).
		// For INBOUND we clone a "base route" from the existing VS.
		baseRoute, ok := findAnyExistingRoute(vs)
		if !ok {
			// If VS was just created and has empty http rules, we still must have a route.
			// For managed (OUTBOUND) VS, build a default route to the destination hosts.
			// For referenced INBOUND VS with no route anywhere, fail closed.
			if desired.Managed {
				baseRoute = buildManagedOutboundDefaultRoute(desired.Hosts)
				ensureVSHasAtLeastOneDefaultRouteRule(vs, desired.Hosts)
			} else {
				fi.Status.Phase = ErrorPhase
				fi.Status.Message = fmt.Sprintf("target VirtualService %s/%s has no route/redirect/direct_response in any http rule; cannot inject faults safely", vsNS, vsName)
				_ = r.Status().Update(ctx, &fi)
				return ctrl.Result{}, nil
			}
		}

		// Attach route to each desired rule (if missing) and keep timeout at correct level.
		desiredRules := make([]map[string]any, 0, len(desired.Rules))
		for _, rule := range desired.Rules {
			rule = cloneMap(rule)
			ensureHTTPRuleHasRoute(rule, baseRoute)
			desiredRules = append(desiredRules, rule)
		}

		changed := patchVirtualServiceHTTP(vs, fiRuleNamePrefix(&fi), desiredRules)

		if created || changed || meshGatewayChanged {
			if err := r.Update(ctx, vs); err != nil {
				fi.Status.Phase = "Error"
				fi.Status.Message = fmt.Sprintf("failed updating VirtualService %s/%s: %v", vsNS, vsName, err)
				_ = r.Status().Update(ctx, &fi)
				return ctrl.Result{}, err
			}
		}
	}

	// 5) Cleanup orphaned managed VS
	if err := r.cleanupOrphanedManagedVS(ctx, &fi, managedVSNames); err != nil {
		log.Error(err, "failed cleanupOrphanedManagedVS")
	}

	fi.Status.Phase = "Running"
	fi.Status.Message = "Active: injected rules are applied"
	_ = r.Status().Update(ctx, &fi)

	return ctrl.Result{RequeueAfter: time.Until(fi.Status.ExpiresAt.Time)}, nil
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
//
// It configures the controller to watch FaultInjection resources and enqueue
// reconcile requests for changes.
//
// The controller name is set to "faultinjection".
func (r *FaultInjectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&chaosv1alpha1.FaultInjection{}).
		Named("faultinjection").
		Complete(r)
}

// validateSpec enforces guardrails and basic correctness of the FaultInjection spec.
//
// It validates:
//
//   - blastRadius.durationSeconds >= 1
//   - each action.percent <= blastRadius.maxTrafficPercent
//   - action.type is supported and contains required type-specific fields
//     (HTTP_LATENCY requires http.delay.fixedDelaySeconds, HTTP_ABORT requires
//     http.abort.httpStatus)
//   - each route match has at least one URI selector (uriPrefix or uriExact)
//   - action.direction is supported and contains required direction-specific fields:
//   - INBOUND requires http.virtualServiceRef.name
//   - OUTBOUND requires http.destinationHosts
//   - optional blastRadius.maxPodsAffected, if set:
//   - requires http.sourceSelector.matchLabels for each action (to bound impact)
//   - counts pods in the FaultInjection namespace matching those labels
//   - errors if the count exceeds maxPodsAffected
//
// On error, Reconcile will attempt to clean up injected rules and mark the experiment Error.
func (r *FaultInjectionReconciler) validateSpec(ctx context.Context, fi *chaosv1alpha1.FaultInjection) error {
	br := fi.Spec.BlastRadius
	if br.DurationSeconds <= 0 {
		return fmt.Errorf("blastRadius.durationSeconds must be >= 1")
	}

	for _, a := range fi.Spec.Actions.MeshFaults {
		if a.Percent > br.MaxTrafficPercent {
			return fmt.Errorf("blastRadius exceeded: action %q percent=%d > maxTrafficPercent=%d", a.Name, a.Percent, br.MaxTrafficPercent)
		}

		switch a.Type {
		case HTTPLatency:
			if a.HTTP.Delay == nil || a.HTTP.Delay.FixedDelaySeconds <= 0 {
				return fmt.Errorf("action %q requires http.delay.fixedDelaySeconds for HTTP_LATENCY", a.Name)
			}
		case HTTPAbort:
			if a.HTTP.Abort == nil || a.HTTP.Abort.HTTPStatus < 100 {
				return fmt.Errorf("action %q requires http.abort.httpStatus for HTTP_ABORT", a.Name)
			}
		default:
			return fmt.Errorf("action %q has unsupported type %q", a.Name, a.Type)
		}

		for i, rt := range a.HTTP.Routes {
			m := rt.Match
			if strings.TrimSpace(m.URIPrefix) == "" && strings.TrimSpace(m.URIExact) == "" {
				return fmt.Errorf("action %q route[%d] invalid: either uriPrefix or uriExact must be set", a.Name, i)
			}
		}

		switch a.Direction {
		case INBOUNDDirection:
			if a.HTTP.VirtualServiceRef == nil || strings.TrimSpace(a.HTTP.VirtualServiceRef.Name) == "" {
				return fmt.Errorf("action %q (INBOUND) requires http.virtualServiceRef.name", a.Name)
			}
		case OUTBOUNDDirection:
			if len(a.HTTP.DestinationHosts) == 0 {
				return fmt.Errorf("action %q (OUTBOUND) requires http.destinationHosts", a.Name)
			}
		default:
			return fmt.Errorf("action %q has unsupported direction %q", a.Name, a.Direction)
		}

		// maxPodsAffected enforcement if configured
		if br.MaxPodsAffected > 0 {
			labels := map[string]string(nil)
			if a.HTTP.SourceSelector != nil {
				labels = a.HTTP.SourceSelector.MatchLabels
			}
			if len(labels) == 0 {
				return fmt.Errorf("blastRadius exceeded: maxPodsAffected=%d but action %q has no http.sourceSelector.matchLabels (cannot bound impact safely)", br.MaxPodsAffected, a.Name)
			}
			n, err := r.countPods(ctx, fi.Namespace, labels)
			if err != nil {
				return fmt.Errorf("failed to enforce maxPodsAffected for action %q: %v", a.Name, err)
			}
			if int64(n) > br.MaxPodsAffected {
				return fmt.Errorf("blastRadius exceeded: action %q would affect %d pods > maxPodsAffected=%d", a.Name, n, br.MaxPodsAffected)
			}
		}
	}

	return nil
}

// countPods lists pods in the given namespace that match the provided label selectors
// and returns the number of matching pods.
//
// This is used to enforce blastRadius.maxPodsAffected. If match is empty, the count
// applies to all pods in the namespace.
//
// Implementation detail: Pods are listed via UnstructuredList to avoid depending on
// typed corev1 imports in this controller package.
func (r *FaultInjectionReconciler) countPods(ctx context.Context, ns string, match map[string]string) (int, error) {
	var pods unstructured.UnstructuredList
	pods.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PodList"})

	opts := []client.ListOption{client.InNamespace(ns)}
	if len(match) > 0 {
		opts = append(opts, client.MatchingLabels(match))
	}

	if err := r.List(ctx, &pods, opts...); err != nil {
		return 0, err
	}
	return len(pods.Items), nil
}

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

	// IMPORTANT: managed VS must be valid even before we prepend faults:
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
// For managed VS, we set spec.gateways=["mesh"] if it is missing.
// For non-managed VS, we do nothing.
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

// deepEqualHTTP performs a conservative equality check on two unstructured HTTP rule lists.
//
// It uses a fmt.Sprintf-based comparison, which is not a true deep-equal but is
// sufficient for stable, deterministic objects where map iteration order is not
// relied upon for semantics. If you need stricter semantic comparison, replace this
// with a canonical JSON serialization or reflect.DeepEqual with deterministic maps.
func deepEqualHTTP(a, b []any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
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

		if a.Direction == "OUTBOUND" && a.HTTP.SourceSelector != nil && len(a.HTTP.SourceSelector.MatchLabels) > 0 {
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

	// clone so we never share mutable backing slices
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

// --- cleanup logic (unchanged) ---

// cleanupAll removes injected rules from all VirtualServices impacted by this FaultInjection
// and deletes any managed outbound VirtualServices owned by the FaultInjection.
//
// It determines targets by:
//   - reading the FaultInjection spec (INBOUND VirtualServiceRef + OUTBOUND managed names)
//   - listing VirtualServices labeled with chaos.sghaida.io/fi=<fi.Name> in the FI namespace
//
// For each target VirtualService:
//   - injected rules (name prefix fiRuleNamePrefix) are removed
//   - if the VS is managed-by=fi-operator and labeled for this FI, it is deleted
//   - otherwise, the patched VS is updated if it changed
//
// cleanupAll is used in two main scenarios:
//   - experiment expiry (mark Completed)
//   - guardrail failures (mark Error)
func (r *FaultInjectionReconciler) cleanupAll(ctx context.Context, fi *chaosv1alpha1.FaultInjection) error {
	targets := map[string]struct{}{}

	for _, a := range fi.Spec.Actions.MeshFaults {
		if a.Direction == "INBOUND" && a.HTTP.VirtualServiceRef != nil {
			targets[joinKey(fi.Namespace, a.HTTP.VirtualServiceRef.Name)] = struct{}{}
		}
		if a.Direction == "OUTBOUND" {
			targets[joinKey(fi.Namespace, managedOutboundVSName(fi, &a))] = struct{}{}
		}
	}

	var vsList unstructured.UnstructuredList
	vsList.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1beta1", Kind: "VirtualServiceList"})
	_ = r.List(ctx, &vsList, client.InNamespace(fi.Namespace), client.MatchingLabels{"chaos.sghaida.io/fi": fi.Name})

	for _, item := range vsList.Items {
		targets[joinKey(item.GetNamespace(), item.GetName())] = struct{}{}
	}

	prefix := fiRuleNamePrefix(fi)

	for k := range targets {
		ns, name := splitKey(k)

		vs := &unstructured.Unstructured{}
		vs.SetGroupVersionKind(virtualServiceGVK())
		if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, vs); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}

		changed := patchVirtualServiceHTTP(vs, prefix, nil)

		labels := vs.GetLabels()
		if labels != nil && labels["managed-by"] == "fi-operator" && labels["chaos.sghaida.io/fi"] == fi.Name {
			_ = r.Delete(ctx, vs)
			continue
		}

		if changed {
			if err := r.Update(ctx, vs); err != nil {
				return err
			}
		}
	}

	// Best-effort: delete "fault deployment" workloads created for the FI in this namespace.
	// This intentionally does not fail cleanup if deletion fails; we prefer to keep cleanup robust.
	_ = r.cleanupFIManagedDeployments(ctx, fi)

	return nil
}

// cleanupFIManagedDeployments deletes Deployments in fi.Namespace owned by the FaultInjection.
//
// This is used as part of cleanupAll to remove demo/test "fault deployments" created alongside the FI.
// It is best-effort: it returns an error only if the list call fails.
func (r *FaultInjectionReconciler) cleanupFIManagedDeployments(ctx context.Context, fi *chaosv1alpha1.FaultInjection) error {
	var depList unstructured.UnstructuredList
	depList.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DeploymentList"})

	// IMPORTANT (update):
	// Only delete Deployments that are *marked as* kind:FaultInjection via ownerReferences.
	// We additionally match on the owning FI name and UID to avoid deleting Deployments
	// owned by other FaultInjection objects in the same namespace.
	if err := r.List(ctx, &depList, client.InNamespace(fi.Namespace)); err != nil {
		return err
	}

	for _, item := range depList.Items {
		owners := item.GetOwnerReferences()
		if len(owners) == 0 {
			continue
		}

		ownedByThisFI := false
		for _, o := range owners {
			if o.Kind != "FaultInjection" {
				continue
			}

			// Be strict: match both name and UID when available.
			if o.Name == fi.Name {
				if fi.UID == "" || string(o.UID) == "" || o.UID == fi.UID {
					ownedByThisFI = true
					break
				}
			}
		}

		if !ownedByThisFI {
			continue
		}

		dep := item.DeepCopy()
		_ = r.Delete(ctx, dep) // best-effort
	}

	return nil
}

// cleanupOrphanedManagedVS deletes managed outbound VirtualServices for the given FaultInjection
// that are no longer desired by the current spec.
//
// stillWanted is a set of "<namespace>/<vs-name>" keys that should exist after reconciliation.
//
// The function lists managed VirtualServices in fi.Namespace labeled:
//   - managed-by=fi-operator
//   - chaos.sghaida.io/fi=<fi.Name>
//
// Any listed VS not present in stillWanted is deleted.
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
	}

	return nil
}

// --- naming + misc (unchanged) ---

// fiRuleNamePrefix returns the prefix used to identify all injected HTTP rules
// for the given FaultInjection.
//
// All injected rule names begin with "fi-<fi.Name>-", allowing patchVirtualServiceHTTP
// to remove prior injected rules during reconciliation and cleanup.
func fiRuleNamePrefix(fi *chaosv1alpha1.FaultInjection) string {
	return fmt.Sprintf("fi-%s-", fi.Name)
}

// injectedRuleName returns the unique, stable HTTP rule name for an injected action.
//
// Names are normalized to match Kubernetes/Istio naming constraints and to remain stable
// across reconciles.
func injectedRuleName(fi *chaosv1alpha1.FaultInjection, a *chaosv1alpha1.MeshFaultAction) string {
	return fmt.Sprintf("fi-%s-%s", fi.Name, sanitizeName(a.Name))
}

// managedOutboundVSName returns the name of the managed outbound VirtualService for an OUTBOUND action.
//
// The managed VirtualService name is derived from the FaultInjection name and action name.
// This ensures each action gets its own isolated VirtualService target if desired.
func managedOutboundVSName(fi *chaosv1alpha1.FaultInjection, a *chaosv1alpha1.MeshFaultAction) string {
	return fmt.Sprintf("fi-%s-%s", fi.Name, sanitizeName(a.Name))
}

// sanitizeName normalizes an arbitrary string into a DNS-like, lowercase, dash-separated name.
//
// It:
//   - lowercases and trims whitespace
//   - replaces underscores and spaces with '-'
//   - replaces any non [a-z0-9-] rune with '-'
//   - trims leading/trailing '-'
//   - returns "x" if the result is empty
//
// This is used to generate safe VirtualService and rule names from user-provided action names.
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

// joinKey constructs a stable "<namespace>/<name>" key used in maps/sets.
func joinKey(ns, name string) string {
	return ns + "/" + name
}

// splitKey splits a "<namespace>/<name>" key into namespace and name.
//
// If the key is malformed, it returns ("", key).
func splitKey(k string) (string, string) {
	parts := strings.SplitN(k, "/", 2)
	if len(parts) != 2 {
		return "", k
	}
	return parts[0], parts[1]
}

// toAnySlice converts a []string into []any for unstructured object construction.
func toAnySlice(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

// uniqueAppend merges add into base, removing duplicates, and returns a sorted slice.
//
// Sorting ensures deterministic VirtualService specs and stable diffs.
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

// getString fetches a string value from a map and returns "" if missing or not a string.
func getString(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

// --- small cloning helpers ---

// cloneAnySlice returns a shallow copy of the provided []any.
//
// This prevents sharing backing arrays between desired rules and existing rule state.
// Elements themselves are not deep-cloned (maps inside remain shared). If deep cloning
// is needed in the future, this function can be extended to recursively copy nested
// maps/slices.
func cloneAnySlice(in []any) []any {
	// shallow clone elements; for our purpose it’s enough (dest maps remain shared).
	// If you want deep clone, we can recursively copy.
	return append([]any(nil), in...)
}

// cloneMap returns a shallow copy of a map[string]any.
//
// Nested objects are not deep-copied.
func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}
