// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	"github.com/dsx-ai-factory/workload-map/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/pkg/jq/execution"
)

// Causes a refused write wraps, for callers to branch on with errors.Is.
var (
	// ErrFragmentShape: the stored fragment does not decode into the
	// projection type, or holds a list the merge cannot process (a null entry
	// among objects), so the workload stores something else at the path.
	ErrFragmentShape = errors.New("stored fragment does not fit the projection type")
	// ErrReadBackMismatch: the merge cannot express the change, so the merged
	// fragment would not read back as the supplied value.
	ErrReadBackMismatch = errors.New("merged fragment reads back differently than the supplied value")
	// ErrConflictingWrites: two views of one value carry different changes.
	ErrConflictingWrites = errors.New("conflicting writes to the same value")
	// ErrIdentityChange: a write at the object root would change what the
	// object is.
	ErrIdentityChange = errors.New("write would change the object identity")
	// ErrDirectiveKey: a supplied value carries a strategic merge directive as
	// a literal key, which the merge would interpret instead of storing.
	ErrDirectiveKey = errors.New("value carries a strategic merge directive key")
	// ErrFragmentAbsent: the object does not store the component instance the
	// write addresses, so the write would create it from a zero projection.
	ErrFragmentAbsent = errors.New("object does not store the fragment, the write would create it")
)

// WriteError reports a refused update. Nothing was written. Err wraps one of
// the Err* causes above, or an internal encoding failure.
type WriteError struct {
	Path string
	Err  error
}

func (e *WriteError) Error() string {
	return fmt.Sprintf("write to '%s' refused, nothing was written: %v", e.Path, e.Err)
}

func (e *WriteError) Unwrap() error {
	return e.Err
}

// InstanceCountMismatchError reports a path that resolves a different number of
// locations than the number of values supplied for it.
type InstanceCountMismatchError struct {
	Path      string
	Locations int
	Values    int
}

func (e *InstanceCountMismatchError) Error() string {
	return fmt.Sprintf("path '%s' resolves %d locations but %d values were supplied", e.Path, e.Locations, e.Values)
}

// writePlan stages verified fragment writes against one snapshot of the object
// and commits them in a single atomic splice: either every staged write lands
// or the object stays untouched, and later fragments diff against the same
// baseline as earlier ones.
type writePlan struct {
	accessor *Accessor
	paths    [][]any
	values   []any
	// roots caches componentRoots per instance count.
	roots map[int][][]any
}

// writeValue distinguishes removing a map entry from assigning JSON null.
type writeValue struct {
	value   any
	present bool
}

// keyedListMerger carries a bare list write together with the patch merge keys
// its elements need: the pod spec's own tags do not travel with a bare slice.
type keyedListMerger interface {
	keyedList() (wrapped corev1.PodSpec, field string)
}

type containersValue []corev1.Container

func (v containersValue) keyedList() (corev1.PodSpec, string) {
	return corev1.PodSpec{Containers: v}, "containers"
}

type resourceClaimsValue []corev1.PodResourceClaim

func (v resourceClaimsValue) keyedList() (corev1.PodSpec, string) {
	return corev1.PodSpec{ResourceClaims: v}, "resourceClaims"
}

// identityKeys are the object-identity fields an update may never touch when
// the declared path resolves to the object root.
var identityKeys = []string{"apiVersion", "kind", "status"}

// mergeWrite sets the projection at path to exactly the supplied values, as a
// single staged plan. See writePlan.stage for the write semantics.
func (a *Accessor) mergeWrite(ctx context.Context, definition v1alpha1.ComponentDefinition, path string, values []any, schema any) error {
	plan := a.newWritePlan()
	if err := plan.stage(ctx, definition, path, values, schema); err != nil {
		return err
	}
	return plan.commit(ctx)
}

func (a *Accessor) newWritePlan() *writePlan {
	return &writePlan{accessor: a}
}

// stage resolves, diffs, merges and verifies one projection write without
// touching the object:
//
//  1. resolve the concrete locations the path addresses (typed error if the
//     expression computes a value, aliases locations through negative indexes,
//     or slices arrays), and read the stored fragment at each location
//  2. re-read each fragment through the projection type, so encoder artifacts
//     appear on both sides of the diff and cancel
//  3. diff baseline against the supplied value into a patch; an empty patch
//     skips the location entirely
//  4. merge the patch onto the raw stored fragment, keeping every field the
//     projection type does not know
//  5. verify the merged fragment reads back as exactly the supplied value
//  6. refuse a change to a component instance the object does not store: an
//     absent fragment reads as a zero projection, and writing a changed value
//     would create its component (a Worker replica spec holding only the
//     written fields); see componentRoots for what counts as the component
//  7. combine compatible overlapping writes and refuse conflicting changes
//
// A nil value skips its location. schema carries the patch merge keys (for
// example corev1.PodSpec); a nil schema treats the fragment as a plain leaf.
//
// A path that is not writable (a computed expression such as
// "{requests: .spec.resources}", or a negative index) is a read-only view:
// writing back exactly what it reads is a no-op, so read-modify-write round
// trips keep working, and any change fails with PathNotWritableError.
//
// A list without a merge key (tolerations, affinity terms) is atomic, as the
// API declares it: changing it replaces every entry, the same as strategic
// merge patch and server-side apply, so fields the projection cannot see
// inside its entries are cleared. Verification reads back only the projection.
func (p *writePlan) stage(ctx context.Context, definition v1alpha1.ComponentDefinition, path string, values []any, schema any) error {
	paths, err := resolveLocations(ctx, p.accessor.jqRunner, path)
	if err == nil {
		for i := range paths {
			if paths[i], err = canonicalLocation(paths[i]); err != nil {
				err = &execution.PathNotWritableError{Expression: path, Err: err}
				break
			}
		}
	}
	if err != nil {
		var notWritable *execution.PathNotWritableError
		if errors.As(err, &notWritable) && p.isUnchanged(ctx, definition, path, values, schema) {
			return nil
		}
		return err
	}
	values, err = alignValues(definition, len(paths), values, path)
	if err != nil {
		return err
	}
	object, err := p.accessor.jqRunner.GetObject()
	if err != nil {
		return err
	}
	roots := p.componentRoots(ctx, definition, path, paths)

	for i, location := range paths {
		if values[i] == nil {
			continue
		}
		fragment := fragmentAt(object, location)
		merged, changed, err := mergeFragment(fragment, values[i], schema)
		if err != nil {
			return &WriteError{Path: path, Err: err}
		}
		if !changed {
			continue
		}
		if fragmentAt(object, roots[i]) == nil {
			return &WriteError{Path: path, Err: fmt.Errorf("%w: nothing is stored at %v", ErrFragmentAbsent, roots[i])}
		}
		if len(location) == 0 {
			if err := guardIdentity(fragment, merged); err != nil {
				return &WriteError{Path: path, Err: err}
			}
		}
		if err := p.add(ctx, object, location, merged); err != nil {
			return &WriteError{Path: path, Err: err}
		}
	}
	return nil
}

// componentRoots returns, per resolved location, where the object stores the
// component instance the write addresses. A write fills in fields below the
// root, including an absent optional fragment (a sparse pod spec overlay the
// controller merges onto its base), and never creates the root itself (the
// Worker replica spec of a Master-only job). For a template, pod spec or
// metadata definition the root is the parent of the fragment. For a fragmented
// definition it is the deepest location every path of the definition shares
// (the clique entry under its scheduler name, labels and containers), or the
// parent of a lone path (the values map under a lone resources path). Paths
// that compute a value or resolve a different number of instances do not take
// part. The roots depend only on the definition and the instance count, so one
// plan resolves them once per count.
func (p *writePlan) componentRoots(ctx context.Context, definition v1alpha1.ComponentDefinition, path string, paths [][]any) [][]any {
	if roots, ok := p.roots[len(paths)]; ok {
		return roots
	}
	var fragmented *v1alpha1.FragmentedPodSpecDefinition
	if definition.SpecDefinition != nil {
		fragmented = definition.SpecDefinition.FragmentedPodSpecDefinition
	}
	if fragmented == nil {
		roots := make([][]any, len(paths))
		for i, location := range paths {
			roots[i] = location[:max(len(location)-1, 0)]
		}
		return p.cacheRoots(len(paths), roots)
	}
	var aligned [][][]any
	for _, sibling := range []*string{fragmented.SchedulerNamePath, fragmented.LabelsPath, fragmented.AnnotationsPath,
		fragmented.ResourcesPath, fragmented.ResourceClaimsPath, fragmented.PodAffinityPath, fragmented.NodeAffinityPath,
		fragmented.ContainersPath, fragmented.ContainerPath, fragmented.PriorityClassNamePath, fragmented.ImagePath} {
		if sibling == nil {
			continue
		}
		if *sibling == path {
			aligned = append(aligned, paths)
			continue
		}
		locations, err := resolveLocations(ctx, p.accessor.jqRunner, *sibling)
		if err != nil || len(locations) != len(paths) {
			continue
		}
		for i := range locations {
			if locations[i], err = canonicalLocation(locations[i]); err != nil {
				break
			}
		}
		if err == nil {
			aligned = append(aligned, locations)
		}
	}
	roots := make([][]any, len(paths))
	for i := range paths {
		root := paths[i]
		if len(aligned) == 1 && len(root) > 0 {
			root = root[:len(root)-1]
		}
		for _, locations := range aligned {
			shared := 0
			for shared < len(root) && shared < len(locations[i]) && root[shared] == locations[i][shared] {
				shared++
			}
			root = root[:shared]
		}
		roots[i] = root
	}
	return p.cacheRoots(len(paths), roots)
}

func (p *writePlan) cacheRoots(instances int, roots [][]any) [][]any {
	if p.roots == nil {
		p.roots = map[int][][]any{}
	}
	p.roots[instances] = roots
	return roots
}

// isUnchanged reports whether every supplied value equals what path reads now,
// using the same comparison as a write that turns out to be a no-op.
func (p *writePlan) isUnchanged(ctx context.Context, definition v1alpha1.ComponentDefinition, path string, values []any, schema any) bool {
	current, err := p.accessor.jqRunner.Evaluate(ctx, path)
	if err != nil {
		return false
	}
	values, err = alignValues(definition, len(current), values, path)
	if err != nil {
		return false
	}
	for i, value := range values {
		if value == nil {
			continue
		}
		// jq can compute numbers as ints; normalize to decoded JSON first.
		var read any
		if err := convertViaJSON(current[i], &read); err != nil {
			return false
		}
		if _, changed, err := mergeFragment(read, value, schema); err != nil || changed {
			return false
		}
	}
	return true
}

// add combines overlapping writes against their common baseline. Only changes
// made by both views can conflict; unchanged fields in either view are ignored.
func (p *writePlan) add(ctx context.Context, object any, location []any, value any) error {
	for i := 0; i < len(p.paths); {
		staged := p.paths[i]
		if !isOverlapping(staged, location) {
			i++
			continue
		}
		ancestor, descendant := location, staged
		parentValue, childValue := value, p.values[i]
		if len(staged) < len(location) {
			ancestor, descendant = staged, location
			parentValue, childValue = p.values[i], value
		}
		baseline := fragmentAt(object, ancestor)
		childWrite := execution.NewDefaultRunner(baseline)
		if err := spliceLocations(ctx, childWrite, [][]any{descendant[len(ancestor):]}, []any{childValue}); err != nil {
			return err
		}
		childObject, err := childWrite.GetObject()
		if err != nil {
			return err
		}
		combined, err := combineWrites(
			writeValue{value: baseline, present: true},
			writeValue{value: parentValue, present: true},
			writeValue{value: childObject, present: true},
		)
		if err != nil {
			return fmt.Errorf("%w: write at %v conflicts with write at %v: %w", ErrConflictingWrites, location, staged, err)
		}
		location, value = ancestor, combined.value
		p.paths = append(p.paths[:i], p.paths[i+1:]...)
		p.values = append(p.values[:i], p.values[i+1:]...)
	}
	p.paths = append(p.paths, location)
	p.values = append(p.values, value)
	return nil
}

// combineWrites three-way merges two changed copies of baseline: each side keeps
// its own changes, and a value both sides changed differently is a conflict.
func combineWrites(baseline, first, second writeValue) (writeValue, error) {
	switch {
	case reflect.DeepEqual(first, second), reflect.DeepEqual(baseline, second):
		return first, nil
	case reflect.DeepEqual(baseline, first):
		return second, nil
	}
	baseMap, isBaseMap := baseline.value.(map[string]any)
	firstMap, isFirstMap := first.value.(map[string]any)
	secondMap, isSecondMap := second.value.(map[string]any)
	if !isFirstMap || !isSecondMap || (baseline.present && baseline.value != nil && !isBaseMap) {
		// Arrays are atomic here: a parent may have reordered or removed the
		// element addressed by a child path resolved against the baseline.
		return writeValue{}, fmt.Errorf("different changes to the same value")
	}
	keys := make(map[string]struct{}, len(baseMap)+len(firstMap)+len(secondMap))
	for _, m := range []map[string]any{baseMap, firstMap, secondMap} {
		for key := range m {
			keys[key] = struct{}{}
		}
	}
	combined := make(map[string]any, len(keys))
	for key := range keys {
		base, basePresent := baseMap[key]
		left, leftPresent := firstMap[key]
		right, rightPresent := secondMap[key]
		field, err := combineWrites(writeValue{base, basePresent}, writeValue{left, leftPresent}, writeValue{right, rightPresent})
		if err != nil {
			return writeValue{}, fmt.Errorf("field %q: %w", key, err)
		}
		if field.present {
			combined[key] = field.value
		}
	}
	return writeValue{value: combined, present: true}, nil
}

// commit splices every staged write in one atomic update. An empty plan leaves
// the object untouched.
func (p *writePlan) commit(ctx context.Context) error {
	if len(p.paths) == 0 {
		return nil
	}
	return spliceLocations(ctx, p.accessor.jqRunner, p.paths, p.values)
}

// resolveLocations returns the concrete locations an expression addresses,
// without mutating. An expression that computes a value instead of addressing
// one fails with PathNotWritableError.
func resolveLocations(ctx context.Context, runner execution.Evaluator, expression string) ([][]any, error) {
	results, err := runner.Evaluate(ctx, fmt.Sprintf("[path(%s)]", expression))
	if err != nil {
		var execErr *execution.JQExecutionError
		if errors.As(err, &execErr) {
			return nil, &execution.PathNotWritableError{Expression: expression, Err: execErr.Err}
		}
		return nil, err
	}
	if len(results) != 1 {
		return nil, &execution.PathNotWritableError{Expression: expression, Err: fmt.Errorf("expected one path array, got %d results", len(results))}
	}
	rawLocations, ok := results[0].([]any)
	if !ok {
		return nil, &execution.PathNotWritableError{Expression: expression, Err: fmt.Errorf("path() returned %T, not an array", results[0])}
	}
	locations := make([][]any, len(rawLocations))
	for i, rawLocation := range rawLocations {
		location, ok := rawLocation.([]any)
		if !ok {
			return nil, &execution.PathNotWritableError{Expression: expression, Err: fmt.Errorf("path %d is %T, not an array", i, rawLocation)}
		}
		locations[i] = location
	}
	return locations, nil
}

// spliceLocations sets each location to its value in one atomic update. It
// builds an expression that addresses exactly those locations, getpath(l1),
// getpath(l2), ..., and assigns through AssignZip, so any Runner can commit.
func spliceLocations(ctx context.Context, runner execution.Assigner, locations [][]any, values []any) error {
	if len(locations) == 0 {
		return nil
	}
	addresses := make([]string, len(locations))
	for i, location := range locations {
		encoded, err := json.Marshal(location)
		if err != nil {
			return fmt.Errorf("encode location %v: %w", location, err)
		}
		addresses[i] = "getpath(" + string(encoded) + ")"
	}
	return runner.AssignZip(ctx, strings.Join(addresses, ", "), values)
}

// alignValues pairs resolved locations with values. Definitions with instance
// ids write one value per location; the rest write the single value everywhere.
func alignValues(definition v1alpha1.ComponentDefinition, locations int, values []any, path string) ([]any, error) {
	if definition.InstanceIdPath != nil {
		if locations != len(values) {
			return nil, &InstanceCountMismatchError{Path: path, Locations: locations, Values: len(values)}
		}
		return values, nil
	}
	if len(values) != 1 {
		return nil, &InstanceCountMismatchError{Path: path, Locations: locations, Values: len(values)}
	}
	aligned := make([]any, locations)
	for i := range aligned {
		aligned[i] = values[0]
	}
	return aligned, nil
}

// mergeFragment computes the minimal patch between the stored fragment and the
// supplied value and applies it to the raw fragment. changed is false when the
// supplied value equals the stored projection.
func mergeFragment(fragment any, value any, schema any) (merged any, changed bool, err error) {
	if list, ok := value.(keyedListMerger); ok {
		return mergeKeyedList(fragment, list)
	}
	newJSON, err := json.Marshal(value)
	if err != nil {
		return nil, false, fmt.Errorf("marshal supplied value: %w", err)
	}
	if err := rejectDirectiveKeys(newJSON); err != nil {
		return nil, false, err
	}
	if schema == nil {
		return mergePlain(fragment, newJSON)
	}
	return mergeStrategic(fragment, newJSON, schema)
}

// mergeKeyedList wraps a bare list fragment and its write under a struct that
// carries the merge keys, merges per named element, and unwraps the result.
func mergeKeyedList(fragment any, list keyedListMerger) (any, bool, error) {
	wrapped, field := list.keyedList()
	merged, changed, err := mergeFragment(map[string]any{field: fragment}, wrapped, corev1.PodSpec{})
	if err != nil || !changed {
		return nil, changed, err
	}
	items := merged.(map[string]any)[field]
	if items == nil {
		// An explicit clear merges to null; the stored value is the empty list
		// the consumer supplied.
		items = []any{}
	}
	return items, true, nil
}

// mergeStrategic diffs through the schema's patch merge keys and applies the
// patch onto the raw fragment map, so keyed lists merge per named element and
// unknown sibling fields survive.
func mergeStrategic(fragment any, newJSON []byte, schema any) (any, bool, error) {
	baselineJSON, err := reproject(fragment, schema)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(baselineJSON, newJSON) {
		return nil, false, nil
	}
	var baseline, supplied map[string]any
	if err := json.Unmarshal(baselineJSON, &baseline); err != nil {
		return nil, false, fmt.Errorf("decode baseline projection: %w", err)
	}
	if err := json.Unmarshal(newJSON, &supplied); err != nil {
		return nil, false, fmt.Errorf("decode supplied projection: %w", err)
	}
	base, _ := fragment.(map[string]any)
	raw := deepCopyMap(base)
	projectResizedGroups(raw, baseline, supplied, schema)
	for _, object := range []map[string]any{baseline, supplied, raw} {
		if err := transformCompositeKeys(object, schema, false); err != nil {
			return nil, false, err
		}
	}
	patchMap, err := strategicpatch.CreateTwoWayMergeMapPatch(baseline, supplied, schema)
	if err != nil {
		return nil, false, fmt.Errorf("diff against stored fragment: %w", err)
	}
	if len(patchMap) == 0 {
		return nil, false, nil
	}
	// The projection types carry retainKeys strategies upstream; honoring them
	// would delete fields the projection cannot see. Stripping the directive
	// keeps unknown element fields, and a merge the projection cannot express
	// (a union flip) fails verification instead of corrupting.
	stripRetainKeys(map[string]any(patchMap))
	merged, err := mergeStoredFragment(raw, patchMap, schema)
	if err != nil {
		return nil, false, fmt.Errorf("%w: merge patch onto stored fragment: %w", ErrFragmentShape, err)
	}
	if err := transformCompositeKeys(merged, schema, true); err != nil {
		return nil, false, err
	}
	if err := verifyProjection(merged, newJSON, schema); err != nil {
		return nil, false, err
	}
	return map[string]any(merged), true, nil
}

// mergeStoredFragment applies the patch onto the raw fragment. A stored keyed
// list may mix a null entry with objects (the projection reads the null as a
// zero value); strategicpatch panics on that when the null comes first
// (sliceElementType) and returns an element type error otherwise. Both mean
// the stored list has a shape the merge cannot process, and the caller reports
// them as ErrFragmentShape. A patch that does not touch the list never reaches
// it, so unrelated edits still land.
func mergeStoredFragment(raw map[string]any, patch strategicpatch.JSONMap, schema any) (merged strategicpatch.JSONMap, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("strategic merge failed on the stored lists: %v", recovered)
		}
	}()
	return strategicpatch.StrategicMergeMapPatch(raw, patch, schema)
}

// mergePlain handles schemaless fragments, which are leaf values the consumer
// owns outright (a label map, a scheduler name): equal values skip the write,
// different values replace the leaf.
func mergePlain(fragment any, newJSON []byte) (any, bool, error) {
	var value any
	if err := json.Unmarshal(newJSON, &value); err != nil {
		return nil, false, fmt.Errorf("decode supplied value: %w", err)
	}
	if reflect.DeepEqual(fragment, value) {
		return nil, false, nil
	}
	return value, true, nil
}

// reproject reads the raw fragment back through the projection type, so both
// diff sides carry identical struct-shape loss and the loss cancels.
func reproject(fragment any, schema any) ([]byte, error) {
	fragmentJSON, err := json.Marshal(fragment)
	if err != nil {
		return nil, fmt.Errorf("marshal stored fragment: %w", err)
	}
	projected := reflect.New(reflect.TypeOf(schema)).Interface()
	if fragment != nil {
		if err := json.Unmarshal(fragmentJSON, projected); err != nil {
			return nil, fmt.Errorf("%w (%T): %w", ErrFragmentShape, schema, err)
		}
	}
	baselineJSON, err := json.Marshal(projected)
	if err != nil {
		return nil, fmt.Errorf("marshal baseline projection: %w", err)
	}
	return baselineJSON, nil
}

// verifyProjection proves, before anything is written, that reading the merged
// fragment back through the projection type yields exactly the supplied value.
// A surprising merge result rejects the write instead of corrupting the object.
func verifyProjection(merged any, supplied []byte, schema any) error {
	readBack, err := reproject(merged, schema)
	if err != nil {
		return err
	}
	if !bytes.Equal(readBack, supplied) {
		return ErrReadBackMismatch
	}
	return nil
}

// guardIdentity refuses root-path writes that change the object's identity:
// a template projection declared at "." must never rewrite what the object is.
func guardIdentity(current any, merged any) error {
	currentMap, _ := current.(map[string]any)
	mergedMap, _ := merged.(map[string]any)
	for _, key := range identityKeys {
		if !reflect.DeepEqual(currentMap[key], mergedMap[key]) {
			return fmt.Errorf("%w: root write would change '%s'", ErrIdentityChange, key)
		}
	}
	currentMeta, _ := currentMap["metadata"].(map[string]any)
	mergedMeta, _ := mergedMap["metadata"].(map[string]any)
	for _, key := range []string{"name", "uid"} {
		if !reflect.DeepEqual(currentMeta[key], mergedMeta[key]) {
			return fmt.Errorf("%w: root write would change 'metadata.%s'", ErrIdentityChange, key)
		}
	}
	return nil
}

// rejectDirectiveKeys refuses values carrying strategic-merge directives as
// literal keys, which the merge would interpret instead of storing.
func rejectDirectiveKeys(valueJSON []byte) error {
	var value any
	if err := json.Unmarshal(valueJSON, &value); err != nil {
		return fmt.Errorf("decode supplied value: %w", err)
	}
	return walkKeys(value, func(key string) error {
		if strings.HasPrefix(key, "$") {
			return fmt.Errorf("%w: '%s'", ErrDirectiveKey, key)
		}
		return nil
	})
}

func walkKeys(value any, check func(string) error) error {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if err := check(key); err != nil {
				return err
			}
			if err := walkKeys(child, check); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := walkKeys(child, check); err != nil {
				return err
			}
		}
	}
	return nil
}

// canonicalLocation validates a resolved location's tokens: map keys and plain
// array indexes only. Negative indexes alias locations the overlap check cannot
// see, and slice tokens shift later locations when their size changes.
func canonicalLocation(location []any) ([]any, error) {
	canonical := make([]any, len(location))
	for i, token := range location {
		switch t := token.(type) {
		case string:
			canonical[i] = t
		case int:
			if t < 0 {
				return nil, fmt.Errorf("negative array index %d is not writable", t)
			}
			canonical[i] = t
		case float64:
			if t < 0 || t != float64(int(t)) {
				return nil, fmt.Errorf("array index %v is not writable", t)
			}
			canonical[i] = int(t)
		default:
			return nil, fmt.Errorf("location token %v (%T) is not writable", token, token)
		}
	}
	return canonical, nil
}

// fragmentAt walks a canonical location over the decoded object. An absent
// fragment returns nil: it diffs against an empty baseline, and stage lets
// the write create it only below a stored component root.
func fragmentAt(object any, location []any) any {
	current := object
	for _, token := range location {
		switch t := token.(type) {
		case string:
			m, ok := current.(map[string]any)
			if !ok {
				return nil
			}
			current = m[t]
		case int:
			s, ok := current.([]any)
			if !ok || t >= len(s) {
				return nil
			}
			current = s[t]
		}
	}
	return current
}

// stripRetainKeys removes $retainKeys directives a generated patch carries.
func stripRetainKeys(value any) {
	switch v := value.(type) {
	case map[string]any:
		delete(v, "$retainKeys")
		for _, child := range v {
			stripRetainKeys(child)
		}
	case []any:
		for _, child := range v {
			stripRetainKeys(child)
		}
	}
}

// isOverlapping reports whether one location is the other or contains it.
func isOverlapping(a, b []any) bool {
	shorter := min(len(a), len(b))
	for i := range shorter {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	copied := make(map[string]any, len(m))
	for key, value := range m {
		copied[key] = deepCopyValue(value)
	}
	return copied
}

func deepCopyValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return deepCopyMap(v)
	case []any:
		copied := make([]any, len(v))
		for i, item := range v {
			copied[i] = deepCopyValue(item)
		}
		return copied
	default:
		return v
	}
}
