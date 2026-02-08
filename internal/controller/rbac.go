// Package controller contains the Kubernetes controller-runtime reconciler that
// implements the FaultInjection custom resource for Istio/Envoy HTTP fault injection.
//
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch;create;patch;update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=watch;create;patch;update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

package controller
