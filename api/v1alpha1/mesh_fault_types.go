package v1alpha1

// MeshFaultAction defines a single HTTP fault injection action applied
// either to inbound or outbound traffic.
//
// Each MeshFaultAction:
// - targets exactly one traffic direction (INBOUND or OUTBOUND)
// - affects a bounded percentage of traffic
// - is independently cleaned up by name
// - may define multiple HTTP route matches
//
// Note: when defining multiple MeshFaultActions within the same FaultInjection,
// ensure that their route matches do not overlap to avoid conflicting VirtualService rules.
// This is the operator's responsibility when defining faults.
// The controller MAY validate for overlapping matches and reject conflicting configurations.
//
// Note: when applying outbound faults with sourceSelector,
// the controller MUST ensure that the affected pods do not exceed blastRadius.maxPodsAffected.
// If the controller cannot safely estimate the number of affected pods,
// it MUST abort the experiment to avoid wide-impact chaos.
//
// Note: the controller MUST clean up any created or patched VirtualServices
// when the FaultInjection completes or is cancelled.
type MeshFaultAction struct {
	// Name uniquely identifies this fault within the FaultInjection.
	//
	// It is used to generate deterministic VirtualService rule names,
	// enabling safe updates and cleanup.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Direction specifies whether the fault applies to inbound
	// traffic (requests entering the service) or outbound traffic
	// (requests leaving the service to a dependency).
	//
	// INBOUND  → patch an existing VirtualService for the service
	// OUTBOUND → patch or create a VirtualService targeting the destination host
	// +kubebuilder:validation:Enum=INBOUND;OUTBOUND
	Direction string `json:"direction"`

	// Type defines the kind of HTTP fault to inject.
	//
	// HTTP_LATENCY → fixed delay injection
	// HTTP_ABORT   → HTTP error response injection
	// +kubebuilder:validation:Enum=HTTP_LATENCY;HTTP_ABORT
	Type string `json:"type"`

	// Percent defines the percentage of matching traffic affected by this fault.
	//
	// Admission policy:
	// - Must be 0..100 (Maximum/Minimum).
	// - Must be <= blastRadius.maxTrafficPercent (cross-field invariant; admission webhook/controller validation).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percent int32 `json:"percent"`

	// HTTP defines how the fault is applied at the HTTP routing layer.
	// +kubebuilder:validation:Required
	HTTP HTTPFaultSpec `json:"http"`
}

// HTTPFaultSpec defines how a mesh fault is applied to HTTP traffic.
// The meaning of fields depends on the fault Direction:
//
// INBOUND:
//   - virtualServiceRef MUST be set
//   - destinationHosts MUST NOT be set
//
// OUTBOUND:
//   - destinationHosts MUST be set
//   - virtualServiceRef MAY be omitted (operator may create a managed VS)
//
// Admission policy (cross-field invariants; enforced by controller or admission webhook):
// - When direction=INBOUND:
//   - virtualServiceRef is required
//   - destinationHosts must be omitted
//
// - When direction=OUTBOUND:
//   - destinationHosts is required
//   - virtualServiceRef is optional
//
// - For type=HTTP_LATENCY:
//   - delay is required
//   - abort must be omitted
//
// - For type=HTTP_ABORT:
//
//   - abort is required
//
//   - delay must be omitted
//
//   - timeout is optional for both types
//
//   - At least one route MUST be specified
//
//   - Each route MUST have at least one match condition
//
//   - Each match MUST specify at least one of uriPrefix or uriExact
//     (if both are specified, uriExact takes precedence)
//
//   - If timeout is set, delay.fixedDelaySeconds MUST be greater than timeout.timeoutSeconds
//     to simulate percentage-based timeouts.
type HTTPFaultSpec struct {
	// VirtualServiceRef references an existing VirtualService to patch.
	//
	// Admission policy:
	// - Required when direction=INBOUND (cross-field invariant).
	// - Optional when direction=OUTBOUND.
	// +optional
	VirtualServiceRef *VirtualServiceRef `json:"virtualServiceRef,omitempty"`

	// DestinationHosts defines the destination hostnames for outbound faults.
	//
	// Admission policy:
	// - Required when direction=OUTBOUND (cross-field invariant).
	// - Must be omitted when direction=INBOUND (cross-field invariant).
	// +kubebuilder:validation:MaxItems=32
	// +optional
	DestinationHosts []string `json:"destinationHosts,omitempty"`

	// SourceSelector restricts OUTBOUND faults to traffic originating
	// from workloads matching these labels.
	//
	// Admission policy:
	// - Strongly recommended for safety; required if blastRadius.maxPodsAffected is set
	//   (cross-field invariant enforced by controller/webhook).
	// +optional
	SourceSelector *LabelSelector `json:"sourceSelector,omitempty"`

	// Routes define HTTP match conditions under which the fault applies.
	// These routes are prepended before existing VirtualService rules.
	//
	// Admission policy:
	// - Must have at least one entry (MinItems=1).
	// - Each match must set at least one of uriPrefix or uriExact (cross-field invariant).
	// +kubebuilder:validation:MinItems=1
	Routes []HTTPRouteTarget `json:"routes"`

	// Delay defines fixed latency injection.
	//
	// Admission policy:
	// - Required for HTTP_LATENCY (cross-field invariant).
	// - Must be omitted for HTTP_ABORT (cross-field invariant).
	// +optional
	Delay *DelayConfig `json:"delay,omitempty"`

	// Abort defines HTTP abort (error) injection.
	//
	// Admission policy:
	// - Required for HTTP_ABORT (cross-field invariant).
	// - Must be omitted for HTTP_LATENCY (cross-field invariant).
	// +optional
	Abort *AbortConfig `json:"abort,omitempty"`

	// Timeout defines an HTTP route timeout.
	//
	// Admission policy:
	// - Optional for both types.
	// - If set, delay.fixedDelaySeconds MUST be greater than timeout.timeoutSeconds
	//   to simulate percentage-based timeouts (cross-field invariant).
	// +optional
	Timeout *TimeoutConfig `json:"timeout,omitempty"`
}

// VirtualServiceRef identifies an existing Istio VirtualService in the same namespace.
// Used to scope inbound faults to a specific service.
type VirtualServiceRef struct {
	// Name of the VirtualService.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// HTTPRouteTarget defines a single HTTP routing match.
// Multiple targets may be specified per fault action.
type HTTPRouteTarget struct {
	// Match defines how incoming requests are matched.
	// +kubebuilder:validation:Required
	Match HTTPMatch `json:"match"`
}

// HTTPMatch defines HTTP request matching criteria.
//
// Admission policy (cross-field invariants; enforced by controller or admission webhook):
// - At least ONE of uriPrefix or uriExact MUST be specified.
// - If both are specified, uriExact takes precedence.
type HTTPMatch struct {
	// URIPrefix matches requests whose URI starts with this prefix.
	// Example: /VendorAPI
	// +kubebuilder:validation:MinLength=1
	// +optional
	URIPrefix string `json:"uriPrefix,omitempty"`

	// URIExact matches requests whose URI exactly equals this value.
	// Example: /VendorAPI/v1/orders
	// +kubebuilder:validation:MinLength=1
	// +optional
	URIExact string `json:"uriExact,omitempty"`

	// Headers defines optional exact-match HTTP headers.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
}
