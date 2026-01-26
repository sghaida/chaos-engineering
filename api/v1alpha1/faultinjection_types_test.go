package v1alpha1

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var _ = Describe("FaultInjection API types", func() {
	It("marshals spec.cancel with omitempty semantics (absent when false, present when true)", func() {
		// cancel=false (zero value) should not appear in JSON
		fi := FaultInjection{
			TypeMeta: metav1.TypeMeta{
				APIVersion: GroupVersion.String(),
				Kind:       "FaultInjection",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fi-test",
				Namespace: "default",
			},
			Spec: FaultInjectionSpec{
				BlastRadius: BlastRadiusSpec{
					DurationSeconds:   60,
					MaxTrafficPercent: 10,
					// Scope omitted intentionally
				},
				Actions: ActionsSpec{
					MeshFaults: []MeshFaultAction{
						{
							Name:      "inbound-latency",
							Direction: "INBOUND",
							Type:      "HTTP_LATENCY",
							Percent:   10,
							HTTP: HTTPFaultSpec{
								VirtualServiceRef: &VirtualServiceRef{Name: "httpbin"},
								Routes: []HTTPRouteTarget{
									{Match: HTTPMatch{URIPrefix: "/"}},
								},
								Delay: &DelayConfig{FixedDelaySeconds: 1},
							},
						},
					},
				},
				// Cancel is false by default
			},
		}

		b, err := json.Marshal(fi)
		Expect(err).NotTo(HaveOccurred())

		var m map[string]any
		Expect(json.Unmarshal(b, &m)).To(Succeed())

		spec := m["spec"].(map[string]any)
		_, hasCancel := spec["cancel"]
		Expect(hasCancel).To(BeFalse(), "cancel should be omitted when false (omitempty)")

		// Now set cancel=true and ensure it shows up
		fi.Spec.Cancel = true
		b2, err := json.Marshal(fi)
		Expect(err).NotTo(HaveOccurred())

		var m2 map[string]any
		Expect(json.Unmarshal(b2, &m2)).To(Succeed())

		spec2 := m2["spec"].(map[string]any)
		v, hasCancel2 := spec2["cancel"]
		Expect(hasCancel2).To(BeTrue(), "cancel should be present when true")
		Expect(v).To(Equal(true))
	})

	It("omits optional fields when unset (stopConditions, blastRadius.scope)", func() {
		fi := FaultInjection{
			TypeMeta: metav1.TypeMeta{
				APIVersion: GroupVersion.String(),
				Kind:       "FaultInjection",
			},
			ObjectMeta: metav1.ObjectMeta{Name: "fi-omit", Namespace: "default"},
			Spec: FaultInjectionSpec{
				BlastRadius: BlastRadiusSpec{
					DurationSeconds:   60,
					MaxTrafficPercent: 10,
					// Scope unset
				},
				Actions: ActionsSpec{
					MeshFaults: []MeshFaultAction{
						{
							Name:      "inbound-latency",
							Direction: "INBOUND",
							Type:      "HTTP_LATENCY",
							Percent:   10,
							HTTP: HTTPFaultSpec{
								VirtualServiceRef: &VirtualServiceRef{Name: "httpbin"},
								Routes:            []HTTPRouteTarget{{Match: HTTPMatch{URIPrefix: "/"}}},
								Delay:             &DelayConfig{FixedDelaySeconds: 1},
							},
						},
					},
				},
				// StopConditions nil
			},
		}

		b, err := json.Marshal(fi)
		Expect(err).NotTo(HaveOccurred())

		var m map[string]any
		Expect(json.Unmarshal(b, &m)).To(Succeed())

		spec := m["spec"].(map[string]any)

		_, hasStop := spec["stopConditions"]
		Expect(hasStop).To(BeFalse(), "stopConditions should be omitted when nil (omitempty)")

		br := spec["blastRadius"].(map[string]any)
		_, hasScope := br["scope"]
		Expect(hasScope).To(BeFalse(), "blastRadius.scope should be omitted when empty (omitempty)")
	})

	It("registers in the scheme and supports encode/decode roundtrip", func() {
		sch := runtime.NewScheme()
		Expect(AddToScheme(sch)).To(Succeed())

		gvk := schema.GroupVersionKind{
			Group:   GroupVersion.Group,
			Version: GroupVersion.Version,
			Kind:    "FaultInjection",
		}

		// Create an object and set GVK
		orig := &FaultInjection{
			TypeMeta: metav1.TypeMeta{
				APIVersion: GroupVersion.String(),
				Kind:       "FaultInjection",
			},
			ObjectMeta: metav1.ObjectMeta{Name: "fi-roundtrip", Namespace: "default"},
			Spec: FaultInjectionSpec{
				BlastRadius: BlastRadiusSpec{DurationSeconds: 60, MaxTrafficPercent: 10},
				Actions: ActionsSpec{
					MeshFaults: []MeshFaultAction{
						{
							Name:      "inbound-latency",
							Direction: "INBOUND",
							Type:      "HTTP_LATENCY",
							Percent:   10,
							HTTP: HTTPFaultSpec{
								VirtualServiceRef: &VirtualServiceRef{Name: "httpbin"},
								Routes:            []HTTPRouteTarget{{Match: HTTPMatch{URIPrefix: "/"}}},
								Delay:             &DelayConfig{FixedDelaySeconds: 1},
							},
						},
					},
				},
			},
		}
		orig.GetObjectKind().SetGroupVersionKind(gvk)

		codecs := serializer.NewCodecFactory(sch)
		enc := codecs.EncoderForVersion(codecs.LegacyCodec(gvk.GroupVersion()), gvk.GroupVersion())
		dec := codecs.UniversalDecoder(gvk.GroupVersion())

		data, err := runtime.Encode(enc, orig)
		Expect(err).NotTo(HaveOccurred())
		Expect(data).NotTo(BeEmpty())

		obj, gotGVK, err := dec.Decode(data, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(gotGVK.Kind).To(Equal("FaultInjection"))

		round, ok := obj.(*FaultInjection)
		Expect(ok).To(BeTrue())
		Expect(round.Name).To(Equal("fi-roundtrip"))
		Expect(round.Namespace).To(Equal("default"))
		Expect(round.Spec.BlastRadius.DurationSeconds).To(Equal(int64(60)))
		Expect(round.Spec.Actions.MeshFaults).To(HaveLen(1))
		Expect(round.Spec.Actions.MeshFaults[0].Name).To(Equal("inbound-latency"))
	})
})
