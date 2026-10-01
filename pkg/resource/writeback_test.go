// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package resource

import (
	"context"
	"encoding/json"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/workload-map/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/pkg/jq/execution"
)

var _ = Describe("write-back", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	// asJSON normalizes any value for deep comparison.
	asJSON := func(v any) string {
		b, err := json.Marshal(v)
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}

	newAccessor := func(object map[string]any) *Accessor {
		return NewAccessor(execution.NewDefaultRunner(object))
	}

	podObject := func() map[string]any {
		return map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "web-1", "labels": map[string]any{"app": "web"}},
			"spec": map[string]any{
				"nodeName":   "node-7",
				"containers": []any{map[string]any{"name": "main", "image": "nginx:1.25"}},
			},
			"status": map[string]any{"phase": "Running", "podIP": "10.0.0.12"},
		}
	}

	rootTemplateDefinition := v1alpha1.ComponentDefinition{
		Name:           "pod",
		SpecDefinition: &v1alpha1.SpecDefinition{PodTemplateSpecPath: ptr.To(".")},
	}

	Context("root template path (a Pod read as its own template)", func() {
		It("adds one label and keeps the object identical otherwise", func() {
			accessor := newAccessor(podObject())

			templates, err := accessor.ExtractPodTemplateSpec(ctx, rootTemplateDefinition)
			Expect(err).NotTo(HaveOccurred())
			Expect(templates).To(HaveLen(1))

			templates[0].Labels["team"] = "ml"
			Expect(accessor.UpdatePodTemplateSpec(ctx, rootTemplateDefinition, templates)).To(Succeed())

			want := podObject()
			want["metadata"].(map[string]any)["labels"].(map[string]any)["team"] = "ml"
			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(want)))
		})

		It("leaves the object byte-identical on a no-op update", func() {
			accessor := newAccessor(podObject())

			templates, err := accessor.ExtractPodTemplateSpec(ctx, rootTemplateDefinition)
			Expect(err).NotTo(HaveOccurred())
			Expect(accessor.UpdatePodTemplateSpec(ctx, rootTemplateDefinition, templates)).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(podObject())))
		})
	})

	Context("a pod spec sharing its path with wrapper fields (KServe transformer shape)", func() {
		transformerObject := func() map[string]any {
			return map[string]any{
				"apiVersion": "serving.example.io/v1", "kind": "Service",
				"metadata": map[string]any{"name": "svc-1"},
				"spec": map[string]any{
					"transformer": map[string]any{
						"minReplicas": float64(2),
						"scaleTarget": float64(80),
						"labels":      map[string]any{"tier": "serving"},
						"containers":  []any{map[string]any{"name": "t", "image": "transformer:v1"}},
					},
				},
			}
		}
		definition := v1alpha1.ComponentDefinition{
			Name: "transformer",
			SpecDefinition: &v1alpha1.SpecDefinition{
				PodSpecPath:  ptr.To(".spec.transformer"),
				MetadataPath: ptr.To(".spec.transformer"),
			},
		}

		It("keeps the wrapper's own fields when writing the pod spec view", func() {
			accessor := newAccessor(transformerObject())

			specs, err := accessor.ExtractPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].SchedulerName = "custom-scheduler"
			Expect(accessor.UpdatePodSpec(ctx, definition, specs)).To(Succeed())

			want := transformerObject()
			want["spec"].(map[string]any)["transformer"].(map[string]any)["schedulerName"] = "custom-scheduler"
			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(want)))
		})

		It("keeps the containers when writing the metadata view of the same path", func() {
			accessor := newAccessor(transformerObject())

			metadata, err := accessor.ExtractPodMetadata(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			metadata[0].Labels["team"] = "ml"
			Expect(accessor.UpdatePodMetadata(ctx, definition, metadata)).To(Succeed())

			want := transformerObject()
			want["spec"].(map[string]any)["transformer"].(map[string]any)["labels"].(map[string]any)["team"] = "ml"
			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(want)))
		})
	})

	Context("a sparse overlay merged by its controller (LLMInferenceService shape)", func() {
		overlayObject := func() map[string]any {
			return map[string]any{
				"apiVersion": "serving.example.io/v1alpha2", "kind": "LLMInferenceService",
				"metadata": map[string]any{"name": "llm-1"},
				"spec": map[string]any{
					"model": map[string]any{"uri": "pvc://models"},
					"template": map[string]any{
						"containers": []any{map[string]any{
							"name": "main", "image": "runtime:v1",
							"vendorExtension": "keep-me",
						}},
					},
				},
			}
		}
		definition := v1alpha1.ComponentDefinition{
			Name:           "main",
			SpecDefinition: &v1alpha1.SpecDefinition{PodSpecPath: ptr.To(".spec.template")},
		}

		It("patches one container by name and keeps fields the projection cannot represent", func() {
			accessor := newAccessor(overlayObject())

			specs, err := accessor.ExtractPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].Containers[0].Image = "runtime:v2"
			specs[0].Containers = append(specs[0].Containers, corev1.Container{Name: "sidecar", Image: "sidecar:v1"})
			Expect(accessor.UpdatePodSpec(ctx, definition, specs)).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			containers := got["spec"].(map[string]any)["template"].(map[string]any)["containers"].([]any)
			Expect(containers).To(HaveLen(2))
			main := containers[0].(map[string]any)
			Expect(main["image"]).To(Equal("runtime:v2"))
			Expect(main["vendorExtension"]).To(Equal("keep-me"), "field outside the projection type must survive the write")
			Expect(main).NotTo(HaveKey("resources"), "no typed zero value may materialize on an untouched element")
			Expect(containers[1].(map[string]any)["name"]).To(Equal("sidecar"))
		})

		It("creates the subtree when the declared path is absent", func() {
			object := overlayObject()
			delete(object["spec"].(map[string]any), "template")
			accessor := newAccessor(object)

			Expect(accessor.UpdatePodSpec(ctx, definition, []corev1.PodSpec{{
				Containers: []corev1.Container{{Name: "main", Image: "runtime:v1"}},
			}})).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			containers := got["spec"].(map[string]any)["template"].(map[string]any)["containers"].([]any)
			Expect(containers[0].(map[string]any)["image"]).To(Equal("runtime:v1"))
		})
	})

	Context("guard rails", func() {
		It("refuses a path that computes a value instead of addressing one", func() {
			definition := v1alpha1.ComponentDefinition{
				Name:           "pod",
				SpecDefinition: &v1alpha1.SpecDefinition{PodSpecPath: ptr.To(`(.spec.replicas // 1)`)},
			}
			accessor := newAccessor(podObject())

			err := accessor.UpdatePodSpec(ctx, definition, []corev1.PodSpec{{SchedulerName: "s"}})
			var notWritable *execution.PathNotWritableError
			Expect(errors.As(err, &notWritable)).To(BeTrue(), "got: %v", err)

			got, getErr := accessor.GetObject()
			Expect(getErr).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(podObject())), "a refused write must not touch the object")
		})

		It("refuses values carrying strategic merge directives as literal keys", func() {
			accessor := newAccessor(podObject())
			definition := v1alpha1.ComponentDefinition{
				Name:           "pod",
				SpecDefinition: &v1alpha1.SpecDefinition{MetadataPath: ptr.To(".metadata")},
			}

			err := accessor.UpdatePodMetadata(ctx, definition, []metav1.ObjectMeta{{
				Name:   "web-1",
				Labels: map[string]string{"app": "web", "$patch": "delete"},
			}})
			var verification *WriteVerificationError
			Expect(errors.As(err, &verification)).To(BeTrue(), "got: %v", err)
		})

		It("refuses a root write that would change the object's identity", func() {
			current := map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "a"}}
			merged := map[string]any{"kind": "Job", "metadata": map[string]any{"name": "a"}}
			Expect(guardIdentity(current, merged)).To(MatchError(ContainSubstring("kind")))

			renamed := map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "b"}}
			Expect(guardIdentity(current, renamed)).To(MatchError(ContainSubstring("metadata.name")))

			Expect(guardIdentity(current, current)).To(Succeed())
		})

		It("commits a fragmented update atomically: a refused fragment rolls back everything", func() {
			object := map[string]any{
				"spec": map[string]any{
					"schedulerName": "default",
					"podLabels":     map[string]any{"app": "web"},
				},
			}
			accessor := newAccessor(object)
			definition := v1alpha1.ComponentDefinition{
				Name: "svc",
				SpecDefinition: &v1alpha1.SpecDefinition{
					FragmentedPodSpecDefinition: &v1alpha1.FragmentedPodSpecDefinition{
						SchedulerNamePath: ptr.To(".spec.schedulerName"),
						LabelsPath:        ptr.To(".spec.podLabels"),
					},
				},
			}

			err := accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{{
				SchedulerName: "custom",
				Labels:        map[string]string{"$patch": "delete"},
			}})
			var verification *WriteVerificationError
			Expect(errors.As(err, &verification)).To(BeTrue(), "got: %v", err)

			got, getErr := accessor.GetObject()
			Expect(getErr).NotTo(HaveOccurred())
			Expect(got["spec"].(map[string]any)["schedulerName"]).To(Equal("default"),
				"the valid fragment must not commit when a later one is refused")
		})

		It("clears a containers list to empty, not to null", func() {
			object := map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "main", "image": "a:v1"}},
				},
			}
			accessor := newAccessor(object)
			definition := v1alpha1.ComponentDefinition{
				Name: "svc",
				SpecDefinition: &v1alpha1.SpecDefinition{
					FragmentedPodSpecDefinition: &v1alpha1.FragmentedPodSpecDefinition{
						ContainersPath: ptr.To(".spec.containers"),
					},
				},
			}

			Expect(accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{{
				Containers: []corev1.Container{},
			}})).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(got["spec"].(map[string]any)["containers"]).To(Equal([]any{}))
		})

		It("writes only the changed fragment when projections overlap", func() {
			object := map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "main", "image": "a:v1", "vendorExtension": "keep"}},
				},
			}
			accessor := newAccessor(object)
			definition := v1alpha1.ComponentDefinition{
				Name: "svc",
				SpecDefinition: &v1alpha1.SpecDefinition{
					FragmentedPodSpecDefinition: &v1alpha1.FragmentedPodSpecDefinition{
						ImagePath:     ptr.To(".spec.containers[0].image"),
						ContainerPath: ptr.To(".spec.containers[0]"),
					},
				},
			}

			// The consumer read both views, changed only the image: the stale
			// container view diffs empty and must not restore the old image.
			Expect(accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{{
				Image:     "a:v2",
				Container: &corev1.Container{Name: "main", Image: "a:v1"},
			}})).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			main := got["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			Expect(main["image"]).To(Equal("a:v2"))
			Expect(main["vendorExtension"]).To(Equal("keep"))

			// Both views changed at once is order-dependent: refused.
			err = accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{{
				Image:     "a:v3",
				Container: &corev1.Container{Name: "main", Image: "a:v4"},
			}})
			var verification *WriteVerificationError
			Expect(errors.As(err, &verification)).To(BeTrue(), "got: %v", err)
		})

		It("keeps unknown element fields when a retainKeys-tagged list entry changes", func() {
			object := map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"containers": []any{map[string]any{"name": "main", "image": "a:v1"}},
						"resourceClaims": []any{map[string]any{
							"name": "gpu", "resourceClaimName": "old", "vendorExtension": "keep",
						}},
					},
				},
			}
			accessor := newAccessor(object)
			definition := v1alpha1.ComponentDefinition{
				Name:           "svc",
				SpecDefinition: &v1alpha1.SpecDefinition{PodSpecPath: ptr.To(".spec.template")},
			}

			specs, err := accessor.ExtractPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].ResourceClaims[0].ResourceClaimName = ptr.To("new")
			Expect(accessor.UpdatePodSpec(ctx, definition, specs)).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			claim := got["spec"].(map[string]any)["template"].(map[string]any)["resourceClaims"].([]any)[0].(map[string]any)
			Expect(claim["resourceClaimName"]).To(Equal("new"))
			Expect(claim["vendorExtension"]).To(Equal("keep"), "retainKeys must not delete fields the projection cannot see")
		})

		It("refuses negative array indexes, which alias locations", func() {
			accessor := newAccessor(podObject())
			definition := v1alpha1.ComponentDefinition{
				Name:           "pod",
				SpecDefinition: &v1alpha1.SpecDefinition{PodSpecPath: ptr.To(".spec.containers[-1]")},
			}

			err := accessor.UpdatePodSpec(ctx, definition, []corev1.PodSpec{{SchedulerName: "s"}})
			var notWritable *execution.PathNotWritableError
			Expect(errors.As(err, &notWritable)).To(BeTrue(), "got: %v", err)

			got, getErr := accessor.GetObject()
			Expect(getErr).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(podObject())))
		})

		It("deletes a label the consumer removed, and only that label", func() {
			accessor := newAccessor(podObject())
			definition := v1alpha1.ComponentDefinition{
				Name:           "pod",
				SpecDefinition: &v1alpha1.SpecDefinition{MetadataPath: ptr.To(".metadata")},
			}

			metadata, err := accessor.ExtractPodMetadata(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			delete(metadata[0].Labels, "app")
			Expect(accessor.UpdatePodMetadata(ctx, definition, metadata)).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			want := podObject()
			// The emptied map reads back the same as an absent one through the
			// projection, so the key goes away, exactly like the apiserver.
			delete(want["metadata"].(map[string]any), "labels")
			Expect(asJSON(got)).To(MatchJSON(asJSON(want)))
		})
	})
})
