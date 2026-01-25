package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("FaultInjection Controller", func() {
	ctx := context.Background()
	const ns = "default"

	It("reconciles a valid INBOUND FaultInjection: prepends injected rule and sets Running status", func() {
		By("creating an inbound target VirtualService")
		vs := testNewInboundVirtualService(ns, "inbound-vs")
		Expect(k8sClient.Create(ctx, vs)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewVirtualService(ns, "inbound-vs")) }()

		By("creating a FaultInjection CR (unstructured)")
		fi := testNewFaultInjectionUnstructured(ns, "fi-inbound",
			testFIActionInboundLatency("latency", "inbound-vs", 10, 1),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		By("reconciling")
		controllerReconciler := &FaultInjectionReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}

		res, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-inbound", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))

		By("verifying VirtualService injected rule was prepended and is routable")
		gotVS := testNewVirtualService(ns, "inbound-vs")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "inbound-vs", Namespace: ns}, gotVS)).To(Succeed())

		http := testMustHTTPList(gotVS)
		Expect(len(http)).To(BeNumerically(">=", 2))

		r0 := testMustRuleMap(http[0])
		Expect(r0["name"].(string)).To(HavePrefix("fi-fi-inbound-"))
		Expect(r0).To(HaveKey("fault"))
		Expect(r0).To(HaveKey("route"))

		r1 := testMustRuleMap(http[1])
		Expect(r1["name"]).To(Equal("existing-route"))

		By("verifying FaultInjection status is Running with timestamps")
		fi2 := testFetchFI(ctx, ns, "fi-inbound")
		st := testGetStatusMap(fi2)
		Expect(st["phase"]).To(Equal("Running"))
		Expect(st["message"]).To(ContainSubstring("Active"))
		Expect(st["startedAt"]).NotTo(BeNil())
		Expect(st["expiresAt"]).NotTo(BeNil())
	})

	It("reconciles a valid OUTBOUND FaultInjection: creates managed VS, attaches mesh gateway, and sets Running status", func() {
		By("creating a FaultInjection OUTBOUND CR")
		fi := testNewFaultInjectionUnstructured(ns, "fi-outbound",
			testFIActionOutboundLatency("latency", []string{"example.com"}, 10, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, fi)
			_ = k8sClient.Delete(ctx, testNewVirtualService(ns, "fi-fi-outbound-latency"))
		}()

		By("reconciling")
		controllerReconciler := &FaultInjectionReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-outbound", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying managed VirtualService created")
		managedName := "fi-fi-outbound-latency"
		gotVS := testNewVirtualService(ns, managedName)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: managedName, Namespace: ns}, gotVS)).To(Succeed())

		labels := gotVS.GetLabels()
		Expect(labels["managed-by"]).To(Equal("fi-operator"))
		Expect(labels["chaos.sghaida.io/fi"]).To(Equal("fi-outbound"))

		spec := testMustSpecMap(gotVS)
		Expect(spec["gateways"]).To(Equal([]any{"mesh"}))

		http := testMustHTTPList(gotVS)
		Expect(len(http)).To(BeNumerically(">=", 2))

		r0 := testMustRuleMap(http[0])
		Expect(r0["name"].(string)).To(HavePrefix("fi-fi-outbound-"))
		Expect(r0).To(HaveKey("fault"))
		Expect(r0).To(HaveKey("route"))

		By("verifying OUTBOUND match includes sourceLabels when provided")
		matches, ok := r0["match"].([]any)
		Expect(ok).To(BeTrue())
		Expect(matches).NotTo(BeEmpty())
		m0 := testMustRuleMap(matches[0])
		sl, ok := m0["sourceLabels"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(sl["app"]).To(Equal("a"))

		By("verifying FaultInjection status is Running")
		fi2 := testFetchFI(ctx, ns, "fi-outbound")
		st := testGetStatusMap(fi2)
		Expect(st["phase"]).To(Equal("Running"))
		Expect(st["startedAt"]).NotTo(BeNil())
		Expect(st["expiresAt"]).NotTo(BeNil())
	})

	It("INBOUND fails closed if target VirtualService has no routable rules: sets Error and does not inject", func() {
		By("creating an inbound VirtualService with no route anywhere")
		vs := testNewVirtualService(ns, "no-route-vs")
		vs.Object["spec"] = map[string]any{
			"hosts": []any{"example.local"},
			"http": []any{
				map[string]any{
					"name":  "no-route",
					"match": []any{map[string]any{"uri": map[string]any{"prefix": "/"}}},
					// no route/redirect/direct_response
				},
			},
		}
		Expect(k8sClient.Create(ctx, vs)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewVirtualService(ns, "no-route-vs")) }()

		By("creating an INBOUND FaultInjection targeting the no-route VS")
		fi := testNewFaultInjectionUnstructured(ns, "fi-noroute",
			testFIActionInboundLatency("latency", "no-route-vs", 10, 1),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		By("reconciling")
		controllerReconciler := &FaultInjectionReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-noroute", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying status Error")
		fi2 := testFetchFI(ctx, ns, "fi-noroute")
		st := testGetStatusMap(fi2)
		Expect(st["phase"]).To(Equal("Error"))
		Expect(st["message"]).To(ContainSubstring("has no route"))

		By("verifying VirtualService was not injected")
		gotVS := testNewVirtualService(ns, "no-route-vs")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "no-route-vs", Namespace: ns}, gotVS)).To(Succeed())
		http := testMustHTTPList(gotVS)
		r0 := testMustRuleMap(http[0])
		Expect(r0["name"]).To(Equal("no-route"))
	})

	It("guardrail: percent > maxTrafficPercent sets Error and does not create managed VS", func() {
		fi := testNewFaultInjectionUnstructured(ns, "fi-bad-percent",
			testFIActionOutboundLatency("latency", []string{"example.com"}, 80, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(60, 50, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		controllerReconciler := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-bad-percent", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		fi2 := testFetchFI(ctx, ns, "fi-bad-percent")
		st := testGetStatusMap(fi2)
		Expect(st["phase"]).To(Equal("Error"))
		Expect(st["message"]).To(ContainSubstring("blastRadius exceeded"))

		managedName := "fi-fi-bad-percent-latency"
		e := k8sClient.Get(ctx, types.NamespacedName{Name: managedName, Namespace: ns}, testNewVirtualService(ns, managedName))
		Expect(apierrors.IsNotFound(e)).To(BeTrue())
	})

	It("CRD validation: durationSeconds=0 is rejected by the API server (controller is not involved)", func() {
		fi := testNewFaultInjectionUnstructured(ns, "fi-bad-duration",
			testFIActionOutboundLatency("latency", []string{"example.com"}, 10, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(0, 100, 0), // invalid per CRD schema: must be >= 1
		)

		err := k8sClient.Create(ctx, fi)
		Expect(err).To(HaveOccurred())

		// Typically 422 Invalid (FieldValueInvalid). Check in a robust way:
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	})

	It("guardrail: maxPodsAffected>0 requires sourceSelector.matchLabels (controller sets Error)", func() {
		// This is controller-only validation (not CRD schema), so the object is creatable.
		fi := testNewFaultInjectionUnstructured(ns, "fi-bad-maxpods",
			testFIActionOutboundLatency("latency", []string{"example.com"}, 10, 1, nil /* no sourceSelector */),
			testFIBlastRadius(60, 100, 1), // maxPodsAffected > 0 triggers requirement
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		controllerReconciler := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-bad-maxpods", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		fi2 := testFetchFI(ctx, ns, "fi-bad-maxpods")
		st := testGetStatusMap(fi2)
		Expect(st["phase"]).To(Equal("Error"))
		Expect(st["message"]).To(ContainSubstring("maxPodsAffected"))
		Expect(st["message"]).To(ContainSubstring("sourceSelector"))
	})

	It("expiry cleans up: deletes managed VS and deletes only Deployments owned by this FaultInjection (strict UID match)", func() {
		fi := testNewFaultInjectionUnstructured(ns, "fi-expire",
			testFIActionOutboundLatency("latency", []string{"example.com"}, 10, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(1, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, fi)
			_ = k8sClient.Delete(ctx, testNewVirtualService(ns, "fi-fi-expire-latency"))
		}()

		controllerReconciler := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-expire", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		gotFI := testFetchFI(ctx, ns, "fi-expire")
		fiUID := gotFI.GetUID()
		Expect(string(fiUID)).NotTo(BeEmpty())

		ownedDep := testNewDeploymentUnstructured(ns, "owned-dep", []metav1.OwnerReference{
			{
				APIVersion: gotFI.GetAPIVersion(),
				Kind:       gotFI.GetKind(),
				Name:       gotFI.GetName(),
				UID:        fiUID,
			},
		})
		Expect(k8sClient.Create(ctx, ownedDep)).To(Succeed())

		// Same kind/name but DIFFERENT UID => must NOT be deleted
		wrongUIDDep := testNewDeploymentUnstructured(ns, "wrong-uid-dep", []metav1.OwnerReference{
			{
				APIVersion: gotFI.GetAPIVersion(),
				Kind:       gotFI.GetKind(),
				Name:       gotFI.GetName(),
				UID:        types.UID("different-uid"),
			},
		})
		Expect(k8sClient.Create(ctx, wrongUIDDep)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewDeployment(ns, "wrong-uid-dep")) }()

		notOwnedDep := testNewDeploymentUnstructured(ns, "not-owned-dep", []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: types.UID("1111")},
		})
		Expect(k8sClient.Create(ctx, notOwnedDep)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewDeployment(ns, "not-owned-dep")) }()

		By("forcing expiry by setting status.startedAt in the past")
		Expect(testForceFIStartedAt(ctx, gotFI, time.Now().Add(-10*time.Second))).To(Succeed())

		By("reconciling again to trigger expiry cleanup")
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-expire", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		managedName := "fi-fi-expire-latency"

		Eventually(func() bool {
			e := k8sClient.Get(ctx, types.NamespacedName{Name: managedName, Namespace: ns}, testNewVirtualService(ns, managedName))
			return apierrors.IsNotFound(e)
		}).Should(BeTrue())

		Eventually(func() bool {
			e := k8sClient.Get(ctx, types.NamespacedName{Name: "owned-dep", Namespace: ns}, testNewDeployment(ns, "owned-dep"))
			return apierrors.IsNotFound(e)
		}).Should(BeTrue())

		Consistently(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "wrong-uid-dep", Namespace: ns}, testNewDeployment(ns, "wrong-uid-dep"))
		}).Should(Succeed())

		Consistently(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "not-owned-dep", Namespace: ns}, testNewDeployment(ns, "not-owned-dep"))
		}).Should(Succeed())

		fi3 := testFetchFI(ctx, ns, "fi-expire")
		st := testGetStatusMap(fi3)
		Expect(st["phase"]).To(Equal("Completed"))
		Expect(st["message"]).To(ContainSubstring("Expired"))
	})
})

/* -----------------------------
   Test-only helpers
--------------------------------*/

func testNewVirtualService(namespace, name string) *unstructured.Unstructured {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1beta1",
		Kind:    "VirtualService",
	})
	vs.SetNamespace(namespace)
	vs.SetName(name)
	return vs
}

func testNewInboundVirtualService(namespace, name string) *unstructured.Unstructured {
	vs := testNewVirtualService(namespace, name)
	vs.Object["spec"] = map[string]any{
		"hosts": []any{"example.local"},
		"http": []any{
			map[string]any{
				"name":  "existing-route",
				"match": []any{map[string]any{"uri": map[string]any{"prefix": "/"}}},
				"route": []any{
					map[string]any{"destination": map[string]any{"host": "example-svc"}},
				},
			},
		},
	}
	return vs
}

func testMustSpecMap(u *unstructured.Unstructured) map[string]any {
	spec, ok := u.Object["spec"].(map[string]any)
	Expect(ok).To(BeTrue(), "spec must be a map")
	return spec
}

func testMustHTTPList(u *unstructured.Unstructured) []any {
	spec := testMustSpecMap(u)
	http, ok := spec["http"].([]any)
	Expect(ok).To(BeTrue(), "spec.http must be a []any")
	return http
}

func testMustRuleMap(item any) map[string]any {
	rm, ok := item.(map[string]any)
	Expect(ok).To(BeTrue(), "item must be map[string]any")
	return rm
}

func testNewDeployment(namespace, name string) *unstructured.Unstructured {
	d := &unstructured.Unstructured{}
	d.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	d.SetNamespace(namespace)
	d.SetName(name)
	return d
}

func testNewDeploymentUnstructured(namespace, name string, owners []metav1.OwnerReference) *unstructured.Unstructured {
	d := testNewDeployment(namespace, name)
	d.SetOwnerReferences(owners)
	d.Object["spec"] = map[string]any{
		"replicas": int64(1),
		"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": name}},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
			},
		},
	}
	return d
}

func testNewFaultInjectionUnstructured(namespace, name string, opts ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	fi := &unstructured.Unstructured{}
	fi.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "chaos.sghaida.io",
		Version: "v1alpha1",
		Kind:    "FaultInjection",
	})
	fi.SetNamespace(namespace)
	fi.SetName(name)
	for _, o := range opts {
		o(fi)
	}
	return fi
}

func testFIBlastRadius(durationSeconds int64, maxTrafficPercent int64, maxPodsAffected int64) func(*unstructured.Unstructured) {
	return func(fi *unstructured.Unstructured) {
		spec := testGetOrCreateMap(fi, "spec")
		spec["blastRadius"] = map[string]any{
			"durationSeconds":   durationSeconds,
			"maxTrafficPercent": maxTrafficPercent,
			"maxPodsAffected":   maxPodsAffected,
		}
	}
}

func testFIActionInboundLatency(actionName, vsName string, percent int64, delaySeconds int64) func(*unstructured.Unstructured) {
	return func(fi *unstructured.Unstructured) {
		spec := testGetOrCreateMap(fi, "spec")
		actions := testGetOrCreateMapFrom(spec, "actions")
		meshFaults := testGetOrCreateSliceFrom(actions, "meshFaults")

		meshFaults = append(meshFaults, map[string]any{
			"name":      actionName,
			"type":      "HTTP_LATENCY",
			"direction": "INBOUND",
			"percent":   percent,
			"http": map[string]any{
				"virtualServiceRef": map[string]any{"name": vsName},
				"routes": []any{
					map[string]any{
						"match": map[string]any{
							"uriPrefix": "/",
							"headers":   map[string]any{},
						},
					},
				},
				"delay": map[string]any{"fixedDelaySeconds": delaySeconds},
			},
		})

		actions["meshFaults"] = meshFaults
		spec["actions"] = actions
	}
}

//nolint:unparam // actionName kept for readability in test helpers
func testFIActionOutboundLatency(actionName string, hosts []string, percent int64, delaySeconds int64, sourceLabels map[string]string) func(*unstructured.Unstructured) {
	return func(fi *unstructured.Unstructured) {
		spec := testGetOrCreateMap(fi, "spec")
		actions := testGetOrCreateMapFrom(spec, "actions")
		meshFaults := testGetOrCreateSliceFrom(actions, "meshFaults")

		h := make([]any, 0, len(hosts))
		for _, s := range hosts {
			h = append(h, s)
		}

		var srcSel any
		if len(sourceLabels) > 0 {
			m := map[string]any{}
			for k, v := range sourceLabels {
				m[k] = v
			}
			srcSel = map[string]any{"matchLabels": m}
		}

		http := map[string]any{
			"destinationHosts": h,
			"routes": []any{
				map[string]any{
					"match": map[string]any{
						"uriPrefix": "/",
						"headers":   map[string]any{},
					},
				},
			},
			"delay": map[string]any{"fixedDelaySeconds": delaySeconds},
		}
		if srcSel != nil {
			http["sourceSelector"] = srcSel
		}

		meshFaults = append(meshFaults, map[string]any{
			"name":      actionName,
			"type":      "HTTP_LATENCY",
			"direction": "OUTBOUND",
			"percent":   percent,
			"http":      http,
		})

		actions["meshFaults"] = meshFaults
		spec["actions"] = actions
	}
}

func testGetOrCreateMap(u *unstructured.Unstructured, key string) map[string]any {
	m, ok := u.Object[key].(map[string]any)
	if !ok || m == nil {
		m = map[string]any{}
		u.Object[key] = m
	}
	return m
}

func testGetOrCreateMapFrom(parent map[string]any, key string) map[string]any {
	m, ok := parent[key].(map[string]any)
	if !ok || m == nil {
		m = map[string]any{}
		parent[key] = m
	}
	return m
}

func testGetOrCreateSliceFrom(parent map[string]any, key string) []any {
	s, ok := parent[key].([]any)
	if !ok || s == nil {
		s = []any{}
		parent[key] = s
	}
	return s
}

//nolint:unparam // namespace kept explicit to mirror production object shape
func testFetchFI(ctx context.Context, namespace, name string) *unstructured.Unstructured {
	fi := testNewFaultInjectionUnstructured(namespace, name)
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, fi)).To(Succeed())
	return fi
}

func testGetStatusMap(fi *unstructured.Unstructured) map[string]any {
	st, _ := fi.Object["status"].(map[string]any)
	if st == nil {
		return map[string]any{}
	}
	return st
}

func testForceFIStartedAt(ctx context.Context, fi *unstructured.Unstructured, startedAt time.Time) error {
	st, ok := fi.Object["status"].(map[string]any)
	if !ok || st == nil {
		st = map[string]any{}
		fi.Object["status"] = st
	}
	st["startedAt"] = metav1.NewTime(startedAt).Format(time.RFC3339)
	return k8sClient.Status().Update(ctx, fi)
}
