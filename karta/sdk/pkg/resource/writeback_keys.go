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

// emptyKeyMarker marks, on merge copies, an entry storing its key as "".
const emptyKeyMarker = "$kartaEmptyMergeKey"

// keyedList is a list the projection type merges by a key.
type keyedList struct {
	field string
	key   string
	elem  reflect.Type
}

// transformCompositeKeys encodes each keyed entry's identity (merge key,
// declared second field, occurrence) into its merge key, or restores it.
func transformCompositeKeys(object map[string]any, schema any, restore bool) error {
	return walkKeyedLists(reflect.TypeOf(schema), object, func(list []any, keyed keyedList) error {
		declared, hasDeclared := declaredQualifier(keyed.elem)
		occurrences := map[string]int{}
		qualify := func(item map[string]any) any {
			group := fmt.Sprint(mergeKeyOf(item, keyed.key))
			if !hasDeclared {
				occurrences[group]++
				return occurrences[group] - 1
			}
			second := declared(item)
			group = fmt.Sprint(group, "/", second)
			occurrences[group]++
			return []any{second, occurrences[group] - 1}
		}
		return transformListKeys(list, keyed.key, restore, qualify)
	})
}

// declaredQualifier returns a list's second identity field, if it has one.
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

// walkKeyedLists visits each keyed list of the schema, outer lists first; lists
// without a merge key are atomic and skipped.
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

// jsonName is a field's JSON key; inline marks an embedded struct.
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

// transformListKeys encodes [key, qualifier] into the key, or restores it. A
// missing or empty key encodes as null and restores to its stored form.
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
				if item[emptyKeyMarker] == true {
					item[primary] = ""
				}
				delete(item, emptyKeyMarker)
				continue
			}
			item[primary] = components[0]
			continue
		}
		if item[primary] == "" {
			item[emptyKeyMarker] = true
		}
		encoded, err := json.Marshal([2]any{mergeKeyOf(item, primary), qualify(item)})
		if err != nil {
			return fmt.Errorf("encode merge key %s: %w", primary, err)
		}
		item[primary] = string(encoded)
	}
	return nil
}

// projectResizedGroups gives Update semantics to a key whose count changes: its
// stored entries become their projection and lose unknown fields, so occurrence
// matching cannot move a field onto another entry.
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
			// Recurse into the supplied entry with the same identity.
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

// groupIdentity is an entry's merge key plus its declared second field.
func groupIdentity(keyed keyedList) func(entry any) string {
	qualify, declared := declaredQualifier(keyed.elem)
	return func(entry any) string {
		item, _ := entry.(map[string]any)
		if !declared {
			return fmt.Sprint(mergeKeyOf(item, keyed.key))
		}
		return fmt.Sprint(mergeKeyOf(item, keyed.key), "/", qualify(item))
	}
}

// mergeKeyOf is an entry's merge key, with "" read as absent.
func mergeKeyOf(item map[string]any, key string) any {
	if value := item[key]; value != "" {
		return value
	}
	return nil
}
