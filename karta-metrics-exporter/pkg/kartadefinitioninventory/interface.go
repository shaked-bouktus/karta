// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package kartadefinitioninventory

import (
	"errors"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
)

type Interface interface {
	Lookup(schema.GroupVersionKind) (Selection, error)
}

type Selection struct {
	Definition *v1alpha1.Karta
	GVK        schema.GroupVersionKind
	Origin     Origin
}

type Origin struct {
	Source          Source
	Name            string
	UID             types.UID
	ResourceVersion string
}

type Source string

const (
	SourceCatalog Source = "catalog"
	SourceCluster Source = "cluster"
)

var (
	ErrInvalidOptions    = errors.New("invalid definition source options")
	ErrNotReady          = errors.New("definition sources not ready")
	ErrUnavailable       = errors.New("definition source unavailable")
	ErrPermissionDenied  = errors.New("definition source permission denied")
	ErrInvalidDefinition = errors.New("invalid Karta definition")
)

// Options selects definitions from the built-in catalog and from the cluster.
type Options struct {
	Catalog SourceOptions
	Cluster SourceOptions
}

// SourceOptions selects a definition when its name is in Include; an empty
// list allows all, and Exclude wins.
type SourceOptions struct {
	Enabled bool
	Include []string
	Exclude []string
}

// DefaultOptions reads the cluster's Karta definitions and leaves the catalog off.
func DefaultOptions() Options {
	return Options{Cluster: SourceOptions{Enabled: true}}
}
