package controller

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

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
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1024),
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
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1024),
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
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1024),
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

		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1024),
		}
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

		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1024),
		}
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

		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1024),
		}

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

	It("cancellation cleans up immediately: removes injected rules, deletes managed VS, and deletes only Deployments owned by this FaultInjection", func() {
		By("creating an inbound target VirtualService")
		inboundVS := testNewInboundVirtualService(ns, "cancel-inbound-vs")
		Expect(k8sClient.Create(ctx, inboundVS)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewVirtualService(ns, "cancel-inbound-vs")) }()

		By("creating a FaultInjection with INBOUND + OUTBOUND actions")
		fi := testNewFaultInjectionUnstructured(ns, "fi-cancel",
			testFIActionInboundLatency("in-latency", "cancel-inbound-vs", 10, 1),
			testFIActionOutboundLatency("out-latency", []string{"example.com"}, 10, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, fi)
			_ = k8sClient.Delete(ctx, testNewVirtualService(ns, "fi-fi-cancel-out-latency"))
		}()

		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1024),
		}

		By("reconciling once to apply faults (create managed VS + inject inbound rule)")
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-cancel", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		By("asserting inbound VS has an injected rule at the top")
		gotInbound := testNewVirtualService(ns, "cancel-inbound-vs")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cancel-inbound-vs", Namespace: ns}, gotInbound)).To(Succeed())
		http := testMustHTTPList(gotInbound)
		Expect(http).NotTo(BeEmpty())
		r0 := testMustRuleMap(http[0])
		Expect(r0["name"].(string)).To(HavePrefix("fi-fi-cancel-"))

		By("asserting managed outbound VS exists")
		managedName := "fi-fi-cancel-out-latency"
		gotManaged := testNewVirtualService(ns, managedName)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: managedName, Namespace: ns}, gotManaged)).To(Succeed())

		By("creating deployments: owned, wrong-uid, and not-owned")
		gotFI := testFetchFI(ctx, ns, "fi-cancel")
		fiUID := gotFI.GetUID()
		Expect(string(fiUID)).NotTo(BeEmpty())

		ownedDep := testNewDeploymentUnstructured(ns, "cancel-owned-dep", []metav1.OwnerReference{
			{
				APIVersion: gotFI.GetAPIVersion(),
				Kind:       gotFI.GetKind(),
				Name:       gotFI.GetName(),
				UID:        fiUID,
			},
		})
		Expect(k8sClient.Create(ctx, ownedDep)).To(Succeed())

		wrongUIDDep := testNewDeploymentUnstructured(ns, "cancel-wrong-uid-dep", []metav1.OwnerReference{
			{
				APIVersion: gotFI.GetAPIVersion(),
				Kind:       gotFI.GetKind(),
				Name:       gotFI.GetName(),
				UID:        types.UID("different-uid"),
			},
		})
		Expect(k8sClient.Create(ctx, wrongUIDDep)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewDeployment(ns, "cancel-wrong-uid-dep")) }()

		notOwnedDep := testNewDeploymentUnstructured(ns, "cancel-not-owned-dep", []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: types.UID("1111")},
		})
		Expect(k8sClient.Create(ctx, notOwnedDep)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewDeployment(ns, "cancel-not-owned-dep")) }()

		By("patching spec.cancel=true")
		// Update the unstructured FI object and write it back.
		fiToPatch := testFetchFI(ctx, ns, "fi-cancel")
		spec := testGetOrCreateMap(fiToPatch, "spec")
		spec["cancel"] = true
		Expect(k8sClient.Update(ctx, fiToPatch)).To(Succeed())

		By("reconciling again to trigger cancellation cleanup")
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-cancel", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying inbound VS no longer has injected rules")
		Eventually(func() string {
			v := testNewVirtualService(ns, "cancel-inbound-vs")
			_ = k8sClient.Get(ctx, types.NamespacedName{Name: "cancel-inbound-vs", Namespace: ns}, v)
			http := testMustHTTPList(v)
			if len(http) == 0 {
				return ""
			}
			r0 := testMustRuleMap(http[0])
			n, _ := r0["name"].(string)
			return n
		}).Should(Equal("existing-route"))

		By("verifying managed outbound VS is deleted")
		Eventually(func() bool {
			e := k8sClient.Get(ctx, types.NamespacedName{Name: managedName, Namespace: ns}, testNewVirtualService(ns, managedName))
			return apierrors.IsNotFound(e)
		}).Should(BeTrue())

		By("verifying owned deployment is deleted but others remain")
		Eventually(func() bool {
			e := k8sClient.Get(ctx, types.NamespacedName{Name: "cancel-owned-dep", Namespace: ns}, testNewDeployment(ns, "cancel-owned-dep"))
			return apierrors.IsNotFound(e)
		}).Should(BeTrue())

		Consistently(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "cancel-wrong-uid-dep", Namespace: ns}, testNewDeployment(ns, "cancel-wrong-uid-dep"))
		}).Should(Succeed())

		Consistently(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: "cancel-not-owned-dep", Namespace: ns}, testNewDeployment(ns, "cancel-not-owned-dep"))
		}).Should(Succeed())

		By("verifying FaultInjection status is Cancelled with stopReason + cancelledAt")
		fiFinal := testFetchFI(ctx, ns, "fi-cancel")
		st := testGetStatusMap(fiFinal)
		Expect(st["phase"]).To(Equal("Cancelled"))
		Expect(st["message"]).To(ContainSubstring("Cancelled"))
		Expect(st["cancelledAt"]).NotTo(BeNil())
		Expect(st["stopReason"]).To(Equal("external: spec.cancel=true"))
	})

	It("cancellation is idempotent: repeated reconciles while spec.cancel=true do not change status.cancelledAt", func() {
		By("creating an inbound target VirtualService")
		inboundVS := testNewInboundVirtualService(ns, "cancel-idem-inbound-vs")
		Expect(k8sClient.Create(ctx, inboundVS)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewVirtualService(ns, "cancel-idem-inbound-vs")) }()

		By("creating a FaultInjection and reconciling once to initialize status")
		fi := testNewFaultInjectionUnstructured(ns, "fi-cancel-idem",
			testFIActionInboundLatency("in-latency", "cancel-idem-inbound-vs", 10, 1),
			testFIActionOutboundLatency("out-latency", []string{"example.com"}, 10, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, fi)
			_ = k8sClient.Delete(ctx, testNewVirtualService(ns, "fi-fi-cancel-idem-out-latency"))
		}()

		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: record.NewFakeRecorder(1024),
		}

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-cancel-idem", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		By("setting spec.cancel=true")
		fiToPatch := testFetchFI(ctx, ns, "fi-cancel-idem")
		spec := testGetOrCreateMap(fiToPatch, "spec")
		spec["cancel"] = true
		Expect(k8sClient.Update(ctx, fiToPatch)).To(Succeed())

		By("reconciling to perform cancellation and capture cancelledAt")
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-cancel-idem", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		// Wait until controller wrote status.cancelledAt
		var firstCancelledAt string
		Eventually(func() string {
			fi2 := testFetchFI(ctx, ns, "fi-cancel-idem")
			st := testGetStatusMap(fi2)

			if st["phase"] != "Cancelled" {
				return ""
			}
			ca, _ := st["cancelledAt"].(string)
			return ca
		}).ShouldNot(BeEmpty())

		fi2 := testFetchFI(ctx, ns, "fi-cancel-idem")
		st2 := testGetStatusMap(fi2)
		firstCancelledAt, _ = st2["cancelledAt"].(string)
		Expect(firstCancelledAt).NotTo(BeEmpty())

		By("reconciling again while cancel=true")
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-cancel-idem", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying cancelledAt is unchanged")
		fi3 := testFetchFI(ctx, ns, "fi-cancel-idem")
		st3 := testGetStatusMap(fi3)
		secondCancelledAt, _ := st3["cancelledAt"].(string)

		Expect(st3["phase"]).To(Equal("Cancelled"))
		Expect(secondCancelledAt).To(Equal(firstCancelledAt))
		Expect(st3["stopReason"]).To(Equal("external: spec.cancel=true"))
	})

	It("reconciles HTTP_ABORT for INBOUND: injects abort fault and keeps route", func() {
		By("creating an inbound target VirtualService")
		vs := testNewInboundVirtualService(ns, "abort-inbound-vs")
		Expect(k8sClient.Create(ctx, vs)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewVirtualService(ns, "abort-inbound-vs")) }()

		By("creating a FaultInjection CR with HTTP_ABORT")
		fi := testNewFaultInjectionUnstructured(ns, "fi-abort-in",
			testFIActionInboundAbort("abort", "abort-inbound-vs", 10, 503),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		By("reconciling")
		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: testRecorder(),
		}
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-abort-in", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		By("verifying injected rule has abort fault + route")
		gotVS := testNewVirtualService(ns, "abort-inbound-vs")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "abort-inbound-vs", Namespace: ns}, gotVS)).To(Succeed())

		http := testMustHTTPList(gotVS)
		Expect(http).NotTo(BeEmpty())

		r0 := testMustRuleMap(http[0])
		Expect(r0["name"].(string)).To(HavePrefix("fi-fi-abort-in-"))
		Expect(r0).To(HaveKey("route"))

		fault := testMustRuleMap(r0["fault"])
		abort := testMustRuleMap(fault["abort"])
		Expect(abort["httpStatus"]).To(BeNumerically("==", 503))
	})

	It("reconciles HTTP_ABORT for OUTBOUND: creates managed VS and default route uses first host", func() {
		fi := testNewFaultInjectionUnstructured(ns, "fi-abort-out",
			testFIActionOutboundAbort("abort", []string{"b.example.com", "a.example.com"}, 10, 500, map[string]string{"app": "a"}),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, fi)
			_ = k8sClient.Delete(ctx, testNewVirtualService(ns, "fi-fi-abort-out-abort"))
		}()

		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: testRecorder(),
		}
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-abort-out", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		managedName := "fi-fi-abort-out-abort"
		gotVS := testNewVirtualService(ns, managedName)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: managedName, Namespace: ns}, gotVS)).To(Succeed())

		spec := testMustSpecMap(gotVS)

		By("hosts are sorted/deterministic")
		hosts, ok := spec["hosts"].([]any)
		Expect(ok).To(BeTrue())
		Expect(hosts).To(Equal([]any{"a.example.com", "b.example.com"}))

		By("default route points to the first host (a.example.com)")
		http := testMustHTTPList(gotVS)
		Expect(http).ToNot(BeEmpty())

		// default rule is usually last after prepending injected rules, but it must exist.
		// Find it by name prefix "default-".
		var found bool
		for _, it := range http {
			rm := testMustRuleMap(it)
			if n, _ := rm["name"].(string); strings.HasPrefix(n, "default-") || n == "default" {
				route := rm["route"].([]any)
				dst := testMustRuleMap(testMustRuleMap(route[0])["destination"])
				Expect(dst["host"]).To(Equal("a.example.com"))
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "expected a default route rule to exist")
	})

	It("managed VS gateways: reconcile sets gateways=['mesh'] and does not duplicate on subsequent reconcile", func() {
		fi := testNewFaultInjectionUnstructured(ns, "fi-gw",
			testFIActionOutboundLatency("latency", []string{"example.com"}, 10, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, fi)
			_ = k8sClient.Delete(ctx, testNewVirtualService(ns, "fi-fi-gw-latency"))
		}()

		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: testRecorder(),
		}

		By("first reconcile creates managed VS with mesh gateway")
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-gw", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		gotVS := testNewVirtualService(ns, "fi-fi-gw-latency")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fi-fi-gw-latency", Namespace: ns}, gotVS)).To(Succeed())
		spec := testMustSpecMap(gotVS)
		Expect(spec["gateways"]).To(Equal([]any{"mesh"}))

		By("mutate gateways to include mesh already; second reconcile should not duplicate")
		// simulate external change: keep mesh but also add another gw
		spec["gateways"] = []any{"mesh", "some-other-gw"}
		Expect(k8sClient.Update(ctx, gotVS)).To(Succeed())

		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-gw", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		gotVS2 := testNewVirtualService(ns, "fi-fi-gw-latency")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fi-fi-gw-latency", Namespace: ns}, gotVS2)).To(Succeed())
		spec2 := testMustSpecMap(gotVS2)

		// ensure no duplication introduced (still exactly 2, and includes mesh)
		gw2 := spec2["gateways"].([]any)
		Expect(gw2).To(Equal([]any{"mesh", "some-other-gw"}))
	})

	It("sanitizeName normalizes names and never returns empty", func() {
		Expect(sanitizeName("  Hello_World ")).To(Equal("hello-world"))
		Expect(sanitizeName("___")).To(Equal("x"))
		Expect(sanitizeName("A!B@C")).To(Equal("a-b-c"))
		Expect(sanitizeName("--hi--")).To(Equal("hi"))
	})

	It("uniqueAppend merges, de-duplicates, and sorts deterministically", func() {
		base := []string{"b", "a"}
		add := []string{"c", "a", "d"}
		out := uniqueAppend(base, add)
		Expect(out).To(Equal([]string{"a", "b", "c", "d"}))
	})

	It("splitKey handles normal and malformed keys", func() {
		ns, name := splitKey("default/foo")
		Expect(ns).To(Equal("default"))
		Expect(name).To(Equal("foo"))

		ns2, name2 := splitKey("no-slash")
		Expect(ns2).To(Equal(""))
		Expect(name2).To(Equal("no-slash"))
	})

	It("patchVirtualServiceHTTP handles missing spec/http and preserves non-map items; removes injected prefix", func() {
		vs := testNewVirtualService("default", "x")

		// case 1: no spec/http present
		changed := patchVirtualServiceHTTP(vs, "fi-x-", []map[string]any{
			{"name": "fi-x-a", "match": []any{}, "fault": map[string]any{}, "route": []any{map[string]any{"destination": map[string]any{"host": "h"}}}},
		})
		Expect(changed).To(BeTrue())
		http := testMustHTTPList(vs)
		Expect(http).To(HaveLen(1))

		// case 2: existing includes injected + non-map item + normal map
		vs2 := testNewVirtualService("default", "y")
		vs2.Object["spec"] = map[string]any{
			"http": []any{
				map[string]any{"name": "fi-x-old", "match": []any{}}, // should be removed
				"KEEP-ME", // must be preserved
				map[string]any{"name": "user-rule", "match": []any{}}, // must be preserved
			},
		}

		desired := []map[string]any{
			{"name": "fi-x-new", "match": []any{}, "fault": map[string]any{}, "route": []any{map[string]any{"destination": map[string]any{"host": "h"}}}},
		}

		changed2 := patchVirtualServiceHTTP(vs2, "fi-x-", desired)
		Expect(changed2).To(BeTrue())

		h2 := testMustHTTPList(vs2)
		Expect(h2).To(HaveLen(3)) // desired + KEEP-ME + user-rule

		r0 := testMustRuleMap(h2[0])
		Expect(r0["name"]).To(Equal("fi-x-new"))
		Expect(h2[1]).To(Equal("KEEP-ME"))
		r2 := testMustRuleMap(h2[2])
		Expect(r2["name"]).To(Equal("user-rule"))

		// case 3: applying same desired again should be "no change"
		changed3 := patchVirtualServiceHTTP(vs2, "fi-x-", desired)
		Expect(changed3).To(BeFalse())
	})

	It("ensureManagedOutboundVSGatewaysMesh sets gateways only for managed=true", func() {
		vs := testNewVirtualService("default", "z")
		vs.Object["spec"] = map[string]any{}

		Expect(ensureManagedOutboundVSGatewaysMesh(vs, false)).To(BeFalse())

		changed := ensureManagedOutboundVSGatewaysMesh(vs, true)
		Expect(changed).To(BeTrue())

		spec := testMustSpecMap(vs)
		Expect(spec["gateways"]).To(Equal([]any{"mesh"}))

		// idempotent if already has mesh
		changed2 := ensureManagedOutboundVSGatewaysMesh(vs, true)
		Expect(changed2).To(BeFalse())
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

//nolint:unparam
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

//nolint:unparam
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

//nolint:unparam
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

//nolint:unparam
func testGetOrCreateMap(u *unstructured.Unstructured, key string) map[string]any {
	m, ok := u.Object[key].(map[string]any)
	if !ok || m == nil {
		m = map[string]any{}
		u.Object[key] = m
	}
	return m
}

//nolint:unparam
func testGetOrCreateMapFrom(parent map[string]any, key string) map[string]any {
	m, ok := parent[key].(map[string]any)
	if !ok || m == nil {
		m = map[string]any{}
		parent[key] = m
	}
	return m
}

//nolint:unparam
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

func testRecorder() record.EventRecorder {
	return record.NewFakeRecorder(1024)
}

func testFIActionInboundAbort(actionName, vsName string, percent int64, httpStatus int64) func(*unstructured.Unstructured) {
	return func(fi *unstructured.Unstructured) {
		spec := testGetOrCreateMap(fi, "spec")
		actions := testGetOrCreateMapFrom(spec, "actions")
		meshFaults := testGetOrCreateSliceFrom(actions, "meshFaults")

		meshFaults = append(meshFaults, map[string]any{
			"name":      actionName,
			"type":      "HTTP_ABORT",
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
				"abort": map[string]any{"httpStatus": httpStatus},
			},
		})

		actions["meshFaults"] = meshFaults
		spec["actions"] = actions
	}
}

func testFIActionOutboundAbort(actionName string, hosts []string, percent int64, httpStatus int64, sourceLabels map[string]string) func(*unstructured.Unstructured) {
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
			"abort": map[string]any{"httpStatus": httpStatus},
		}
		if srcSel != nil {
			http["sourceSelector"] = srcSel
		}

		meshFaults = append(meshFaults, map[string]any{
			"name":      actionName,
			"type":      "HTTP_ABORT",
			"direction": "OUTBOUND",
			"percent":   percent,
			"http":      http,
		})

		actions["meshFaults"] = meshFaults
		spec["actions"] = actions
	}
}
