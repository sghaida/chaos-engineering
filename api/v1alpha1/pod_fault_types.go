package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PodFaultActionType is the type discriminator for pod-level actions.
type PodFaultActionType string

// ExecutionMode controls how an action is applied over the experiment window.
type ExecutionMode string

// PodSelectionMode controls how pods are selected.
type PodSelectionMode string

// PodTerminationMode selects graceful vs forceful termination.
type PodTerminationMode string

const (
	// PodFaultActionTypePodDelete deletes selected pods (graceful or forceful).
	PodFaultActionTypePodDelete PodFaultActionType = "POD_DELETE"

	// ExecutionModeOneShot applies the action once per experiment.
	ExecutionModeOneShot ExecutionMode = "ONE_SHOT"

	// ExecutionModeWindowed applies the action periodically during the experiment window.
	ExecutionModeWindowed ExecutionMode = "WINDOWED"

	// PodSelectionModeCount Count of Impacted pods for Pod fault
	PodSelectionModeCount PodSelectionMode = "COUNT"

	// PodSelectionModePercent Percentage of impacted pods for Pod fault
	PodSelectionModePercent PodSelectionMode = "PERCENT"

	// PodTerminationModeGraceful Graceful option for killing pods
	PodTerminationModeGraceful PodTerminationMode = "GRACEFUL"

	// PodTerminationModeForceful Forceful option for killing pods
	PodTerminationModeForceful PodTerminationMode = "FORCEFUL"
)

// PodFaultAction defines a single pod-level chaos action.
// Audit is mandatory: the controller MUST emit events, log, and record the affected pod names in status.
// Cross-field validation:
// - FORCEFUL requires gracePeriodSeconds == 0
// - GRACEFUL requires gracePeriodSeconds > 0
// PodFaultAction defines how a pod-level fault is applied to Kubernetes workloads.
//
// Semantics:
//
// - type=POD_DELETE:
//   - Deletes selected pods in the target (graceful or forceful).
//   - The controller MUST record the impacted pod names in status and emit events for auditability.
//
// Defaulting:
//
// - policy.executionMode defaults to ONE_SHOT.
// - target.namespace defaults to the FaultInjection namespace (controller-enforced) when omitted.
// - guardrails.refuseIfPodsLessThan defaults to 1.
// - guardrails.respectPodDisruptionBudget defaults to true.
//
// Admission policy (cross-field invariants; enforced by CRD validation, admission webhook, or controller):
//
// - Termination mode and grace period:
//   - When termination.mode=FORCEFUL:
//   - termination.gracePeriodSeconds MUST be 0
//   - When termination.mode=GRACEFUL:
//   - termination.gracePeriodSeconds MUST be > 0
//
// - Execution mode and window:
//   - When policy.executionMode=WINDOWED:
//   - window is required
//   - When policy.executionMode=ONE_SHOT (or policy omitted):
//   - window MUST be omitted
//
// - Pod selection mode:
//   - When selection.mode=COUNT:
//   - selection.count is required
//   - selection.percent MUST be omitted
//   - When selection.mode=PERCENT:
//   - selection.percent is required (1..100)
//   - selection.count MUST be omitted
//
// Safety expectations:
//
//   - guardrails are mandatory.
//   - selection.maxPodsAffected (if set) acts as an additional per-action cap and is still bounded by
//     spec.blastRadius.maxPodsAffected (if the parent FaultInjection defines it).
//
// +kubebuilder:validation:XValidation:rule="(self.termination.mode == 'FORCEFUL' && self.termination.gracePeriodSeconds == 0) || (self.termination.mode == 'GRACEFUL' && self.termination.gracePeriodSeconds > 0)",message="termination.gracePeriodSeconds must be 0 for FORCEFUL and >0 for GRACEFUL"
// - WINDOWED requires window to be set; ONE_SHOT forbids window
// +kubebuilder:validation:XValidation:rule="((has(self.policy) && self.policy.executionMode == 'WINDOWED') && has(self.window)) || ((!has(self.policy) || self.policy.executionMode == 'ONE_SHOT') && !has(self.window))",message="window must be set when executionMode=WINDOWED and must be omitted when executionMode=ONE_SHOT"
type PodFaultAction struct {
	// Name is a stable identifier for this action within the experiment.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Type selects the action behavior.
	// +kubebuilder:validation:Enum=POD_DELETE
	Type PodFaultActionType `json:"type"`

	// Target selects pods to affect.
	// +kubebuilder:validation:Required
	Target PodTargetSpec `json:"target"`

	// Selection controls how many pods are affected.
	// +kubebuilder:validation:Required
	Selection PodSelectionSpec `json:"selection"`

	// Termination controls graceful vs forceful termination semantics.
	// +kubebuilder:validation:Required
	Termination PodTerminationSpec `json:"termination"`

	// Policy controls how this action is applied.
	// +optional
	Policy *PodFaultPolicy `json:"policy,omitempty"`

	// Window controls windowed execution parameters.
	// Required when policy.executionMode=WINDOWED and forbidden otherwise.
	// +optional
	Window *PodWindowSpec `json:"window,omitempty"`

	// Guardrails are mandatory safety constraints for pod faults.
	// +kubebuilder:validation:Required
	Guardrails PodGuardrailsSpec `json:"guardrails"`
}

// PodFaultPolicy defines execution policy for a pod fault.
type PodFaultPolicy struct {
	// ExecutionMode defaults to ONE_SHOT.
	// +kubebuilder:validation:Enum=ONE_SHOT;WINDOWED
	// +kubebuilder:default:=ONE_SHOT
	// +optional
	ExecutionMode ExecutionMode `json:"executionMode,omitempty"`
}

// PodWindowSpec defines parameters for windowed pod faults.
type PodWindowSpec struct {
	// IntervalSeconds is the cadence for re-applying the action during WINDOWED execution.
	// +kubebuilder:validation:Minimum=1
	IntervalSeconds int32 `json:"intervalSeconds"`

	// MaxTotalPodsAffected is a hard cap across the entire experiment window for this action.
	// +kubebuilder:validation:Minimum=1
	MaxTotalPodsAffected int32 `json:"maxTotalPodsAffected"`
}

// PodTargetSpec selects pods by label selector.
type PodTargetSpec struct {
	// Namespace is the namespace to select pods from.
	// If omitted, defaults to the FaultInjection namespace (controller-enforced).
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Selector matches pods by labels.
	// +kubebuilder:validation:Required
	Selector LabelSelector `json:"selector"`
}

// PodSelectionSpec defines the number of pods to affect.
// - mode=COUNT requires count and forbids percent
// - mode=PERCENT requires percent and forbids count
//
// percent is bounded to 1..100.
// +kubebuilder:validation:XValidation:rule="(self.mode == 'COUNT' && has(self.count) && !has(self.percent)) || (self.mode == 'PERCENT' && has(self.percent) && !has(self.count))",message="set exactly one of selection.count or selection.percent based on selection.mode"
type PodSelectionSpec struct {
	// Mode selects count-based or percent-based selection.
	// +kubebuilder:validation:Enum=COUNT;PERCENT
	Mode PodSelectionMode `json:"mode"`

	// Count is used when mode=COUNT.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Count *int32 `json:"count,omitempty"`

	// Percent is used when mode=PERCENT.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	Percent *int32 `json:"percent,omitempty"`

	// MaxPodsAffected is an optional per-action cap (also bounded by spec.blastRadius.maxPodsAffected).
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxPodsAffected *int32 `json:"maxPodsAffected,omitempty"`
}

// PodTerminationSpec defines graceful vs forceful deletion semantics.
//
// Invariants are enforced on PodFaultAction:
//
// - mode=FORCEFUL requires gracePeriodSeconds=0
// - mode=GRACEFUL requires gracePeriodSeconds>0
type PodTerminationSpec struct {
	// Mode selects graceful vs forceful termination.
	// +kubebuilder:validation:Enum=GRACEFUL;FORCEFUL
	Mode PodTerminationMode `json:"mode"`

	// GracePeriodSeconds controls deletion grace period.
	// Cross-field validation is enforced on PodFaultAction.
	// +kubebuilder:validation:Minimum=0
	GracePeriodSeconds int64 `json:"gracePeriodSeconds"`
}

// PodGuardrailsSpec defines mandatory safety checks for pod faults.
type PodGuardrailsSpec struct {
	// RefuseIfPodsLessThan prevents taking down tiny fleets accidentally.
	// Default: 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=1
	// +optional
	RefuseIfPodsLessThan int32 `json:"refuseIfPodsLessThan,omitempty"`

	// RespectPodDisruptionBudget controls whether the controller must respect PDB constraints.
	// Default: true.
	// +kubebuilder:default:=true
	// +optional
	RespectPodDisruptionBudget bool `json:"respectPodDisruptionBudget,omitempty"`
}

// PodFaultTickStatus records per-action, per-tick execution state for pod faults.
// controller will use it to record affected pod names in status (planned/executed)
type PodFaultTickStatus struct {
	// ActionName is the pod fault action name from spec.actions.podFaults[].name.
	ActionName string `json:"actionName"`

	// TickID is "oneshot" or a window tick number string ("0","1",...).
	TickID string `json:"tickId"`

	// PlannedAt is when the controller selected target pods for this tick.
	// +optional
	PlannedAt *metav1.Time `json:"plannedAt,omitempty"`

	// PlannedPods are the exact pod names selected by the controller for this tick.
	// +optional
	PlannedPods []string `json:"plannedPods,omitempty"`

	// JobName is the executor Job name created for this tick.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// ExecutedAt is when the controller observed completion/failure of the executor job.
	// +optional
	ExecutedAt *metav1.Time `json:"executedAt,omitempty"`

	// State is one of: Planned, Running, Succeeded, Failed.
	// +optional
	State string `json:"state,omitempty"`

	// Message provides additional context (error, completion note).
	// +optional
	Message string `json:"message,omitempty"`
}
