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

// keyedList is a list the projection type merges by a key.
type keyedList struct {
	field string
	key   string
	elem  reflect.Type
}

// transformCompositeKeys gives every keyed list of the schema an identity
// strategic merge can match on, on private copies; the keys are restored after
// the merge. Ports and topology spread constraints use the two fields
// server-side apply declares. Every other list is keyed by its merge key plus
// occurrence, since a stored object may repeat a key (two env definitions of
// PATH), and that keeps each entry editable with its unknown fields.
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

// declaredQualifier returns the second identity field of a list the API keys
// by two fields.
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

// walkKeyedLists visits each patchMergeKey list of the schema at its position
// in the object, then the lists inside its elements. Lists without a merge key
// are atomic and are skipped.
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

// jsonName is the JSON key of a struct field; inline marks an embedded struct
// whose fields live on the parent object.
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

// transformListKeys encodes [primary key, qualifier] into the primary key, or
// restores it. Entries that are not objects or lack the key keep a null
// identity, so an untouched one round-trips and an edited one fails
// verification.
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

// projectResizedGroups applies Update semantics to a key whose occurrence
// count the write changes: the stored entries of that key are replaced in raw
// by their projection, so they come out as supplied and drop unknown fields,
// as a PUT would. Otherwise occurrence matching could shift an unknown field
// onto another entry of the same key, which verification cannot see. Other
// keys keep their unknown fields. raw and baseline are index-aligned.
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

// groupIdentity returns an entry's merge key, plus the declared second field
// where the list has one.
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
