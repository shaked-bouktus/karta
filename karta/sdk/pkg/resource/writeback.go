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

	"github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/jq/execution"
)

// Causes a refused write wraps, for errors.Is.
var (
	// ErrFragmentShape: the stored fragment does not decode or holds a null entry.
	ErrFragmentShape = errors.New("stored fragment does not fit the projection type")
	// ErrReadBackMismatch: the merge cannot express the change.
	ErrReadBackMismatch = errors.New("merged fragment reads back differently than the supplied value")
	// ErrConflictingWrites: two views of one value carry different changes.
	ErrConflictingWrites = errors.New("conflicting writes to the same value")
	// ErrIdentityChange: a root write would change what the object is.
	ErrIdentityChange = errors.New("write would change the object identity")
	// ErrDirectiveKey: a value carries a key strategic merge would interpret.
	ErrDirectiveKey = errors.New("value carries a strategic merge directive key")
	// ErrFragmentAbsent: the write would create a component the object lacks.
	ErrFragmentAbsent = errors.New("object does not store the fragment, the write would create it")
)

// WriteError reports a refused update. Nothing was written.
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

// InstanceCountMismatchError reports locations and values that do not pair up.
type InstanceCountMismatchError struct {
	Path      string
	Locations int
	Values    int
}

func (e *InstanceCountMismatchError) Error() string {
	return fmt.Sprintf("path '%s' resolves %d locations but %d values were supplied", e.Path, e.Locations, e.Values)
}

// writePlan stages writes against one snapshot and commits them atomically.
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

// keyedListMerger names the PodSpec field whose merge keys a bare list uses.
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

// identityKeys never change through a write at the object root.
var identityKeys = []string{"apiVersion", "kind", "status"}

// optionalWrappers may be missing from a stored template (nil affinity, empty
// metadata); a field under one belongs to the component above it.
var optionalWrappers = map[string]bool{"affinity": true, "metadata": true}

// mergeWrite writes values at path as one staged plan.
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

// stage merges one projection write into the plan without touching the object.
// Per location it diffs the value against the stored fragment read through
// the projection type, merges the patch onto the raw fragment so unknown
// fields survive, verifies the read-back, and refuses creating an absent
// component. A nil value skips its location; a nil schema means a plain leaf.
// A computed path is a read-only view: writing back what it reads is a no-op,
// and a change fails with PathNotWritableError. Lists without a merge key are
// atomic and replaced whole, as in Kubernetes.
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

// componentRoots returns, per location, where the object stores the component
// a write addresses; a write may fill fields below it but never create it. The
// root is the fragment's parent or, for a fragmented definition, the deepest
// location its fields share (see fieldRoot). Cached per instance count.
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
		root := fieldRoot(paths[i])
		for _, locations := range aligned {
			other := fieldRoot(locations[i])
			shared := 0
			for shared < len(root) && shared < len(other) && root[shared] == other[shared] {
				shared++
			}
			root = root[:shared]
		}
		roots[i] = root
	}
	return p.cacheRoots(len(paths), roots)
}

// fieldRoot is a field's parent, less an optional wrapper.
func fieldRoot(location []any) []any {
	root := location[:max(len(location)-1, 0)]
	if len(root) > 0 {
		if wrapper, ok := root[len(root)-1].(string); ok && optionalWrappers[wrapper] {
			root = root[:len(root)-1]
		}
	}
	return root
}

func (p *writePlan) cacheRoots(instances int, roots [][]any) [][]any {
	if p.roots == nil {
		p.roots = map[int][][]any{}
	}
	p.roots[instances] = roots
	return roots
}

// isUnchanged reports whether every value equals what path reads now.
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
		// jq may compute ints; compare as decoded JSON.
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

// add stages a write, three-way combining it with overlapping staged writes.
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

// combineWrites three-way merges two changed copies of baseline; a value both
// sides changed differently is a conflict.
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
		// Arrays are atomic: a parent write may move the element a child addresses.
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

// commit splices every staged write in one atomic update.
func (p *writePlan) commit(ctx context.Context) error {
	if len(p.paths) == 0 {
		return nil
	}
	return spliceLocations(ctx, p.accessor.jqRunner, p.paths, p.values)
}

// resolveLocations returns the locations an expression addresses, or
// PathNotWritableError when it computes a value.
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

// spliceLocations sets each location through one AssignZip over getpath()
// expressions, so any Runner can commit.
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

// alignValues pairs locations with values: one per instance with instance ids,
// otherwise the single value everywhere.
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

// mergeFragment merges value onto the stored fragment; changed is false when it
// equals the stored projection.
func mergeFragment(fragment any, value any, schema any) (merged any, changed bool, err error) {
	if list, ok := value.(keyedListMerger); ok {
		return mergeKeyedList(fragment, list)
	}
	newJSON, err := json.Marshal(value)
	if err != nil {
		return nil, false, fmt.Errorf("marshal supplied value: %w", err)
	}
	if schema == nil {
		return mergePlain(fragment, newJSON)
	}
	return mergeStrategic(fragment, newJSON, schema)
}

// mergeKeyedList merges a bare list through its PodSpec field.
func mergeKeyedList(fragment any, list keyedListMerger) (any, bool, error) {
	wrapped, field := list.keyedList()
	merged, changed, err := mergeFragment(map[string]any{field: fragment}, wrapped, corev1.PodSpec{})
	if err != nil || !changed {
		return nil, changed, err
	}
	items := merged.(map[string]any)[field]
	if items == nil {
		// An explicit clear merges to null; store the supplied empty list.
		items = []any{}
	}
	return items, true, nil
}

// mergeStrategic diffs through the schema's merge keys and patches the raw
// fragment, so keyed lists merge per element and unknown fields survive.
func mergeStrategic(fragment any, newJSON []byte, schema any) (any, bool, error) {
	baselineJSON, err := reproject(fragment, schema)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(baselineJSON, newJSON) {
		return nil, false, nil
	}
	if err := rejectDirectiveKeys(newJSON); err != nil {
		return nil, false, err
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
	// Upstream retainKeys strategies would delete fields the projection cannot
	// see; dropping them keeps unknown fields.
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

// mergeStoredFragment applies the patch. strategicpatch panics on a keyed list
// with a null entry; that becomes an error.
func mergeStoredFragment(raw map[string]any, patch strategicpatch.JSONMap, schema any) (merged strategicpatch.JSONMap, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("strategic merge failed on the stored lists: %v", recovered)
		}
	}()
	return strategicpatch.StrategicMergeMapPatch(raw, patch, schema)
}

// mergePlain replaces a schemaless leaf (labels, a scheduler name) unless equal.
func mergePlain(fragment any, newJSON []byte) (any, bool, error) {
	var value any
	if err := json.Unmarshal(newJSON, &value); err != nil {
		return nil, false, fmt.Errorf("decode supplied value: %w", err)
	}
	if reflect.DeepEqual(fragment, value) {
		return nil, false, nil
	}
	if err := rejectDirectiveKeys(newJSON); err != nil {
		return nil, false, err
	}
	return value, true, nil
}

// reproject reads the fragment through the projection type, so both diff sides
// lose the same fields.
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

// verifyProjection checks the merged fragment reads back as the supplied value.
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

// guardIdentity refuses a root write that changes the object identity.
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
	for _, key := range []string{"name", "namespace", "uid"} {
		if !reflect.DeepEqual(currentMeta[key], mergedMeta[key]) {
			return fmt.Errorf("%w: root write would change 'metadata.%s'", ErrIdentityChange, key)
		}
	}
	return nil
}

// rejectDirectiveKeys refuses the keys strategicpatch interprets. Other "$"
// keys are data: CSI volume attributes use them.
func rejectDirectiveKeys(valueJSON []byte) error {
	var value any
	if err := json.Unmarshal(valueJSON, &value); err != nil {
		return fmt.Errorf("decode supplied value: %w", err)
	}
	return walkKeys(value, func(key string) error {
		if key == "$patch" || key == "$retainKeys" ||
			strings.HasPrefix(key, "$setElementOrder/") || strings.HasPrefix(key, "$deleteFromPrimitiveList/") {
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

// canonicalLocation accepts map keys and plain indexes: negative indexes alias
// locations and slices shift them.
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

// fragmentAt returns the value at location, or nil when it is absent.
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
