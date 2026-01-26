package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	changed = "CHANGED"
)

var _ = Describe("DeepCopy / Scheme", func() {
	It("DeepCopy creates an independent copy of FaultInjection (no shared pointers/slices)", func() {
		fi := &FaultInjection{
			TypeMeta: metav1.TypeMeta{
				APIVersion: GroupVersion.String(),
				Kind:       "FaultInjection",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fi",
				Namespace: "default",
				Labels:    map[string]string{"a": "b"},
			},
			Spec: FaultInjectionSpec{
				Cancel: true,
				BlastRadius: BlastRadiusSpec{
					DurationSeconds:   60,
					MaxTrafficPercent: 10,
					Scope:             "namespace",
					MaxPodsAffected:   2,
				},
				Actions: ActionsSpec{
					MeshFaults: []MeshFaultAction{
						{
							Name:      "a1",
							Direction: "INBOUND",
							Type:      "HTTP_LATENCY",
							Percent:   10,
							HTTP: HTTPFaultSpec{
								VirtualServiceRef: &VirtualServiceRef{Name: "vs"},
								Routes: []HTTPRouteTarget{
									{Match: HTTPMatch{URIPrefix: "/", Headers: map[string]string{"x": "y"}}},
								},
								Delay:   &DelayConfig{FixedDelaySeconds: 1},
								Timeout: &TimeoutConfig{TimeoutSeconds: 1},
							},
						},
					},
				},
				StopConditions: &StopConditionsSpec{
					OverallBreachesCount: ptrInt32(1),
					FailOnNoMetrics:      ptrBool(true),
					Rules: []StopRule{
						{
							Name:   "r1",
							PromQL: `up == 0`,
							Compare: MetricCompare{
								Op:        "EQ",
								Threshold: 1,
							},
						},
					},
				},
			},
			Status: FaultInjectionStatus{
				Phase:   "Running",
				Message: "x",
			},
		}

		cp := fi.DeepCopy()
		Expect(cp).NotTo(BeNil())
		Expect(cp).NotTo(BeIdenticalTo(fi))

		// mutate original and ensure copy doesn't change
		fi.Labels["a"] = changed
		fi.Spec.Actions.MeshFaults[0].Name = changed
		fi.Spec.Actions.MeshFaults[0].HTTP.Routes[0].Match.Headers["x"] = changed
		fi.Spec.StopConditions.Rules[0].Name = changed
		fi.Spec.Actions.MeshFaults[0].HTTP.VirtualServiceRef.Name = changed
		Expect(cp.Labels["a"]).To(Equal("b"))
		Expect(cp.Spec.Actions.MeshFaults[0].Name).To(Equal("a1"))
		Expect(cp.Spec.Actions.MeshFaults[0].HTTP.Routes[0].Match.Headers["x"]).To(Equal("y"))
		Expect(cp.Spec.StopConditions.Rules[0].Name).To(Equal("r1"))
		Expect(cp.Spec.Actions.MeshFaults[0].HTTP.VirtualServiceRef.Name).To(Equal("vs"))
	})

	It("AddToScheme registers FaultInjection so scheme can create objects by GVK", func() {
		sch := runtime.NewScheme()
		Expect(AddToScheme(sch)).To(Succeed())

		gvk := schema.GroupVersionKind{
			Group:   GroupVersion.Group,
			Version: GroupVersion.Version,
			Kind:    "FaultInjection",
		}

		obj, err := sch.New(gvk)
		Expect(err).NotTo(HaveOccurred())
		_, ok := obj.(*FaultInjection)
		Expect(ok).To(BeTrue())
	})

	It("exercises DeepCopy/DeepCopyInto/DeepCopyObject across top-level and nested types", func() {
		overall := int32(3)
		failOnNoMetrics := true
		q := 0.95

		fi := &FaultInjection{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "chaos.sghaida.io/v1alpha1",
				Kind:       "FaultInjection",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fi-deepcopy",
				Namespace: "default",
				Labels:    map[string]string{"a": "b"},
			},
			Spec: FaultInjectionSpec{
				BlastRadius: BlastRadiusSpec{
					DurationSeconds:   60,
					MaxTrafficPercent: 50,
					Scope:             "namespace",
					MaxPodsAffected:   2,
				},
				Actions: ActionsSpec{
					MeshFaults: []MeshFaultAction{
						{
							Name:      "latency",
							Direction: "OUTBOUND",
							Type:      "HTTP_LATENCY",
							Percent:   10,
							HTTP: HTTPFaultSpec{
								DestinationHosts: []string{"example.com", "example2.com"},
								SourceSelector: &LabelSelector{
									MatchLabels: map[string]string{"app": "curl-client"},
								},
								Routes: []HTTPRouteTarget{
									{
										Match: HTTPMatch{
											URIPrefix: "/",
											Headers:   map[string]string{"x": "y"},
										},
									},
								},
								Delay: &DelayConfig{FixedDelaySeconds: 2},
								Timeout: &TimeoutConfig{
									TimeoutSeconds: 1,
								},
							},
						},
					},
				},
				Cancel: true,
				StopConditions: &StopConditionsSpec{
					OverallBreachesCount: &overall,
					FailOnNoMetrics:      &failOnNoMetrics,
					Defaults: &StopDefaults{
						IntervalSeconds:     2,
						WindowSeconds:       10,
						ConsecutiveBreaches: 0, // overall mode => should be unused
					},
					Rules: []StopRule{
						{
							Name: "r1",
							// promql variant
							PromQL: "sum(rate(http_requests_total[1m]))",
							Compare: MetricCompare{
								Op:        "GT",
								Threshold: 1,
							},
						},
						{
							Name: "r2",
							// structured variant
							Structured: &StructuredQuery{
								Metric: MetricRef{
									Name: "http_request_duration_seconds_bucket",
									Type: "histogram",
								},
								Query: MetricQuery{
									Kind:        "quantile",
									Aggregation: "sum",
									Quantile:    &q,
								},
								Match: MetricMatch{
									Labels:  map[string]string{"job": "x", "pod": "~api-.*"},
									GroupBy: []string{"pod"},
								},
							},
							Compare: MetricCompare{
								Op:        "GTE",
								Threshold: 0.5,
							},
						},
					},
				},
			},
			Status: FaultInjectionStatus{
				Phase:      "Running",
				Message:    "hello",
				StopReason: "external",
				Rules: []StopRuleStatus{
					{
						Name:         "r1",
						BreachCount:  2,
						LastObserved: 123.4,
						LastQuery:    "q",
						Message:      "ok",
					},
				},
			},
		}

		By("DeepCopyInto produces an independent copy")
		var into FaultInjection
		fi.DeepCopyInto(&into)
		Expect(into.Name).To(Equal(fi.Name))
		Expect(into.Spec.Actions.MeshFaults).To(HaveLen(1))

		// mutate copy and ensure original is unchanged (maps/slices/pointers)
		into.Labels["a"] = changed
		Expect(fi.Labels["a"]).To(Equal("b"))

		into.Spec.Actions.MeshFaults[0].HTTP.DestinationHosts[0] = changed
		Expect(fi.Spec.Actions.MeshFaults[0].HTTP.DestinationHosts[0]).To(Equal("example.com"))

		into.Spec.Actions.MeshFaults[0].HTTP.SourceSelector.MatchLabels["app"] = changed
		Expect(fi.Spec.Actions.MeshFaults[0].HTTP.SourceSelector.MatchLabels["app"]).To(Equal("curl-client"))

		By("DeepCopy returns a new instance")
		cp := fi.DeepCopy()
		Expect(cp).NotTo(BeNil())
		Expect(cp).NotTo(BeIdenticalTo(fi))

		cp.Spec.Cancel = false
		Expect(fi.Spec.Cancel).To(BeTrue())

		By("DeepCopyObject returns a runtime.Object that is also independent")
		obj := fi.DeepCopyObject()
		Expect(obj).NotTo(BeNil())

		typed, ok := obj.(*FaultInjection)
		Expect(ok).To(BeTrue())
		Expect(typed).NotTo(BeIdenticalTo(fi))

		typed.Spec.BlastRadius.DurationSeconds = 999
		Expect(fi.Spec.BlastRadius.DurationSeconds).To(Equal(int64(60)))

		By("FaultInjectionList DeepCopyObject/DeepCopy should work too")
		lst := &FaultInjectionList{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "chaos.sghaida.io/v1alpha1",
				Kind:       "FaultInjectionList",
			},
			ListMeta: metav1.ListMeta{ResourceVersion: "1"},
			Items:    []FaultInjection{*fi.DeepCopy()},
		}

		lstObj := lst.DeepCopyObject()
		Expect(lstObj).NotTo(BeNil())

		lst2 := lst.DeepCopy()
		Expect(lst2).NotTo(BeNil())
		Expect(lst2).NotTo(BeIdenticalTo(lst))

		// mutate list copy and ensure original not changed
		lst2.Items[0].Spec.BlastRadius.MaxPodsAffected = 999
		Expect(lst.Items[0].Spec.BlastRadius.MaxPodsAffected).To(Equal(int64(2)))

		By("DeepCopy should deep-copy nested StopConditions fields (maps/slices/pointers) even when leaf types have no DeepCopy methods")
		// IMPORTANT:
		// controller-gen does NOT generate DeepCopy methods for leaf structs like MetricRef/MetricQuery/MetricMatch/MetricCompare,
		// so we validate deep-copy behavior by mutating those nested fields in the copied object and asserting the original is unchanged.

		cp2 := fi.DeepCopy()

		// mutate nested compare + structured query fields in the copy
		cp2.Spec.StopConditions.Rules[0].Compare.Op = "LT"
		cp2.Spec.StopConditions.Rules[0].Compare.Threshold = 999

		cp2.Spec.StopConditions.Rules[1].Structured.Metric.Name = changed
		cp2.Spec.StopConditions.Rules[1].Structured.Query.Kind = "rate"
		cp2.Spec.StopConditions.Rules[1].Structured.Match.Labels["job"] = changed
		cp2.Spec.StopConditions.Rules[1].Structured.Match.GroupBy[0] = changed

		// original must stay the same
		Expect(fi.Spec.StopConditions.Rules[0].Compare.Op).To(Equal("GT"))
		Expect(fi.Spec.StopConditions.Rules[0].Compare.Threshold).To(Equal(float64(1)))

		Expect(fi.Spec.StopConditions.Rules[1].Structured.Metric.Name).To(Equal("http_request_duration_seconds_bucket"))
		Expect(fi.Spec.StopConditions.Rules[1].Structured.Query.Kind).To(Equal("quantile"))
		Expect(fi.Spec.StopConditions.Rules[1].Structured.Match.Labels["job"]).To(Equal("x"))
		Expect(fi.Spec.StopConditions.Rules[1].Structured.Match.GroupBy[0]).To(Equal("pod"))
	})
})

func ptrBool(v bool) *bool    { return &v }
func ptrInt32(v int32) *int32 { return &v }
