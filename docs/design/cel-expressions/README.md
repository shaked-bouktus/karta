<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

<a id="implementation-evidence"></a>

# CEL and references: SDK implementation design

[KEP-0002](../../keps/0002-cel-expressions/README.md) separates CEL reads from write locations in a Karta definition.  
The SDK reads and changes workloads through those rules. The application saves changes to Kubernetes.  
These examples describe the proposed `v1alpha2` SDK. They cannot compile against current upstream, which still uses jq.  
Karta definitions use `*kartav1alpha2.Karta`; decoded workloads use `*unstructured.Unstructured`.

## Validation

Most Karta users run the SDK without the Karta operator, so the SDK checks every definition itself.  
Each SDK entry point compiles the definition before it uses it: `resource.NewComponentFactory`, `resource.NewComponentFactoryFromObject`, `instructions.NewStructureSummary` and `tree.Open`.  
Compilation validates the definition, expands each shared location into `read` and `write`, and compiles the CEL programs.  
After compilation, SDK code sees only `read` and `write`. The operator's validating webhook runs the same check on create and update.  
The validator is also available to SDK callers:

```go
if err := kartav1alpha2.NewKartaValidator(kserveKarta).Validate(); err != nil {
    return err
}
```

Validation checks field shapes, compiles CEL and rejects unavailable inputs. A valid definition returns no error.

| Invalid definition | Example |
| --- | --- |
| Duplicate variable | Two `spec.variables` entries both use `name: model`. |
| Missing variable | An expression reads `variables.image`, but `image` is not declared. |
| Dependency cycle | `a` reads `variables.b`, and `b` reads `variables.a`. Neither can be evaluated first. |

A fixed `write.path` is a JSON Pointer into the workload, such as `/spec/predictor/model` in an InferenceService.  
The validator checks fixed paths when loading the definition. Paths returned by `write.pathExpression` are checked after evaluation.

## Value accessors

A `Location` names one place in the workload. A write is a location. A read is a location or an expression.  
An accessor sets a shared location, or a read, a write, or both:

```go
type Location struct {
    MatchExpression string  `json:"matchExpression,omitempty"`
    Path            *string `json:"path,omitempty"`
    PathExpression  string  `json:"pathExpression,omitempty"`
}

type ReadAccessor struct {
    Location   `json:",inline"`
    Expression string                `json:"expression,omitempty"`
    Default    *apiextensionsv1.JSON `json:"default,omitempty"`
}

type ValueAccessor struct {
    Location `json:",inline"`
    Default  *apiextensionsv1.JSON `json:"default,omitempty"`
    Read     *ReadAccessor         `json:"read,omitempty"`
    Write    *Location             `json:"write,omitempty"`
}
```

A location sets exactly one of `Path` or `PathExpression`. A read sets exactly one of those two or `Expression`.  
A shared location cannot appear together with `Read` or `Write`. Compilation copies it into both, so it has no behavior of its own.  
A location read walks the pointer through `object` and uses no CEL for a fixed path.  
A missing segment or list index returns `resource.ErrValueNotFound`. A segment through a scalar is an error.  
A null value at the end of the path or in the middle also returns `resource.ErrValueNotFound`.  
Typed getters cannot show a null apart from a missing value, and the API server drops most nulls from stored objects.  
A read expression that returns null or an empty optional returns `resource.ErrValueNotFound`. The SDK unwraps an optional that holds a value.

A shared location or a location read can set `Default`. Compilation copies a shared default into `Read` only.  
The SDK returns the default when the value is missing or null. A false match still returns `resource.ErrValueNotFound`.  
Validation rejects a default on an expression read and converts each default to the field type.  
For example, `default: "one"` on `replicas` fails when the definition loads, not when a read runs.

Each operation evaluates its match before the read expression or location. An omitted match means true.  
A false read match returns `resource.ErrValueNotFound`; whole-workload reads skip that field.  
A false write match prevents the change. Type and evaluation errors remain errors.  
An omitted `Path` differs from `path: ""`, which selects the whole workload.

| Expression | Inputs |
| --- | --- |
| Shared variable or instance ID expression | Original workload as `object`, plus `variables` and `references` |
| Read expression, path expression, or match | The same inputs, plus `instance` and `index` |
| Pod selector or grouping expression | One Pod as `object` only |
| Reference lookup or selector | `object` is the workload passed to the SDK, such as a TrainJob. These expressions cannot use `variables` or `references`. |

A component with `instanceIds` has one instance for each returned ID.  
The SDK evaluates each read, write and match once for each instance. Getters return the values keyed by ID.  
For IDs `["gpu", "cpu"]`, the `"gpu"` instance has `instance = "gpu"` and `index = 0`.  
A read that returns `resource.ErrValueNotFound` affects only its own instance. The other instances keep their values.  
Without `instanceIds`, the component has one instance with `instance = ""` and `index = 0`.  
If its Pod location is missing or its match is false, its reads return `resource.ErrValueNotFound`. The component is then absent, which is not an error.  
Variables do not receive `instance` or `index`, so each variable is evaluated once for each workload.  
Write expressions cannot read the caller's new value.

The ID expression returns a list. An empty or duplicate ID fails the component with `resource.ErrInstanceMismatch`. An empty list means no instances.  
The `karta` package from the Workload front door KEP returns the same sentinel, for example as an alias.  
Each path is evaluated for its own ID, so the number of values always matches the number of IDs.  
The SDK keeps the order of the list, and `index` is the position in it.  
IDs from a list must map it without a filter or a sort, so `index` addresses the same entry in the workload.  
Compilation enforces part of this rule. If a read, write or match of the component uses `index`, the SDK walks the parsed syntax tree of the ID expression.  
It rejects a call to `sort`, `sortBy`, `reverse`, `slice` or `distinct`, and a `filter` or three-argument `map` macro.  
The SDK cannot detect every reorder. For example, `a + b` shifts the positions of `b` without an error. The recorded e2e flow proves the order.  
IDs from map keys must use `.sort()`, because CEL map keys have no fixed order.

The SDK compiles each definition revision once and reuses the CEL programs.  
Each evaluation has its own workload snapshot and reference inputs, and resolves only the variables it needs.  
The evaluator supports cel-go optional values, strings and list extensions.

CEL receives workload and reference data as plain maps, lists and values.  
Before evaluation, the SDK removes map entries whose value is null from the workload, the references and each Pod.  
List elements stay in place, so `index` still addresses the same entry. Writes apply to the original object.  
`orValue` then replaces a null value as well as a missing value. This gives the jq `//` result for every catalog definition.  
Results must have the expected type: a boolean for a match or a string for a write path.  
Instance IDs are strings: a list of strings from `instanceIds`, and one string from a Pod instance selector.  
Authors convert other types with `string(x)`, so the workload and the Pod use the same text.  
A scale value is a CEL integer, a double with no fraction, or a string that holds a base-10 integer. It must fit in int32.  
The string form reads Knative annotations such as `autoscaling.knative.dev/min-scale: "2"` with a location `read`.  
A scale write sends an integer, so a field that keeps scale in a string sets `read` only:

```yaml
minReplicas:
  read:
    path: /spec/template/metadata/annotations/autoscaling.knative.dev~1min-scale
    default: 1
```

Each CEL evaluation has a cost limit of 1,000,000, the Kubernetes limit for one validation call, and a timeout of 1 second.  
The timeout covers CEL only. Reference reads use the caller's context. Evaluation also respects context cancellation.  
One SDK option changes both limits on the factories, `tree.Open`, `instructions.NewStructureSummary` and the Pod querier.  
A limit that is exceeded is an error, never `resource.ErrValueNotFound`. The SDK has no result count limit and no cost estimate at load time.  
Errors identify the component and field without including object contents.

## Writing a value

A write names a component, an instance and a `Field`. The `Field` names an accessor in the Karta definition.  
The `write` rule of that accessor gives the location. The caller never names a path in the workload.  
Each definition maps the same `Field` to the right place in its own workload type.

```go
type Write struct {
    Component string
    Instance  string
    Field     Field
    Value     any
}

type Target struct {
    Component string
    Instance  string
    Field     Field
}

type Field string

const (
    PodTemplateSpec   Field = "podTemplateSpec"
    PodSpec           Field = "podSpec"
    Metadata          Field = "metadata"
    SchedulerName     Field = "schedulerName"
    Labels            Field = "labels"
    Annotations       Field = "annotations"
    Resources         Field = "resources"
    ResourceClaims    Field = "resourceClaims"
    PodAffinity       Field = "podAffinity"
    NodeAffinity      Field = "nodeAffinity"
    Containers        Field = "containers"
    Container         Field = "container"
    PriorityClassName Field = "priorityClassName"
    Image             Field = "image"
    Replicas          Field = "replicas"
    MinReplicas       Field = "minReplicas"
    MaxReplicas       Field = "maxReplicas"
)

var (
    ErrFieldNotWritable = errors.New("field is not writable")
    ErrNotSuspendable   = errors.New("root component has no suspend actions")
)
```

Each `Field` value is the key of its accessor in `specDefinition`, `fragmentedPodSpecDefinition` or `scaleDefinition`.  
The keys are unique across the three, so a `Field` needs no prefix.  
For KServe, `Container` selects `fragmentedPodSpecDefinition.container`, and `SchedulerName` writes `/spec/predictor/schedulerName`.  
A write returns `ErrFieldNotWritable` when the component does not map the field, maps it without `write`, or its write match is false.  
A generic caller can then try another field. For example, it can write `PodTemplateSpec` when a definition does not map `SchedulerName`.  
Suspend and resume actions are not fields. [Suspend and resume](#suspend-and-resume) describes them.

This call updates the model image in the KEP's InferenceService:

```go
editor, err := tree.Open(ctx, kserveKarta, inferenceService)
if err != nil {
    return err
}
if err := editor.Mutate(ctx, tree.Write{
    Component: "predictor",
    Field:     tree.Container,
    Value:     map[string]any{"image": "ghcr.io/example/inference:v2"},
}); err != nil {
    return err
}
updated, err := editor.GetResource()
if err != nil {
    return err
}
return k8sClient.Update(ctx, updated)
```

The image becomes `ghcr.io/example/inference:v2`; `storageUri` and other unspecified fields stay unchanged.  
The editor changes a copy; `k8sClient.Update` saves it to Kubernetes.  
Update errors return to the application. After a resource-version conflict, it fetches the workload again and opens a new editor.

`Mutate(ctx, writes...)` applies one batch atomically. A later call starts from the last successful batch.  
It resolves all matches and destinations against the starting workload.  
Fields that are not writable, invalid pointers, duplicate or overlapping targets, and unknown or duplicate instance IDs all fail before mutation.  
It then applies each JSON Merge Patch to a private copy. Missing object parents are created, but paths through scalars or missing array entries fail.  
Finally, it checks workload identity and extracts the tree again with a new factory. The updated object and tree become available together only if every operation succeeds.  
The extraction is a validity check: the batch fails if any read fails. It compares no values.  
A write can change what another component reads, when that component reads the same location or a value computed from it.  
For example, a Grove clique's replica count is the root `spec.replicas` times the clique's own replicas. A write to the root count changes both.

`Write.Value` accepts non-null JSON values. Maps merge, scalars and arrays replace, and null map values delete keys.  
To set a label and the scheduler name, a caller sends only those keys and reads nothing first.  
Changing a container list requires the full array. Merge Patch cannot merge containers by name or store a literal null in a map.  
For `Replicas`, `MinReplicas` and `MaxReplicas`, `Value` must be a JSON integer that fits in int32. Other values fail before mutation.  
A definition that keeps a scale value in a string, such as a Knative annotation, maps it with `read` only.  
The initial API supports only Merge Patch. Raw JSON writes replace typed `Component.Update*` methods in v1alpha2 so operator-specific fields can be preserved.

### Reading before a write

`Get` returns the raw JSON value at a field's write location:

```go
current, err := editor.Get(ctx, tree.Target{
    Component: "predictor",
    Field:     tree.Container,
})
if err != nil {
    return err
}
```

For the KEP's InferenceService, `current` holds both `image` and `storageUri`.  
`Get` uses the same instance, match and path rules as `Mutate`, so the caller sees the value that a patch applies to.  
It reads the editor's copy after the last successful `Mutate` and returns a copy of the value.  
A missing location returns `resource.ErrValueNotFound`. A later write still creates the missing object parents.  
A field that `Mutate` cannot write returns `ErrFieldNotWritable`.  
To change one entry of a list, the caller reads the full array with `Get`, changes it, and writes the full array back.  
Fields that the Go types in `corev1` do not know stay in place.

### Suspend and resume

`editor.Suspend(ctx)` applies the root component's suspend actions. `editor.Resume(ctx)` applies its resume actions.  
`editor.IsSuspendable()` reports whether the root declares them. Without them, both calls return `ErrNotSuspendable`.  
The actions run in their listed order on a private copy, as one atomic batch. A later action can overwrite an earlier one.  
An action sets its location to exactly its value, as jq `Assign` does in v1alpha1. It does not merge.  
A `null` value removes the key at the location. A missing key is not an error.  
Action locations follow the write rules. A false match is an error, and missing object parents are created.  
A path through a scalar or a missing array entry fails.  
Each action runs once per instance, with `instance` and `index`. Suspending a suspended workload succeeds and changes nothing.

### Pod-template helpers

`Mutate` writes one field at a time, so a caller must know which fields a definition maps.  
A pod-template helper can hide that choice. It builds a `corev1.PodTemplateSpec` for each instance from `Get` and runs the caller's callback on it.  
It then turns each changed field into a `Write` for the field that the definition maps, and applies all writes in one `Mutate`.  
For a changed list, it edits the raw array from `Get`, so fields that `corev1` does not know stay in place.  
A callback error keeps its own cause. The helper wraps it with the component and instance, and adds no sentinel.  
Such a helper is a separate proposal. This design defines only the engine below it: `tree.Open`, `Get` and `Mutate`.

## Reading a value

This read uses the KEP's original InferenceService, with image `ghcr.io/example/inference:v1`.  
Its predictor component exposes the mapped container through a typed getter:

```go
factory := resource.NewComponentFactoryFromObject(kserveKarta, inferenceService)
predictor, err := factory.GetComponent("predictor")
if err != nil {
    return err
}
podSpecFragments, err := predictor.GetFragmentedPodSpec(ctx)
if err != nil {
    return err
}
fmt.Println(podSpecFragments[""].Container.Image)
```

The result is `ghcr.io/example/inference:v1`. The empty key identifies the single unnamed instance.  
CEL evaluates the mapping against the whole InferenceService as `object`; the getter converts the result to a container value.  
The typed getters are views for display and logic. They drop fields that their Go types do not know, such as `storageUri`.  
To change a value and write it back, read it with `editor.Get`. [Reading before a write](#reading-before-a-write) describes it.

## Tree traversal

The [LeaderWorkerSet example](../../examples/quickstart/lws.yaml) has a `group` component with `leader` and `worker` children.  
`lwsKarta` is its catalog definition converted to CEL; `leaderWorkerSet` is the example decoded as an unstructured object:

```go
factory := resource.NewComponentFactoryFromObject(lwsKarta, leaderWorkerSet)
workload, err := tree.Build(ctx, factory)
if err != nil {
    return err
}
var visit func([]tree.ComponentNode)
visit = func(nodes []tree.ComponentNode) {
    for _, component := range nodes {
        fmt.Println(component.Name)
        for _, instance := range component.Instances {
            visit(instance.Children)
        }
    }
}
visit(workload.Children)
```

The local `visit` function prints `group`, `leader`, then `worker`.  
`tree.Build` validates the definition and extracts component values. Its `Children` excludes the root, which is available through `factory.GetRootComponent()`.  
The tree describes component structure. Replica counts remain values rather than expanding into generated replicas.

## Pod mapping

For a Pod already associated with an InferenceService, the SDK identifies its component.  
Here, `pod` is a `*corev1.Pod` with labels `component: predictor` and `serving.kserve.io/inferenceservice: model`:

```go
summary, err := instructions.NewStructureSummary(kserveKarta)
if err != nil {
    return err
}
querier := resource.NewPodQuerier(pod)
component, err := instructions.InferPodComponent(ctx, querier, summary)
if err != nil {
    return err
}
fmt.Println(component)
```

The result is `predictor`. Pod selectors evaluate against the Pod as `object`.  
With the InferenceService factory from the read example, `instructions.InferPodComponentInstance(ctx, querier, component, factory)` returns its instance ID as a `*string`.  
This predictor has one unnamed instance, so the result is nil.

`querier.ExtractReplicaKey(ctx, selector.ReplicaSelector)` returns the replica that a Pod belongs to, such as the LWS group index.  
Its expression must return a non-empty string, as a grouping expression must. A missing value, an empty string or another type is an error.  
The SDK no longer converts other types to text with `fmt.Sprintf("%v")`.

## Scheduling groups

The same Pod, component and summary identify its scheduling group and keys:

```go
group, err := instructions.GetPodGroupingEffectiveComponent(
    ctx, querier, component, summary)
if err != nil {
    return err
}
if group == nil {
    return nil
}
keys, err := querier.ExtractGroupKeysFor(ctx, *group.MemberDefinition)
if err != nil {
    return err
}
fmt.Println(group.PodGroupName, keys)
```

For the KEP's KServe definition, the result is `service [model]`. A transformer Pod for the same InferenceService has the same group and key.  
`GetPodGroupingEffectiveComponent` returns the member that names the Pod's component, or else its nearest parent. Members have no filters, so the SDK has no `PassesFilters`.  
`ExtractGroupKeysFor` evaluates that member's CEL `groupByExpressions` against the Pod.  
Each key must be a non-empty string. Otherwise the call returns an error that names the member and the expression index.  
The application collects Pods with the same workload identity, group name and ordered keys. The consuming scheduler admits each group as a gang.  
Pod collection, scheduler PodGroup resources and scheduling remain outside the SDK.

A consuming scheduler gets the minimum Pod count of a gang from the SDK:

```go
minPods, err := instructions.CalculateSubtreeScale(
    ctx, group.EffectiveComponent, instanceID, factory, summary)
```

`instanceID` is a `*string`. Nil counts all instances of the component.  
For each component, the function reads `minReplicas`, or `replicas` when `minReplicas` is missing or zero.  
With an instance ID, the subtree root counts only that instance. Without one, it adds the counts of all instances.  
A parent returns its own count multiplied by the sum of its children's counts.  
A count of zero means that another level sets the scale, so the nonzero level's count passes through.  
If no component in the definition has a scale, the function counts the leaf components, one for each instance.  
`v1alpha2` keeps this arithmetic. Only the scale reads change from jq to CEL.

The count covers the whole component subtree, not one gang that group keys make.  
For the LeaderWorkerSet catalog definition with two groups of three Pods, `group` returns 18.  
This is a known limitation of the current SDK. A fix changes the gang sizes that schedulers use, so it is outside this design.

The function reads scales through the factory, so the reference rules apply.  
References are off unless the factory has a reference source. A scheduler without RBAC for the referenced kinds passes none.  
A scale read that needs a reference then fails, unless its expression has a fallback such as `"trainingRuntime" in references ? ... : 1`.

## References

Trainer v2 reads an image from the TrainJob override or its referenced ClusterTrainingRuntime.  
The application supplies a reader that can fetch the declared reference or passes the runtime object directly.

![Reference resolution through a Kubernetes reader or supplied objects.](sdk-references-kubernetes.png)

### Kubernetes reader

`ResourceReader` provides lookups and lists without requiring a Kubernetes client in the core SDK:

```go
type ResourceReader interface {
    Get(ctx context.Context, gvk schema.GroupVersionKind,
        namespace, name string) (*unstructured.Unstructured, error)
    List(ctx context.Context, gvk schema.GroupVersionKind,
        query ListQuery) ([]unstructured.Unstructured, error)
}

type ListQuery struct {
    Namespace string
    Selector  labels.Selector
}
```

`schema.GroupVersionKind` identifies the Kubernetes type.  
The controller-runtime adapter uses the application's reader and REST mapper:

```go
reader := ctrlreader.New(mgr.GetAPIReader(), mgr.GetRESTMapper())
factory := resource.NewComponentFactoryFromObject(trainJobKarta, trainJob,
    resource.WithReferenceReader(reader))
root, err := factory.GetRootComponent()
if err != nil {
    return err
}
podSpecFragments, err := root.GetFragmentedPodSpec(ctx)
if err != nil {
    return err
}
fmt.Println(podSpecFragments[""].Image)
```

For the KEP's TrainJob and runtime, the result is `ghcr.io/example/trainer:v1`.  
The lookup expression reads the runtime name `torch` from the TrainJob.  
The adapter sets the object or list GVK and calls `Get` for a lookup or `List` for a label selector.  
These Go client calls return the complete result or an error; no `kubectl` process runs.

The REST mapper determines resource scope. Cluster-scoped targets use no namespace; namespaced targets use the workload namespace.  
A namespaced target with an empty namespace is an error. Access uses the application's credentials and RBAC permissions.

### Objects supplied by the application

An application that already has the runtime can pass it directly:

```go
resolved := references.ResolvedReferences{
    "trainingRuntime": references.NewLookupValue(runtimeObject),
}
factory := resource.NewComponentFactoryFromObject(trainJobKarta, trainJob,
    resource.WithReferences(resolved))
```

`runtimeObject` is the KEP runtime as an `*unstructured.Unstructured`. Each reference holds a lookup result or list:

```go
type ResolvedReferences map[string]ReferenceValue

type ReferenceValue struct {
    Object *unstructured.Unstructured
    List   []unstructured.Unstructured
}
```

At most one field is set. `NewLookupValue(nil)` records a missing object; `NewListValue(nil)` creates a non-nil empty list.  
The SDK copies the values, and the image getter returns the same result without client calls.

Supplied objects must follow the declaration's name, kind and namespace rules.  
The SDK checks names and binding shape without fetching objects to verify selection. Supplying both values and a reader is an error.  
Lookup-recipe tests use an in-memory `ResourceReader` over recorded objects.

### Deployment status with or without Pod references

The input is a Deployment named `web` in `team-a`, with two replicas and selector `app: web`.  
The selector below matches that Deployment's full selector and lists Pods in its namespace.

```yaml
# Under spec.structureDefinition in deploymentKarta.
references:
  - name: pods
    apiVersion: v1
    kind: Pod
    list:
      matchLabels:
        app:
          expression: object.spec.selector.matchLabels.app
rootComponent:
  name: deployment
  kind: {group: apps, version: v1, kind: Deployment}
  statusDefinition:
    statusMappings:
      running:
        - byExpression:
            expression: >-
              object.spec.replicas > 0 &&
              object.?status.?observedGeneration.orValue(0) >= object.metadata.generation &&
              object.?status.?updatedReplicas.orValue(0) == object.spec.replicas &&
              ("pods" in references ?
                references.pods.size() == object.spec.replicas &&
                references.pods.all(p,
                  p.?metadata.?deletionTimestamp.orValue(null) == null &&
                  p.?status.?phase.orValue("") == "Running" &&
                  p.?status.?conditions.orValue([]).exists(c,
                    c.type == "Ready" && c.status == "True")) :
                object.?status.?conditions.orValue([]).exists(c,
                  c.type == "Available" && c.status == "True"))
```

`object` is the Deployment. The generation check compares its own `observedGeneration` and `metadata.generation`; Pod generations are unrelated.  
With Pod references, the rule also requires the desired number of Pods, all Running, Ready and not terminating.  
Without them, it uses the Deployment's `Available` condition. A successful empty Pod list does not use that fallback.

The proposed Go option chooses whether this factory uses references:

```go
withPods := resource.NewComponentFactoryFromObject(deploymentKarta, deployment,
    resource.WithReferencesEnabled(true),
    resource.WithReferenceReader(reader))

statusOnly := resource.NewComponentFactoryFromObject(deploymentKarta, deployment,
    resource.WithReferencesEnabled(false))
```

`WithReferencesEnabled` defaults to true and is also accepted by `tree.Open`. False prevents reference reads and exposes an empty `references` map, even if values were supplied.  
The CEL expression defines the fallback; the SDK does not choose a workload status on its own. A definition that accesses a missing reference without a fallback still fails.  
When enabled, reference read errors remain errors. This option does not add a CRD field.

Both factories use the same status getter:

```go
root, err := withPods.GetRootComponent()
if err != nil {
    return err
}
status, err := root.GetStatus(ctx)
if err != nil {
    return err
}
fmt.Println(status.MatchedStatuses)
```

For two Ready Pods in the current rollout, `withPods` returns `[Running]`.  
Using `statusOnly` requires the same generation and updated-replica checks plus `Available=True`, without listing Pods.  
The result is a computed Karta status; neither call writes the Deployment's Kubernetes `.status`.

### Resolution and lifetime

Each reference has a unique name and declares one lookup or list. Lookups require a nonempty name.  
Lists accept `matchLabels` and `matchExpressions`, with literal `value` or CEL `expression` entries. Results are sorted by namespace and name.  
Lookup and selector expressions only see the original workload, so references cannot form chains.

Reads without reference dependencies make no fetches.  
With references enabled, the first dependent read resolves all declared references and keeps successful results. Failed resolutions are not cached.  
Dependencies come from expression syntax, so Trainer's fallback resolves its runtime even when the TrainJob has an image override.  
Whole-tree extraction can trigger resolution immediately. Component getters allow status-only reads.

For a missing object, the adapter returns `references.ErrNotFound`. The SDK leaves that lookup unbound and continues evaluation.  
A TrainJob image override can still be returned; without an override, reading the missing runtime fails. An empty list binds `[]`.  
Other reader errors, including denied reads, stop evaluation even when an override is present. With references enabled, a dependent read also fails if the factory has no reference source.  
A cache-backed reader is responsible for its setup and freshness; `GetAPIReader` reads the API directly.

Each workload observation gets a new factory, used serially. Its reader results or supplied values expose reference updates and deletions.  
The consuming application owns watches and retries. Reads of several Kubernetes objects do not form an atomic snapshot.

After a mutation, the new factory keeps the reference option. If enabled, it resolves references against the updated workload.  
With references enabled and supplied values, a mutation cannot change a reference's lookup name or selector. Such a change requires an updated input object and a new editor with matching reference values.

## Release verification

Release tests compare status results on existing recordings and check that jq and CEL mutations produce equivalent objects.  
New recordings cover Pod mapping and grouping. Tests also cover match failures, null patch rejection, both reference sources, changed reference recipes, and the KEP's schema and migration cases.  
Source and API changes also require generated-manifest and presubmit checks.
