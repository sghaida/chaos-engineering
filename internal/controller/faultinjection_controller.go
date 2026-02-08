// Package controller contains the controller-runtime reconciler that implements the
// chaos.sghaida.io/v1alpha1 FaultInjection custom resource.
//
// It turns FaultInjection specs into *concrete, time-bounded* changes in the cluster:
//
//   - Mesh faults: inject Istio/Envoy HTTP faults by patching VirtualService HTTP rules.
//   - Pod faults: schedule pod-level disruptions via a per-tick Lease + Job executor,
//     with controller-side deterministic pod selection and status audit.
//
// # Overview
//
// The controller watches FaultInjection objects and reconciles them into:
//
//   - INBOUND mesh faults: prepend deterministic injected HTTP rules into an existing
//     VirtualService referenced by name.
//   - OUTBOUND mesh faults: create/manage a dedicated VirtualService owned by the
//     FaultInjection (labels + ownerRef) and patch that resource.
//   - Pod faults: create/manage per-tick coordination Leases and Jobs that execute
//     controller-selected pod deletions (or other pod actions as the executor evolves).
//
// The reconciler is safe-by-default and fails closed: if guardrails are violated or a
// target is unsafe, it removes injected state and moves the FaultInjection to Error.
//
// # Lifecycle
//
// On the first reconcile, the controller initializes the lifecycle timestamps:
//
//   - Status.StartedAt = now
//   - Status.ExpiresAt = StartedAt + spec.blastRadius.durationSeconds
//
// The controller requeues until ExpiresAt, then automatically cleans up injected state
// and marks the experiment Completed.
//
// Cancellation is immediate: if spec.cancel=true, the controller cleans up and marks
// the FaultInjection Cancelled.
//
// # Guardrails and validation
//
// The controller validates inputs defensively in addition to CRD schema/XValidation:
//
//   - Duration: blastRadius.durationSeconds must be >= 1.
//   - Traffic caps: each mesh action percent must be <= blastRadius.maxTrafficPercent.
//   - Required fields by mesh fault type:
//   - HTTP_LATENCY requires http.delay.fixedDelaySeconds > 0.
//   - HTTP_ABORT requires http.abort.httpStatus >= 100.
//   - Required routing selectors: each route match must specify uriPrefix or uriExact.
//   - Required fields by direction:
//   - INBOUND requires http.virtualServiceRef.name.
//   - OUTBOUND requires http.destinationHosts.
//   - Optional blastRadius.maxPodsAffected enforcement (mesh):
//   - requires http.sourceSelector.matchLabels to bound impact,
//   - counts matching pods in the FI namespace and fails if it exceeds the cap.
//   - Pod-fault scheduling invariants (windowing, selection mode, termination mode, guardrails).
//
// Invalid specs fail closed: injected rules are removed, managed artifacts are deleted,
// and the FaultInjection is moved to Error phase.
//
// # Idempotent application model
//
// Mesh rule application is deterministic and idempotent:
//
//   - Desired injected rules are generated deterministically from spec.
//   - Previously injected rules are identified by a stable name prefix (fi-<name>-).
//   - Reconciliation first removes previously injected rules, then prepends desired rules.
//   - Re-applying yields stable outcomes even with retries and concurrent reconciles.
//
// Pod-fault application is also deterministic and idempotent per (action,tick):
//
//   - The controller selects exact pod names deterministically (sorted candidates).
//   - A per-tick Lease ensures only one executor runs for a given (action,tick).
//   - A deterministic Job name is used; create is a no-op if it already exists.
//   - The controller records planned and executed pods in FI status for auditability.
//
// # Targeting model
//
// Mesh faults:
//
//   - INBOUND actions patch an existing VirtualService referenced by name.
//   - OUTBOUND actions create/manage a dedicated VirtualService owned by the FI,
//     allowing safe creation/deletion without impacting unrelated resources.
//
// Pod faults:
//
//   - Candidate pods are listed using label selectors in the target namespace.
//   - Exact targets are selected in the controller and passed to the Job (not discovered by the Job).
//
// # Routing safety
//
// Istio requires each HTTP rule to be valid (typically containing "route", "redirect",
// or "direct_response").
//
// Injected mesh rules are created without a route and then made valid:
//
//   - INBOUND: the controller clones a base route/redirect/direct_response from an
//     existing rule in the target VirtualService.
//   - OUTBOUND (managed VS): the controller ensures a default route exists and uses it.
//   - If an INBOUND target has no routable rule anywhere, reconciliation fails closed
//     to avoid producing an invalid VirtualService.
//
// # Resource ownership and cleanup
//
// Managed outbound VirtualServices are labeled:
//
//   - managed-by=fi-operator
//   - chaos.sghaida.io/fi=<FaultInjection name>
//
// They also have an owner reference to the FaultInjection, enabling garbage collection
// when the CR is deleted.
//
// Cleanup happens on expiry, cancellation, and validation errors:
//
//   - Removes injected HTTP rules from all affected VirtualServices (by prefix).
//   - Deletes managed outbound VirtualServices owned by the FaultInjection.
//   - Deletes orphaned managed outbound VirtualServices no longer desired by the spec.
//   - Best-effort cleanup for pod-fault artifacts (Jobs + Leases) labeled for the FI.
//   - Best-effort cleanup of FI-owned demo/test Deployments (ownerRef to FaultInjection)
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	chaosv1alpha1 "github.com/sghaida/fi-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// ErrorPhase indicates the FaultInjection is in an error state.
	ErrorPhase = "Error"
	// RunningPhase indicates the FaultInjection is currently running.
	RunningPhase = "Running"
	// CompletedPhase indicates the FaultInjection has completed successfully.
	CompletedPhase = "Completed"
	// CancelledPhase indicates the FaultInjection has been cancelled externally.
	CancelledPhase = "Cancelled"
	// HolderIdentity Operator name
	HolderIdentity = "fi-operator"
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

	// Recorder is an event recorder for emitting Kubernetes events.
	Recorder events.EventRecorder

	// HolderIdentity for Lease locking.
	// Needed to set Lease.Spec.HolderIdentity; typically controller Pod name or unique instance ID.
	HolderIdentity string
}

// Reconcile reconciles a single FaultInjection resource into the desired cluster state.
//
// High-level flow:
//  1. Fetch the FaultInjection (ignore NotFound).
//  2. Ensure lifecycle timestamps (Status.StartedAt, Status.ExpiresAt) exist and persist them.
//  3. If spec.cancel=true: cleanup immediately and mark Cancelled.
//  4. If expired (now > Status.ExpiresAt): cleanup and mark Completed.
//  5. Validate spec guardrails (mesh + pod faults). On failure: cleanup and mark Error.
//  6. Mesh: build desired injected rules grouped by VirtualService target and apply them.
//  7. Mesh: delete orphaned managed outbound VirtualServices no longer desired.
//  8. Mark FaultInjection Running (transition event emitted once).
//  9. Pod faults: schedule/execute due ticks (Lease + Job), and record planned/executed status.
//  10. Requeue based on the earliest of:
//     - experiment expiry (ExpiresAt), and
//     - next windowed pod-fault tick boundary (if any).
//
// Requeue semantics:
//   - Terminal outcomes (Completed/Cancelled/Error) typically return no requeue.
//   - Non-terminal success returns Result{RequeueAfter: ...} to drive windowed execution.
//
// Ownership model:
//   - This controller assumes full ownership of any injected HTTP rules it created,
//     identified by fiRuleNamePrefix(fi). It does not merge arbitrary user-authored rules;
//     it removes prior injected rules and prepends the desired injected rules each reconcile.
func (r *FaultInjectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fi chaosv1alpha1.FaultInjection
	if err := r.Get(ctx, req.NamespacedName, &fi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("reconcile",
		"name", fi.Name,
		"ns", fi.Namespace,
		"generation", fi.GetGeneration(),
		"resourceVersion", fi.GetResourceVersion(),
		"cancel", fi.Spec.Cancel,
		"durationSeconds", fi.Spec.BlastRadius.DurationSeconds,
		"phase", fi.Status.Phase,
		"startedAt", fi.Status.StartedAt,
		"expiresAt", fi.Status.ExpiresAt,
	)

	now := time.Now().UTC()

	lifecycleChanged := r.ensureLifecycle(log, &fi)
	if lifecycleChanged {
		// Persist StartedAt/ExpiresAt immediately (handles durationSeconds patch correctly)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var latest chaosv1alpha1.FaultInjection
			if err := r.Get(ctx, req.NamespacedName, &latest); err != nil {
				return err
			}

			// Copy only lifecycle fields (avoid clobbering other status written elsewhere)
			latest.Status.StartedAt = fi.Status.StartedAt
			latest.Status.ExpiresAt = fi.Status.ExpiresAt

			return r.Status().Update(ctx, &latest)
		}); err != nil {
			log.Error(err, "failed to persist lifecycle status", "name", fi.Name, "ns", fi.Namespace)
			r.eventf(&fi, "Warning", "StatusUpdateFailed", "status-update",
				"Failed to persist lifecycle status: %v", err)
			return ctrl.Result{}, err
		}

		// Reload so subsequent logic uses persisted values (optional but nice)
		if err := r.Get(ctx, req.NamespacedName, &fi); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	// Cancelled => immediate cleanup
	if fi.Spec.Cancel {
		return r.handleCancellation(ctx, log, req, &fi, now)
	}

	// If lifecycle not initialized yet, initialize and requeue.
	if fi.Status.ExpiresAt == nil || fi.Status.StartedAt == nil {
		if changed := r.ensureLifecycle(log, &fi); changed {
			// Status was updated; requeue so we could see persisted StartedAt/ExpiresAt
			return ctrl.Result{Requeue: true}, nil
		}

		// Nothing changed (should be rare), but still stop this reconcile
		// to avoid dereferencing nil StartedAt/ExpiresAt below.
		return ctrl.Result{}, nil
	}

	// Expired => cleanup
	if now.After(fi.Status.ExpiresAt.Time) {
		return r.handleExpiry(ctx, log, &fi)
	}

	// Guardrails
	if err := r.validateSpec(ctx, &fi); err != nil {
		return r.handleValidationError(ctx, log, &fi, err)
	}

	desiredByVSTarget, managedVSNames := r.buildDesiredByVSTarget(&fi)

	stop, err := r.applyDesiredTargets(ctx, log, &fi, desiredByVSTarget)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Terminal/stop conditions (e.g., unsafe INBOUND target) must not fall through and
	// overwrite status with Running.
	if stop {
		return ctrl.Result{}, nil
	}

	if err := r.cleanupOrphanedManagedVS(ctx, &fi, managedVSNames); err != nil {
		log.Error(err, "failed cleanupOrphanedManagedVS", "name", fi.Name, "ns", fi.Namespace)
		r.eventf(&fi, "Warning", "OrphanCleanupFailed", "cleanup-orphans", "Failed cleaning orphaned managed VirtualServices: %v", err)
	}

	if err := r.markRunning(ctx, log, &fi); err != nil {
		return ctrl.Result{}, err
	}
	// markRunning() updates status via Status().Update() on a freshly fetched object.
	// Reload so applyPodFaults sees the latest status (StartedAt/ExpiresAt/Phase/etc).
	if err := r.Get(ctx, req.NamespacedName, &fi); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	podStop, nextDue, err := r.applyPodFaults(ctx, log, &fi, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if podStop {
		return ctrl.Result{}, nil
	}

	// Requeue should consider next windowed pod tick due (if any), not only expiry.
	// WINDOWED actions must run on schedule; avoid waiting until expiresAt.
	requeueAfter := time.Until(fi.Status.ExpiresAt.Time)
	if nextDue != nil {
		if d := time.Until(*nextDue); d > 0 && d < requeueAfter {
			requeueAfter = d
		}
		if d := time.Until(*nextDue); d <= 0 {
			requeueAfter = 1 * time.Second
		}
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// ensureLifecycle initializes and maintains FaultInjection lifecycle timestamps.
//
// Behavior:
//   - If fi is nil, returns changed=false (defensive).
//   - Validates blastRadius.durationSeconds > 0 (fail-closed; emits event and returns).
//   - If Status.StartedAt is nil, sets it to now and emits a "Started" event.
//   - Computes expiresAt = StartedAt + durationSeconds.
//   - If Status.ExpiresAt is nil or differs from computed expiresAt, sets it and emits
//     a "LifecycleUpdated" event.
//
// Notes:
//   - ensureLifecycle mutates the in-memory FaultInjection object only.
//     Callers are responsible for persisting status updates.
//   - ExpiresAt is recomputed to remain consistent with StartedAt and the current spec.
//     This supports handling durationSeconds changes deterministically.
func (r *FaultInjectionReconciler) ensureLifecycle(
	log logr.Logger,
	fi *chaosv1alpha1.FaultInjection,
) (changed bool) {
	now := time.Now().UTC()

	if fi == nil {
		log.Info("nil FaultInjection passed to ensureLifecycle")
		return false
	}

	br := fi.Spec.BlastRadius
	durSeconds := br.DurationSeconds          // int64 (time math)
	maxTrafficPercent := br.MaxTrafficPercent // int32 (0–100)

	log = log.WithValues(
		"name", fi.Name,
		"namespace", fi.Namespace,
		"durationSeconds", durSeconds,
		"maxTrafficPercent", maxTrafficPercent,
	)

	// Fail-closed guard (admission should prevent this, but keep reconciler resilient).
	if durSeconds <= 0 {
		if r.Recorder != nil {
			r.eventf(
				fi, "Warning", "InvalidSpec", "validate",
				"blastRadius.durationSeconds must be > 0 (got %d)",
				durSeconds,
			)
		}
		log.Info("invalid durationSeconds; lifecycle not initialized", "durationSeconds", durSeconds)
		return false
	}

	// Initialize StartedAt if missing.
	if fi.Status.StartedAt == nil {
		t := metav1.NewTime(now)
		fi.Status.StartedAt = &t
		changed = true

		if r.Recorder != nil {
			r.eventf(
				fi, "Normal", "Started", "lifecycle-init",
				"Experiment started; durationSeconds=%d maxTrafficPercent=%d",
				durSeconds,
				maxTrafficPercent,
			)
		}

		log.Info("experiment started",
			"startedAt", t.Format(time.RFC3339),
		)
	}

	// Safety net (should never happen).
	if fi.Status.StartedAt == nil {
		if r.Recorder != nil {
			r.eventf(
				fi, "Warning", "LifecycleError", "lifecycle-init",
				"internal error: StartedAt is nil after initialization",
			)
		}
		log.Info("internal lifecycle error: startedAt is nil after initialization")
		return changed
	}

	start := fi.Status.StartedAt.Time
	expiresAt := start.Add(time.Duration(durSeconds) * time.Second)

	// Set / refresh ExpiresAt.
	if fi.Status.ExpiresAt == nil || !fi.Status.ExpiresAt.Time.Equal(expiresAt) {
		t := metav1.NewTime(expiresAt)
		fi.Status.ExpiresAt = &t
		changed = true

		if r.Recorder != nil {
			r.eventf(
				fi, "Normal", "LifecycleUpdated", "lifecycle-update",
				"Lifecycle set; startedAt=%s expiresAt=%s",
				fi.Status.StartedAt.Format(time.RFC3339),
				t.Format(time.RFC3339),
			)
		}

		log.Info("lifecycle updated",
			"expiresAt", t.Format(time.RFC3339),
		)
	}

	return changed
}

// handleCancellation performs immediate cleanup when spec.cancel=true.
//
// Behavior:
//   - If already Cancelled or Completed, it is a no-op.
//   - Emits "CancelRequested" once when cancellation is first observed.
//   - Calls cleanupAll to remove injected mesh rules, delete managed VS, and cleanup
//     FI-owned artifacts (best-effort).
//   - Persists status (conflict-retry):
//   - Status.Phase = Cancelled
//   - Status.Message = "Cancelled: cleaned up injected rules"
//   - Status.CancelledAt set once
//   - Status.StopReason defaulted if empty
//   - Emits "Cancelled" on success.
//
// Return:
//   - ctrl.Result{} with nil error on success (no requeue).
//   - error if cleanup or status persistence fails.
func (r *FaultInjectionReconciler) handleCancellation(ctx context.Context, log logr.Logger, req ctrl.Request, fi *chaosv1alpha1.FaultInjection, now time.Time) (ctrl.Result, error) {

	if fi.Status.Phase == CancelledPhase || fi.Status.Phase == CompletedPhase {
		return ctrl.Result{}, nil
	}

	if fi.Status.CancelledAt == nil {
		r.event(fi, "Normal", "CancelRequested", "cancel", "Cancellation requested via spec.cancel=true")
	}

	log.Info("experiment cancellation requested; starting cleanup", "name", fi.Name, "ns", fi.Namespace)
	r.event(fi, "Normal", "CleanupStarted", "cleanup", "Starting cleanup for cancelled experiment")

	if err := r.cleanupAll(ctx, fi); err != nil {
		log.Error(err, "cancellation cleanup failed", "name", fi.Name, "ns", fi.Namespace)
		r.eventf(fi, "Warning", "CleanupFailed", "cleanup", "Cancellation cleanup failed: %v", err)
		return ctrl.Result{}, err
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest chaosv1alpha1.FaultInjection
		if err := r.Get(ctx, req.NamespacedName, &latest); err != nil {
			return err
		}

		latest.Status.Phase = CancelledPhase
		latest.Status.Message = "Cancelled: cleaned up injected rules"

		if latest.Status.CancelledAt == nil {
			ct := metav1.NewTime(now)
			latest.Status.CancelledAt = &ct
		}
		if latest.Status.StopReason == "" {
			latest.Status.StopReason = "external: spec.cancel=true"
		}

		return r.Status().Update(ctx, &latest)
	}); err != nil {
		log.Error(err, "failed to persist Cancelled status after cancellation", "name", fi.Name, "ns", fi.Namespace)
		r.eventf(fi, "Warning", "StatusUpdateFailed", "status-update", "Failed to persist Cancelled status: %v", err)
		return ctrl.Result{}, err
	}

	r.event(fi, "Normal", "Cancelled", "cancel", "Experiment cancelled; cleanup completed")
	log.Info("experiment cancelled; cleanup completed", "name", fi.Name, "ns", fi.Namespace)
	return ctrl.Result{}, nil
}

// handleExpiry performs cleanup when the experiment has expired (now > Status.ExpiresAt).
//
// Behavior:
//   - Calls cleanupAll (best-effort cleanup of FI-managed artifacts).
//   - Sets and persists:
//   - Status.Phase = Completed
//   - Status.Message = "Expired: cleaned up injected rules"
//   - Emits "CleanupStarted" and "Expired" events.
//
// Return:
//   - ctrl.Result{} with nil error on success (no requeue).
//   - error if cleanupAll or status update fails.
func (r *FaultInjectionReconciler) handleExpiry(ctx context.Context, log logr.Logger, fi *chaosv1alpha1.FaultInjection) (ctrl.Result, error) {
	log.Info("experiment expired; starting cleanup", "name", fi.Name, "ns", fi.Namespace, "expiresAt", fi.Status.ExpiresAt.Time)
	r.event(fi, "Normal", "CleanupStarted", "cleanup", "Starting cleanup for expired experiment")

	if err := r.cleanupAll(ctx, fi); err != nil {
		log.Error(err, "expiry cleanup failed", "name", fi.Name, "ns", fi.Namespace)
		r.eventf(fi, "Warning", "CleanupFailed", "cleanup", "Expiry cleanup failed: %v", err)
		return ctrl.Result{}, err
	}

	fi.Status.Phase = CompletedPhase
	fi.Status.Message = "Expired: cleaned up injected rules"

	if err := r.Status().Update(ctx, fi); err != nil {
		log.Error(err, "failed to persist Completed status after expiry", "name", fi.Name, "ns", fi.Namespace)
		r.eventf(fi, "Warning", "StatusUpdateFailed", "status-update", "Failed to persist Completed status after expiry: %v", err)

		return ctrl.Result{}, err
	}

	r.event(fi, "Normal", "Expired", "expiry", "Experiment expired; cleanup completed")
	log.Info("experiment expired; cleanup completed", "name", fi.Name, "ns", fi.Namespace)
	return ctrl.Result{}, nil
}

// handleValidationError handles guardrail/spec-validation failures and fails closed.
//
// Behavior:
//   - Emits "ValidationFailed" and "CleanupStarted" events.
//   - Calls cleanupAll to remove injected state (best-effort).
//   - Sets and persists:
//   - Status.Phase = Error
//   - Status.Message = validation error string
//
// Failure handling:
//   - cleanupAll failure is logged/emitted but does not prevent marking Error.
//   - status update failure is returned (terminal reconcile error).
//
// Return:
//   - ctrl.Result{} with nil error when Error status is persisted (no requeue).
//   - error only when the status update fails.
func (r *FaultInjectionReconciler) handleValidationError(ctx context.Context, log logr.Logger, fi *chaosv1alpha1.FaultInjection, verr error) (ctrl.Result, error) {
	log.Error(verr, "spec validation failed; starting cleanup", "name", fi.Name, "ns", fi.Namespace)
	r.eventf(fi, "Warning", "ValidationFailed", "validate", "Spec rejected: %v", verr)
	r.event(fi, "Normal", "CleanupStarted", "cleanup", "Starting cleanup after spec validation failure")

	if cerr := r.cleanupAll(ctx, fi); cerr != nil {
		log.Error(cerr, "cleanup after validation failure failed", "name", fi.Name, "ns", fi.Namespace)
		r.eventf(fi, "Warning", "CleanupFailed", "cleanup", "Cleanup after validation failure failed: %v", cerr)
	}

	fi.Status.Phase = ErrorPhase
	fi.Status.Message = verr.Error()

	if err := r.Status().Update(ctx, fi); err != nil {
		log.Error(err, "failed to persist Error status after validation failure", "name", fi.Name, "ns", fi.Namespace)
		r.eventf(fi, "Warning", "StatusUpdateFailed", "status-update", "Failed to persist Error status after validation failure: %v", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// applyDesiredTargets applies desired injected mesh HTTP rules to all VirtualService targets.
//
// Input:
//   - desiredByVSTarget keyed by "<namespace>/<virtualservice-name>", with:
//   - Rules: desired injected http rules (unstructured maps)
//   - Managed: whether the VS is managed (OUTBOUND)
//   - Hosts: destination hosts for managed outbound default routing
//
// Per target, it:
//   - Gets the VirtualService, or creates it if Managed=true.
//   - Ensures managed outbound VS attaches to the mesh gateway when applicable.
//   - Finds a base routable action (route/redirect/direct_response):
//   - If INBOUND: must exist in the target VS, otherwise fail closed.
//   - If Managed OUTBOUND: ensures a default route exists and uses it.
//   - Ensures each injected rule has a routable action by cloning/attaching baseRoute.
//   - Patches .spec.http idempotently:
//   - remove previously injected rules by fiRuleNamePrefix(fi)
//   - prepend desired injected rules
//   - Updates the VirtualService only if created/changed/gateways changed.
//   - Emits events for creation, patching, and unsafe targets.
//
// Return:
//   - stop=true when a terminal/unsafe target condition is hit (so caller must not
//     overwrite Error status with Running).
//   - error for API failures that should retry reconciliation.
func (r *FaultInjectionReconciler) applyDesiredTargets(
	ctx context.Context,
	log logr.Logger,
	fi *chaosv1alpha1.FaultInjection,
	desiredByVSTarget map[string]vsDesired,
) (bool, error) {
	for targetKey, desired := range desiredByVSTarget {
		vsNS, vsName := splitKey(targetKey)

		vs, created, err := r.getOrCreateVirtualService(ctx, fi, vsNS, vsName, desired)
		if err != nil {
			log.Error(err, "failed getting/creating VirtualService", "fi", fi.Name, "vsNS", vsNS, "vsName", vsName)
			r.eventf(fi, "Warning", "VirtualServiceGetOrCreateFailed", "vs-get-or-create", "Failed getting/creating VirtualService %s/%s: %v", vsNS, vsName, err)

			fi.Status.Phase = ErrorPhase
			fi.Status.Message = fmt.Sprintf("failed getting/creating VirtualService %s/%s: %v", vsNS, vsName, err)
			_ = r.Status().Update(ctx, fi)
			return false, err
		}

		meshGatewayChanged := ensureManagedOutboundVSGatewaysMesh(vs, desired.Managed)

		baseRoute, ok := findAnyExistingRoute(vs)
		if !ok {
			if desired.Managed {
				baseRoute = buildManagedOutboundDefaultRoute(desired.Hosts)
				ensureVSHasAtLeastOneDefaultRouteRule(vs, desired.Hosts)

				r.eventf(fi, "Normal", "DefaultRouteEnsured", "vs-default-route", "Ensured default route for managed VirtualService %s/%s (hosts=%v)", vsNS, vsName, desired.Hosts)
			} else {
				fi.Status.Phase = ErrorPhase
				fi.Status.Message = fmt.Sprintf(
					"target VirtualService %s/%s has no route/redirect/direct_response in any http rule; cannot inject faults safely",
					vsNS, vsName,
				)

				r.eventf(fi, "Warning", "UnsafeTarget", "vs-unsafe-target", "Target VirtualService %s/%s has no route/redirect/direct_response; cannot inject faults safely", vsNS, vsName)
				_ = r.Status().Update(ctx, fi)

				// IMPORTANT: stop reconciliation so markRunning does not overwrite Error.
				return true, nil
			}
		}

		desiredRules := make([]map[string]any, 0, len(desired.Rules))
		for _, rule := range desired.Rules {
			rule = cloneMap(rule)
			ensureHTTPRuleHasRoute(rule, baseRoute)
			desiredRules = append(desiredRules, rule)
		}

		changed := patchVirtualServiceHTTP(vs, fiRuleNamePrefix(fi), desiredRules)

		if created || changed || meshGatewayChanged {
			if err := r.Update(ctx, vs); err != nil {
				r.eventf(fi, "Warning", "VirtualServiceUpdateFailed", "vs-update", "Failed updating VirtualService %s/%s: %v", vsNS, vsName, err)
				return false, err
			}

			if created {
				r.eventf(fi, "Normal", "VirtualServiceCreated", "vs-create", "Created managed VirtualService %s/%s", vsNS, vsName)
			} else if changed || meshGatewayChanged {
				r.eventf(fi, "Normal", "VirtualServicePatched", "vs-patch", "Patched VirtualService %s/%s (injected rules updated)", vsNS, vsName)
			}
		}
	}
	return false, nil
}

// markRunning transitions the FaultInjection status to Running.
//
// Behavior:
//   - If the current object already has Status.Phase == Running, it is a no-op.
//   - Uses RetryOnConflict and re-fetches the latest object before updating status.
//   - On transition to Running, sets:
//   - Status.Phase = Running
//   - Status.Message = "Active: injected rules are applied"
//   - Emits a Normal "Active" event only when this call causes the transition.
//
// Return:
//   - nil on success.
//   - error if the status update fails.
func (r *FaultInjectionReconciler) markRunning(
	ctx context.Context,
	log logr.Logger,
	fi *chaosv1alpha1.FaultInjection,
) error {

	// Fast path: if this reconcile already sees Running, do nothing.
	// This avoids unnecessary writes and conflict retries.
	if fi.Status.Phase == RunningPhase {
		return nil
	}

	key := client.ObjectKeyFromObject(fi)

	// Emit the "Active" event if THIS call
	// actually transitions the object into Running.
	emitActiveEvent := false

	// Status updates are subject to optimistic locking (resourceVersion).
	// RetryOnConflict ensures re-fetch and re-apply on conflict.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest chaosv1alpha1.FaultInjection

		// Always operate on the latest version from the API server.
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}

		// Another reconcile may have already marked it Running.
		// In that case, do nothing and exit successfully.
		if latest.Status.Phase == RunningPhase {
			return nil
		}

		// Transition to Running.
		latest.Status.Phase = RunningPhase
		latest.Status.Message = "Active: injected rules are applied"

		// Emit the Active event *after*
		// the status update succeeds.
		emitActiveEvent = true

		// Persist only the status subresource.
		return r.Status().Update(ctx, &latest)
	})

	if err != nil {
		// Conflict retries exhausted or a real API error occurred.
		log.Error(err, "failed to persist Running status", "name", fi.Name, "ns", fi.Namespace)

		r.eventf(
			fi,
			"Warning",
			"StatusUpdateFailed",
			"status-update",
			"Failed to persist FaultInjection status: %v",
			err,
		)
		return err
	}

	// Emit event and log only if THIS reconcile caused the transition.
	if emitActiveEvent {
		r.event(
			fi,
			"Normal",
			"Active",
			"mark-running",
			"Experiment is active; injected rules are applied",
		)

		// Log with expiresAt when available (defensive against nil).
		if fi.Status.ExpiresAt != nil {
			log.Info(
				"experiment is active",
				"name", fi.Name,
				"ns", fi.Namespace,
				"expiresAt", fi.Status.ExpiresAt.Time,
			)
		} else {
			log.Info(
				"experiment is active",
				"name", fi.Name,
				"ns", fi.Namespace,
			)
		}
	}

	return nil
}

// SetupWithManager registers the reconciler with the controller-runtime Manager.
//
// Behavior:
//   - Initializes Recorder if nil.
//   - Sets a defensive default HolderIdentity if empty (required for Lease locking).
//   - Registers the controller to watch FaultInjection resources.
//   - Sets controller name to "faultinjection".
func (r *FaultInjectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("faultinjection-controller")
	}

	// Defensive default for HolderIdentity as lease locking needs a holder identity
	if strings.TrimSpace(r.HolderIdentity) == "" {
		r.HolderIdentity = HolderIdentity
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&chaosv1alpha1.FaultInjection{}).
		Named("faultinjection").
		Complete(r)
}

// validateSpec enforces guardrails and basic correctness of the FaultInjection spec.
//
// Validations (mesh faults):
//   - blastRadius.durationSeconds >= 1
//   - each action.percent <= blastRadius.maxTrafficPercent
//   - supported action types and their required fields:
//   - HTTP_LATENCY requires http.delay.fixedDelaySeconds > 0
//   - HTTP_ABORT requires http.abort.httpStatus >= 100
//   - each route match specifies uriPrefix or uriExact
//   - supported directions and required fields:
//   - INBOUND requires http.virtualServiceRef.name
//   - OUTBOUND requires http.destinationHosts
//   - optional blastRadius.maxPodsAffected enforcement (mesh):
//   - requires http.sourceSelector.matchLabels
//   - counts matching pods in fi.Namespace and errors if count exceeds the cap
//
// Validations (pod faults):
//   - Delegates to validatePodFaults for scheduling/selection/termination/guardrail checks.
//
// Return:
//   - nil if valid.
//   - error describing the first violated guardrail.
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

	// validate Pod Faults
	if err := r.validatePodFaults(ctx, fi); err != nil {
		return err
	}

	return nil
}

// countPods lists pods in a namespace matching the given labels and returns the count.
//
// Usage:
//   - Used to enforce blastRadius.maxPodsAffected for mesh faults.
//
// Notes:
//   - If match is empty, counts all pods in the namespace.
//   - Uses UnstructuredList to avoid coupling this package to typed corev1 Pod imports.
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

// cleanupAll removes injected mesh rules and deletes FI-owned/managed artifacts.
//
// It determines targets by:
//   - Reading the FaultInjection spec:
//   - INBOUND: http.virtualServiceRef.name
//   - OUTBOUND: managedOutboundVSName(fi, action)
//   - Listing VirtualServices in fi.Namespace labeled chaos.sghaida.io/fi=<fi.Name>
//     (covers managed outbound VS and any labeled targets)
//
// For each target VirtualService:
//   - Removes injected rules by prefix (fiRuleNamePrefix(fi)).
//   - If the VS is managed-by=fi-operator and labeled for this FI, deletes the VS.
//   - Otherwise, updates the VS only if rules were removed.
//
// Additional best-effort cleanup:
//   - Deletes pod-fault artifacts (Jobs + Leases) labeled for this FI.
//   - Deletes FI-owned Deployments in the namespace (ownerRef FaultInjection).
//
// Used by:
//   - handleExpiry (Completed)
//   - handleCancellation (Cancelled)
//   - handleValidationError (Error)
//
// Return:
//   - error only for non-recoverable API errors (e.g., list failures, update failures).
//   - best-effort deletion errors are emitted as events and processing continues.
func (r *FaultInjectionReconciler) cleanupAll(ctx context.Context, fi *chaosv1alpha1.FaultInjection) error {
	targets := map[string]struct{}{}

	for _, a := range fi.Spec.Actions.MeshFaults {
		if a.Direction == INBOUNDDirection && a.HTTP.VirtualServiceRef != nil {
			targets[joinKey(fi.Namespace, a.HTTP.VirtualServiceRef.Name)] = struct{}{}
		}
		if a.Direction == OUTBOUNDDirection {
			targets[joinKey(fi.Namespace, managedOutboundVSName(fi, &a))] = struct{}{}
		}
	}

	var vsList unstructured.UnstructuredList
	vsList.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1beta1", Kind: "VirtualServiceList"})
	if err := r.List(ctx, &vsList, client.InNamespace(fi.Namespace), client.MatchingLabels{"chaos.sghaida.io/fi": fi.Name}); err != nil {
		r.eventf(fi, "Warning", "CleanupListFailed", "cleanup", "Failed listing labeled VirtualServices for cleanup: %v", err)
		return err
	}

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
			if err := r.Delete(ctx, vs); err != nil && !apierrors.IsNotFound(err) {
				r.eventf(
					fi, "Warning", "VirtualServiceDeleteFailed", "vs-delete",
					"Failed deleting managed VirtualService %s/%s: %v", ns, name, err,
				)
				// continue best-effort
			} else {
				r.eventf(
					fi, "Normal", "VirtualServiceDeleted", "vs-delete",
					"Deleted managed VirtualService %s/%s", ns, name,
				)
			}
			continue
		}

		if changed {
			if err := r.Update(ctx, vs); err != nil {
				r.eventf(
					fi, "Warning", "CleanupUpdateFailed", "vs-update",
					"Failed updating VirtualService during cleanup %s/%s: %v", ns, name, err,
				)
				return err
			}
			r.eventf(
				fi, "Normal", "RulesRemoved", "cleanup",
				"Removed injected rules from VirtualService %s/%s", ns, name,
			)
		}
	}

	// Best-effort: cleanup podfault artifacts (Jobs + Leases) labeled for this FI.
	// cancellation/expiry must stop windowed execution immediately.
	_ = r.cleanupPodFaultArtifacts(ctx, fi)

	// Best-effort: delete "fault deployment" workloads created for the FI in this namespace.
	// This intentionally does not fail cleanup if deletion fails; prefer to keep cleanup robust.
	_ = r.cleanupFIManagedDeployments(ctx, fi)

	return nil
}

// cleanupFIManagedDeployments deletes Deployments in fi.Namespace owned by the FaultInjection.
//
// Ownership detection:
//   - Filters by ownerReferences kind=FaultInjection and matches owner name,
//     and (when available) matches owner UID to avoid accidental deletes.
//
// Best-effort:
//   - Continues on individual delete errors (emits events).
//   - Returns error only if the list call fails.
func (r *FaultInjectionReconciler) cleanupFIManagedDeployments(ctx context.Context, fi *chaosv1alpha1.FaultInjection) error {
	var depList unstructured.UnstructuredList
	depList.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DeploymentList"})

	// IMPORTANT:
	// Only delete Deployments that are *marked as* kind:FaultInjection via ownerReferences.
	// additionally match on the owning FI name and UID to avoid deleting Deployments
	// owned by other FaultInjection objects in the same namespace.
	if err := r.List(ctx, &depList, client.InNamespace(fi.Namespace)); err != nil {
		r.eventf(fi, "Warning", "DeploymentListFailed", "dep-list", "Failed listing Deployments for cleanup: %v", err)
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
		if err := r.Delete(ctx, dep); err != nil && !apierrors.IsNotFound(err) {
			r.eventf(
				fi, "Warning", "DeploymentDeleteFailed", "dep-delete",
				"Failed deleting FI-owned Deployment %s/%s: %v", dep.GetNamespace(), dep.GetName(), err,
			)
			continue
		}

		r.eventf(
			fi, "Normal", "DeploymentDeleted", "dep-delete",
			"Deleted FI-owned Deployment %s/%s", dep.GetNamespace(), dep.GetName(),
		)
	}

	return nil
}

// fiRuleNamePrefix returns the name prefix used to identify injected mesh HTTP rules.
//
// All injected rule names begin with "fi-<fi.Name>-", allowing patchVirtualServiceHTTP
// to remove prior injected rules deterministically during reconciliation and cleanup.
func fiRuleNamePrefix(fi *chaosv1alpha1.FaultInjection) string {
	return fmt.Sprintf("fi-%s-", fi.Name)
}
