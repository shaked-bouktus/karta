// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package resource

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// keyedList is one list of the projection type that strategic merge patch
// merges by a key: the JSON field, the merge key, and the element type.
type keyedList struct {
	field string
	key   string
	elem  reflect.Type
}

// transformCompositeKeys adapts Kubernetes list identities to strategic merge's
// single-key API. Only private copies are transformed; the original key values
// are restored before verification or write-back. Unknown fields stay attached
// to their list element across edits, insertion, deletion, and reordering.
//
// Every keyed list of the schema (containers, volumes, env, ports, hostAliases,
// imagePullSecrets, ...) is found through its patchMergeKey struct tag. Two
// lists have a real second key, the one server-side apply declares: ports are
// [containerPort, protocol] and topology spread constraints are [topologyKey,
// whenUnsatisfiable]. Any other list may repeat its key in a stored object (the
// kubelet uses the last env definition, X=$(X):more appends to an earlier one),
// so its identity is the key plus its occurrence, and every definition stays
// editable. projectResizedGroups keeps matching by occurrence from moving
// unknown fields.
func transformCompositeKeys(object map[string]any, schema any, restore bool) error {
	return walkKeyedLists(reflect.TypeOf(schema), object, func(list []any, keyed keyedList) error {
		qualify, ok := declaredQualifier(keyed.elem)
		if !ok {
			occurrences := map[string]int{}
			qualify = func(item map[string]any) any {
				identity := fmt.Sprint(item[keyed.key])
				occurrences[identity]++
				return occurrences[identity] - 1
			}
		}
		return transformListKeys(list, keyed.key, restore, qualify)
	})
}

// declaredQualifier returns the second key of a list whose identity the API
// declares as two fields, and false for a list keyed by one field.
func declaredQualifier(elem reflect.Type) (func(item map[string]any) any, bool) {
	switch elem {
	case reflect.TypeFor[corev1.ContainerPort]():
		return func(item map[string]any) any {
			if protocol := item["protocol"]; protocol != nil && protocol != "" {
				return protocol
			}
			return string(corev1.ProtocolTCP)
		}, true
	case reflect.TypeFor[corev1.TopologySpreadConstraint]():
		return func(item map[string]any) any { return item["whenUnsatisfiable"] }, true
	default:
		return nil, false
	}
}

// walkKeyedLists visits every keyed list the schema type declares, at the
// matching position in the decoded object: a list field tagged with
// patchMergeKey, then the elements' own keyed lists. Lists without a merge key
// are atomic and are not entered; the merge replaces their elements whole.
func walkKeyedLists(t reflect.Type, object map[string]any, visit func(list []any, keyed keyedList) error) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || object == nil {
		return nil
	}
	for i := range t.NumField() {
		field := t.Field(i)
		name, inline := jsonName(field)
		if name == "" && !inline {
			continue
		}
		if inline {
			if err := walkKeyedLists(field.Type, object, visit); err != nil {
				return err
			}
			continue
		}
		switch value := object[name].(type) {
		case map[string]any:
			if err := walkKeyedLists(field.Type, value, visit); err != nil {
				return err
			}
		case []any:
			keyed, ok := keyedListOf(field, name)
			if !ok {
				continue
			}
			if err := visit(value, keyed); err != nil {
				return err
			}
			for _, raw := range value {
				if item, ok := raw.(map[string]any); ok {
					if err := walkKeyedLists(keyed.elem, item, visit); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// keyedListOf describes a struct field that is a list merged by a key.
func keyedListOf(field reflect.StructField, name string) (keyedList, bool) {
	key := field.Tag.Get("patchMergeKey")
	elem := field.Type
	for elem.Kind() == reflect.Slice || elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	if key == "" || elem.Kind() != reflect.Struct {
		return keyedList{}, false
	}
	return keyedList{field: name, key: key, elem: elem}, true
}

// jsonName is the key encoding/json writes a struct field under, or inline for
// an embedded struct whose fields live on the parent object. An unexported or
// skipped field has no name.
func jsonName(field reflect.StructField) (name string, inline bool) {
	if field.PkgPath != "" {
		return "", false
	}
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, options, _ := strings.Cut(tag, ",")
	if strings.Contains(options, "inline") || (name == "" && field.Anonymous) {
		return "", true
	}
	if name == "" {
		name = field.Name
	}
	return name, false
}

// transformListKeys encodes each entry's identity, its primary key plus the
// qualifier, into the primary key, or restores it. Stored entries that are not
// objects or lack the primary key are invalid for the apiserver but must not
// block unrelated writes: they keep a null identity, so an untouched entry
// round-trips and an edited one fails verification.
func transformListKeys(list []any, primary string, restore bool, qualify func(item map[string]any) any) error {
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if restore {
			key, ok := item[primary].(string)
			if !ok {
				return fmt.Errorf("expected an encoded merge key for %s", primary)
			}
			var components [2]any
			if err := json.Unmarshal([]byte(key), &components); err != nil {
				return fmt.Errorf("restore merge key %s: %w", primary, err)
			}
			if components[0] == nil {
				delete(item, primary)
				continue
			}
			item[primary] = components[0]
			continue
		}
		encoded, err := json.Marshal([2]any{item[primary], qualify(item)})
		if err != nil {
			return fmt.Errorf("encode merge key %s: %w", primary, err)
		}
		item[primary] = string(encoded)
	}
	return nil
}

// projectResizedGroups follows Update semantics for a list key whose number of
// occurrences the write changes: the stored entries of that key are replaced in
// raw by their projection, so the key's entries come out exactly as supplied and
// lose the fields the projection cannot represent, as a PUT through the
// projection type would. Matching by occurrence is positional, so without this
// a removed or inserted entry would shift an unknown field onto another entry
// of the same key, and verification reads back only the projection. In-place
// edits, reorders and changes to other keys keep every unknown field. Lists
// with a declared second key group by both fields. raw, baseline and supplied
// are the stored fragment, its projection, and the supplied value, before
// transformCompositeKeys; raw and baseline are index-aligned.
func projectResizedGroups(raw, baseline, supplied map[string]any, schema any) {
	projectResizedGroupsOf(reflect.TypeOf(schema), raw, baseline, supplied)
}

func projectResizedGroupsOf(t reflect.Type, raw, baseline, supplied map[string]any) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || raw == nil || baseline == nil || supplied == nil {
		return
	}
	for i := range t.NumField() {
		field := t.Field(i)
		name, inline := jsonName(field)
		if name == "" && !inline {
			continue
		}
		if inline {
			projectResizedGroupsOf(field.Type, raw, baseline, supplied)
			continue
		}
		keyed, ok := keyedListOf(field, name)
		if !ok {
			rawChild, _ := raw[name].(map[string]any)
			storedChild, _ := baseline[name].(map[string]any)
			suppliedChild, _ := supplied[name].(map[string]any)
			projectResizedGroupsOf(field.Type, rawChild, storedChild, suppliedChild)
			continue
		}
		rawList, _ := raw[name].([]any)
		storedList, _ := baseline[name].([]any)
		suppliedList, _ := supplied[name].([]any)
		if len(rawList) != len(storedList) {
			continue
		}
		group := groupIdentity(keyed)
		stored, wanted := map[string]int{}, map[string]int{}
		for _, entry := range storedList {
			stored[group(entry)]++
		}
		for _, entry := range suppliedList {
			wanted[group(entry)]++
		}
		seen := map[string]int{}
		for i, entry := range storedList {
			g := group(entry)
			if (stored[g] > 1 || wanted[g] > 1) && stored[g] != wanted[g] {
				rawList[i] = deepCopyValue(entry)
			}
			// Recurse into the supplied entry with the same identity: same group,
			// same occurrence within it.
			occurrence := seen[g]
			seen[g]++
			var target map[string]any
			for _, candidate := range suppliedList {
				if group(candidate) != g {
					continue
				}
				if occurrence == 0 {
					target, _ = candidate.(map[string]any)
					break
				}
				occurrence--
			}
			rawItem, _ := rawList[i].(map[string]any)
			storedItem, _ := entry.(map[string]any)
			projectResizedGroupsOf(keyed.elem, rawItem, storedItem, target)
		}
	}
}

// groupIdentity names the group an entry belongs to: its merge key, plus the
// declared second key where the list has one.
func groupIdentity(keyed keyedList) func(entry any) string {
	qualify, declared := declaredQualifier(keyed.elem)
	return func(entry any) string {
		item, _ := entry.(map[string]any)
		if !declared {
			return fmt.Sprint(item[keyed.key])
		}
		return fmt.Sprint(item[keyed.key], "/", qualify(item))
	}
}
