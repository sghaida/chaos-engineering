package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
)

var _ = Describe("helpers", func() {
	Describe("sanitizeName", func() {
		It("lowercases, trims, and normalizes separators", func() {
			Expect(sanitizeName("  Hello World  ")).To(Equal("hello-world"))
			Expect(sanitizeName("Hello_World")).To(Equal("hello-world"))
			Expect(sanitizeName("Hello   World")).To(Equal("hello---world")) // spaces become '-'
		})

		It("replaces non [a-z0-9-] runes with '-'", func() {
			Expect(sanitizeName("a*b")).To(Equal("a-b"))
			Expect(sanitizeName("a.b")).To(Equal("a-b"))
			Expect(sanitizeName("a/b")).To(Equal("a-b"))
		})

		It("trims leading/trailing '-' and returns 'x' if empty", func() {
			Expect(sanitizeName("---")).To(Equal("x"))
			Expect(sanitizeName("   ")).To(Equal("x"))
			Expect(sanitizeName("___")).To(Equal("x"))
			Expect(sanitizeName("-a-")).To(Equal("a"))
		})

		It("keeps already safe names stable", func() {
			Expect(sanitizeName("fi-action-1")).To(Equal("fi-action-1"))
			Expect(sanitizeName("abc123-xyz")).To(Equal("abc123-xyz"))
		})
	})

	Describe("joinKey / splitKey", func() {
		It("joins namespace/name and splits it back", func() {
			k := joinKey("ns", "obj")
			Expect(k).To(Equal("ns/obj"))

			ns, name := splitKey(k)
			Expect(ns).To(Equal("ns"))
			Expect(name).To(Equal("obj"))
		})

		It("returns ('', key) if no '/' is present", func() {
			ns, name := splitKey("justname")
			Expect(ns).To(Equal(""))
			Expect(name).To(Equal("justname"))
		})

		It("splits only on the first '/'", func() {
			ns, name := splitKey("a/b/c")
			Expect(ns).To(Equal("a"))
			Expect(name).To(Equal("b/c"))
		})
	})

	Describe("toAnySlice", func() {
		It("converts []string to []any with same values", func() {
			in := []string{"a", "b", "c"}
			out := toAnySlice(in)
			Expect(out).To(HaveLen(3))
			Expect(out[0]).To(Equal(any("a")))
			Expect(out[1]).To(Equal(any("b")))
			Expect(out[2]).To(Equal(any("c")))
		})

		It("returns empty slice for empty input", func() {
			out := toAnySlice(nil)
			Expect(out).To(BeEmpty())
		})
	})

	Describe("uniqueAppend", func() {
		It("appends only new items and sorts deterministically", func() {
			base := []string{"b", "a"}
			add := []string{"c", "a", "d", "b"}

			out := uniqueAppend(base, add)
			Expect(out).To(Equal([]string{"a", "b", "c", "d"}))
		})

		It("handles nil and empty inputs", func() {
			Expect(uniqueAppend(nil, nil)).To(BeEmpty())
			Expect(uniqueAppend([]string{}, []string{})).To(BeEmpty())
			Expect(uniqueAppend([]string{"x"}, nil)).To(Equal([]string{"x"}))
		})
	})

	Describe("getString", func() {
		It("returns the string value when present", func() {
			m := map[string]any{"k": "v"}
			Expect(getString(m, "k")).To(Equal("v"))
		})

		It("returns empty string if missing or not a string", func() {
			m := map[string]any{"k": 123}
			Expect(getString(m, "missing")).To(Equal(""))
			Expect(getString(m, "k")).To(Equal(""))
		})
	})

	Describe("cloneAnySlice", func() {
		It("returns a shallow copy (mutating clone slice elements does not affect original slice elements)", func() {
			orig := []any{"a", "b"}
			cl := cloneAnySlice(orig)

			Expect(cl).To(Equal(orig))

			cl[0] = "z"
			Expect(orig[0]).To(Equal(any("a")))
			Expect(cl[0]).To(Equal(any("z")))
		})

		It("does not deep-copy nested reference types", func() {
			nested := map[string]any{"x": 1}
			orig := []any{nested}
			cl := cloneAnySlice(orig)

			cl[0].(map[string]any)["x"] = 2
			Expect(orig[0].(map[string]any)["x"]).To(Equal(2))
		})
	})

	Describe("cloneMap", func() {
		It("returns a new map with the same top-level keys/values", func() {
			in := map[string]any{"a": 1, "b": "x"}
			out := cloneMap(in)

			Expect(out).To(Equal(in))
			// New map instance
			out["a"] = 2
			Expect(in["a"]).To(Equal(1))
		})

		It("does not deep-copy nested reference types", func() {
			nested := map[string]any{"x": 1}
			in := map[string]any{"nested": nested}
			out := cloneMap(in)

			out["nested"].(map[string]any)["x"] = 99
			Expect(in["nested"].(map[string]any)["x"]).To(Equal(99))
		})
	})

	Describe("event / eventf", func() {
		It("does not panic when Recorder is nil", func() {
			r := &FaultInjectionReconciler{Recorder: nil}
			obj := &v1.Pod{} // satisfies runtime.Object

			Expect(func() {
				r.event(obj, "Normal", "Reason", "Action", "note")
				r.eventf(obj, "Normal", "Reason", "Action", "hello %s", "world")
			}).NotTo(Panic())
		})
	})
})
