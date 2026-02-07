package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	chaosv1alpha1 "github.com/sghaida/fi-operator/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// Defaults for pod-fault job/lease safety.
	//
	// Lease:
	//   - Short lease duration keeps progress bounded if a reconciler crashes mid-tick.
	//   - Safety skew tolerates small clock skew/controller delays before takeover.
	//
	// Job:
	//   - ActiveDeadlineSeconds bounds execution time per tick.
	//   - TTLSecondsAfterFinished garbage-collects completed Jobs.
	//   - BackoffLimit=0 makes failures fail-fast (no retries).
	defaultPodFaultLeaseDurationSeconds int32 = 60
	defaultPodFaultLeaseSafetySkew            = 10 * time.Second

	defaultPodFaultJobActiveDeadlineSeconds   int64 = 120
	defaultPodFaultJobTTLSecondsAfterFinished int32 = 300
	defaultPodFaultJobBackoffLimit            int32 = 0
)

// validatePodFaults validates pod-fault actions for reconciler safety.
//
// This is defensive validation in addition to CRD schema/XValidation.
// It ensures the controller never schedules unsafe or ambiguous work.
//
// Invariants enforced:
//   - Namespace scoping: action target namespace must be empty (defaults to FI namespace)
//     or exactly equal to the FaultInjection namespace.
//   - Execution mode:
//   - ONE_SHOT must omit window.
//   - WINDOWED requires window with intervalSeconds>=1 and maxTotalPodsAffected>=1.
//   - Selection mode:
//   - COUNT requires selection.count >= 1.
//   - PERCENT requires selection.percent in [1..100].
//   - Termination invariants:
//   - FORCEFUL requires gracePeriodSeconds==0.
//   - GRACEFUL requires gracePeriodSeconds>0.
//   - Guardrails:
//   - refuseIfPodsLessThan must be >= 1.
func (r *FaultInjectionReconciler) validatePodFaults(ctx context.Context, fi *chaosv1alpha1.FaultInjection) error {
	for _, a := range fi.Spec.Actions.PodFaults {
		// Namespace scoping: allow empty (defaults to FI ns) OR exact FI namespace only.
		targetNS := strings.TrimSpace(a.Target.Namespace)
		if targetNS != "" && targetNS != fi.Namespace {
			return fmt.Errorf(
				"podFault %q target.namespace=%q must be empty or equal to FaultInjection namespace %q",
				a.Name, a.Target.Namespace, fi.Namespace,
			)
		}
		if targetNS == "" {
			targetNS = fi.Namespace
		}

		// Resolve execution mode default ONE_SHOT.
		mode := chaosv1alpha1.ExecutionModeOneShot
		if a.Policy != nil && a.Policy.ExecutionMode != "" {
			mode = a.Policy.ExecutionMode
		}

		// Window invariants.
		if mode == chaosv1alpha1.ExecutionModeWindowed {
			if a.Window == nil {
				return fmt.Errorf("podFault %q requires window when executionMode=WINDOWED", a.Name)
			}
			if a.Window.IntervalSeconds < 1 {
				return fmt.Errorf("podFault %q window.intervalSeconds must be >= 1", a.Name)
			}
			if a.Window.MaxTotalPodsAffected < 1 {
				return fmt.Errorf("podFault %q window.maxTotalPodsAffected must be >= 1", a.Name)
			}
		} else {
			if a.Window != nil {
				return fmt.Errorf("podFault %q must omit window when executionMode=ONE_SHOT", a.Name)
			}
		}

		// Selection invariants.
		switch a.Selection.Mode {
		case chaosv1alpha1.PodSelectionModeCount:
			if a.Selection.Count == nil || *a.Selection.Count < 1 {
				return fmt.Errorf("podFault %q selection.count is required and must be >= 1 for mode=COUNT", a.Name)
			}
		case chaosv1alpha1.PodSelectionModePercent:
			if a.Selection.Percent == nil || *a.Selection.Percent < 1 || *a.Selection.Percent > 100 {
				return fmt.Errorf("podFault %q selection.percent is required and must be 1..100 for mode=PERCENT", a.Name)
			}
		default:
			return fmt.Errorf("podFault %q has unsupported selection.mode=%q", a.Name, a.Selection.Mode)
		}

		// Termination invariants (CRD XValidation should enforce, but keep defensive here).
		if a.Termination.Mode == chaosv1alpha1.PodTerminationModeForceful && a.Termination.GracePeriodSeconds != 0 {
			return fmt.Errorf("podFault %q termination.gracePeriodSeconds must be 0 for FORCEFUL", a.Name)
		}
		if a.Termination.Mode == chaosv1alpha1.PodTerminationModeGraceful && a.Termination.GracePeriodSeconds <= 0 {
			return fmt.Errorf("podFault %q termination.gracePeriodSeconds must be >0 for GRACEFUL", a.Name)
		}

		// Guardrails structural validation.
		if a.Guardrails.RefuseIfPodsLessThan < 1 {
			return fmt.Errorf("podFault %q guardrails.refuseIfPodsLessThan must be >= 1", a.Name)
		}

		// Guardrails *runtime* validation: refuse if not enough candidate pods exist.
		labels := map[string]string(nil)
		if len(a.Target.Selector.MatchLabels) > 0 {
			labels = a.Target.Selector.MatchLabels
		}

		// List pods in targetNS with matchLabels (if provided).
		n, err := r.countPods(ctx, targetNS, labels)
		if err != nil {
			return fmt.Errorf("podFault %q failed to count candidate pods: %v", a.Name, err)
		}
		if int32(n) < a.Guardrails.RefuseIfPodsLessThan {
			return fmt.Errorf(
				"podFault %q guardrail violated: refuseIfPodsLessThan=%d but only %d candidate pods match selector in namespace %q",
				a.Name,
				a.Guardrails.RefuseIfPodsLessThan,
				n,
				targetNS,
			)
		}
	}
	return nil
}

// podFaultPlan is a fully-resolved execution plan for a single pod-fault action tick.
//
// The plan is built deterministically from the FaultInjection spec + startedAt + now,
// and contains the concrete artifacts names (Job/Lease) and selection intent.
//
// Notes:
//   - The controller selects *exact pod names* and stores them into the plan before creating a Job.
//     The Job must be a dumb executor and must not list pods (auditability/idempotence).
//   - Exactly one of SelectionCount or SelectionPercent is expected to be set.
type podFaultPlan struct {
	// ActionName is the user-provided action name (fi.spec.actions.podFaults[].name).
	ActionName string

	// TickID is "oneshot" or a windowed tick index encoded as string.
	TickID string

	// DueAt is the next tick boundary for WINDOWED mode; nil for ONE_SHOT.
	DueAt *time.Time

	// JobName is the deterministic Job name for this (action,tick).
	JobName string

	// LeaseName is the deterministic Lease name for this (action,tick).
	LeaseName string

	// Namespace is the target namespace where candidate pods are selected (defaults to FI namespace).
	Namespace string

	// SelectionCount and SelectionPercent carry selection intent (exactly one set).
	SelectionCount   *int32
	SelectionPercent *int32

	// PodNames are the concrete target pod names selected by the controller.
	PodNames []string

	// SelectorLabels are matchLabels used to list candidate pods.
	SelectorLabels map[string]string

	// TerminationMode defines graceful vs forceful delete.
	TerminationMode chaosv1alpha1.PodTerminationMode

	// GracePeriodSecs is used for GRACEFUL termination mode.
	GracePeriodSecs int64

	// MaxPodsAffectedEffective is the final cap after combining:
	//   - fi.spec.blastRadius.maxPodsAffected
	//   - action.selection.maxPodsAffected
	//   - window.maxTotalPodsAffected (WINDOWED only)
	MaxPodsAffectedEffective int32

	// RespectPDB indicates whether the executor should respect PodDisruptionBudget.
	RespectPDB bool

	// RefuseIfPodsLessThan refuses execution if the candidate set is too small.
	RefuseIfPodsLessThan int32
}

// applyPodFaults evaluates and schedules pod-fault actions.
//
// Behavior:
//   - If no pod faults exist, returns (stop=false,nextDue=nil,nil).
//   - Requires fi.status.startedAt for tick computation; if missing, pod faults are skipped.
//   - For each plan:
//     1) If Job exists:
//   - If complete: record executed success in FI status and continue.
//   - If failed: record executed failure, mark FI phase Error, and stop.
//   - Else: job running; continue.
//     2) If no Job exists:
//   - Select candidate pods and deterministically choose exact targets.
//   - Record "Planned" in FI status (intent) before any lease/job creation.
//   - Acquire/takeover a per-tick Lease (single executor per (action,tick)).
//   - Create a deterministic Job that deletes only the selected pods.
//
// Return values:
//   - stop: true if a job failed and the reconciler should stop the experiment.
//   - nextDue: earliest upcoming tick boundary across all windowed actions (used for requeue).
func (r *FaultInjectionReconciler) applyPodFaults(
	ctx context.Context,
	log logr.Logger,
	fi *chaosv1alpha1.FaultInjection,
	now time.Time,
) (stop bool, nextDue *time.Time, err error) {

	if len(fi.Spec.Actions.PodFaults) == 0 {
		return false, nil, nil
	}

	// We need StartedAt for tick computation. If missing, do not execute pod faults yet.
	if fi.Status.StartedAt == nil {
		log.Info("podfaults skipped: StartedAt is nil")
		return false, nil, nil
	}
	startedAt := fi.Status.StartedAt.Time

	plans, earliestDue, perr := r.buildPodFaultPlans(fi, now, startedAt)
	if perr != nil {
		return false, earliestDue, perr
	}

	for _, p := range plans {
		// 1) If Job exists => observe its state.
		job := &batchv1.Job{}
		jobKey := types.NamespacedName{Namespace: fi.Namespace, Name: p.JobName}
		jobExists := false

		if err := r.Get(ctx, jobKey, job); err == nil {
			jobExists = true
		} else if !apierrors.IsNotFound(err) {
			r.eventf(fi, "Warning", "PodFaultJobGetFailed", "podfault-job-get",
				"Failed getting podfault Job %s/%s: %v", fi.Namespace, p.JobName, err)
			return false, earliestDue, err
		}

		if jobExists {
			if isJobComplete(job) {
				// Record executed result in FI status.
				_ = r.recordPodFaultExecuted(ctx, fi, p, now, true, "completed")

				r.eventf(fi, "Normal", "PodFaultCompleted", "podfault-complete",
					"Pod fault action %q tick %q completed (job=%s/%s)",
					p.ActionName, p.TickID, job.Namespace, job.Name)
				continue
			}

			if isJobFailed(job) {
				// Record executed failure in FI status and fail-closed.
				_ = r.recordPodFaultExecuted(ctx, fi, p, now, false, "failed")

				fi.Status.Phase = ErrorPhase
				fi.Status.Message = fmt.Sprintf(
					"pod fault action %q tick %q failed (job=%s/%s)",
					p.ActionName, p.TickID, job.Namespace, job.Name,
				)
				_ = r.Status().Update(ctx, fi)

				r.eventf(fi, "Warning", "PodFaultFailed", "podfault-failed",
					"Pod fault action %q tick %q failed (job=%s/%s)",
					p.ActionName, p.TickID, job.Namespace, job.Name)
				return true, earliestDue, nil
			}

			log.Info("podfault job running",
				"fi", fi.Name, "action", p.ActionName, "tick", p.TickID, "job", job.Name)
			continue
		}

		// 2) No job yet => select exact targets.
		candidates, err := r.listCandidatePods(ctx, p.Namespace, p.SelectorLabels)
		if err != nil {
			r.eventf(fi, "Warning", "PodFaultPodListFailed", "podfault-select",
				"Failed listing candidate pods for action %q tick %q: %v", p.ActionName, p.TickID, err)
			return false, earliestDue, err
		}

		// Guardrail: refuse if too few pods exist.
		if int32(len(candidates)) < p.RefuseIfPodsLessThan {
			msg := fmt.Sprintf(
				"pod fault action %q refused: candidates=%d < refuseIfPodsLessThan=%d",
				p.ActionName, len(candidates), p.RefuseIfPodsLessThan,
			)

			r.eventf(fi, "Warning", "PodFaultRefused", "podfault-select", msg)

			fi.Status.Phase = ErrorPhase
			fi.Status.Message = msg
			_ = r.Status().Update(ctx, fi)

			return true, earliestDue, nil // stop reconcile; no job creation
		}

		// Derive selection mode from plan fields.
		var mode chaosv1alpha1.PodSelectionMode
		switch {
		case p.SelectionCount != nil:
			mode = chaosv1alpha1.PodSelectionModeCount
		case p.SelectionPercent != nil:
			mode = chaosv1alpha1.PodSelectionModePercent
		default:
			r.eventf(fi, "Warning", "PodFaultInvalidPlan", "podfault-select",
				"Invalid pod fault plan for action %q tick %q: neither selectionCount nor selectionPercent is set",
				p.ActionName, p.TickID)
			continue
		}

		selected := selectPodNamesDeterministically(
			candidates,
			mode,
			p.SelectionCount,
			p.SelectionPercent,
			p.MaxPodsAffectedEffective,
		)

		if len(selected) == 0 {
			r.eventf(fi, "Normal", "PodFaultNoTargets", "podfault-select",
				"No target pods selected for action %q tick %q", p.ActionName, p.TickID)
			continue
		}

		// Set explicit pod names on the plan so the Job deletes by name only.
		p.PodNames = selected

		// Record planned pods in FI status before lease/job.
		_ = r.recordPodFaultPlanned(ctx, fi, p, now)

		// 3) Acquire/takeover per-tick lease.
		acquired, reason, lerr := r.tryAcquireOrTakeoverLease(ctx, fi, p.LeaseName, now)
		if lerr != nil {
			r.eventf(fi, "Warning", "PodFaultLeaseError", "podfault-lease",
				"Lease error for action %q tick %q: %v", p.ActionName, p.TickID, lerr)
			return false, earliestDue, lerr
		}
		if !acquired {
			r.eventf(fi, "Normal", "PodFaultLockBusy", "podfault-lease",
				"Pod fault lock busy for action %q tick %q (lease=%s): %s",
				p.ActionName, p.TickID, p.LeaseName, reason)
			continue
		}

		r.eventf(fi, "Normal", "PodFaultLockAcquired", "podfault-lease",
			"Pod fault lock acquired for action %q tick %q (lease=%s): %s",
			p.ActionName, p.TickID, p.LeaseName, reason)

		// 4) Create deterministic Job (idempotent).
		if err := r.createPodFaultJobIfNotExists(ctx, fi, p); err != nil {
			r.eventf(fi, "Warning", "PodFaultJobCreateFailed", "podfault-job-create",
				"Failed creating podfault Job for action %q tick %q: %v", p.ActionName, p.TickID, err)
			return false, earliestDue, err
		}

		r.eventf(fi, "Normal", "PodFaultJobCreated", "podfault-job-create",
			"Created podfault Job for action %q tick %q (job=%s/%s)",
			p.ActionName, p.TickID, fi.Namespace, p.JobName)
	}

	return false, earliestDue, nil
}

// buildPodFaultPlans builds per-action (and per-tick) execution plans.
//
// For each action it:
//   - Resolves execution mode default (ONE_SHOT).
//   - Resolves target namespace default (FI namespace).
//   - Computes effective max pods cap from FI/action/window caps.
//   - Computes tickID and earliest nextDue boundary for WINDOWED mode.
//   - Creates deterministic JobName and LeaseName for (fi,action,tick).
//
// It returns:
//   - plans: all plans to evaluate in this reconcile.
//   - earliestDue: earliest upcoming tick boundary across WINDOWED actions (nil if none).
func (r *FaultInjectionReconciler) buildPodFaultPlans(
	fi *chaosv1alpha1.FaultInjection,
	now time.Time,
	startedAt time.Time,
) (plans []podFaultPlan, earliestDue *time.Time, err error) {

	for _, a := range fi.Spec.Actions.PodFaults {
		mode := resolvePodFaultMode(a)
		targetNS := resolvePodFaultTargetNS(fi, a)

		if err := validatePodFaultSelection(a); err != nil {
			return nil, earliestDue, err
		}

		effectiveMax := capEffectiveMaxPods(fi, a, mode)

		// Tick & due
		tickID := "oneshot"
		var dueAt *time.Time

		if mode == chaosv1alpha1.ExecutionModeWindowed {
			wTickID, wDueAt, shouldPlan, nextDue, werr := computeWindowedTick(a, now, startedAt)
			if werr != nil {
				return nil, earliestDue, werr
			}

			updateEarliestDue(&earliestDue, nextDue)

			if !shouldPlan {
				continue
			}
			tickID, dueAt = wTickID, wDueAt
		}

		jobName, leaseName := podFaultNames(fi.Name, a.Name, tickID)

		plans = append(plans, podFaultPlan{
			ActionName: a.Name,
			TickID:     tickID,
			DueAt:      dueAt,

			JobName:   jobName,
			LeaseName: leaseName,

			Namespace:        targetNS,
			SelectorLabels:   a.Target.Selector.MatchLabels,
			SelectionCount:   a.Selection.Count,
			SelectionPercent: a.Selection.Percent,

			TerminationMode: a.Termination.Mode,
			GracePeriodSecs: a.Termination.GracePeriodSeconds,

			MaxPodsAffectedEffective: effectiveMax,
			RespectPDB:               a.Guardrails.RespectPodDisruptionBudget,
			RefuseIfPodsLessThan:     a.Guardrails.RefuseIfPodsLessThan,
		})
	}

	return plans, earliestDue, nil
}

// listCandidatePods lists candidate pods for a pod-fault action.
//
// The controller must be aware of the exact pods it intends to delete.
// Candidate selection happens in the controller, not in the executor Job.
//
// It filters out terminating pods (pods with deletionTimestamp) and returns
// a deterministically sorted slice by pod name.
func (r *FaultInjectionReconciler) listCandidatePods(
	ctx context.Context,
	namespace string,
	matchLabels map[string]string,
) ([]unstructured.Unstructured, error) {

	var pods unstructured.UnstructuredList
	pods.SetGroupVersionKind(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PodList"})

	opts := []client.ListOption{client.InNamespace(namespace)}
	if len(matchLabels) > 0 {
		opts = append(opts, client.MatchingLabels(matchLabels))
	}

	if err := r.List(ctx, &pods, opts...); err != nil {
		return nil, err
	}

	out := make([]unstructured.Unstructured, 0, len(pods.Items))
	for _, p := range pods.Items {
		if p.GetDeletionTimestamp() != nil {
			continue
		}
		out = append(out, p)
	}

	// Deterministic order.
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

// selectPodNamesDeterministically chooses the exact pods for deletion.
//
// Deterministic selection supports:
//   - idempotence (same inputs => same selection)
//   - auditability (controller can record exact targets)
//   - repeatability during reconciliation retries
//
// Selection rules:
//   - COUNT: desired = selection.count
//   - PERCENT: desired = ceil(n * selection.percent / 100)
//   - desired is clamped to [1..n]
//   - if maxPods > 0, desired is additionally clamped to maxPods
//
// Candidates must already be in deterministic order (caller sorts by name).
func selectPodNamesDeterministically(
	candidates []unstructured.Unstructured,
	mode chaosv1alpha1.PodSelectionMode,
	count *int32,
	percent *int32,
	maxPods int32,
) []string {

	n := len(candidates)
	if n == 0 {
		return nil
	}

	desired := 0
	switch mode {
	case chaosv1alpha1.PodSelectionModeCount:
		if count != nil {
			desired = int(*count)
		}
	case chaosv1alpha1.PodSelectionModePercent:
		if percent != nil {
			// ceil(n * percent / 100)
			desired = int((int64(n)*int64(*percent) + 99) / 100)
		}
	default:
		desired = 0
	}

	if desired < 1 {
		return nil
	}
	if desired > n {
		desired = n
	}
	if maxPods > 0 && desired > int(maxPods) {
		desired = int(maxPods)
	}

	names := make([]string, 0, desired)
	for i := 0; i < desired; i++ {
		names = append(names, candidates[i].GetName())
	}
	return names
}

// tryAcquireOrTakeoverLease acquires a per-tick coordination Lease.
//
// This provides single-writer safety for creating/running the pod-fault Job for a given (action,tick).
//
// Behavior:
//   - If the Lease does not exist, it attempts to create it with holder=r.HolderIdentity.
//   - If the Lease exists and is not stale, acquisition fails with a "busy" reason.
//   - If the Lease is stale, the reconciler attempts a takeover using optimistic conflict retries.
//
// Staleness:
//   - last = renewTime if present, otherwise acquireTime.
//   - stale after: leaseDurationSeconds + defaultPodFaultLeaseSafetySkew.
func (r *FaultInjectionReconciler) tryAcquireOrTakeoverLease(
	ctx context.Context,
	fi *chaosv1alpha1.FaultInjection,
	leaseName string,
	now time.Time,
) (bool, string, error) {

	holder := strings.TrimSpace(r.HolderIdentity)
	if holder == "" {
		holder = "fi-operator"
	}

	key := types.NamespacedName{Namespace: fi.Namespace, Name: leaseName}
	lease := &coordv1.Lease{}

	// Fast-path: create if missing.
	if err := r.Get(ctx, key, lease); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, "get failed", err
		}

		d := defaultPodFaultLeaseDurationSeconds
		lease = &coordv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: fi.Namespace,
				Name:      leaseName,
				Labels: map[string]string{
					"chaos.sghaida.io/fi":   fi.Name,
					"chaos.sghaida.io/kind": "podfault",
				},
			},
			Spec: coordv1.LeaseSpec{
				HolderIdentity:       &holder,
				AcquireTime:          &metav1.MicroTime{Time: now},
				RenewTime:            &metav1.MicroTime{Time: now},
				LeaseDurationSeconds: &d,
			},
		}
		if r.Scheme != nil {
			_ = controllerutil.SetControllerReference(fi, lease, r.Scheme)
		}

		if err := r.Create(ctx, lease); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, "created by another reconciler", nil
			}
			return false, "create failed", err
		}
		return true, "created", nil
	}

	// --- helpers ---
	lastTime := func(l *coordv1.Lease) time.Time {
		if l.Spec.RenewTime != nil {
			return l.Spec.RenewTime.Time
		}
		if l.Spec.AcquireTime != nil {
			return l.Spec.AcquireTime.Time
		}
		return time.Time{}
	}
	leaseDuration := func(l *coordv1.Lease) int32 {
		d := defaultPodFaultLeaseDurationSeconds
		if l.Spec.LeaseDurationSeconds != nil && *l.Spec.LeaseDurationSeconds > 0 {
			d = *l.Spec.LeaseDurationSeconds
		}
		return d
	}
	isStaleAt := func(last time.Time, durSec int32, now time.Time) bool {
		if last.IsZero() {
			return true
		}
		dur := time.Duration(durSec) * time.Second

		// IMPORTANT: cap skew so it doesn't dominate tiny leases (e.g. dur=1s).
		skew := min(defaultPodFaultLeaseSafetySkew, dur)

		return now.After(last.Add(dur + skew))
	}

	// Determine staleness.
	last := lastTime(lease)
	d := leaseDuration(lease)

	if !isStaleAt(last, d, now) {
		cur := ""
		if lease.Spec.HolderIdentity != nil {
			cur = *lease.Spec.HolderIdentity
		}
		dur := time.Duration(d) * time.Second
		skew := min(defaultPodFaultLeaseSafetySkew, dur)

		staleAfter := dur + skew
		return false, fmt.Sprintf(`held by %q; renewTime=%s; staleAfter=%s`, cur, last.Format(time.RFC3339Nano), staleAfter), nil
	}

	// Takeover with conflict retry.
	var errLeaseNoLongerStale = errors.New("lease no longer stale")

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &coordv1.Lease{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}

		lLast := lastTime(latest)
		ld := leaseDuration(latest)

		if !isStaleAt(lLast, ld, now) {
			// Someone renewed it while we were trying: treat as busy.
			return errLeaseNoLongerStale
		}

		latest.Spec.HolderIdentity = &holder
		latest.Spec.RenewTime = &metav1.MicroTime{Time: now}
		latest.Spec.AcquireTime = &metav1.MicroTime{Time: now}

		if latest.Spec.LeaseDurationSeconds == nil || *latest.Spec.LeaseDurationSeconds <= 0 {
			dd := defaultPodFaultLeaseDurationSeconds
			latest.Spec.LeaseDurationSeconds = &dd
		}

		return r.Update(ctx, latest)
	})
	if err != nil {
		if errors.Is(err, errLeaseNoLongerStale) {
			return false, "lease no longer stale (renewed by another holder)", nil
		}
		return false, "takeover update failed", err
	}

	// Confirm we hold it.
	confirm := &coordv1.Lease{}
	if err := r.Get(ctx, key, confirm); err != nil {
		return false, "confirm get failed", err
	}
	if confirm.Spec.HolderIdentity == nil || *confirm.Spec.HolderIdentity != holder {
		return false, "takeover lost to another holder", nil
	}

	return true, "taken over (stale)", nil
}

// createPodFaultJobIfNotExists creates the per-tick executor Job if missing.
//
// The controller passes explicit pod names to the Job via POD_NAMES_CSV.
// The Job must not list pods; it must delete only the exact targets selected by the controller.
//
// The job is deterministic and idempotent:
//   - If the Job already exists, this is a no-op.
//   - Job labels include FI name, action slug, and tick for cleanup/auditability.
func (r *FaultInjectionReconciler) createPodFaultJobIfNotExists(
	ctx context.Context,
	fi *chaosv1alpha1.FaultInjection,
	p podFaultPlan,
) error {

	key := types.NamespacedName{Namespace: fi.Namespace, Name: p.JobName}
	existing := &batchv1.Job{}
	if err := r.Get(ctx, key, existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	ttl := defaultPodFaultJobTTLSecondsAfterFinished
	ad := defaultPodFaultJobActiveDeadlineSeconds
	bo := defaultPodFaultJobBackoffLimit

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fi.Namespace,
			Name:      p.JobName,
			Labels: map[string]string{
				"chaos.sghaida.io/fi":     fi.Name,
				"chaos.sghaida.io/kind":   "podfault",
				"chaos.sghaida.io/action": sanitizeName(p.ActionName),
				"chaos.sghaida.io/tick":   p.TickID,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &bo,
			ActiveDeadlineSeconds:   &ad,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"chaos.sghaida.io/fi":   fi.Name,
						"chaos.sghaida.io/kind": "podfault",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "fi-podfault-executor",
					Containers: []corev1.Container{
						{
							Name:    "executor",
							Image:   "REPLACE_ME_EXECUTOR_IMAGE",
							Command: []string{"/bin/sh", "-lc"},
							Args: []string{
								"echo 'TODO: implement podfault executor'; sleep 1",
							},
							Env: []corev1.EnvVar{
								{Name: "FI_NAME", Value: fi.Name},
								{Name: "ACTION_NAME", Value: p.ActionName},
								{Name: "TICK_ID", Value: p.TickID},
								{Name: "TARGET_NAMESPACE", Value: p.Namespace},
								{Name: "POD_NAMES_CSV", Value: strings.Join(p.PodNames, ",")},
								{Name: "TERMINATION_MODE", Value: string(p.TerminationMode)},
								{Name: "GRACE_PERIOD_SECONDS", Value: fmt.Sprintf("%d", p.GracePeriodSecs)},
							},
						},
					},
				},
			},
		},
	}

	if r.Scheme != nil {
		if err := controllerutil.SetControllerReference(fi, job, r.Scheme); err != nil {
			return err
		}
	}

	return r.Create(ctx, job)
}

// recordPodFaultPlanned records the selected target pods in FaultInjection status.
//
// This is written before lease/job creation so status reflects intent even if later steps fail.
// Updates are idempotent per (actionName,tickID) using upsertPodFaultTickStatus.
func (r *FaultInjectionReconciler) recordPodFaultPlanned(
	ctx context.Context,
	fi *chaosv1alpha1.FaultInjection,
	p podFaultPlan,
	now time.Time,
) error {

	key := client.ObjectKeyFromObject(fi)
	tNow := metav1.NewTime(now.UTC())

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest chaosv1alpha1.FaultInjection
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}

		e := upsertPodFaultTickStatus(&latest.Status, p.ActionName, p.TickID)
		e.PlannedAt = &tNow
		e.PlannedPods = append([]string(nil), p.PodNames...) // copy
		e.JobName = p.JobName
		e.State = "Planned"
		e.Message = ""

		return r.Status().Update(ctx, &latest)
	})
}

// recordPodFaultExecuted records per-tick execution outcome in FaultInjection status.
//
// It updates the same (actionName,tickID) entry and records:
//   - executed timestamp
//   - succeeded/failed state
//   - optional message
//
// If planned pods were not recorded earlier, it records the executed pod set as PlannedPods.
func (r *FaultInjectionReconciler) recordPodFaultExecuted(
	ctx context.Context,
	fi *chaosv1alpha1.FaultInjection,
	p podFaultPlan,
	now time.Time,
	succeeded bool,
	msg string,
) error {

	key := client.ObjectKeyFromObject(fi)
	tNow := metav1.NewTime(now.UTC())

	state := "Failed"
	if succeeded {
		state = "Succeeded"
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest chaosv1alpha1.FaultInjection
		if err := r.Get(ctx, key, &latest); err != nil {
			return err
		}

		e := upsertPodFaultTickStatus(&latest.Status, p.ActionName, p.TickID)

		// Preserve planned pods if already set; otherwise record what we executed.
		if len(e.PlannedPods) == 0 && len(p.PodNames) > 0 {
			e.PlannedPods = append([]string(nil), p.PodNames...)
		}

		e.JobName = p.JobName
		e.ExecutedAt = &tNow
		e.State = state
		e.Message = msg

		return r.Status().Update(ctx, &latest)
	})
}

// cleanupPodFaultArtifacts deletes pod-fault Jobs and Leases for a given FaultInjection.
//
// It targets only objects labeled with:
//   - chaos.sghaida.io/fi = <fi.name>
//   - chaos.sghaida.io/kind = podfault
//
// Cleanup is best-effort:
//   - NotFound errors are ignored.
//   - Individual delete failures are emitted as events and the loop continues.
func (r *FaultInjectionReconciler) cleanupPodFaultArtifacts(ctx context.Context, fi *chaosv1alpha1.FaultInjection) error {
	// Jobs
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(fi.Namespace), client.MatchingLabels{
		"chaos.sghaida.io/fi":   fi.Name,
		"chaos.sghaida.io/kind": "podfault",
	}); err != nil {
		r.eventf(fi, "Warning", "PodFaultJobListFailed", "podfault-cleanup",
			"Failed listing podfault Jobs for cleanup: %v", err)
		return err
	}

	for i := range jobs.Items {
		j := jobs.Items[i].DeepCopy()
		if err := r.Delete(ctx, j); err != nil && !apierrors.IsNotFound(err) {
			r.eventf(fi, "Warning", "PodFaultJobDeleteFailed", "podfault-cleanup",
				"Failed deleting podfault Job %s/%s: %v", j.Namespace, j.Name, err)
			continue
		}
		r.eventf(fi, "Normal", "PodFaultJobDeleted", "podfault-cleanup",
			"Deleted podfault Job %s/%s", j.Namespace, j.Name)
	}

	// Leases
	var leases coordv1.LeaseList
	if err := r.List(ctx, &leases, client.InNamespace(fi.Namespace), client.MatchingLabels{
		"chaos.sghaida.io/fi":   fi.Name,
		"chaos.sghaida.io/kind": "podfault",
	}); err != nil {
		r.eventf(fi, "Warning", "PodFaultLeaseListFailed", "podfault-cleanup",
			"Failed listing podfault Leases for cleanup: %v", err)
		return err
	}

	for i := range leases.Items {
		l := leases.Items[i].DeepCopy()
		if err := r.Delete(ctx, l); err != nil && !apierrors.IsNotFound(err) {
			r.eventf(fi, "Warning", "PodFaultLeaseDeleteFailed", "podfault-cleanup",
				"Failed deleting podfault Lease %s/%s: %v", l.Namespace, l.Name, err)
			continue
		}
		r.eventf(fi, "Normal", "PodFaultLeaseDeleted", "podfault-cleanup",
			"Deleted podfault Lease %s/%s", l.Namespace, l.Name)
	}

	return nil
}

// upsertPodFaultTickStatus finds or creates a PodFaultTickStatus entry.
//
// This avoids duplicate entries and supports idempotent updates per (actionName,tickID).
func upsertPodFaultTickStatus(st *chaosv1alpha1.FaultInjectionStatus, actionName, tickID string) *chaosv1alpha1.PodFaultTickStatus {
	for i := range st.PodFaults {
		if st.PodFaults[i].ActionName == actionName && st.PodFaults[i].TickID == tickID {
			return &st.PodFaults[i]
		}
	}
	st.PodFaults = append(st.PodFaults, chaosv1alpha1.PodFaultTickStatus{
		ActionName: actionName,
		TickID:     tickID,
	})
	return &st.PodFaults[len(st.PodFaults)-1]
}

// isJobComplete returns true if the Job has a JobComplete condition set to True.
func isJobComplete(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isJobFailed returns true if the Job has a JobFailed condition set to True.
func isJobFailed(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func resolvePodFaultMode(a chaosv1alpha1.PodFaultAction) chaosv1alpha1.ExecutionMode {
	mode := chaosv1alpha1.ExecutionModeOneShot
	if a.Policy != nil && a.Policy.ExecutionMode != "" {
		mode = a.Policy.ExecutionMode
	}
	return mode
}

func resolvePodFaultTargetNS(fi *chaosv1alpha1.FaultInjection, a chaosv1alpha1.PodFaultAction) string {
	targetNS := strings.TrimSpace(a.Target.Namespace)
	if targetNS == "" {
		targetNS = fi.Namespace
	}
	return targetNS
}

func validatePodFaultSelection(a chaosv1alpha1.PodFaultAction) error {
	switch a.Selection.Mode {
	case chaosv1alpha1.PodSelectionModeCount:
		if a.Selection.Count == nil || *a.Selection.Count <= 0 {
			return fmt.Errorf("podFault %q has invalid selection.count (nil or <=0)", a.Name)
		}
	case chaosv1alpha1.PodSelectionModePercent:
		if a.Selection.Percent == nil || *a.Selection.Percent <= 0 || *a.Selection.Percent > 100 {
			return fmt.Errorf("podFault %q has invalid selection.percent (nil or not in 1..100)", a.Name)
		}
	default:
		return fmt.Errorf("podFault %q has unsupported selection.mode=%q", a.Name, a.Selection.Mode)
	}
	return nil
}

func capEffectiveMaxPods(fi *chaosv1alpha1.FaultInjection, a chaosv1alpha1.PodFaultAction, mode chaosv1alpha1.ExecutionMode) int32 {
	effectiveMax := int32(0)

	// Cap by FI blastRadius.maxPodsAffected if set (int64).
	if fi.Spec.BlastRadius.MaxPodsAffected > 0 {
		const maxInt32 = int64(^uint32(0) >> 1)
		if fi.Spec.BlastRadius.MaxPodsAffected > maxInt32 {
			effectiveMax = int32(maxInt32)
		} else {
			effectiveMax = int32(fi.Spec.BlastRadius.MaxPodsAffected)
		}
	}

	// Cap by action selection.maxPodsAffected if set.
	if a.Selection.MaxPodsAffected != nil && *a.Selection.MaxPodsAffected > 0 {
		if effectiveMax == 0 || *a.Selection.MaxPodsAffected < effectiveMax {
			effectiveMax = *a.Selection.MaxPodsAffected
		}
	}

	// Cap by window.maxTotalPodsAffected (WINDOWED only).
	if mode == chaosv1alpha1.ExecutionModeWindowed && a.Window != nil && a.Window.MaxTotalPodsAffected > 0 {
		if effectiveMax == 0 || a.Window.MaxTotalPodsAffected < effectiveMax {
			effectiveMax = a.Window.MaxTotalPodsAffected
		}
	}

	return effectiveMax
}

// computeWindowedTick returns:
// - tickID/dueAt when we should plan *this* reconcile
// - maybeNextDue used to update earliestDue (even when we skip plan because now < startedAt)
func computeWindowedTick(a chaosv1alpha1.PodFaultAction, now, startedAt time.Time) (tickID string, dueAt *time.Time, shouldPlan bool, maybeNextDue *time.Time, err error) {
	if a.Window == nil {
		return "", nil, false, nil, fmt.Errorf("podFault %q is WINDOWED but window is nil", a.Name)
	}

	interval := time.Duration(a.Window.IntervalSeconds) * time.Second
	if interval <= 0 {
		return "", nil, false, nil, fmt.Errorf("podFault %q has invalid window.intervalSeconds=%d", a.Name, a.Window.IntervalSeconds)
	}

	// If now is before startedAt, don't schedule yet; next due is startedAt.
	if now.Before(startedAt) {
		t := startedAt
		return "", nil, false, &t, nil
	}

	// Current tick index since startedAt.
	elapsed := now.Sub(startedAt)
	tick := int64(elapsed / interval)
	tickID = fmt.Sprintf("%d", tick)

	// Next due is next tick boundary.
	next := startedAt.Add(time.Duration(tick+1) * interval)
	dueAt = &next
	return tickID, dueAt, true, &next, nil
}

func podFaultNames(fiName, actionName, tickID string) (jobName, leaseName string) {
	actionSlug := sanitizeName(actionName)
	jobName = fmt.Sprintf("fi-%s-pod-%s-%s", fiName, actionSlug, tickID)
	leaseName = fmt.Sprintf("fi-%s-pod-%s-%s", fiName, actionSlug, tickID)
	return
}

func updateEarliestDue(earliest **time.Time, candidate *time.Time) {
	if candidate == nil {
		return
	}
	if *earliest == nil || candidate.Before(**earliest) {
		t := *candidate // copy value so caller doesn’t keep pointer to local
		*earliest = &t
	}
}
