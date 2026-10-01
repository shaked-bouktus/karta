// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"

	"github.com/dsx-ai-factory/workload-map/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/pkg/jq/execution"
)

// WriteVerificationError reports an update whose merged result would not read
// back as the value the consumer supplied. Nothing was written.
type WriteVerificationError struct {
	Path string
	Err  error
}

func (e *WriteVerificationError) Error() string {
	return fmt.Sprintf("write to '%s' failed verification, nothing was written: %v", e.Path, e.Err)
}

func (e *WriteVerificationError) Unwrap() error {
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
}

// keyedListMerger carries a bare list write together with the patch merge keys
// its elements need: the pod spec's own tags do not travel with a bare slice.
type keyedListMerger interface {
	keyedList() (wrapped any, schema any)
}

type containersValue []corev1.Container

func (v containersValue) keyedList() (any, any) {
	type wrapper struct {
		Items []corev1.Container `json:"items" patchStrategy:"merge" patchMergeKey:"name"`
	}
	return wrapper{Items: v}, wrapper{}
}

type resourceClaimsValue []corev1.PodResourceClaim

func (v resourceClaimsValue) keyedList() (any, any) {
	type wrapper struct {
		Items []corev1.PodResourceClaim `json:"items" patchStrategy:"merge" patchMergeKey:"name"`
	}
	return wrapper{Items: v}, wrapper{}
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
//  6. refuse locations that overlap a write already staged in this plan
//
// A nil value skips its location. schema carries the patch merge keys (for
// example corev1.PodSpec); a nil schema treats the fragment as a plain leaf.
func (p *writePlan) stage(ctx context.Context, definition v1alpha1.ComponentDefinition, path string, values []any, schema any) error {
	paths, err := p.accessor.jqRunner.ResolvePaths(ctx, path)
	if err != nil {
		return err
	}
	for i := range paths {
		if paths[i], err = canonicalLocation(paths[i]); err != nil {
			return &execution.PathNotWritableError{Expression: path, Err: err}
		}
	}
	values, err = alignValues(definition, paths, values, path)
	if err != nil {
		return err
	}
	object, err := p.accessor.jqRunner.GetObject()
	if err != nil {
		return err
	}

	for i, location := range paths {
		if values[i] == nil {
			continue
		}
		fragment := fragmentAt(object, location)
		merged, changed, err := mergeFragment(fragment, values[i], schema)
		if err != nil {
			return &WriteVerificationError{Path: path, Err: err}
		}
		if !changed {
			continue
		}
		if len(location) == 0 {
			if err := guardIdentity(fragment, merged); err != nil {
				return &WriteVerificationError{Path: path, Err: err}
			}
		}
		if err := p.add(location, merged); err != nil {
			return &WriteVerificationError{Path: path, Err: err}
		}
	}
	return nil
}

// add appends a write, refusing locations that overlap one already staged:
// two writes into the same subtree would make the result order-dependent.
func (p *writePlan) add(location []any, value any) error {
	for _, staged := range p.paths {
		if isOverlapping(staged, location) {
			return fmt.Errorf("write at %v overlaps a write already staged at %v", location, staged)
		}
	}
	p.paths = append(p.paths, location)
	p.values = append(p.values, value)
	return nil
}

// commit splices every staged write in one atomic update. An empty plan leaves
// the object untouched.
func (p *writePlan) commit(ctx context.Context) error {
	if len(p.paths) == 0 {
		return nil
	}
	return p.accessor.jqRunner.SpliceValues(ctx, p.paths, p.values)
}

// alignValues pairs resolved locations with values. Definitions with instance
// ids write one value per location; the rest write the single value everywhere.
func alignValues(definition v1alpha1.ComponentDefinition, paths [][]any, values []any, path string) ([]any, error) {
	if definition.InstanceIdPath != nil {
		if len(paths) != len(values) {
			return nil, &InstanceCountMismatchError{Path: path, Locations: len(paths), Values: len(values)}
		}
		return values, nil
	}
	if len(values) != 1 {
		return nil, &InstanceCountMismatchError{Path: path, Locations: len(paths), Values: len(values)}
	}
	aligned := make([]any, len(paths))
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
	return mergeStrategic(fragment, newJSON, value, schema)
}

// mergeKeyedList wraps a bare list fragment and its write under a struct that
// carries the merge keys, merges per named element, and unwraps the result.
func mergeKeyedList(fragment any, list keyedListMerger) (any, bool, error) {
	wrapped, schema := list.keyedList()
	merged, changed, err := mergeFragment(map[string]any{"items": fragment}, wrapped, schema)
	if err != nil || !changed {
		return nil, changed, err
	}
	items := merged.(map[string]any)["items"]
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
func mergeStrategic(fragment any, newJSON []byte, value any, schema any) (any, bool, error) {
	baselineJSON, err := reproject(fragment, schema)
	if err != nil {
		return nil, false, err
	}
	patch, err := strategicpatch.CreateTwoWayMergePatch(baselineJSON, newJSON, schema)
	if err != nil {
		return nil, false, fmt.Errorf("diff against stored fragment: %w", err)
	}
	var patchMap map[string]any
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, false, fmt.Errorf("decode patch: %w", err)
	}
	if len(patchMap) == 0 {
		return nil, false, nil
	}
	// The projection types carry retainKeys strategies upstream; honoring them
	// would delete fields the projection cannot see. Stripping the directive
	// keeps unknown element fields, and a merge the projection cannot express
	// (a union flip) fails verification instead of corrupting.
	stripRetainKeys(patchMap)
	base, _ := fragment.(map[string]any)
	merged, err := strategicpatch.StrategicMergeMapPatch(deepCopyMap(base), patchMap, schema)
	if err != nil {
		return nil, false, fmt.Errorf("merge patch onto stored fragment: %w", err)
	}
	if err := verifyProjection(merged, value, schema); err != nil {
		return nil, false, err
	}
	return map[string]any(merged), true, nil
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
			return nil, fmt.Errorf("stored fragment does not fit %T: %w", schema, err)
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
func verifyProjection(merged any, value any, schema any) error {
	readBack, err := reproject(merged, schema)
	if err != nil {
		return err
	}
	supplied, err := reproject(value, schema)
	if err != nil {
		return err
	}
	if string(readBack) != string(supplied) {
		return fmt.Errorf("merged fragment reads back differently than the supplied value")
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
			return fmt.Errorf("root write would change '%s'", key)
		}
	}
	currentMeta, _ := currentMap["metadata"].(map[string]any)
	mergedMeta, _ := mergedMap["metadata"].(map[string]any)
	for _, key := range []string{"name", "uid"} {
		if !reflect.DeepEqual(currentMeta[key], mergedMeta[key]) {
			return fmt.Errorf("root write would change 'metadata.%s'", key)
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
			return fmt.Errorf("key '%s' is a strategic merge directive and cannot be stored", key)
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
// fragment returns nil: it diffs against an empty baseline and gets created.
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
