// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package resource

import (
	"encoding/json"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
)

// containerFields are the pod spec lists whose elements are containers.
var containerFields = []string{"containers", "initContainers", "ephemeralContainers"}

// transformCompositeKeys adapts Kubernetes list identities to strategic merge's
// single-key API. Only private copies are transformed; the original key values
// are restored before verification or write-back. Unknown fields stay attached
// to their list element across edits, insertion, deletion, and reordering.
//
// Env vars may repeat a name: the kubelet uses the last definition, and
// X=$(X):more appends to an earlier one. Their identity is the name plus its
// occurrence, so every definition stays editable; projectResizedEnvGroups
// keeps matching by occurrence from moving unknown fields.
func transformCompositeKeys(object map[string]any, schema any, restore bool) error {
	switch schema.(type) {
	case corev1.PodTemplateSpec:
		spec, _ := object["spec"].(map[string]any)
		return transformCompositeKeys(spec, corev1.PodSpec{}, restore)
	case corev1.PodSpec:
		for _, field := range containerFields {
			containers, _ := object[field].([]any)
			for _, raw := range containers {
				container, _ := raw.(map[string]any)
				if err := transformCompositeKeys(container, corev1.Container{}, restore); err != nil {
					return err
				}
			}
		}
		return transformListKeys(object["topologySpreadConstraints"], "topologyKey", restore, func(item map[string]any) any {
			return item["whenUnsatisfiable"]
		})
	case corev1.Container:
		err := transformListKeys(object["ports"], "containerPort", restore, func(item map[string]any) any {
			if protocol := item["protocol"]; protocol != nil && protocol != "" {
				return protocol
			}
			return string(corev1.ProtocolTCP)
		})
		if err != nil {
			return err
		}
		occurrences := map[string]int{}
		return transformListKeys(object["env"], "name", restore, func(item map[string]any) any {
			name, _ := item["name"].(string)
			occurrences[name]++
			return occurrences[name] - 1
		})
	default:
		return nil
	}
}

// transformListKeys encodes each entry's identity, its primary key plus the
// qualifier, into the primary key, or restores it. Stored entries that are not
// objects or lack the primary key are invalid for the apiserver but must not
// block unrelated writes: they keep a null identity, so an untouched entry
// round-trips and an edited one fails verification.
func transformListKeys(value any, primary string, restore bool, qualify func(item map[string]any) any) error {
	list, _ := value.([]any)
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

// projectResizedEnvGroups follows Update semantics for an env name whose
// number of definitions the write changes: the stored definitions of that name
// are replaced in raw by their projection, so the name's definitions come out
// exactly as supplied and lose the fields the projection cannot represent, as a
// PUT through the projection type would. Matching by occurrence is positional,
// so without this a removed or inserted definition would shift an unknown
// field onto another definition, and verification reads back only the
// projection. In-place edits, reorders and changes to other names keep every
// unknown field. raw, baseline and supplied are the stored fragment, its
// projection, and the supplied value, before transformCompositeKeys.
func projectResizedEnvGroups(raw, baseline, supplied map[string]any, schema any) {
	switch schema.(type) {
	case corev1.PodTemplateSpec:
		rawSpec, _ := raw["spec"].(map[string]any)
		baselineSpec, _ := baseline["spec"].(map[string]any)
		suppliedSpec, _ := supplied["spec"].(map[string]any)
		projectResizedEnvGroups(rawSpec, baselineSpec, suppliedSpec, corev1.PodSpec{})
	case corev1.PodSpec:
		for _, field := range containerFields {
			rawContainers, _ := raw[field].([]any)
			storedContainers, _ := baseline[field].([]any)
			suppliedContainers, _ := supplied[field].([]any)
			for i, entry := range storedContainers {
				stored, _ := entry.(map[string]any)
				var target map[string]any
				for _, candidate := range suppliedContainers {
					if c, _ := candidate.(map[string]any); c != nil && reflect.DeepEqual(c["name"], stored["name"]) {
						target = c
						break
					}
				}
				var rawContainer map[string]any
				if i < len(rawContainers) {
					rawContainer, _ = rawContainers[i].(map[string]any)
				}
				projectResizedEnvGroups(rawContainer, stored, target, corev1.Container{})
			}
		}
	case corev1.Container:
		rawEnv, _ := raw["env"].([]any)
		storedEnv, _ := baseline["env"].([]any)
		suppliedEnv, _ := supplied["env"].([]any)
		if supplied == nil || len(rawEnv) != len(storedEnv) {
			return
		}
		definitions := func(list []any) map[string]int {
			counts := map[string]int{}
			for _, entry := range list {
				envVar, _ := entry.(map[string]any)
				name, _ := envVar["name"].(string)
				counts[name]++
			}
			return counts
		}
		stored, wanted := definitions(storedEnv), definitions(suppliedEnv)
		for i, entry := range storedEnv {
			envVar, _ := entry.(map[string]any)
			name, _ := envVar["name"].(string)
			if (stored[name] > 1 || wanted[name] > 1) && stored[name] != wanted[name] {
				rawEnv[i] = deepCopyValue(entry)
			}
		}
	}
}
