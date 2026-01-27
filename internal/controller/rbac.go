// Package controller contains the Kubernetes controller-runtime reconciler that
// implements the FaultInjection custom resource for Istio/Envoy HTTP fault injection.
//
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=chaos.sghaida.io,resources=faultinjections/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
package controller
