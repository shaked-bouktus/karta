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
	"k8s.io/apimachinery/pkg/api/resource"
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
			var refused *WriteError
			Expect(errors.As(err, &refused)).To(BeTrue(), "got: %v", err)
			Expect(err).To(MatchError(ErrDirectiveKey))
		})

		It("refuses a root write that would change the object's identity", func() {
			current := map[string]any{"kind": "Pod", "metadata": map[string]any{"name": "a"}}
			merged := map[string]any{"kind": "Job", "metadata": map[string]any{"name": "a"}}
			Expect(guardIdentity(current, merged)).To(MatchError(ErrIdentityChange))
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
			var refused *WriteError
			Expect(errors.As(err, &refused)).To(BeTrue(), "got: %v", err)
			Expect(err).To(MatchError(ErrDirectiveKey))

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

			// Conflicting image values must be refused without committing either view.
			before := asJSON(got)
			err = accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{{
				Image:     "a:v3",
				Container: &corev1.Container{Name: "main", Image: "a:v4"},
			}})
			var refused *WriteError
			Expect(errors.As(err, &refused)).To(BeTrue(), "got: %v", err)
			Expect(err).To(MatchError(ErrConflictingWrites))
			got, err = accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(before))
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

	Context("resolving and splicing locations through the Runner interface", func() {
		groupsObject := func() map[string]any {
			return map[string]any{"spec": map[string]any{"groups": []any{
				map[string]any{"name": "a", "replicas": float64(1)},
				map[string]any{"name": "b", "replicas": float64(2)},
			}}}
		}

		It("resolves one location per match, in document order", func() {
			runner := execution.NewDefaultRunner(groupsObject())
			Expect(resolveLocations(ctx, runner, ".spec.groups[].replicas")).To(Equal([][]any{
				{"spec", "groups", 0, "replicas"},
				{"spec", "groups", 1, "replicas"},
			}))
		})

		It("resolves an absent trailing key so a write can create it", func() {
			runner := execution.NewDefaultRunner(groupsObject())
			Expect(resolveLocations(ctx, runner, ".spec.template")).To(Equal([][]any{{"spec", "template"}}))
		})

		It("refuses an expression that computes a value", func() {
			runner := execution.NewDefaultRunner(groupsObject())
			_, err := resolveLocations(ctx, runner, "(.spec.replicas // 1)")
			var notWritable *execution.PathNotWritableError
			Expect(errors.As(err, &notWritable)).To(BeTrue(), "got: %v", err)
			Expect(notWritable.Expression).To(Equal("(.spec.replicas // 1)"))
		})

		It("splices every location in one update", func() {
			runner := execution.NewDefaultRunner(groupsObject())
			Expect(spliceLocations(ctx, runner,
				[][]any{{"spec", "groups", 0, "replicas"}, {"spec", "groups", 1, "replicas"}},
				[]any{float64(3), float64(4)})).To(Succeed())

			got, err := runner.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(`{"spec":{"groups":[{"name":"a","replicas":3},{"name":"b","replicas":4}]}}`))
		})

		It("splices the root location and keys that need escaping", func() {
			runner := execution.NewDefaultRunner(groupsObject())
			Expect(spliceLocations(ctx, runner, [][]any{{}}, []any{map[string]any{"kind": "Pod"}})).To(Succeed())
			Expect(spliceLocations(ctx, runner,
				[][]any{{"metadata", "annotations", `quote" backslash\ $(X) example.com/key`}},
				[]any{"v"})).To(Succeed())

			got, err := runner.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(`{"kind":"Pod","metadata":{"annotations":{"quote\" backslash\\ $(X) example.com/key":"v"}}}`))
		})

		It("refuses the whole splice when one location cannot be set, without touching the object", func() {
			runner := execution.NewDefaultRunner(groupsObject())
			err := spliceLocations(ctx, runner,
				[][]any{{"spec", "groups", 0, "replicas"}, {"spec", "groups", 0, "replicas", 0}},
				[]any{float64(3), float64(4)})
			Expect(err).To(HaveOccurred())

			got, getErr := runner.GetObject()
			Expect(getErr).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(groupsObject())))
		})

		It("refuses a count mismatch without touching the object", func() {
			runner := execution.NewDefaultRunner(groupsObject())
			Expect(spliceLocations(ctx, runner, [][]any{{"spec"}}, []any{1, 2})).To(MatchError(ContainSubstring("length mismatch")))

			got, err := runner.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(groupsObject())))
		})

		It("writes nothing when there are no locations", func() {
			runner := execution.NewDefaultRunner(groupsObject())
			Expect(spliceLocations(ctx, runner, nil, nil)).To(Succeed())

			got, err := runner.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(groupsObject())))
		})
	})

	Context("refusal causes", func() {
		// A stored keyed list may hold a null entry. The projection reads it as a
		// zero value, the raw list keeps nil, and strategicpatch cannot merge a list
		// that mixes nulls with objects. An edit that touches the list is refused
		// as a shape problem; an edit elsewhere leaves the list alone and lands.
		DescribeTable("refuses an edit to a keyed list that stores a null entry and leaves unrelated edits alone",
			func(object string, touch func(*corev1.PodSpec)) {
				var decoded map[string]any
				Expect(json.Unmarshal([]byte(object), &decoded)).To(Succeed())
				want := deepCopyMap(decoded)
				accessor := newAccessor(decoded)
				definition := v1alpha1.ComponentDefinition{
					Name:           "pod",
					SpecDefinition: &v1alpha1.SpecDefinition{PodSpecPath: ptr.To(".spec")},
				}

				specs, err := accessor.ExtractPodSpec(ctx, definition)
				Expect(err).NotTo(HaveOccurred())
				touch(&specs[0])
				err = accessor.UpdatePodSpec(ctx, definition, specs)
				var refused *WriteError
				Expect(errors.As(err, &refused)).To(BeTrue(), "got: %v", err)
				Expect(err).To(MatchError(ErrFragmentShape))
				got, err := accessor.GetObject()
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(want))

				specs, err = accessor.ExtractPodSpec(ctx, definition)
				Expect(err).NotTo(HaveOccurred())
				specs[0].SchedulerName = "kai-scheduler"
				Expect(accessor.UpdatePodSpec(ctx, definition, specs)).To(Succeed())
				want["spec"].(map[string]any)["schedulerName"] = "kai-scheduler"
				got, err = accessor.GetObject()
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(want), "the unrelated edit must leave the malformed list untouched")
			},
			Entry("a leading null env entry",
				`{"spec":{"containers":[{"name":"main","image":"app:v1","env":[null,{"name":"X","value":"old"}]}]}}`,
				func(s *corev1.PodSpec) { s.Containers[0].Env[1].Value = "new" }),
			Entry("a trailing null env entry",
				`{"spec":{"containers":[{"name":"main","image":"app:v1","env":[{"name":"X","value":"old"},null]}]}}`,
				func(s *corev1.PodSpec) { s.Containers[0].Env[0].Value = "new" }),
			Entry("a null port entry",
				`{"spec":{"containers":[{"name":"main","image":"app:v1","ports":[null,{"containerPort":80}]}]}}`,
				func(s *corev1.PodSpec) { s.Containers[0].Ports[1].Name = "http" }),
			Entry("a null volume entry",
				`{"spec":{"containers":[{"name":"main","image":"app:v1"}],"volumes":[null,{"name":"data","emptyDir":{}}]}}`,
				func(s *corev1.PodSpec) { s.Volumes[1].Name = "scratch" }),
		)

		It("refuses a stored fragment that does not fit the projection type", func() {
			// A requests entry holding an object is not a resource.Quantity.
			object := map[string]any{"spec": map[string]any{
				"resources": map[string]any{"requests": map[string]any{"gpu": map[string]any{"count": float64(1)}}},
			}}
			accessor := newAccessor(object)
			definition := v1alpha1.ComponentDefinition{
				Name: "svc",
				SpecDefinition: &v1alpha1.SpecDefinition{
					FragmentedPodSpecDefinition: &v1alpha1.FragmentedPodSpecDefinition{ResourcesPath: ptr.To(".spec.resources")},
				},
			}

			err := accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{{
				Resources: &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}},
			}})
			Expect(err).To(MatchError(ErrFragmentShape))
		})

		// A stored entry without its merge key keeps a null identity: the merge
		// cannot address it, so an edit to it fails read-back and nothing is written.
		It("refuses a change the merge cannot express (an env entry without a name)", func() {
			object := map[string]any{"spec": map[string]any{"containers": []any{
				map[string]any{"name": "main", "image": "a:v1", "env": []any{
					map[string]any{"value": "first", "vendorExtension": "keep"},
				}},
			}}}
			want := deepCopyMap(object)
			accessor := newAccessor(object)
			definition := v1alpha1.ComponentDefinition{
				Name:           "pod",
				SpecDefinition: &v1alpha1.SpecDefinition{PodSpecPath: ptr.To(".spec")},
			}

			specs, err := accessor.ExtractPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].Containers[0].Env[0].Value = "next"

			err = accessor.UpdatePodSpec(ctx, definition, specs)
			Expect(err).To(MatchError(ErrReadBackMismatch))
			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		})

		// An absent fragment reads as a zero projection; writing a changed value
		// would create it (a Worker replica spec with an empty pod template).
		It("refuses to create a template the object does not store (Master-only PyTorchJob shape)", func() {
			object := map[string]any{"spec": map[string]any{"pytorchReplicaSpecs": map[string]any{
				"Master": map[string]any{"replicas": float64(1), "template": map[string]any{"spec": map[string]any{
					"containers": []any{map[string]any{"name": "pytorch", "image": "train:v1"}},
				}}},
			}}}
			want := deepCopyMap(object)
			accessor := newAccessor(object)
			definition := v1alpha1.ComponentDefinition{
				Name:           "worker",
				SpecDefinition: &v1alpha1.SpecDefinition{PodTemplateSpecPath: ptr.To(".spec.pytorchReplicaSpecs.Worker.template")},
			}

			templates, err := accessor.ExtractPodTemplateSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			Expect(templates).To(HaveLen(1))
			Expect(accessor.UpdatePodTemplateSpec(ctx, definition, templates)).To(Succeed(), "an unchanged zero template is a no-op")

			templates[0].Spec.SchedulerName = "kai-scheduler"
			err = accessor.UpdatePodTemplateSpec(ctx, definition, templates)
			var refused *WriteError
			Expect(errors.As(err, &refused)).To(BeTrue(), "got: %v", err)
			Expect(err).To(MatchError(ErrFragmentAbsent))
			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		})

		DescribeTable("creates fields of a stored fragmented component and never the component",
			func(object string, definition v1alpha1.FragmentedPodSpecDefinition, edit func(*FragmentedPodSpec), wantErr error) {
				var decoded map[string]any
				Expect(json.Unmarshal([]byte(object), &decoded)).To(Succeed())
				want := deepCopyMap(decoded)
				accessor := newAccessor(decoded)
				component := v1alpha1.ComponentDefinition{
					Name:           "component",
					SpecDefinition: &v1alpha1.SpecDefinition{FragmentedPodSpecDefinition: &definition},
				}

				fragments, err := accessor.ExtractFragmentedPodSpec(ctx, component)
				Expect(err).NotTo(HaveOccurred())
				Expect(fragments).To(HaveLen(1))
				edit(&fragments[0])

				err = accessor.UpdateFragmentedPodSpec(ctx, component, fragments)
				got, getErr := accessor.GetObject()
				Expect(getErr).NotTo(HaveOccurred())
				if wantErr != nil {
					Expect(err).To(MatchError(wantErr))
					Expect(got).To(Equal(want))
					return
				}
				Expect(err).NotTo(HaveOccurred())
				readBack, err := accessor.ExtractFragmentedPodSpec(ctx, component)
				Expect(err).NotTo(HaveOccurred())
				Expect(readBack).To(Equal(fragments))
			},
			Entry("node affinity under an absent affinity map of a stored component (NIMService shape)",
				`{"spec":{"replicas":1,"resources":{"limits":{"cpu":"1"}}}}`,
				v1alpha1.FragmentedPodSpecDefinition{SchedulerNamePath: ptr.To(".spec.schedulerName"), ResourcesPath: ptr.To(".spec.resources"), NodeAffinityPath: ptr.To(".spec.affinity.nodeAffinity")},
				func(f *FragmentedPodSpec) {
					f.NodeAffinity = &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "gpu", Operator: corev1.NodeSelectorOpExists}}}},
					}}
				}, nil),
			Entry("a lone resources path whose parent is stored (Milvus etcd shape)",
				`{"spec":{"dependencies":{"etcd":{"inCluster":{"values":{"replicaCount":1}}}}}}`,
				v1alpha1.FragmentedPodSpecDefinition{ResourcesPath: ptr.To(".spec.dependencies.etcd.inCluster.values.resources")},
				func(f *FragmentedPodSpec) {
					f.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}
				}, nil),
			Entry("a lone resources path whose parent is absent (Milvus pulsar shape)",
				`{"spec":{"dependencies":{"pulsar":{"external":false}}}}`,
				v1alpha1.FragmentedPodSpecDefinition{ResourcesPath: ptr.To(".spec.dependencies.pulsar.inCluster.values.resources")},
				func(f *FragmentedPodSpec) {
					f.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}
				}, ErrFragmentAbsent),
			Entry("a component the object does not store (standalone Milvus proxy shape)",
				`{"spec":{"components":{"standalone":{"replicas":1}}}}`,
				v1alpha1.FragmentedPodSpecDefinition{SchedulerNamePath: ptr.To(".spec.components.proxy.schedulerName"), LabelsPath: ptr.To(".spec.components.proxy.podLabels"), ResourcesPath: ptr.To(".spec.components.proxy.resources")},
				func(f *FragmentedPodSpec) { f.SchedulerName = "kai-scheduler" }, ErrFragmentAbsent),
		)

		It("reports a fragment the definition has no path for as DefinitionNotFoundError", func() {
			accessor := newAccessor(map[string]any{"spec": map[string]any{"resources": map[string]any{}}})
			definition := v1alpha1.ComponentDefinition{
				Name: "nimcache",
				SpecDefinition: &v1alpha1.SpecDefinition{
					FragmentedPodSpecDefinition: &v1alpha1.FragmentedPodSpecDefinition{ResourcesPath: ptr.To(".spec.resources")},
				},
			}

			err := accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{{SchedulerName: "custom"}})
			Expect(isDefinitionNotFoundError(err)).To(BeTrue(), "got: %v", err)
		})
	})

	Context("computed paths (NIMCache shape)", func() {
		nimCacheObject := func() map[string]any {
			return map[string]any{"spec": map[string]any{
				"schedulerName": "default",
				"resources":     map[string]any{"cpu": "1", "memory": "2Gi"},
			}}
		}
		// The resources view is computed, so there is no location to write to.
		definition := v1alpha1.ComponentDefinition{
			Name: "nimcache",
			SpecDefinition: &v1alpha1.SpecDefinition{
				FragmentedPodSpecDefinition: &v1alpha1.FragmentedPodSpecDefinition{
					SchedulerNamePath: ptr.To(".spec.schedulerName"),
					ResourcesPath:     ptr.To("{requests: .spec.resources}"),
				},
			},
		}

		It("treats an unchanged computed value as a no-op next to a real change", func() {
			accessor := newAccessor(nimCacheObject())

			specs, err := accessor.ExtractFragmentedPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].SchedulerName = "custom"
			Expect(accessor.UpdateFragmentedPodSpec(ctx, definition, specs)).To(Succeed())

			want := nimCacheObject()
			want["spec"].(map[string]any)["schedulerName"] = "custom"
			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(want)))
		})

		It("refuses a changed computed value and commits nothing", func() {
			accessor := newAccessor(nimCacheObject())

			specs, err := accessor.ExtractFragmentedPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].SchedulerName = "custom"
			delete(specs[0].Resources.Requests, corev1.ResourceMemory)

			err = accessor.UpdateFragmentedPodSpec(ctx, definition, specs)
			var notWritable *execution.PathNotWritableError
			Expect(errors.As(err, &notWritable)).To(BeTrue(), "got: %v", err)
			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(nimCacheObject())))
		})
	})

	Context("overlapping fragment views (Dynamo mainContainer shape)", func() {
		definition := v1alpha1.ComponentDefinition{
			Name: "svc",
			SpecDefinition: &v1alpha1.SpecDefinition{
				FragmentedPodSpecDefinition: &v1alpha1.FragmentedPodSpecDefinition{
					ImagePath:     ptr.To(".spec.mainContainer.image"),
					ContainerPath: ptr.To(".spec.mainContainer"),
				},
			},
		}

		// The container view keeps its stale image unless the entry sets it.
		DescribeTable("combines compatible changes from both views",
			func(mutate func(*corev1.Container), wantContainer map[string]any) {
				accessor := newAccessor(map[string]any{"spec": map[string]any{"mainContainer": map[string]any{
					"name": "main", "image": "app:v1", "vendorExtension": "keep",
				}}})

				specs, err := accessor.ExtractFragmentedPodSpec(ctx, definition)
				Expect(err).NotTo(HaveOccurred())
				specs[0].Image = "app:v2"
				mutate(specs[0].Container)
				Expect(accessor.UpdateFragmentedPodSpec(ctx, definition, specs)).To(Succeed())

				got, err := accessor.GetObject()
				Expect(err).NotTo(HaveOccurred())
				Expect(asJSON(got)).To(MatchJSON(asJSON(map[string]any{"spec": map[string]any{"mainContainer": wantContainer}})))
			},
			Entry("the same image in both views",
				func(c *corev1.Container) { c.Image = "app:v2" },
				map[string]any{"name": "main", "image": "app:v2", "vendorExtension": "keep"}),
			Entry("an independent field in the container view",
				func(c *corev1.Container) { c.Args = []string{"serve"} },
				map[string]any{"name": "main", "image": "app:v2", "args": []any{"serve"}, "vendorExtension": "keep"}),
		)
	})

	Context("overlapping writes staged in one plan", func() {
		DescribeTable("combines a parent write with child writes in either order",
			func(childFirst bool) {
				accessor := newAccessor(map[string]any{"spec": map[string]any{
					"image": "app:v1", "args": []any{"old"}, "vendorExtension": "keep",
				}})
				paths := []string{".spec", ".spec.image", ".spec.command"}
				values := []any{
					map[string]any{"image": "app:v1", "args": []any{"new"}, "vendorExtension": "keep"},
					"app:v2",
					[]any{"serve"},
				}
				if childFirst {
					paths[0], paths[2] = paths[2], paths[0]
					values[0], values[2] = values[2], values[0]
				}

				plan := accessor.newWritePlan()
				for i, path := range paths {
					Expect(plan.stage(ctx, v1alpha1.ComponentDefinition{}, path, []any{values[i]}, nil)).To(Succeed())
				}
				Expect(plan.commit(ctx)).To(Succeed())

				got, err := accessor.GetObject()
				Expect(err).NotTo(HaveOccurred())
				Expect(asJSON(got)).To(MatchJSON(`{"spec":{"image":"app:v2","args":["new"],"command":["serve"],"vendorExtension":"keep"}}`))
			},
			Entry("parent first", false),
			Entry("children first", true),
		)

		It("refuses a parent list reorder that moves the element a child write addressed", func() {
			containersObject := func() map[string]any {
				return map[string]any{"spec": map[string]any{"containers": []any{
					map[string]any{"name": "a", "image": "app:v1"},
					map[string]any{"name": "b", "image": "app:v1"},
				}}}
			}
			accessor := newAccessor(containersObject())

			plan := accessor.newWritePlan()
			Expect(plan.stage(ctx, v1alpha1.ComponentDefinition{}, ".spec.containers[0].image", []any{"app:v2"}, nil)).To(Succeed())
			reordered := []any{
				map[string]any{"name": "b", "image": "app:v1"},
				map[string]any{"name": "a", "image": "app:v1"},
			}
			Expect(plan.stage(ctx, v1alpha1.ComponentDefinition{}, ".spec.containers", []any{reordered}, nil)).To(MatchError(ErrConflictingWrites))

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(containersObject())))
		})
	})

	Context("components with instance ids", func() {
		servicesObject := func() map[string]any {
			return map[string]any{"services": []any{
				map[string]any{
					"name": "a", "schedulerName": "first",
					"containers": []any{map[string]any{"name": "main", "image": "app:v1"}},
				},
				map[string]any{
					"name": "b", "schedulerName": "keep", "labels": map[string]any{"app": "keep"},
					"containers": []any{map[string]any{"name": "main", "image": "app:v1"}},
				},
			}}
		}
		definition := v1alpha1.ComponentDefinition{
			Name:           "service",
			InstanceIdPath: ptr.To(".services[].name"),
			SpecDefinition: &v1alpha1.SpecDefinition{
				FragmentedPodSpecDefinition: &v1alpha1.FragmentedPodSpecDefinition{
					SchedulerNamePath: ptr.To(".services[].schedulerName"),
					LabelsPath:        ptr.To(".services[].labels"),
					ContainersPath:    ptr.To(".services[].containers"),
				},
			},
		}

		It("writes the instance that changed and leaves the others untouched", func() {
			accessor := newAccessor(servicesObject())

			Expect(accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{
				{SchedulerName: "custom", Labels: map[string]string{"app": "new"}},
				{},
			})).To(Succeed())

			want := servicesObject()
			first := want["services"].([]any)[0].(map[string]any)
			first["schedulerName"] = "custom"
			first["labels"] = map[string]any{"app": "new"}
			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(want)))
		})

		It("leaves the object byte-identical when nothing changed", func() {
			accessor := newAccessor(servicesObject())

			specs, err := accessor.ExtractFragmentedPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			Expect(accessor.UpdateFragmentedPodSpec(ctx, definition, specs)).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(servicesObject())))
		})

		It("commits nothing when one instance's fragment is refused", func() {
			accessor := newAccessor(servicesObject())

			err := accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{
				{SchedulerName: "custom"},
				{Labels: map[string]string{"$patch": "delete"}},
			})
			var refused *WriteError
			Expect(errors.As(err, &refused)).To(BeTrue(), "got: %v", err)
			Expect(err).To(MatchError(ErrDirectiveKey))

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(servicesObject())))
		})

		It("refuses fewer values than instances", func() {
			accessor := newAccessor(servicesObject())

			err := accessor.UpdateFragmentedPodSpec(ctx, definition, []FragmentedPodSpec{{SchedulerName: "custom"}})
			var mismatch *InstanceCountMismatchError
			Expect(errors.As(err, &mismatch)).To(BeTrue(), "got: %v", err)

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(servicesObject())))
		})

		It("leaves a multi-instance pod template byte-identical when nothing changed", func() {
			jobSetObject := func() map[string]any {
				job := func(name, image string) map[string]any {
					return map[string]any{"name": name, "template": map[string]any{"spec": map[string]any{"template": map[string]any{
						"spec": map[string]any{"containers": []any{map[string]any{"name": "main", "image": image}}},
					}}}}
				}
				return map[string]any{"spec": map[string]any{"replicatedJobs": []any{job("a", "app:v1"), job("b", "app:v2")}}}
			}
			templateDefinition := v1alpha1.ComponentDefinition{
				Name:           "replicated-job",
				InstanceIdPath: ptr.To(".spec.replicatedJobs[].name"),
				SpecDefinition: &v1alpha1.SpecDefinition{
					PodTemplateSpecPath: ptr.To(".spec.replicatedJobs[].template.spec.template"),
				},
			}
			accessor := newAccessor(jobSetObject())

			templates, err := accessor.ExtractPodTemplateSpec(ctx, templateDefinition)
			Expect(err).NotTo(HaveOccurred())
			Expect(accessor.UpdatePodTemplateSpec(ctx, templateDefinition, templates)).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			Expect(asJSON(got)).To(MatchJSON(asJSON(jobSetObject())))
		})
	})

	Context("combineWrites", func() {
		DescribeTable("keeps each side's changes against the baseline",
			func(baseline, first, second, want map[string]any) {
				got, err := combineWrites(writeValue{baseline, true}, writeValue{first, true}, writeValue{second, true})
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(Equal(writeValue{value: want, present: true}))
			},
			Entry("one side deletes a key the other leaves alone",
				map[string]any{"a": "old", "b": "old"}, map[string]any{"b": "old"}, map[string]any{"a": "old", "b": "new"},
				map[string]any{"b": "new"}),
			Entry("one side sets null while the other edits a sibling",
				map[string]any{"a": "old", "b": "old"}, map[string]any{"a": nil, "b": "old"}, map[string]any{"a": "old", "b": "new"},
				map[string]any{"a": nil, "b": "new"}),
			Entry("both sides add different keys",
				map[string]any(nil), map[string]any{"a": "new"}, map[string]any{"b": "new"},
				map[string]any{"a": "new", "b": "new"}),
		)

		It("refuses a delete on one side and a null on the other", func() {
			_, err := combineWrites(
				writeValue{map[string]any{"a": "old"}, true},
				writeValue{map[string]any{}, true},
				writeValue{map[string]any{"a": nil}, true},
			)
			Expect(err).To(HaveOccurred())
		})
	})

	Context("atomic lists", func() {
		It("replaces tolerations whole like Kubernetes does, clearing unknown fields inside their entries", func() {
			accessor := newAccessor(map[string]any{"spec": map[string]any{"template": map[string]any{
				"containers":  []any{map[string]any{"name": "main", "image": "app:v1"}},
				"tolerations": []any{map[string]any{"key": "gpu", "operator": "Exists", "vendorExtension": "lost"}},
			}}})
			definition := v1alpha1.ComponentDefinition{
				Name:           "svc",
				SpecDefinition: &v1alpha1.SpecDefinition{PodSpecPath: ptr.To(".spec.template")},
			}

			specs, err := accessor.ExtractPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].Tolerations = append(specs[0].Tolerations, corev1.Toleration{Key: "batch", Operator: corev1.TolerationOpExists})
			Expect(accessor.UpdatePodSpec(ctx, definition, specs)).To(Succeed())

			got, err := accessor.GetObject()
			Expect(err).NotTo(HaveOccurred())
			tolerations := got["spec"].(map[string]any)["template"].(map[string]any)["tolerations"].([]any)
			Expect(tolerations).To(HaveLen(2))
			Expect(tolerations[0]).NotTo(HaveKey("vendorExtension"), "tolerations carry no merge key, so a change replaces every entry")
		})
	})
})
