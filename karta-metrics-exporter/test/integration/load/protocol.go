// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"

type Command struct {
	Cohorts          []ConcurrentCohort     `json:"cohorts,omitempty"`
	Operation        string                 `json:"operation"`
	Expected         []Expectation          `json:"expected,omitempty"`
	Fixtures         []Fixture              `json:"fixtures,omitempty"`
	RootRevision     string                 `json:"root_revision,omitempty"`
	PodRevision      string                 `json:"pod_revision,omitempty"`
	Deleted          bool                   `json:"deleted,omitempty"`
	ProfilePath      string                 `json:"profile_path,omitempty"`
	Definition       *DefinitionExpectation `json:"definition,omitempty"`
	DefinitionOrigin *inventory.Origin      `json:"definition_origin,omitempty"`
}

type Response struct {
	Concurrent            []ConcurrentMeasurement `json:"concurrent,omitempty"`
	HealthTrace           *HealthTrace            `json:"health_trace,omitempty"`
	Error                 string                  `json:"error,omitempty"`
	Ready                 bool                    `json:"ready"`
	Valid                 bool                    `json:"valid"`
	Validation            string                  `json:"validation,omitempty"`
	Pods                  int                     `json:"pods"`
	Workloads             int                     `json:"workloads"`
	Stats                 RecorderStats           `json:"stats"`
	Measurement           *Measurement            `json:"measurement,omitempty"`
	DefinitionMeasurement *DefinitionMeasurement  `json:"definition_measurement,omitempty"`
}
