package controller

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	chaosv1alpha1 "github.com/sghaida/fi-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("FaultInjection Controller - Mesh faults", func() {
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
			Recorder: events.NewFakeRecorder(1024),
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
			Recorder: events.NewFakeRecorder(1024),
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
			Recorder: events.NewFakeRecorder(1024),
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

	It("INBOUND is idempotent: repeated reconcile does not duplicate injected rules", func() {
		By("creating inbound VS + FI")
		vs := testNewInboundVirtualService(ns, "idem-inbound-vs")
		Expect(k8sClient.Create(ctx, vs)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, testNewVirtualService(ns, "idem-inbound-vs")) }()

		fi := testNewFaultInjectionUnstructured(ns, "fi-inbound-idem",
			testFIActionInboundLatency("latency", "idem-inbound-vs", 10, 1),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		rec := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: events.NewFakeRecorder(1024),
		}

		By("first reconcile injects")
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-inbound-idem", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		By("second reconcile should not add a second injected rule")
		_, err = rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-inbound-idem", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		gotVS := testNewVirtualService(ns, "idem-inbound-vs")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "idem-inbound-vs", Namespace: ns}, gotVS)).To(Succeed())

		http := testMustHTTPList(gotVS)

		// Count injected rules by prefix.
		var injected int
		for _, it := range http {
			rm, ok := it.(map[string]any)
			if !ok {
				continue
			}
			n, _ := rm["name"].(string)
			if strings.HasPrefix(n, "fi-fi-inbound-idem-") {
				injected++
			}
		}
		Expect(injected).To(Equal(1), "expected exactly one injected rule after repeated reconciles")
	})

	It("OUTBOUND managed VS hosts are deterministic even if input contains duplicates; default route uses first host", func() {
		hosts := []string{"b.example.com", "a.example.com", "a.example.com"} // duplicates
		fi := testNewFaultInjectionUnstructured(ns, "fi-out-dup-hosts",
			testFIActionOutboundAbort("abort", hosts, 10, 500, map[string]string{"app": "a"}),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, fi)
			_ = k8sClient.Delete(ctx, testNewVirtualService(ns, "fi-fi-out-dup-hosts-abort"))
		}()

		rec := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: testRecorder()}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-out-dup-hosts", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		managed := testNewVirtualService(ns, "fi-fi-out-dup-hosts-abort")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: managed.GetName()}, managed)).To(Succeed())

		spec := testMustSpecMap(managed)
		gotHostsAny, ok := spec["hosts"].([]any)
		Expect(ok).To(BeTrue())

		var gotHosts []string
		for _, h := range gotHostsAny {
			gotHosts = append(gotHosts, h.(string))
		}

		// The controller might keep duplicates or dedupe; we accept either but enforce sorted determinism.
		sorted := append([]string(nil), gotHosts...)
		sort.Strings(sorted)
		Expect(gotHosts).To(Equal(sorted), "hosts must be sorted deterministically")

		// Ensure default route uses first host in spec.hosts (sorted[0])
		http := testMustHTTPList(managed)
		var want string
		if len(gotHosts) > 0 {
			want = gotHosts[0]
		}

		var found bool
		for _, it := range http {
			rm, ok := it.(map[string]any)
			if !ok {
				continue
			}
			n, _ := rm["name"].(string)
			if strings.HasPrefix(n, "default-") || n == "default" {
				route := rm["route"].([]any)
				dst := testMustRuleMap(testMustRuleMap(route[0])["destination"])
				Expect(dst["host"]).To(Equal(want))
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "expected a default route rule to exist")
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
			Recorder: events.NewFakeRecorder(1024),
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
			Recorder: events.NewFakeRecorder(1024),
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
		spec["gateways"] = []any{"mesh", "some-other-gw"}
		Expect(k8sClient.Update(ctx, gotVS)).To(Succeed())

		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-gw", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())

		gotVS2 := testNewVirtualService(ns, "fi-fi-gw-latency")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fi-fi-gw-latency", Namespace: ns}, gotVS2)).To(Succeed())
		spec2 := testMustSpecMap(gotVS2)

		gw2 := spec2["gateways"].([]any)
		Expect(gw2).To(Equal([]any{"mesh", "some-other-gw"}))
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
			Recorder: events.NewFakeRecorder(1024),
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
			Recorder: events.NewFakeRecorder(1024),
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
			Recorder: events.NewFakeRecorder(1024),
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

	It("expiry path is idempotent: reconciling after completion does not recreate managed resources", func() {
		fi := testNewFaultInjectionUnstructured(ns, "fi-expire-idem",
			testFIActionOutboundLatency("latency", []string{"example.com"}, 10, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(1, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, fi)
			_ = k8sClient.Delete(ctx, testNewVirtualService(ns, "fi-fi-expire-idem-latency"))
		}()

		rec := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: testRecorder()}

		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-expire-idem", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		gotFI := testFetchFI(ctx, ns, "fi-expire-idem")
		Expect(testForceFIStartedAt(ctx, gotFI, time.Now().Add(-10*time.Second))).To(Succeed())

		_, err = rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-expire-idem", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		managedName := "fi-fi-expire-idem-latency"
		Eventually(func() bool {
			e := k8sClient.Get(ctx, types.NamespacedName{Name: managedName, Namespace: ns}, testNewVirtualService(ns, managedName))
			return apierrors.IsNotFound(e)
		}).Should(BeTrue())

		_, err = rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-expire-idem", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

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
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	})
})

var _ = Describe("FaultInjection Controller - Pod faults", func() {
	ctx := context.Background()
	const ns = "default"

	It("podfault guardrail: refuseIfPodsLessThan triggers Error and does not create Job", func() {
		By("creating only 1 candidate pod")
		Expect(k8sClient.Create(ctx, testNewPod(ns, "p1", map[string]string{"app": "victim3"}))).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p1"}})
		}()

		fi := testNewFaultInjectionUnstructured(ns, "fi-pod-guardrail",
			testFIActionPodDeleteCount("kill", map[string]string{"app": "victim3"}, 1, 2, "FORCEFUL", 0),
			testFIBlastRadius(60, 100, 1),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		rec := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: testRecorder()}

		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-pod-guardrail", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		type phaseMsg struct {
			Phase string
			Msg   string
		}
		Eventually(func() phaseMsg {
			fi2 := testFetchFI(ctx, ns, "fi-pod-guardrail")
			st := testGetStatusMap(fi2)
			ph, _ := st["phase"].(string)
			msg, _ := st["message"].(string)
			return phaseMsg{Phase: ph, Msg: msg}
		}, "5s", "100ms").Should(And(
			WithTransform(func(pm phaseMsg) string { return pm.Phase }, Equal("Error")),
			WithTransform(func(pm phaseMsg) string { return pm.Msg }, ContainSubstring("refuseIfPodsLessThan")),
		))

		jobName := "fi-fi-pod-guardrail-pod-kill-oneshot"
		e := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(e)).To(BeTrue())
	})

	It("podfault with selector matching zero pods: does not create Job and keeps Running (or records no-op)", func() {
		fi := testNewFaultInjectionUnstructured(ns, "fi-pod-nopods",
			testFIActionPodDeleteCount("kill", map[string]string{"app": "does-not-exist"}, 1, 1, "FORCEFUL", 0),
			testFIBlastRadius(60, 100, 1),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		rec := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: testRecorder()}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-pod-nopods", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		jobName := "fi-fi-pod-nopods-pod-kill-oneshot"
		e := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(e)).To(BeTrue())

		fi2 := testFetchFI(ctx, ns, "fi-pod-nopods")
		st := testGetStatusMap(fi2)
		Expect(st["phase"]).To(Or(Equal("Running"), Equal("Error")))
	})

	It("reconciles a ONE_SHOT POD_DELETE: creates a podfault Job+Lease and records planned pods in status", func() {
		By("creating candidate pods")
		Expect(k8sClient.Create(ctx, testNewPod(ns, "a-pod", map[string]string{"app": "victim"}))).To(Succeed())
		Expect(k8sClient.Create(ctx, testNewPod(ns, "b-pod", map[string]string{"app": "victim"}))).To(Succeed())
		Expect(k8sClient.Create(ctx, testNewPod(ns, "c-pod", map[string]string{"app": "victim"}))).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "a-pod"}})
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "b-pod"}})
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c-pod"}})
		}()

		By("creating a FaultInjection with a pod fault")
		fi := testNewFaultInjectionUnstructured(ns, "fi-pod-oneshot",
			testFIActionPodDeleteCount("kill", map[string]string{"app": "victim"}, 2, 1, "GRACEFUL", 30),
			testFIBlastRadius(60, 100, 2),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		controllerReconciler := &FaultInjectionReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: testRecorder(),
		}

		By("reconciling")
		res, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "fi-pod-oneshot", Namespace: ns},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))

		By("verifying deterministic job + lease names")
		jobName := "fi-fi-pod-oneshot-pod-kill-oneshot"
		leaseName := "fi-fi-pod-oneshot-pod-kill-oneshot"

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, job)).To(Succeed())

		lease := &coordv1.Lease{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: leaseName}, lease)).To(Succeed())

		By("verifying job gets POD_NAMES_CSV in deterministic order")
		env := job.Spec.Template.Spec.Containers[0].Env
		var podCSV string
		for _, e := range env {
			if e.Name == "POD_NAMES_CSV" {
				podCSV = e.Value
				break
			}
		}
		Expect(podCSV).To(Equal("a-pod,b-pod"))

		By("verifying FaultInjection status records a planned pod fault tick")
		fi2 := testFetchFI(ctx, ns, "fi-pod-oneshot")
		st := testGetStatusMap(fi2)
		podFaultsAny, ok := st["podFaults"].([]any)
		Expect(ok).To(BeTrue())
		Expect(podFaultsAny).NotTo(BeEmpty())

		e0 := testMustRuleMap(podFaultsAny[0])
		Expect(e0["actionName"]).To(Equal("kill"))
		tick, ok := getStringField(e0, "tickId", "tickID", "tick_id")
		Expect(ok).To(BeTrue(), "tick id field not found in status entry")
		Expect(tick).To(Equal("oneshot"))
		Expect(e0["state"]).To(Equal("Planned"))
		Expect(e0["jobName"]).To(Equal(jobName))

		planned, ok := e0["plannedPods"].([]any)
		Expect(ok).To(BeTrue())
		Expect(planned).To(Equal([]any{"a-pod", "b-pod"}))
	})

	It("WINDOWED podfault: first reconcile creates a tick Job; immediate second reconcile is idempotent (no new job)", func() {
		By("creating candidate pods")
		Expect(k8sClient.Create(ctx, testNewPod(ns, "w1", map[string]string{"app": "w"}))).To(Succeed())
		Expect(k8sClient.Create(ctx, testNewPod(ns, "w2", map[string]string{"app": "w"}))).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w1"}})
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w2"}})
		}()

		fi := testNewFaultInjectionUnstructured(ns, "fi-pod-windowed",
			testFIActionPodDeleteCountWindowed("kill", map[string]string{"app": "w"}, 1, 1, "FORCEFUL", 0, 30),
			testFIBlastRadius(60, 100, 1),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		rec := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: testRecorder()}

		By("first reconcile")
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-pod-windowed", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, &client.ListOptions{Namespace: ns})).To(Succeed())

		var windowedJobs []batchv1.Job
		for _, j := range jobs.Items {
			if strings.HasPrefix(j.Name, "fi-fi-pod-windowed-pod-kill-") {
				windowedJobs = append(windowedJobs, j)
			}
		}
		Expect(windowedJobs).To(HaveLen(1))
		firstJobName := windowedJobs[0].Name

		By("second reconcile immediately should NOT create a second job")
		_, err = rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-pod-windowed", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		jobs2 := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs2, &client.ListOptions{Namespace: ns})).To(Succeed())

		var windowedJobs2 []batchv1.Job
		for _, j := range jobs2.Items {
			if strings.HasPrefix(j.Name, "fi-fi-pod-windowed-pod-kill-") {
				windowedJobs2 = append(windowedJobs2, j)
			}
		}
		Expect(windowedJobs2).To(HaveLen(1))
		Expect(windowedJobs2[0].Name).To(Equal(firstJobName))
	})

	It("podfault respects blastRadius.maxPodsAffected cap when selecting pods", func() {
		By("creating 5 candidate pods")
		for i := 1; i <= 5; i++ {
			name := "cap-" + strconv.Itoa(i)
			Expect(k8sClient.Create(ctx, testNewPod(ns, name, map[string]string{"app": "cap"}))).To(Succeed())
			defer func(n string) {
				_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}})
			}(name)
		}

		fi := testNewFaultInjectionUnstructured(ns, "fi-pod-cap",
			testFIActionPodDeleteCount("kill", map[string]string{"app": "cap"}, 5, 1, "FORCEFUL", 0),
			testFIBlastRadius(60, 100, 2),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		rec := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: testRecorder()}
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-pod-cap", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		jobName := "fi-fi-pod-cap-pod-kill-oneshot"
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, job)).To(Succeed())

		var podCSV string
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "POD_NAMES_CSV" {
				podCSV = e.Value
				break
			}
		}
		Expect(strings.Split(podCSV, ",")).To(HaveLen(2))
	})

	It("recordPodFaultExecuted updates tick entry and preserves PlannedPods if already set", func() {
		sch := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(sch)).To(Succeed())
		Expect(chaosv1alpha1.AddToScheme(sch)).To(Succeed())

		fi := &chaosv1alpha1.FaultInjection{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      "fi-exec",
				UID:       types.UID("fi-exec-uid"),
			},
			Spec: chaosv1alpha1.FaultInjectionSpec{
				BlastRadius: chaosv1alpha1.BlastRadiusSpec{
					DurationSeconds:   60,
					MaxTrafficPercent: 100,
				},
				Actions: chaosv1alpha1.ActionsSpec{}, // fake client: no CRD validation
			},
			Status: chaosv1alpha1.FaultInjectionStatus{
				PodFaults: []chaosv1alpha1.PodFaultTickStatus{
					{
						ActionName:  "kill",
						TickID:      "oneshot",
						State:       "Planned",
						PlannedPods: []string{"a", "b"},
					},
				},
			},
		}

		cl := fake.NewClientBuilder().
			WithScheme(sch).
			WithObjects(fi).
			WithStatusSubresource(fi).
			Build()

		rec := &FaultInjectionReconciler{
			Client:   cl,
			Scheme:   sch,
			Recorder: testRecorder(),
		}

		now := time.Now().UTC()
		p := podFaultPlan{
			ActionName: "kill",
			TickID:     "oneshot",
			JobName:    "job-x",
			PodNames:   []string{"x", "y"}, // MUST NOT overwrite PlannedPods
		}

		err := rec.recordPodFaultExecuted(ctx, fi, p, now, true, "ok")
		Expect(err).NotTo(HaveOccurred())

		var got chaosv1alpha1.FaultInjection
		Expect(cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "fi-exec"}, &got)).To(Succeed())

		Expect(got.Status.PodFaults).To(HaveLen(1))
		e := got.Status.PodFaults[0]
		Expect(e.ActionName).To(Equal("kill"))
		Expect(e.TickID).To(Equal("oneshot"))
		Expect(e.State).To(Equal("Succeeded"))
		Expect(e.JobName).To(Equal("job-x"))
		Expect(e.Message).To(Equal("ok"))
		Expect(e.ExecutedAt).NotTo(BeNil())

		// preserved
		Expect(e.PlannedPods).To(Equal([]string{"a", "b"}))
	})

	It("recordPodFaultExecuted sets PlannedPods from executed pods when missing", func() {
		// IMPORTANT: scheme must include your CRD
		sch := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(sch)).To(Succeed())
		Expect(chaosv1alpha1.AddToScheme(sch)).To(Succeed())

		fi := &chaosv1alpha1.FaultInjection{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      "fi-exec2",
				UID:       types.UID("fi-exec2-uid"),
			},
			Spec: chaosv1alpha1.FaultInjectionSpec{
				BlastRadius: chaosv1alpha1.BlastRadiusSpec{
					DurationSeconds:   60,
					MaxTrafficPercent: 100,
				},
				Actions: chaosv1alpha1.ActionsSpec{}, // fake client: no CRD validation
			},
		}

		cl := fake.NewClientBuilder().
			WithScheme(sch).
			WithObjects(fi).
			WithStatusSubresource(fi). // <- critical if recordPodFaultExecuted uses Status().Update
			Build()

		rec := &FaultInjectionReconciler{
			Client:   cl,
			Scheme:   sch,
			Recorder: testRecorder(),
		}

		p := podFaultPlan{
			ActionName: "kill",
			TickID:     "t1",
			JobName:    "job-y",
			PodNames:   []string{"p1"},
		}

		err := rec.recordPodFaultExecuted(ctx, fi, p, time.Now().UTC(), false, "boom")
		Expect(err).NotTo(HaveOccurred())

		var got chaosv1alpha1.FaultInjection
		Expect(cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "fi-exec2"}, &got)).To(Succeed())

		Expect(got.Status.PodFaults).To(HaveLen(1))
		e := got.Status.PodFaults[0]
		Expect(e.ActionName).To(Equal("kill"))
		Expect(e.TickID).To(Equal("t1"))
		Expect(e.State).To(Equal("Failed"))
		Expect(e.JobName).To(Equal("job-y"))
		Expect(e.Message).To(Equal("boom"))
		Expect(e.ExecutedAt).NotTo(BeNil())

		// Backfilled from executed pods
		Expect(e.PlannedPods).To(Equal([]string{"p1"}))
	})

	It("tryAcquireOrTakeoverLease creates lease when missing", func() {
		sch := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(sch)).To(Succeed())
		Expect(chaosv1alpha1.AddToScheme(sch)).To(Succeed())
		Expect(coordv1.AddToScheme(sch)).To(Succeed())

		fi := &chaosv1alpha1.FaultInjection{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      "fi-lease",
				UID:       types.UID("fi-lease-uid"), // used in ownerRef UID
			},
			Spec: chaosv1alpha1.FaultInjectionSpec{
				Actions: chaosv1alpha1.ActionsSpec{},
			},
		}

		cl := fake.NewClientBuilder().
			WithScheme(sch).
			WithObjects(fi).
			Build()

		rec := &FaultInjectionReconciler{
			Client:         cl,
			Scheme:         sch,
			Recorder:       testRecorder(),
			HolderIdentity: "", // expect default "fi-operator"
		}

		p := podFaultPlan{
			ActionName: "a",
			TickID:     "oneshot",
			Namespace:  ns,
			LeaseName:  "lease-a",
		}

		ok, reason, err := rec.tryAcquireOrTakeoverLease(ctx, fi, p, time.Now().UTC())
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal("created"))

		l := &coordv1.Lease{}
		Expect(cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "lease-a"}, l)).To(Succeed())
		Expect(l.Spec.HolderIdentity).NotTo(BeNil())
		Expect(*l.Spec.HolderIdentity).To(Equal("fi-operator"))
	})

	It("tryAcquireOrTakeoverLease returns busy when lease is not stale", func() {
		fi := &chaosv1alpha1.FaultInjection{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "fi-lease2"}}

		holder := "someone"
		d := int32(60)
		now := time.Now().UTC()
		l := &coordv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "lease-b"},
			Spec: coordv1.LeaseSpec{
				HolderIdentity:       &holder,
				AcquireTime:          &metav1.MicroTime{Time: now},
				RenewTime:            &metav1.MicroTime{Time: now},
				LeaseDurationSeconds: &d,
			},
		}
		Expect(k8sClient.Create(ctx, l)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, l) }()

		rec := &FaultInjectionReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: testRecorder(), HolderIdentity: "me",
		}
		p := podFaultPlan{
			ActionName: "a",
			TickID:     "oneshot",
			Namespace:  ns,
			LeaseName:  "lease-b",
		}
		ok, reason, err := rec.tryAcquireOrTakeoverLease(ctx, fi, p, now.Add(5*time.Second))
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(reason).To(ContainSubstring(`held by "someone"`))
	})

	It("tryAcquireOrTakeoverLease takes over when stale", func() {
		sch := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(sch)).To(Succeed())
		Expect(chaosv1alpha1.AddToScheme(sch)).To(Succeed())
		Expect(coordv1.AddToScheme(sch)).To(Succeed())

		fi := &chaosv1alpha1.FaultInjection{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      "fi-lease3",
				UID:       types.UID("fi-lease3-uid"),
			},
			Spec: chaosv1alpha1.FaultInjectionSpec{
				Actions: chaosv1alpha1.ActionsSpec{},
			},
		}

		oldHolder := "old"
		d := int32(1) // 1s lease
		old := time.Now().UTC().Add(-10 * time.Second)

		l := &coordv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      "lease-c",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: chaosv1alpha1.GroupVersion.String(),
						Kind:       "FaultInjection",
						Name:       fi.Name,
						UID:        fi.UID,
					},
				},
				// optional if your code checks labels:
				// Labels: map[string]string{"chaos.sghaida.io/fi": fi.Name},
			},
			Spec: coordv1.LeaseSpec{
				HolderIdentity:       &oldHolder,
				AcquireTime:          &metav1.MicroTime{Time: old},
				RenewTime:            &metav1.MicroTime{Time: old},
				LeaseDurationSeconds: &d,
			},
		}

		cl := fake.NewClientBuilder().
			WithScheme(sch).
			WithObjects(fi, l).
			Build()

		rec := &FaultInjectionReconciler{
			Client:         cl,
			Scheme:         sch,
			Recorder:       testRecorder(),
			HolderIdentity: "new",
		}

		p := podFaultPlan{
			ActionName: "a",
			TickID:     "0",
			Namespace:  ns,
			LeaseName:  "lease-c",
		}

		ok, reason, err := rec.tryAcquireOrTakeoverLease(ctx, fi, p, time.Now().UTC())
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(reason).To(ContainSubstring("taken over"))

		l2 := &coordv1.Lease{}
		Expect(cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: "lease-c"}, l2)).To(Succeed())
		Expect(l2.Spec.HolderIdentity).NotTo(BeNil())
		Expect(*l2.Spec.HolderIdentity).To(Equal("new"))
	})

	It("cancellation cleans up podfault artifacts: deletes podfault Jobs and Leases", func() {
		By("creating candidate pods")
		Expect(k8sClient.Create(ctx, testNewPod(ns, "p1", map[string]string{"app": "victim2"}))).To(Succeed())
		Expect(k8sClient.Create(ctx, testNewPod(ns, "p2", map[string]string{"app": "victim2"}))).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p1"}})
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "p2"}})
		}()

		fi := testNewFaultInjectionUnstructured(ns, "fi-pod-cancel",
			testFIActionPodDeleteCount("kill", map[string]string{"app": "victim2"}, 1, 1, "FORCEFUL", 0),
			testFIBlastRadius(60, 100, 1),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		rec := &FaultInjectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: testRecorder()}

		By("reconciling once to create podfault artifacts")
		_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-pod-cancel", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		jobName := "fi-fi-pod-cancel-pod-kill-oneshot"
		leaseName := "fi-fi-pod-cancel-pod-kill-oneshot"
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, &batchv1.Job{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: leaseName}, &coordv1.Lease{})).To(Succeed())

		By("patching spec.cancel=true")
		fiToPatch := testFetchFI(ctx, ns, "fi-pod-cancel")
		spec := testGetOrCreateMap(fiToPatch, "spec")
		spec["cancel"] = true
		Expect(k8sClient.Update(ctx, fiToPatch)).To(Succeed())

		By("reconciling again to cancel and cleanup")
		_, err = rec.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "fi-pod-cancel", Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			j := &batchv1.Job{}
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: jobName}, j)
			if apierrors.IsNotFound(err) {
				return true
			}
			if err != nil {
				return false
			}
			return j.GetDeletionTimestamp() != nil
		}, "10s", "100ms").Should(BeTrue(), "expected Job to be deleted or at least marked for deletion")

		Eventually(func() bool {
			l := &coordv1.Lease{}
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: leaseName}, l)
			if apierrors.IsNotFound(err) {
				return true
			}
			if err != nil {
				return false
			}
			return l.GetDeletionTimestamp() != nil
		}, "10s", "100ms").Should(BeTrue(), "expected Lease to be deleted or at least marked for deletion")
	})

})

var _ = Describe("pod fault helper functions", func() {
	Describe("validatePodFaultSelection", func() {
		It("accepts COUNT with count>=1", func() {
			c := int32(1)
			a := chaosv1alpha1.PodFaultAction{
				Name: "a1",
				Selection: chaosv1alpha1.PodSelectionSpec{
					Mode:  chaosv1alpha1.PodSelectionModeCount,
					Count: &c,
				},
			}
			Expect(validatePodFaultSelection(a)).To(Succeed())
		})

		It("rejects COUNT with nil or <=0", func() {
			aNil := chaosv1alpha1.PodFaultAction{
				Name: "a1",
				Selection: chaosv1alpha1.PodSelectionSpec{
					Mode: chaosv1alpha1.PodSelectionModeCount,
				},
			}
			Expect(validatePodFaultSelection(aNil)).To(MatchError(ContainSubstring("invalid selection.count")))

			c0 := int32(0)
			a0 := chaosv1alpha1.PodFaultAction{
				Name: "a2",
				Selection: chaosv1alpha1.PodSelectionSpec{
					Mode:  chaosv1alpha1.PodSelectionModeCount,
					Count: &c0,
				},
			}
			Expect(validatePodFaultSelection(a0)).To(MatchError(ContainSubstring("invalid selection.count")))
		})

		It("accepts PERCENT with 1..100", func() {
			p := int32(10)
			a := chaosv1alpha1.PodFaultAction{
				Name: "a1",
				Selection: chaosv1alpha1.PodSelectionSpec{
					Mode:    chaosv1alpha1.PodSelectionModePercent,
					Percent: &p,
				},
			}
			Expect(validatePodFaultSelection(a)).To(Succeed())
		})

		It("rejects PERCENT with nil, <=0 or >100", func() {
			aNil := chaosv1alpha1.PodFaultAction{
				Name: "a1",
				Selection: chaosv1alpha1.PodSelectionSpec{
					Mode: chaosv1alpha1.PodSelectionModePercent,
				},
			}
			Expect(validatePodFaultSelection(aNil)).To(MatchError(ContainSubstring("invalid selection.percent")))

			p0 := int32(0)
			a0 := chaosv1alpha1.PodFaultAction{
				Name: "a2",
				Selection: chaosv1alpha1.PodSelectionSpec{
					Mode:    chaosv1alpha1.PodSelectionModePercent,
					Percent: &p0,
				},
			}
			Expect(validatePodFaultSelection(a0)).To(MatchError(ContainSubstring("invalid selection.percent")))

			p101 := int32(101)
			a101 := chaosv1alpha1.PodFaultAction{
				Name: "a3",
				Selection: chaosv1alpha1.PodSelectionSpec{
					Mode:    chaosv1alpha1.PodSelectionModePercent,
					Percent: &p101,
				},
			}
			Expect(validatePodFaultSelection(a101)).To(MatchError(ContainSubstring("invalid selection.percent")))
		})

		It("rejects unsupported selection mode", func() {
			a := chaosv1alpha1.PodFaultAction{
				Name: "a1",
				Selection: chaosv1alpha1.PodSelectionSpec{
					Mode: chaosv1alpha1.PodSelectionMode("NOPE"),
				},
			}
			Expect(validatePodFaultSelection(a)).To(MatchError(ContainSubstring("unsupported selection.mode")))
		})
	})
})

var _ = Describe("FaultInjection Controller - Utils", func() {
	It("patchVirtualServiceHTTP removes multiple injected rules sharing the same prefix (not just one)", func() {
		vs := testNewVirtualService("default", "multi-injected")
		vs.Object["spec"] = map[string]any{
			"http": []any{
				map[string]any{"name": "fi-x-1", "match": []any{}},
				map[string]any{"name": "fi-x-2", "match": []any{}},
				map[string]any{"name": "user-rule-1", "match": []any{}},
				map[string]any{"name": "fi-x-3", "match": []any{}},
				map[string]any{"name": "user-rule-2", "match": []any{}},
			},
		}

		desired := []map[string]any{
			{"name": "fi-x-new", "match": []any{}, "fault": map[string]any{}, "route": []any{
				map[string]any{"destination": map[string]any{"host": "h"}},
			}},
		}

		changed := patchVirtualServiceHTTP(vs, "fi-x-", desired)
		Expect(changed).To(BeTrue())

		http := testMustHTTPList(vs)
		Expect(http).To(HaveLen(3))

		r0 := testMustRuleMap(http[0])
		Expect(r0["name"]).To(Equal("fi-x-new"))
		r1 := testMustRuleMap(http[1])
		Expect(r1["name"]).To(Equal("user-rule-1"))
		r2 := testMustRuleMap(http[2])
		Expect(r2["name"]).To(Equal("user-rule-2"))
	})

	It("selectPodNamesDeterministically: COUNT and PERCENT modes, rounding and caps", func() {
		names := []string{"p10", "p2", "p1", "p9", "p3", "p4", "p5", "p6", "p8", "p7"}
		cands := make([]unstructured.Unstructured, 0, len(names))
		for _, n := range names {
			u := unstructured.Unstructured{}
			u.SetName(n)
			cands = append(cands, u)
		}

		By("COUNT selects exact count (bounded by n and maxPods)")
		c := int32(3)
		out := selectPodNamesDeterministically(cands, "COUNT", &c, nil, 0)
		Expect(out).To(HaveLen(3))
		Expect(out).To(Equal([]string{"p10", "p2", "p1"}))

		By("COUNT respects maxPods cap")
		maxPods := int32(2)
		out2 := selectPodNamesDeterministically(cands, "COUNT", &c, nil, maxPods)
		Expect(out2).To(Equal([]string{"p10", "p2"}))

		By("PERCENT rounds up (ceil) and is bounded by n and maxPods")
		p := int32(21)
		out3 := selectPodNamesDeterministically(cands, "PERCENT", nil, &p, 0)
		Expect(out3).To(HaveLen(3))
		Expect(out3).To(Equal([]string{"p10", "p2", "p1"}))

		By("PERCENT=0 selects none")
		p0 := int32(0)
		out4 := selectPodNamesDeterministically(cands, "PERCENT", nil, &p0, 0)
		Expect(out4).To(BeNil())
	})

	It("ensureVSHasAtLeastOneDefaultRouteRule inserts a default rule when spec.http missing/empty", func() {
		vs := testNewVirtualService("default", "managed-empty")

		ensureVSHasAtLeastOneDefaultRouteRule(vs, []string{"a.example.com", "b.example.com"})

		spec := testMustSpecMap(vs)
		_ = spec

		http := testMustHTTPList(vs)
		Expect(http).To(HaveLen(1))

		r0 := testMustRuleMap(http[0])
		Expect(r0["name"]).To(Equal("default"))
		Expect(r0).To(HaveKey("route"))

		spec["http"] = []any{map[string]any{"name": "already-there", "route": []any{}}}
		vs.Object["spec"] = spec

		ensureVSHasAtLeastOneDefaultRouteRule(vs, []string{"x.example.com"})
		http2 := testMustHTTPList(vs)
		Expect(http2).To(HaveLen(1))
		Expect(testMustRuleMap(http2[0])["name"]).To(Equal("already-there"))
	})

	It("ensureVSHasAtLeastOneDefaultRouteRule handles non-map spec defensively", func() {
		vs := testNewVirtualService("default", "bad-spec")
		vs.Object["spec"] = "not-a-map"

		ensureVSHasAtLeastOneDefaultRouteRule(vs, []string{"a.example.com"})

		http := testMustHTTPList(vs)
		Expect(http).To(HaveLen(1))
		Expect(testMustRuleMap(http[0])["name"]).To(Equal("default"))
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

	It("splitKey handles multiple slashes: keeps first segment as ns and rest as name", func() {
		ns2, name2 := splitKey("a/b/c")
		Expect(ns2).To(Equal("a"))
		Expect(name2).To(Equal("b/c"))
	})

	It("patchVirtualServiceHTTP handles missing spec/http and preserves non-map items; removes injected prefix", func() {
		vs := testNewVirtualService("default", "x")

		changed := patchVirtualServiceHTTP(vs, "fi-x-", []map[string]any{
			{"name": "fi-x-a", "match": []any{}, "fault": map[string]any{}, "route": []any{map[string]any{"destination": map[string]any{"host": "h"}}}},
		})
		Expect(changed).To(BeTrue())
		http := testMustHTTPList(vs)
		Expect(http).To(HaveLen(1))

		vs2 := testNewVirtualService("default", "y")
		vs2.Object["spec"] = map[string]any{
			"http": []any{
				map[string]any{"name": "fi-x-old", "match": []any{}},
				"KEEP-ME",
				map[string]any{"name": "user-rule", "match": []any{}},
			},
		}

		desired := []map[string]any{
			{"name": "fi-x-new", "match": []any{}, "fault": map[string]any{}, "route": []any{map[string]any{"destination": map[string]any{"host": "h"}}}},
		}

		changed2 := patchVirtualServiceHTTP(vs2, "fi-x-", desired)
		Expect(changed2).To(BeTrue())

		h2 := testMustHTTPList(vs2)
		Expect(h2).To(HaveLen(3))

		r0 := testMustRuleMap(h2[0])
		Expect(r0["name"]).To(Equal("fi-x-new"))
		Expect(h2[1]).To(Equal("KEEP-ME"))
		r2 := testMustRuleMap(h2[2])
		Expect(r2["name"]).To(Equal("user-rule"))

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

		changed2 := ensureManagedOutboundVSGatewaysMesh(vs, true)
		Expect(changed2).To(BeFalse())
	})

	It("isJobComplete and isJobFailed detect conditions", func() {
		j := &batchv1.Job{}
		j.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}
		Expect(isJobComplete(j)).To(BeTrue())
		Expect(isJobFailed(j)).To(BeFalse())

		j2 := &batchv1.Job{}
		j2.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
		}
		Expect(isJobComplete(j2)).To(BeFalse())
		Expect(isJobFailed(j2)).To(BeTrue())
	})

	It("status map helper: testForceFIStartedAt writes RFC3339 string into status.startedAt", func() {
		ctx := context.Background()
		const ns = "default"

		fi := testNewFaultInjectionUnstructured(ns, "fi-force-startedat",
			testFIActionOutboundLatency("latency", []string{"example.com"}, 10, 1, map[string]string{"app": "a"}),
			testFIBlastRadius(60, 100, 0),
		)
		Expect(k8sClient.Create(ctx, fi)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, fi) }()

		got := testFetchFI(ctx, ns, "fi-force-startedat")
		t0 := time.Now().Add(-5 * time.Minute).UTC()
		Expect(testForceFIStartedAt(ctx, got, t0)).To(Succeed())

		got2 := testFetchFI(ctx, ns, "fi-force-startedat")
		st := testGetStatusMap(got2)
		s, _ := st["startedAt"].(string)
		Expect(s).NotTo(BeEmpty())

		parsed, err := time.Parse(time.RFC3339, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed.UTC().Format(time.RFC3339)).To(Equal(s))
	})
})

var _ = Describe("job condition helpers", func() {
	It("detects JobComplete true", func() {
		j := &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			},
		}
		Expect(isJobComplete(j)).To(BeTrue())
		Expect(isJobFailed(j)).To(BeFalse())
	})

	It("detects JobFailed true", func() {
		j := &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			},
		}
		Expect(isJobFailed(j)).To(BeTrue())
		Expect(isJobComplete(j)).To(BeFalse())
	})

	It("returns false when conditions absent or not true", func() {
		j := &batchv1.Job{}
		Expect(isJobComplete(j)).To(BeFalse())
		Expect(isJobFailed(j)).To(BeFalse())

		j2 := &batchv1.Job{
			Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
					{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
				},
			},
		}
		Expect(isJobComplete(j2)).To(BeFalse())
		Expect(isJobFailed(j2)).To(BeFalse())
	})
})

var _ = Describe("selectPodNamesDeterministically", func() {
	It("returns nil for empty candidates", func() {
		c := int32(1)
		Expect(selectPodNamesDeterministically(nil, chaosv1alpha1.PodSelectionModeCount, &c, nil, 0)).To(BeNil())
	})

	It("COUNT nil => nil", func() {
		cands := []unstructured.Unstructured{uPod("a")}
		Expect(selectPodNamesDeterministically(cands, chaosv1alpha1.PodSelectionModeCount, nil, nil, 0)).To(BeNil())
	})

	It("COUNT clamps to n and maxPods", func() {
		cands := []unstructured.Unstructured{uPod("a"), uPod("b"), uPod("c")}
		c := int32(10)
		Expect(selectPodNamesDeterministically(cands, chaosv1alpha1.PodSelectionModeCount, &c, nil, 0)).
			To(Equal([]string{"a", "b", "c"}))

		c2 := int32(3)
		Expect(selectPodNamesDeterministically(cands, chaosv1alpha1.PodSelectionModeCount, &c2, nil, 2)).
			To(Equal([]string{"a", "b"}))
	})

	It("PERCENT uses ceil and clamps", func() {
		cands := []unstructured.Unstructured{uPod("a"), uPod("b"), uPod("c"), uPod("d"), uPod("e")}
		p := int32(1) // ceil(5*1/100) = 1
		Expect(selectPodNamesDeterministically(cands, chaosv1alpha1.PodSelectionModePercent, nil, &p, 0)).
			To(Equal([]string{"a"}))

		p2 := int32(21) // ceil(5*21/100)=ceil(1.05)=2
		Expect(selectPodNamesDeterministically(cands, chaosv1alpha1.PodSelectionModePercent, nil, &p2, 0)).
			To(Equal([]string{"a", "b"}))

		p3 := int32(100)
		Expect(selectPodNamesDeterministically(cands, chaosv1alpha1.PodSelectionModePercent, nil, &p3, 3)).
			To(Equal([]string{"a", "b", "c"}))
	})

	It("unsupported mode yields nil", func() {
		cands := []unstructured.Unstructured{uPod("a")}
		c := int32(1)
		Expect(selectPodNamesDeterministically(cands, chaosv1alpha1.PodSelectionMode("NOPE"), &c, nil, 0)).To(BeNil())
	})
})

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	Expect(chaosv1alpha1.AddToScheme(s)).To(Succeed())
	Expect(batchv1.AddToScheme(s)).To(Succeed())
	Expect(coordv1.AddToScheme(s)).To(Succeed())
	Expect(corev1.AddToScheme(s)).To(Succeed())
	return s
}

var _ = Describe("pod fault k8s helpers", func() {
	var (
		ctx context.Context
		r   *FaultInjectionReconciler
		fi  *chaosv1alpha1.FaultInjection
		now time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		now = time.Now().UTC()

		// IMPORTANT: build ONE scheme instance and reuse it everywhere in this test.
		sch := testScheme()

		fi = &chaosv1alpha1.FaultInjection{
			TypeMeta: metav1.TypeMeta{
				APIVersion: chaosv1alpha1.GroupVersion.String(),
				Kind:       "FaultInjection",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fi1",
				Namespace: "demo",
			},
		}

		cl := fake.NewClientBuilder().
			WithScheme(sch).
			WithObjects(fi).
			WithStatusSubresource(fi). // REQUIRED because recordPodFaultPlanned/Executed use Status().Update
			Build()

		r = &FaultInjectionReconciler{
			Client:   cl,
			Scheme:   sch,            // reuse same scheme
			Recorder: testRecorder(), // optional but nice
		}
	})

	Describe("createPodFaultJobIfNotExists", func() {
		It("creates the job and is idempotent", func() {
			p := podFaultPlan{
				ActionName:      "kill",
				TickID:          "oneshot",
				JobName:         "job1",
				LeaseName:       "lease1",
				Namespace:       "demo",
				PodNames:        []string{"p1", "p2"},
				TerminationMode: chaosv1alpha1.PodTerminationModeForceful,
				GracePeriodSecs: 0,
			}

			Expect(r.createPodFaultJobIfNotExists(ctx, fi, p)).To(Succeed())

			j := &batchv1.Job{}
			Expect(r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "job1"}, j)).To(Succeed())
			Expect(j.Spec.Template.Spec.ServiceAccountName).To(Equal("fi-podfault-executor"))

			// Call again: should no-op
			Expect(r.createPodFaultJobIfNotExists(ctx, fi, p)).To(Succeed())
		})
	})

	Describe("recordPodFaultPlanned / recordPodFaultExecuted", func() {
		It("upserts planned then executed, preserving planned pods", func() {
			p := podFaultPlan{
				ActionName: "kill",
				TickID:     "0",
				JobName:    "job-x",
				PodNames:   []string{"p1", "p2"},
			}

			Expect(r.recordPodFaultPlanned(ctx, fi, p, now)).To(Succeed())

			latest := &chaosv1alpha1.FaultInjection{}
			Expect(r.Get(ctx, client.ObjectKeyFromObject(fi), latest)).To(Succeed())
			Expect(latest.Status.PodFaults).To(HaveLen(1))
			Expect(latest.Status.PodFaults[0].State).To(Equal("Planned"))
			Expect(latest.Status.PodFaults[0].PlannedPods).To(Equal([]string{"p1", "p2"}))

			// executed with different p.PodNames should NOT override planned pods if already set
			p2 := p
			p2.PodNames = []string{"p9"}

			Expect(r.recordPodFaultExecuted(ctx, fi, p2, now.Add(time.Second), true, "ok")).To(Succeed())

			latest2 := &chaosv1alpha1.FaultInjection{}
			Expect(r.Get(ctx, client.ObjectKeyFromObject(fi), latest2)).To(Succeed())
			Expect(latest2.Status.PodFaults).To(HaveLen(1))
			Expect(latest2.Status.PodFaults[0].State).To(Equal("Succeeded"))
			Expect(latest2.Status.PodFaults[0].Message).To(Equal("ok"))
			Expect(latest2.Status.PodFaults[0].PlannedPods).To(Equal([]string{"p1", "p2"}))
		})

		It("records executed pods if planned pods were not recorded", func() {
			p := podFaultPlan{
				ActionName: "kill",
				TickID:     "0",
				JobName:    "job-x",
				PodNames:   []string{"p1"},
			}

			Expect(r.recordPodFaultExecuted(ctx, fi, p, now, false, "boom")).To(Succeed())

			latest := &chaosv1alpha1.FaultInjection{}
			Expect(r.Get(ctx, client.ObjectKeyFromObject(fi), latest)).To(Succeed())
			Expect(latest.Status.PodFaults).To(HaveLen(1))
			Expect(latest.Status.PodFaults[0].State).To(Equal("Failed"))
			Expect(latest.Status.PodFaults[0].PlannedPods).To(Equal([]string{"p1"}))
		})
	})
})

/* -----------------------------
   Test-only helpers
--------------------------------*/

func uPod(name string) unstructured.Unstructured {
	var u unstructured.Unstructured
	u.SetName(name)
	return u
}

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

//nolint:unparam
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

//nolint:unparam
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

func testRecorder() events.EventRecorder {
	return events.NewFakeRecorder(1024)
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

//nolint:unparam
func testNewPod(namespace, name string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
		},
	}
}

//nolint:unparam
func testFIActionPodDeleteCount(
	actionName string,
	matchLabels map[string]string,
	count int64,
	refuseIfPodsLessThan int64,
	terminationMode string,
	gracePeriodSeconds int64,
) func(*unstructured.Unstructured) {
	return func(fi *unstructured.Unstructured) {
		spec := testGetOrCreateMap(fi, "spec")
		actions := testGetOrCreateMapFrom(spec, "actions")
		podFaults := testGetOrCreateSliceFrom(actions, "podFaults")

		ml := map[string]any{}
		for k, v := range matchLabels {
			ml[k] = v
		}

		podFaults = append(podFaults, map[string]any{
			"name": actionName,
			"type": "POD_DELETE",
			"target": map[string]any{
				"selector": map[string]any{
					"matchLabels": ml,
				},
			},
			"policy": map[string]any{
				"executionMode": "ONE_SHOT",
			},
			"selection": map[string]any{
				"mode":  "COUNT",
				"count": count,
			},
			"termination": map[string]any{
				"mode":               terminationMode,
				"gracePeriodSeconds": gracePeriodSeconds,
			},
			"guardrails": map[string]any{
				"refuseIfPodsLessThan":       refuseIfPodsLessThan,
				"respectPodDisruptionBudget": true,
			},
		})

		actions["podFaults"] = podFaults
		spec["actions"] = actions
	}
}

func testFIActionPodDeleteCountWindowed(
	actionName string,
	matchLabels map[string]string,
	count int64,
	refuseIfPodsLessThan int64,
	terminationMode string,
	gracePeriodSeconds int64,
	intervalSeconds int64,
) func(*unstructured.Unstructured) {
	return func(fi *unstructured.Unstructured) {
		spec := testGetOrCreateMap(fi, "spec")
		actions := testGetOrCreateMapFrom(spec, "actions")
		podFaults := testGetOrCreateSliceFrom(actions, "podFaults")

		ml := map[string]any{}
		for k, v := range matchLabels {
			ml[k] = v
		}

		podFaults = append(podFaults, map[string]any{
			"name": actionName,
			"type": "POD_DELETE",
			"target": map[string]any{
				"selector": map[string]any{
					"matchLabels": ml,
				},
			},
			"policy": map[string]any{
				"executionMode": "WINDOWED",
			},
			"window": map[string]any{
				"intervalSeconds":      intervalSeconds,
				"maxTotalPodsAffected": int64(1),
			},
			"selection": map[string]any{
				"mode":  "COUNT",
				"count": count,
			},
			"termination": map[string]any{
				"mode":               terminationMode,
				"gracePeriodSeconds": gracePeriodSeconds,
			},
			"guardrails": map[string]any{
				"refuseIfPodsLessThan":       refuseIfPodsLessThan,
				"respectPodDisruptionBudget": true,
			},
		})

		actions["podFaults"] = podFaults
		spec["actions"] = actions
	}
}

func getStringField(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok2 := v.(string); ok2 {
				return s, true
			}
		}
	}
	return "", false
}
