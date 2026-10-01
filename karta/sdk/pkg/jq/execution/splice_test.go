// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package execution

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ResolvePaths and SpliceValues", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	source := func() map[string]any {
		return map[string]any{
			"spec": map[string]any{
				"groups": []any{
					map[string]any{"name": "a", "replicas": float64(1)},
					map[string]any{"name": "b", "replicas": float64(2)},
				},
			},
		}
	}

	It("resolves one location per match, in document order", func() {
		runner := NewDefaultRunner(source())
		paths, err := runner.ResolvePaths(ctx, ".spec.groups[].replicas")
		Expect(err).NotTo(HaveOccurred())
		Expect(paths).To(Equal([][]any{
			{"spec", "groups", 0, "replicas"},
			{"spec", "groups", 1, "replicas"},
		}))
	})

	It("resolves an absent trailing key so a write can create it", func() {
		runner := NewDefaultRunner(source())
		paths, err := runner.ResolvePaths(ctx, ".spec.template")
		Expect(err).NotTo(HaveOccurred())
		Expect(paths).To(Equal([][]any{{"spec", "template"}}))
	})

	It("refuses an expression that computes a value", func() {
		runner := NewDefaultRunner(source())
		_, err := runner.ResolvePaths(ctx, "(.spec.replicas // 1)")
		var notWritable *PathNotWritableError
		Expect(errors.As(err, &notWritable)).To(BeTrue(), "got: %v", err)
		Expect(notWritable.Expression).To(Equal("(.spec.replicas // 1)"))
	})

	It("splices every path atomically", func() {
		runner := NewDefaultRunner(source())
		paths, err := runner.ResolvePaths(ctx, ".spec.groups[].replicas")
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.SpliceValues(ctx, paths, []any{float64(3), float64(4)})).To(Succeed())

		object, err := runner.GetObject()
		Expect(err).NotTo(HaveOccurred())
		groups := object.(map[string]any)["spec"].(map[string]any)["groups"].([]any)
		Expect(groups[0].(map[string]any)["replicas"]).To(Equal(float64(3)))
		Expect(groups[1].(map[string]any)["replicas"]).To(Equal(float64(4)))
	})

	It("rejects a count mismatch before touching the object", func() {
		runner := NewDefaultRunner(source())
		err := runner.SpliceValues(ctx, [][]any{{"spec"}}, []any{1, 2})
		Expect(err).To(MatchError(ContainSubstring("length mismatch")))

		object, err := runner.GetObject()
		Expect(err).NotTo(HaveOccurred())
		Expect(object).To(Equal(any(source())))
	})

	It("writes nothing when there are no paths", func() {
		runner := NewDefaultRunner(source())
		Expect(runner.SpliceValues(ctx, nil, nil)).To(Succeed())
		object, err := runner.GetObject()
		Expect(err).NotTo(HaveOccurred())
		Expect(object).To(Equal(any(source())))
	})
})
