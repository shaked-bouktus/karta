// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	kartav1alpha1 "github.com/dsx-ai-factory/workload-map/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/pkg/jq/execution"
	"github.com/dsx-ai-factory/workload-map/pkg/resource"
	"github.com/dsx-ai-factory/workload-map/test/e2e/recorder"
)

// unknownKey marks fields the typed projection does not know. A platform that stores a
// workload with a newer Kubernetes than Karta's k8s.io/api sees exactly this shape.
const unknownKey = "x-karta-unknown"

// templateEdit is one realistic change a scheduler or platform controller makes to a pod
// template. It reports whether the template had anything to change and how many list
// entries legitimately lose their unknown fields: removed entries, and the definitions
// of an env name whose count the edit changes (Karta writes those as an Update would).
type templateEdit struct {
	name  string
	apply func(t *corev1.PodTemplateSpec) (applied bool, lossyEntries int)
}

// fragmentEdit is the same for definitions that scatter the pod spec over several paths.
// field names the definition path the edit needs; without it Karta must refuse the write.
type fragmentEdit struct {
	name  string
	field func(def *kartav1alpha1.FragmentedPodSpecDefinition) *string
	apply func(f *resource.FragmentedPodSpec)
}

var gpu = apiresource.MustParse("1")

var templateEdits = []templateEdit{
	{"no-op write", func(t *corev1.PodTemplateSpec) (bool, int) { return true, 0 }},
	{"image tag bump on every container", func(t *corev1.PodTemplateSpec) (bool, int) {
		for i := range t.Spec.Containers {
			t.Spec.Containers[i].Image += "-karta"
		}
		return len(t.Spec.Containers) > 0, 0
	}},
	{"env var added to every container", func(t *corev1.PodTemplateSpec) (bool, int) {
		for i := range t.Spec.Containers {
			t.Spec.Containers[i].Env = append(t.Spec.Containers[i].Env, corev1.EnvVar{Name: "KARTA_TEST", Value: "1"})
		}
		return len(t.Spec.Containers) > 0, 0
	}},
	{"duplicate env var appended (X=$(X):more)", func(t *corev1.PodTemplateSpec) (bool, int) {
		lossy := 0
		for i := range t.Spec.Containers {
			if env := t.Spec.Containers[i].Env; len(env) > 0 {
				name := env[0].Name
				t.Spec.Containers[i].Env = append(env, corev1.EnvVar{Name: name, Value: "$(" + name + "):more"})
				lossy++ // the stored definition of that name is written as supplied
			}
		}
		return lossy > 0, lossy
	}},
	{"first env var removed", func(t *corev1.PodTemplateSpec) (bool, int) {
		removed := 0
		for i := range t.Spec.Containers {
			if env := t.Spec.Containers[i].Env; len(env) > 0 {
				t.Spec.Containers[i].Env = env[1:]
				removed++
			}
		}
		return removed > 0, removed
	}},
	{"gpu limit on the first container", func(t *corev1.PodTemplateSpec) (bool, int) {
		if len(t.Spec.Containers) == 0 {
			return false, 0
		}
		c := &t.Spec.Containers[0]
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		c.Resources.Limits["nvidia.com/gpu"] = gpu
		c.Resources.Requests["nvidia.com/gpu"] = gpu
		return true, 0
	}},
	{"cpu request changed", func(t *corev1.PodTemplateSpec) (bool, int) {
		if len(t.Spec.Containers) == 0 {
			return false, 0
		}
		c := &t.Spec.Containers[0]
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		c.Resources.Requests[corev1.ResourceCPU] = apiresource.MustParse("123m")
		return true, 0
	}},
	{"node selector", func(t *corev1.PodTemplateSpec) (bool, int) {
		if t.Spec.NodeSelector == nil {
			t.Spec.NodeSelector = map[string]string{}
		}
		t.Spec.NodeSelector["karta.test/pool"] = "a100"
		return true, 0
	}},
	{"toleration added (atomic list)", func(t *corev1.PodTemplateSpec) (bool, int) {
		t.Spec.Tolerations = append(t.Spec.Tolerations, corev1.Toleration{
			Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		})
		return true, 0
	}},
	{"scheduler name", func(t *corev1.PodTemplateSpec) (bool, int) {
		t.Spec.SchedulerName = "kai-scheduler"
		return true, 0
	}},
	{"pod label and annotation", func(t *corev1.PodTemplateSpec) (bool, int) {
		if t.Labels == nil {
			t.Labels = map[string]string{}
		}
		if t.Annotations == nil {
			t.Annotations = map[string]string{}
		}
		t.Labels["karta.test/managed"] = "true"
		t.Annotations["karta.test/queue"] = "team-a"
		return true, 0
	}},
	{"init container added", func(t *corev1.PodTemplateSpec) (bool, int) {
		t.Spec.InitContainers = append(t.Spec.InitContainers, corev1.Container{
			Name: "karta-init", Image: "busybox:1.36", Command: []string{"sh", "-c", "true"},
		})
		return true, 0
	}},
	{"volume and mount added", func(t *corev1.PodTemplateSpec) (bool, int) {
		if len(t.Spec.Containers) == 0 {
			return false, 0
		}
		t.Spec.Volumes = append(t.Spec.Volumes, corev1.Volume{
			Name: "karta-scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		c := &t.Spec.Containers[0]
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "karta-scratch", MountPath: "/scratch"})
		return true, 0
	}},
	{"priority class", func(t *corev1.PodTemplateSpec) (bool, int) {
		t.Spec.PriorityClassName = "high"
		return true, 0
	}},
	{"required node affinity", func(t *corev1.PodTemplateSpec) (bool, int) {
		if t.Spec.Affinity == nil {
			t.Spec.Affinity = &corev1.Affinity{}
		}
		t.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "nvidia.com/gpu.product", Operator: corev1.NodeSelectorOpIn, Values: []string{"A100"},
					}},
				}},
			},
		}
		return true, 0
	}},
	{"container ports 9400/TCP and 9400/UDP", func(t *corev1.PodTemplateSpec) (bool, int) {
		if len(t.Spec.Containers) == 0 {
			return false, 0
		}
		c := &t.Spec.Containers[0]
		c.Ports = append(c.Ports,
			corev1.ContainerPort{Name: "metrics", ContainerPort: 9400, Protocol: corev1.ProtocolTCP},
			corev1.ContainerPort{Name: "metrics-udp", ContainerPort: 9400, Protocol: corev1.ProtocolUDP},
		)
		return true, 0
	}},
	{"topology spread constraint", func(t *corev1.PodTemplateSpec) (bool, int) {
		t.Spec.TopologySpreadConstraints = append(t.Spec.TopologySpreadConstraints, corev1.TopologySpreadConstraint{
			MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.ScheduleAnyway,
		})
		return true, 0
	}},
	{"service account", func(t *corev1.PodTemplateSpec) (bool, int) {
		t.Spec.ServiceAccountName = "karta-workload"
		return true, 0
	}},
	{"command and args replaced", func(t *corev1.PodTemplateSpec) (bool, int) {
		if len(t.Spec.Containers) == 0 {
			return false, 0
		}
		t.Spec.Containers[0].Command = []string{"sh", "-c"}
		t.Spec.Containers[0].Args = []string{"sleep 3600"}
		return true, 0
	}},
	{"combined controller write (label, image, gpu, node selector)", func(t *corev1.PodTemplateSpec) (bool, int) {
		if len(t.Spec.Containers) == 0 {
			return false, 0
		}
		if t.Labels == nil {
			t.Labels = map[string]string{}
		}
		t.Labels["karta.test/managed"] = "true"
		for i := range t.Spec.Containers {
			t.Spec.Containers[i].Image += "-karta"
		}
		c := &t.Spec.Containers[0]
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		c.Resources.Limits["nvidia.com/gpu"] = gpu
		if t.Spec.NodeSelector == nil {
			t.Spec.NodeSelector = map[string]string{}
		}
		t.Spec.NodeSelector["karta.test/pool"] = "a100"
		return true, 0
	}},
}

var fragmentEdits = []fragmentEdit{
	{"no-op write", nil, func(f *resource.FragmentedPodSpec) {}},
	{"scheduler name", func(d *kartav1alpha1.FragmentedPodSpecDefinition) *string { return d.SchedulerNamePath },
		func(f *resource.FragmentedPodSpec) { f.SchedulerName = "kai-scheduler" }},
	{"label added", func(d *kartav1alpha1.FragmentedPodSpecDefinition) *string { return d.LabelsPath },
		func(f *resource.FragmentedPodSpec) {
			if f.Labels == nil {
				f.Labels = map[string]string{}
			}
			f.Labels["karta.test/managed"] = "true"
		}},
	{"annotation added", func(d *kartav1alpha1.FragmentedPodSpecDefinition) *string { return d.AnnotationsPath },
		func(f *resource.FragmentedPodSpec) {
			if f.Annotations == nil {
				f.Annotations = map[string]string{}
			}
			f.Annotations["karta.test/queue"] = "team-a"
		}},
	{"gpu limit", func(d *kartav1alpha1.FragmentedPodSpecDefinition) *string { return d.ResourcesPath },
		func(f *resource.FragmentedPodSpec) {
			if f.Resources == nil {
				f.Resources = &corev1.ResourceRequirements{}
			}
			if f.Resources.Limits == nil {
				f.Resources.Limits = corev1.ResourceList{}
			}
			f.Resources.Limits["nvidia.com/gpu"] = gpu
		}},
	{"priority class", func(d *kartav1alpha1.FragmentedPodSpecDefinition) *string { return d.PriorityClassNamePath },
		func(f *resource.FragmentedPodSpec) { f.PriorityClassName = "high" }},
	{"required node affinity", func(d *kartav1alpha1.FragmentedPodSpecDefinition) *string { return d.NodeAffinityPath },
		func(f *resource.FragmentedPodSpec) {
			f.NodeAffinity = &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key: "nvidia.com/gpu.product", Operator: corev1.NodeSelectorOpIn, Values: []string{"A100"},
						}},
					}},
				},
			}
		}},
	{"image tag bump on every container", func(d *kartav1alpha1.FragmentedPodSpecDefinition) *string { return d.ContainersPath },
		func(f *resource.FragmentedPodSpec) {
			for i := range f.Containers {
				f.Containers[i].Image += "-karta"
			}
		}},
}

// Every recorded frame is a real object a live cluster produced, with its server defaults
// and status. Each edit is written through Karta on a pristine copy and on a copy whose
// pod fragments carry fields the typed projection does not know. The raw object must change
// only inside the written fragments, keep every unknown field, read back what was written,
// and still read the recorded state afterwards.
var _ = Describe("Karta writes back to the recorded objects", func() {
	recordings, _ := filepath.Glob(recordedGlob)
	if len(recordings) == 0 {
		return // the read suite already fails on missing recordings
	}

	for _, path := range recordings {
		path := path
		name := strings.TrimPrefix(path, "../recorded_data/")
		for _, injectUnknown := range []bool{false, true} {
			variant := "pristine"
			if injectUnknown {
				variant = "with unknown fields"
			}
			Context(name+" ("+variant+")", func() {
				for _, edit := range templateEdits {
					edit := edit
					It(edit.name, func(ctx SpecContext) {
						replayWrites(ctx, path, injectUnknown, edit, fragmentEdit{})
					})
				}
				for _, edit := range fragmentEdits {
					edit := edit
					It("fragment: "+edit.name, func(ctx SpecContext) {
						replayWrites(ctx, path, injectUnknown, templateEdit{}, edit)
					})
				}
			})
		}
	}
})

// A component whose fragment the stored object lacks (a Master-only PyTorchJob has no Worker,
// a standalone Milvus has no queryNode) still reads as one zero-valued instance. Writing a
// changed value to it must not create the fragment: that adds a replica spec or a component
// nobody asked for, with an empty pod template the operator then acts on.
var _ = Describe("Karta refuses to create a fragment the object does not have", func() {
	recordings, _ := filepath.Glob(recordedGlob)
	for _, path := range recordings {
		path := path
		It(strings.TrimPrefix(path, "../recorded_data/"), func(ctx SpecContext) {
			r, karta := openRecording(path)
			applicable := false
			for r.Next() {
				state := r.State()
				before := r.Object().DeepCopy()
				locations := fragmentLocations(ctx, karta, before)
				factory := resource.NewComponentFactoryFromObject(karta, before.DeepCopy())
				components, err := factory.GetChildComponents()
				Expect(err).NotTo(HaveOccurred())
				root, err := factory.GetRootComponent()
				Expect(err).NotTo(HaveOccurred())
				for _, comp := range append(components, root) {
					if !comp.HasPodDefinition() || len(locations[comp.Name()].present) > 0 {
						continue
					}
					def := comp.Definition().SpecDefinition
					var writeErr error
					switch {
					case def.PodTemplateSpecPath != nil:
						got, err := comp.GetPodTemplateSpec(ctx)
						Expect(err).NotTo(HaveOccurred())
						if len(got) == 0 {
							continue
						}
						for id, t := range got {
							t.Spec.SchedulerName = "kai-scheduler"
							got[id] = t
						}
						applicable = true
						writeErr = comp.UpdatePodTemplateSpec(ctx, got)
					case def.FragmentedPodSpecDefinition != nil && def.FragmentedPodSpecDefinition.SchedulerNamePath != nil:
						got, err := comp.GetFragmentedPodSpec(ctx)
						Expect(err).NotTo(HaveOccurred())
						if len(got) == 0 {
							continue
						}
						for id, f := range got {
							f.SchedulerName = "kai-scheduler"
							got[id] = f
						}
						applicable = true
						writeErr = comp.UpdateFragmentedPodSpec(ctx, got)
					default:
						continue
					}
					after, err := factory.GetResource()
					Expect(err).NotTo(HaveOccurred())
					Expect(writeErr).To(MatchError(resource.ErrFragmentAbsent),
						"%s: %s has no stored fragment at %v, yet the write succeeded and created it:\n%s",
						state, comp.Name(), locations[comp.Name()].all, toJSON(after.(*unstructured.Unstructured).Object["spec"]))
					Expect(toJSON(after.(*unstructured.Unstructured).Object)).To(MatchJSON(toJSON(before.Object)), state)
				}
			}
			if !applicable {
				Skip("every fragment of this recording is present")
			}
		})
	}
})

func openRecording(path string) (*recorder.Reader, *kartav1alpha1.Karta) {
	r, err := recorder.OpenRecording(path)
	Expect(err).NotTo(HaveOccurred())
	kartaYAML, err := os.ReadFile(filepath.Join(repoRoot, "docs", "catalog", filepath.Base(r.Recording().KartaFile)))
	Expect(err).NotTo(HaveOccurred())
	karta := &kartav1alpha1.Karta{}
	Expect(yaml.Unmarshal(kartaYAML, karta)).To(Succeed())
	return r, karta
}

func replayWrites(ctx context.Context, path string, injectUnknown bool, tEdit templateEdit, fEdit fragmentEdit) {
	r, karta := openRecording(path)

	applicable := false
	for r.Next() {
		state := r.State()
		before := r.Object().DeepCopy()
		locations := fragmentLocations(ctx, karta, before)
		if injectUnknown {
			for _, componentLocations := range locations {
				for _, loc := range componentLocations.present {
					injectUnknownFields(before.Object, loc)
				}
			}
		}
		beforeJSON := toJSON(before.Object)

		factory := resource.NewComponentFactoryFromObject(karta, before.DeepCopy())
		children, err := factory.GetChildComponents()
		Expect(err).NotTo(HaveOccurred())
		root, err := factory.GetRootComponent()
		Expect(err).NotTo(HaveOccurred())

		var written []*resource.Component
		removedEntries := 0
		refused := false
		for _, comp := range append(children, root) {
			// Components whose fragment the object lacks are covered by the spec above.
			if !comp.HasPodDefinition() || len(locations[comp.Name()].present) == 0 {
				continue
			}
			def := comp.Definition().SpecDefinition
			switch {
			case def.PodTemplateSpecPath != nil && tEdit.apply != nil:
				got, err := comp.GetPodTemplateSpec(ctx)
				Expect(err).NotTo(HaveOccurred(), "%s: component %s", state, comp.Name())
				if len(got) == 0 {
					continue
				}
				updates := map[string]corev1.PodTemplateSpec{}
				compApplied := false
				for id, t := range got {
					tt := *t.DeepCopy()
					ok, removed := tEdit.apply(&tt)
					compApplied = compApplied || ok
					removedEntries += removed
					updates[id] = tt
				}
				if !compApplied {
					continue
				}
				applicable = true
				err = comp.UpdatePodTemplateSpec(ctx, updates)
				Expect(err).NotTo(HaveOccurred(), "%s: write to %s", state, comp.Name())
				written = append(written, comp)
				readBack, err := comp.GetPodTemplateSpec(ctx)
				Expect(err).NotTo(HaveOccurred())
				for id, want := range updates {
					Expect(equality.Semantic.DeepEqual(readBack[id], want)).To(BeTrue(),
						"%s: %s[%s] reads back differently:\n%s\nwant:\n%s", state, comp.Name(), id, toJSON(readBack[id]), toJSON(want))
				}
			case def.FragmentedPodSpecDefinition != nil && fEdit.apply != nil:
				got, err := comp.GetFragmentedPodSpec(ctx)
				Expect(err).NotTo(HaveOccurred(), "%s: component %s", state, comp.Name())
				if len(got) == 0 {
					continue
				}
				updates := map[string]resource.FragmentedPodSpec{}
				compApplied := fEdit.field == nil // the no-op edit is always worth writing
				for id, f := range got {
					ff := deepCopyFragment(f)
					fEdit.apply(&ff)
					compApplied = compApplied || !equality.Semantic.DeepEqual(ff, f)
					updates[id] = ff
				}
				if !compApplied {
					continue
				}
				applicable = true
				err = comp.UpdateFragmentedPodSpec(ctx, updates)
				if fEdit.field != nil && fEdit.field(def.FragmentedPodSpecDefinition) == nil {
					var notFound resource.DefinitionNotFoundError
					Expect(errors.As(err, &notFound)).To(BeTrue(),
						"%s: %s has no path for this field, want DefinitionNotFoundError, got: %v", state, comp.Name(), err)
					refused = true
					continue
				}
				Expect(err).NotTo(HaveOccurred(), "%s: write to %s", state, comp.Name())
				written = append(written, comp)
				readBack, err := comp.GetFragmentedPodSpec(ctx)
				Expect(err).NotTo(HaveOccurred())
				for id, want := range updates {
					Expect(equality.Semantic.DeepEqual(readBack[id], want)).To(BeTrue(),
						"%s: %s[%s] reads back differently:\n%s\nwant:\n%s", state, comp.Name(), id, toJSON(readBack[id]), toJSON(want))
				}
			}
		}
		if len(written) == 0 && !refused {
			continue
		}

		afterObject, err := factory.GetResource()
		Expect(err).NotTo(HaveOccurred())
		after := afterObject.(*unstructured.Unstructured)
		afterJSON := toJSON(after.Object)

		isNoop := tEdit.name == "no-op write" || fEdit.name == "no-op write"
		if isNoop || (refused && len(written) == 0) {
			Expect(afterJSON).To(MatchJSON(beforeJSON), "%s: a no-op or refused write changed the object", state)
			continue
		}
		Expect(afterJSON).NotTo(MatchJSON(beforeJSON), "%s: the write changed nothing", state)

		// Status and identity never move, whatever the fragment path is.
		Expect(toJSON(after.Object["status"])).To(MatchJSON(toJSON(before.Object["status"])), "%s: status changed", state)
		for _, field := range []string{"apiVersion", "kind"} {
			Expect(after.Object[field]).To(Equal(before.Object[field]), state)
		}
		Expect(after.GetName()).To(Equal(before.GetName()), state)
		Expect(after.GetUID()).To(Equal(before.GetUID()), state)
		Expect(after.GetNamespace()).To(Equal(before.GetNamespace()), state)
		Expect(after.GetResourceVersion()).To(Equal(before.GetResourceVersion()), state)

		// Every change sits inside a fragment the written components own.
		var allowed [][]any
		for _, comp := range written {
			allowed = append(allowed, locations[comp.Name()].all...)
		}
		for _, changed := range changedPaths(nil, before.Object, after.Object) {
			Expect(withinFragments(changed, before.Object, after.Object, allowed)).To(BeTrue(),
				"%s: change at %v is outside the written fragments %v", state, changed, allowed)
		}

		// Unknown fields survive, except the ones on entries the edit removed.
		if injectUnknown {
			beforeCount := strings.Count(beforeJSON, `"`+unknownKey+`"`)
			afterCount := strings.Count(afterJSON, `"`+unknownKey+`"`)
			Expect(afterCount).To(BeNumerically(">=", beforeCount-removedEntries),
				"%s: unknown fields were lost (%d before, %d after, %d entries removed)", state, beforeCount, afterCount, removedEntries)
			Expect(afterCount).To(BeNumerically("<=", beforeCount), state)
		}

		// The written object is a fixed point: a second identical write is a no-op, and
		// Karta still reads the recorded state from it.
		again := resource.NewComponentFactoryFromObject(karta, after.DeepCopy())
		rootAgain, err := again.GetRootComponent()
		Expect(err).NotTo(HaveOccurred())
		status, err := rootAgain.GetStatus(ctx)
		Expect(err).NotTo(HaveOccurred(), "%s: status unreadable after the write", state)
		Expect(status.MatchedStatuses).To(ContainElement(kartav1alpha1.ResourceStatus(state)), "%s: state lost after the write", state)
		for _, comp := range written {
			compAgain, err := again.GetComponent(comp.Name())
			Expect(err).NotTo(HaveOccurred())
			if comp.Definition().SpecDefinition.PodTemplateSpecPath != nil {
				got, err := compAgain.GetPodTemplateSpec(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(compAgain.UpdatePodTemplateSpec(ctx, got)).To(Succeed(), state)
			} else {
				got, err := compAgain.GetFragmentedPodSpec(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(compAgain.UpdateFragmentedPodSpec(ctx, got)).To(Succeed(), state)
			}
		}
		fixedPoint, err := again.GetResource()
		Expect(err).NotTo(HaveOccurred())
		Expect(toJSON(fixedPoint.(*unstructured.Unstructured).Object)).To(MatchJSON(afterJSON), "%s: rewriting the result changed it", state)
	}
	if !applicable {
		Skip("nothing in this recording to apply the edit to")
	}
}

// componentLocations are the raw locations a component's spec definition paths address in
// one object: all of them, and the subset that holds a value there.
type componentLocations struct {
	all     [][]any
	present [][]any
}

// fragmentLocations resolves the locations per component, so the suite can tell a fragment
// the object has from one it lacks, and check that a write stays inside the fragments.
func fragmentLocations(ctx context.Context, karta *kartav1alpha1.Karta, object *unstructured.Unstructured) map[string]componentLocations {
	runner := execution.NewDefaultRunner(object.DeepCopy())
	resolve := func(expression *string, into *componentLocations) {
		if expression == nil {
			return
		}
		results, err := runner.Evaluate(ctx, "[path("+*expression+") as $p | [$p, getpath($p) != null]]")
		if err != nil {
			return // a formula path has no location; writes through it are refused elsewhere
		}
		for _, result := range results {
			pairs, _ := result.([]any)
			for _, pair := range pairs {
				entry, _ := pair.([]any)
				location, _ := entry[0].([]any)
				into.all = append(into.all, location)
				if present, _ := entry[1].(bool); present {
					into.present = append(into.present, location)
				}
			}
		}
	}
	out := map[string]componentLocations{}
	components := append([]kartav1alpha1.ComponentDefinition{karta.Spec.StructureDefinition.RootComponent}, karta.Spec.StructureDefinition.ChildComponents...)
	for _, def := range components {
		if def.SpecDefinition == nil {
			continue
		}
		var locations componentLocations
		resolve(def.SpecDefinition.PodTemplateSpecPath, &locations)
		resolve(def.SpecDefinition.PodSpecPath, &locations)
		resolve(def.SpecDefinition.MetadataPath, &locations)
		if f := def.SpecDefinition.FragmentedPodSpecDefinition; f != nil {
			for _, p := range []*string{f.SchedulerNamePath, f.LabelsPath, f.AnnotationsPath, f.ResourcesPath, f.ResourceClaimsPath,
				f.PodAffinityPath, f.NodeAffinityPath, f.ContainersPath, f.ContainerPath, f.PriorityClassNamePath, f.ImagePath} {
				resolve(p, &locations)
			}
		}
		out[def.Name] = locations
	}
	return out
}

// injectUnknownFields plants unknownKey on every merge-keyed map under the location: the
// fragment itself, template metadata and spec, containers, their env and ports, volumes.
// Atomic lists are left alone, since replacing them whole drops their entries by design.
func injectUnknownFields(object map[string]any, location []any) {
	switch fragment := valueAt(object, location).(type) {
	case map[string]any:
		fragment[unknownKey] = "keep"
		for _, key := range []string{"metadata", "spec"} {
			if m, ok := fragment[key].(map[string]any); ok {
				m[unknownKey] = "keep"
			}
		}
		spec, _ := fragment["spec"].(map[string]any)
		if spec == nil {
			spec = fragment // a bare pod spec or a container list parent
		}
		markContainers(spec)
		if volumes, ok := spec["volumes"].([]any); ok {
			for _, v := range volumes {
				if m, ok := v.(map[string]any); ok {
					m[unknownKey] = "keep"
				}
			}
		}
	case []any:
		markContainerList(fragment)
	}
}

func markContainers(spec map[string]any) {
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		if list, ok := spec[field].([]any); ok {
			markContainerList(list)
		}
	}
}

func markContainerList(list []any) {
	for _, raw := range list {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		container[unknownKey] = "keep"
		for _, field := range []string{"env", "ports", "volumeMounts"} {
			if entries, ok := container[field].([]any); ok {
				for _, e := range entries {
					if m, ok := e.(map[string]any); ok {
						m[unknownKey] = "keep"
					}
				}
			}
		}
	}
}

// changedPaths lists every leaf location where before and after differ.
func changedPaths(prefix []any, before, after any) [][]any {
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			return [][]any{append([]any{}, prefix...)}
		}
		var out [][]any
		for key := range b {
			out = append(out, changedPaths(append(append([]any{}, prefix...), key), b[key], a[key])...)
		}
		for key := range a {
			if _, seen := b[key]; !seen {
				out = append(out, append(append([]any{}, prefix...), key))
			}
		}
		return out
	case []any:
		a, ok := after.([]any)
		if !ok {
			return [][]any{append([]any{}, prefix...)}
		}
		var out [][]any
		for i := range b {
			if i >= len(a) {
				out = append(out, append(append([]any{}, prefix...), i))
				continue
			}
			out = append(out, changedPaths(append(append([]any{}, prefix...), i), b[i], a[i])...)
		}
		for i := len(b); i < len(a); i++ {
			out = append(out, append(append([]any{}, prefix...), i))
		}
		return out
	default:
		if fmt.Sprint(before) != fmt.Sprint(after) {
			return [][]any{append([]any{}, prefix...)}
		}
		return nil
	}
}

// withinFragments reports whether a changed location lies inside an allowed fragment. A write
// that creates a missing parent (affinity, when only nodeAffinity is addressed) shows up as a
// change at the parent; then everything the write created under it must lie inside a fragment.
func withinFragments(changed []any, before, after map[string]any, allowed [][]any) bool {
	if hasAllowedPrefix(changed, allowed) {
		return true
	}
	if valueAt(before, changed) != nil {
		return false
	}
	created := valueAt(after, changed)
	var empty any
	switch created.(type) {
	case map[string]any:
		empty = map[string]any{}
	case []any:
		empty = []any{}
	default:
		return false
	}
	for _, leaf := range changedPaths(changed, empty, created) {
		if !hasAllowedPrefix(leaf, allowed) {
			return false
		}
	}
	return true
}

func valueAt(object any, location []any) any {
	value := object
	for _, segment := range location {
		switch s := normalizeIndex(segment).(type) {
		case string:
			m, ok := value.(map[string]any)
			if !ok {
				return nil
			}
			value = m[s]
		case int:
			l, ok := value.([]any)
			if !ok || s < 0 || s >= len(l) {
				return nil
			}
			value = l[s]
		default:
			return nil
		}
	}
	return value
}

func hasAllowedPrefix(changed []any, allowed [][]any) bool {
	for _, location := range allowed {
		if len(location) > len(changed) {
			continue
		}
		match := true
		for i, segment := range location {
			if fmt.Sprint(normalizeIndex(segment)) != fmt.Sprint(normalizeIndex(changed[i])) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func normalizeIndex(segment any) any {
	if f, ok := segment.(float64); ok {
		return int(f)
	}
	return segment
}

func deepCopyFragment(f resource.FragmentedPodSpec) resource.FragmentedPodSpec {
	raw, err := json.Marshal(f)
	Expect(err).NotTo(HaveOccurred())
	var out resource.FragmentedPodSpec
	Expect(json.Unmarshal(raw, &out)).To(Succeed())
	return out
}

func toJSON(value any) string {
	if value == nil {
		return "null"
	}
	raw, err := json.Marshal(value)
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}
