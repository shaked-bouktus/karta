// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package resource

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/workload-map/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/pkg/jq/execution"
)

var _ = Describe("write-back of lists keyed by a composite identity", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	decode := func(raw string) map[string]any {
		var object map[string]any
		Expect(json.Unmarshal([]byte(raw), &object)).To(Succeed())
		return object
	}

	expectObject := func(accessor *Accessor, want map[string]any) {
		got, err := accessor.GetObject()
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want))
	}

	podSpecDefinition := v1alpha1.ComponentDefinition{
		Name:           "pod",
		SpecDefinition: &v1alpha1.SpecDefinition{PodSpecPath: ptr.To(".spec")},
	}

	changes := []string{"noop", "edit", "insert", "delete", "reorder", "clear"}

	var portEntries []TableEntry
	for _, field := range []string{"containers", "initContainers", "ephemeralContainers"} {
		for _, change := range changes {
			portEntries = append(portEntries, Entry(field+" "+change, field, change))
		}
	}

	// Ports 53/TCP and 53/UDP share containerPort, the only strategic merge key.
	DescribeTable("keeps each port entry's identity and unknown fields",
		func(field, change string) {
			object := decode(`{"metadata":{"name":"sample"},"spec":{"containers":[{"name":"main","image":"app:v1","vendorExtension":"container","ports":[{"containerPort":53,"protocol":"TCP","name":"tcp","vendorExtension":"tcp"},{"containerPort":53,"protocol":"UDP","name":"udp","vendorExtension":"udp"}]}],"vendorExtension":"spec"}}`)
			spec := object["spec"].(map[string]any)
			if field != "containers" {
				spec[field] = spec["containers"]
				spec["containers"] = []any{map[string]any{"name": "main", "image": "app:v1"}}
			}
			want := deepCopyMap(object)
			accessor := NewAccessor(execution.NewDefaultRunner(object))

			specs, err := accessor.ExtractPodSpec(ctx, podSpecDefinition)
			Expect(err).NotTo(HaveOccurred())
			ports := &specs[0].Containers[0].Ports
			switch field {
			case "initContainers":
				ports = &specs[0].InitContainers[0].Ports
			case "ephemeralContainers":
				ports = &specs[0].EphemeralContainers[0].Ports
			}
			container := want["spec"].(map[string]any)[field].([]any)[0].(map[string]any)
			stored := container["ports"].([]any)
			switch change {
			case "edit":
				(*ports)[1].Name = "dns-udp"
				stored[1].(map[string]any)["name"] = "dns-udp"
			case "insert":
				*ports = append(*ports, corev1.ContainerPort{ContainerPort: 53, Protocol: corev1.ProtocolSCTP})
				container["ports"] = append(stored, map[string]any{"containerPort": float64(53), "protocol": "SCTP"})
			case "delete":
				*ports = (*ports)[1:]
				container["ports"] = stored[1:]
			case "reorder":
				(*ports)[0], (*ports)[1] = (*ports)[1], (*ports)[0]
				stored[0], stored[1] = stored[1], stored[0]
			case "clear":
				*ports = nil
				delete(container, "ports")
			}

			Expect(accessor.UpdatePodSpec(ctx, podSpecDefinition, specs)).To(Succeed())
			expectObject(accessor, want)
		},
		portEntries,
	)

	var topologyEntries []TableEntry
	for _, change := range changes {
		topologyEntries = append(topologyEntries, Entry(change, change))
	}

	// Both constraints share topologyKey, the only strategic merge key.
	DescribeTable("keeps each topology spread constraint's identity and unknown fields",
		func(change string) {
			object := decode(`{"spec":{"template":{"metadata":{"labels":{"app":"sample"}},"spec":{"containers":[{"name":"main","image":"app:v1","vendorExtension":"container"}],"topologySpreadConstraints":[{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"DoNotSchedule","labelSelector":{"matchLabels":{"app":"sample"}},"vendorExtension":"hard"},{"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway","labelSelector":{"matchLabels":{"app":"sample"}},"vendorExtension":"soft"}]}}}}`)
			if change == "insert" {
				spec := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
				spec["topologySpreadConstraints"] = spec["topologySpreadConstraints"].([]any)[:1]
			}
			want := deepCopyMap(object)
			accessor := NewAccessor(execution.NewDefaultRunner(object))
			definition := v1alpha1.ComponentDefinition{
				Name:           "template",
				SpecDefinition: &v1alpha1.SpecDefinition{PodTemplateSpecPath: ptr.To(".spec.template")},
			}

			templates, err := accessor.ExtractPodTemplateSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			constraints := &templates[0].Spec.TopologySpreadConstraints
			spec := want["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
			stored := spec["topologySpreadConstraints"].([]any)
			switch change {
			case "edit":
				(*constraints)[1].MaxSkew = 2
				stored[1].(map[string]any)["maxSkew"] = float64(2)
			case "insert":
				added := (*constraints)[0]
				added.WhenUnsatisfiable = corev1.ScheduleAnyway
				*constraints = append(*constraints, added)
				addedStored := deepCopyMap(stored[0].(map[string]any))
				delete(addedStored, "vendorExtension")
				addedStored["whenUnsatisfiable"] = "ScheduleAnyway"
				spec["topologySpreadConstraints"] = append(stored, addedStored)
			case "delete":
				*constraints = (*constraints)[1:]
				spec["topologySpreadConstraints"] = stored[1:]
			case "reorder":
				(*constraints)[0], (*constraints)[1] = (*constraints)[1], (*constraints)[0]
				stored[0], stored[1] = stored[1], stored[0]
			case "clear":
				*constraints = nil
				delete(spec, "topologySpreadConstraints")
			}

			Expect(accessor.UpdatePodTemplateSpec(ctx, definition, templates)).To(Succeed())
			expectObject(accessor, want)
		},
		topologyEntries,
	)

	DescribeTable("keeps port identities through fragmented container views",
		func(fragmented *v1alpha1.FragmentedPodSpecDefinition, isList bool) {
			// The first port omits protocol, which defaults to TCP.
			object := decode(`{"spec":{"containers":[{"name":"main","image":"app:v1","vendorExtension":"keep","ports":[{"containerPort":53,"vendorExtension":"tcp"},{"containerPort":53,"protocol":"UDP","vendorExtension":"udp"}]}]}}`)
			want := deepCopyMap(object)
			ports := want["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["ports"].([]any)
			ports[0].(map[string]any)["protocol"] = "TCP"
			ports[1].(map[string]any)["name"] = "dns-udp"
			accessor := NewAccessor(execution.NewDefaultRunner(object))
			definition := v1alpha1.ComponentDefinition{
				Name:           "svc",
				SpecDefinition: &v1alpha1.SpecDefinition{FragmentedPodSpecDefinition: fragmented},
			}

			specs, err := accessor.ExtractFragmentedPodSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			container := specs[0].Container
			if isList {
				container = &specs[0].Containers[0]
			}
			container.Ports[0].Protocol = corev1.ProtocolTCP
			container.Ports[1].Name = "dns-udp"

			Expect(accessor.UpdateFragmentedPodSpec(ctx, definition, specs)).To(Succeed())
			expectObject(accessor, want)
		},
		Entry("a single container view", &v1alpha1.FragmentedPodSpecDefinition{ContainerPath: ptr.To(".spec.containers[0]")}, false),
		Entry("a containers list view", &v1alpha1.FragmentedPodSpecDefinition{ContainersPath: ptr.To(".spec.containers")}, true),
	)

	It("lets an unrelated write through when a stored port lacks its containerPort", func() {
		object := decode(`{"spec":{"containers":[{"name":"main","image":"app:v1","ports":[{"name":"metrics","vendorExtension":"keep"}]}]}}`)
		want := deepCopyMap(object)
		want["spec"].(map[string]any)["schedulerName"] = "custom"
		accessor := NewAccessor(execution.NewDefaultRunner(object))

		specs, err := accessor.ExtractPodSpec(ctx, podSpecDefinition)
		Expect(err).NotTo(HaveOccurred())
		specs[0].SchedulerName = "custom"

		Expect(accessor.UpdatePodSpec(ctx, podSpecDefinition, specs)).To(Succeed())
		expectObject(accessor, want)
	})

	// Every keyed list follows the env rules: identity is the key plus its
	// occurrence, and a key whose count changes is written as supplied.
	Context("keyed lists other than env that repeat a key", func() {
		podObject := func(fields string) map[string]any {
			return decode(`{"spec":{"containers":[{"name":"main","image":"app:v1"}],` + fields + `}}`)
		}

		DescribeTable("merge per occurrence",
			func(stored string, mutate func(*corev1.PodSpec), want string) {
				accessor := NewAccessor(execution.NewDefaultRunner(podObject(stored)))

				specs, err := accessor.ExtractPodSpec(ctx, podSpecDefinition)
				Expect(err).NotTo(HaveOccurred())
				mutate(&specs[0])

				Expect(accessor.UpdatePodSpec(ctx, podSpecDefinition, specs)).To(Succeed())
				expectObject(accessor, podObject(want))
			},
			Entry("imagePullSecrets reduced from two equal entries to one",
				`"imagePullSecrets":[{"name":"x"},{"name":"x"}]`,
				func(s *corev1.PodSpec) { s.ImagePullSecrets = s.ImagePullSecrets[:1] },
				`"imagePullSecrets":[{"name":"x"}]`),
			Entry("hostAliases sharing an ip: the second entry's hostnames change, unknown fields stay",
				`"hostAliases":[{"ip":"127.0.0.1","hostnames":["first"],"vendorExtension":"a"},{"ip":"127.0.0.1","hostnames":["second"],"vendorExtension":"b"}]`,
				func(s *corev1.PodSpec) { s.HostAliases[1].Hostnames = []string{"third"} },
				`"hostAliases":[{"ip":"127.0.0.1","hostnames":["first"],"vendorExtension":"a"},{"ip":"127.0.0.1","hostnames":["third"],"vendorExtension":"b"}]`),
			Entry("hostAliases sharing an ip: one removed, the resized group is written as supplied",
				`"hostAliases":[{"ip":"127.0.0.1","hostnames":["first"],"vendorExtension":"a"},{"ip":"127.0.0.1","hostnames":["second"],"vendorExtension":"b"}]`,
				func(s *corev1.PodSpec) { s.HostAliases = s.HostAliases[:1] },
				`"hostAliases":[{"ip":"127.0.0.1","hostnames":["first"]}]`),
			Entry("hostAliases sharing an ip: the second entry gets its own ip",
				`"hostAliases":[{"ip":"127.0.0.1","hostnames":["first"],"vendorExtension":"a"},{"ip":"127.0.0.1","hostnames":["second"],"vendorExtension":"b"}]`,
				func(s *corev1.PodSpec) { s.HostAliases[1].IP = "127.0.0.2" },
				`"hostAliases":[{"ip":"127.0.0.1","hostnames":["first"]},{"ip":"127.0.0.2","hostnames":["second"]}]`),
			Entry("volumes sharing a name: the second entry's medium changes, unknown fields stay",
				`"volumes":[{"name":"data","emptyDir":{},"vendorExtension":"a"},{"name":"data","emptyDir":{},"vendorExtension":"b"}]`,
				func(s *corev1.PodSpec) { s.Volumes[1].EmptyDir.Medium = corev1.StorageMediumMemory },
				`"volumes":[{"name":"data","emptyDir":{},"vendorExtension":"a"},{"name":"data","emptyDir":{"medium":"Memory"},"vendorExtension":"b"}]`),
			Entry("a reorder keeps every unknown field",
				`"hostAliases":[{"ip":"127.0.0.1","hostnames":["first"],"vendorExtension":"a"},{"ip":"127.0.0.1","hostnames":["second"],"vendorExtension":"b"}]`,
				func(s *corev1.PodSpec) { s.HostAliases[0], s.HostAliases[1] = s.HostAliases[1], s.HostAliases[0] },
				`"hostAliases":[{"ip":"127.0.0.1","hostnames":["second"],"vendorExtension":"a"},{"ip":"127.0.0.1","hostnames":["first"],"vendorExtension":"b"}]`),
		)

		// Two containers with one name are invalid for the apiserver, which rejects
		// the PUT. Karta still addresses the second one and keeps its unknown field.
		It("edits the second of two containers that share a name", func() {
			object := decode(`{"spec":{"containers":[{"name":"main","image":"a:v1"},{"name":"main","image":"b:v1","vendorExtension":"keep"}]}}`)
			accessor := NewAccessor(execution.NewDefaultRunner(object))

			specs, err := accessor.ExtractPodSpec(ctx, podSpecDefinition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].Containers[1].Image = "b:v2"

			Expect(accessor.UpdatePodSpec(ctx, podSpecDefinition, specs)).To(Succeed())
			expectObject(accessor, decode(`{"spec":{"containers":[{"name":"main","image":"a:v1"},{"name":"main","image":"b:v2","vendorExtension":"keep"}]}}`))
		})
	})

	Context("env vars that repeat a name", func() {
		// The kubelet uses the last definition; X=$(X):more appends to an earlier one.
		envObject := func(env string) map[string]any {
			return decode(`{"spec":{"containers":[{"name":"main","image":"app:v1","env":` + env + `}]}}`)
		}
		storedEnv := `[{"name":"PATH","value":"/bin"},{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"fast","vendorExtension":"keep"}]`

		DescribeTable("edits any definition and keeps unknown fields on the others",
			func(mutate func([]corev1.EnvVar) []corev1.EnvVar, wantEnv string) {
				accessor := NewAccessor(execution.NewDefaultRunner(envObject(storedEnv)))

				specs, err := accessor.ExtractPodSpec(ctx, podSpecDefinition)
				Expect(err).NotTo(HaveOccurred())
				specs[0].Containers[0].Env = mutate(specs[0].Containers[0].Env)

				Expect(accessor.UpdatePodSpec(ctx, podSpecDefinition, specs)).To(Succeed())
				expectObject(accessor, envObject(wantEnv))
			},
			Entry("the last, effective definition",
				func(env []corev1.EnvVar) []corev1.EnvVar { env[1].Value = "$(PATH):/usr/local/nvidia/bin"; return env },
				`[{"name":"PATH","value":"/bin"},{"name":"PATH","value":"$(PATH):/usr/local/nvidia/bin"},{"name":"MODE","value":"fast","vendorExtension":"keep"}]`),
			Entry("the first definition",
				func(env []corev1.EnvVar) []corev1.EnvVar { env[0].Value = "/usr/bin"; return env },
				`[{"name":"PATH","value":"/usr/bin"},{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"fast","vendorExtension":"keep"}]`),
			Entry("an appended definition",
				func(env []corev1.EnvVar) []corev1.EnvVar {
					return append(env, corev1.EnvVar{Name: "PATH", Value: "$(PATH):/extra"})
				},
				`[{"name":"PATH","value":"/bin"},{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"fast","vendorExtension":"keep"},{"name":"PATH","value":"$(PATH):/extra"}]`),
			Entry("a removed first definition",
				func(env []corev1.EnvVar) []corev1.EnvVar { return env[1:] },
				`[{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"fast","vendorExtension":"keep"}]`),
			Entry("a reorder",
				func(env []corev1.EnvVar) []corev1.EnvVar { return []corev1.EnvVar{env[2], env[0], env[1]} },
				`[{"name":"MODE","value":"fast","vendorExtension":"keep"},{"name":"PATH","value":"/bin"},{"name":"PATH","value":"$(PATH):/opt/bin"}]`),
			Entry("an unrelated var",
				func(env []corev1.EnvVar) []corev1.EnvVar { env[2].Value = "safe"; return env },
				`[{"name":"PATH","value":"/bin"},{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"safe","vendorExtension":"keep"}]`),
		)

		// A repeated name matches by occurrence, which is positional: when the
		// write changes how many times the name is defined, its definitions are
		// written as supplied and lose their unknown fields, as an Update would.
		// Every other change keeps them.
		unknownOnRepeated := `[{"name":"PATH","value":"/bin","vendorExtension":"keep"},{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"fast"}]`

		DescribeTable("follows Update semantics for a repeated name with unknown fields",
			func(mutate func([]corev1.EnvVar) []corev1.EnvVar, wantEnv string) {
				accessor := NewAccessor(execution.NewDefaultRunner(envObject(unknownOnRepeated)))

				specs, err := accessor.ExtractPodSpec(ctx, podSpecDefinition)
				Expect(err).NotTo(HaveOccurred())
				specs[0].Containers[0].Env = mutate(specs[0].Containers[0].Env)

				Expect(accessor.UpdatePodSpec(ctx, podSpecDefinition, specs)).To(Succeed())
				expectObject(accessor, envObject(wantEnv))
			},
			Entry("a removed definition drops the name's unknown fields",
				func(env []corev1.EnvVar) []corev1.EnvVar { return env[1:] },
				`[{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"fast"}]`),
			Entry("an appended definition drops the name's unknown fields",
				func(env []corev1.EnvVar) []corev1.EnvVar {
					return append(env, corev1.EnvVar{Name: "PATH", Value: "$(PATH):/extra"})
				},
				`[{"name":"PATH","value":"/bin"},{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"fast"},{"name":"PATH","value":"$(PATH):/extra"}]`),
			Entry("an in-place edit keeps them",
				func(env []corev1.EnvVar) []corev1.EnvVar { env[1].Value = "$(PATH):/usr/local/nvidia/bin"; return env },
				`[{"name":"PATH","value":"/bin","vendorExtension":"keep"},{"name":"PATH","value":"$(PATH):/usr/local/nvidia/bin"},{"name":"MODE","value":"fast"}]`),
			Entry("a reorder keeps them",
				func(env []corev1.EnvVar) []corev1.EnvVar { return []corev1.EnvVar{env[2], env[0], env[1]} },
				`[{"name":"MODE","value":"fast"},{"name":"PATH","value":"/bin","vendorExtension":"keep"},{"name":"PATH","value":"$(PATH):/opt/bin"}]`),
			Entry("a change to another var keeps them",
				func(env []corev1.EnvVar) []corev1.EnvVar { env[2].Value = "safe"; return env },
				`[{"name":"PATH","value":"/bin","vendorExtension":"keep"},{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"safe"}]`),
		)

		It("applies the same semantics through a pod template", func() {
			object := decode(`{"spec":{"template":{"spec":{"containers":[{"name":"main","image":"app:v1","env":` + unknownOnRepeated + `}]}}}}`)
			accessor := NewAccessor(execution.NewDefaultRunner(object))
			definition := v1alpha1.ComponentDefinition{
				Name:           "template",
				SpecDefinition: &v1alpha1.SpecDefinition{PodTemplateSpecPath: ptr.To(".spec.template")},
			}

			templates, err := accessor.ExtractPodTemplateSpec(ctx, definition)
			Expect(err).NotTo(HaveOccurred())
			templates[0].Spec.Containers[0].Env = templates[0].Spec.Containers[0].Env[1:]

			Expect(accessor.UpdatePodTemplateSpec(ctx, definition, templates)).To(Succeed())
			expectObject(accessor, decode(`{"spec":{"template":{"spec":{"containers":[{"name":"main","image":"app:v1","env":[{"name":"PATH","value":"$(PATH):/opt/bin"},{"name":"MODE","value":"fast"}]}]}}}}`))
		})

		It("lets a write that leaves env alone through, keeping the unknown fields", func() {
			object := envObject(unknownOnRepeated)
			want := deepCopyMap(object)
			want["spec"].(map[string]any)["schedulerName"] = "custom"
			accessor := NewAccessor(execution.NewDefaultRunner(object))

			specs, err := accessor.ExtractPodSpec(ctx, podSpecDefinition)
			Expect(err).NotTo(HaveOccurred())
			specs[0].SchedulerName = "custom"

			Expect(accessor.UpdatePodSpec(ctx, podSpecDefinition, specs)).To(Succeed())
			expectObject(accessor, want)
		})
	})
})
