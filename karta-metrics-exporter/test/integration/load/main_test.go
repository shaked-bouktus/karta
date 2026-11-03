// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestParseSweepPreservesIncreasingPopulationsAndFractionalRates(t *testing.T) {
	roots, rates, err := parseSweep(" 1,1000, 100000,150000 ", "1, 12.5, 1000000")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(roots, []int{1, 1000, 100000, 150000}) || !slices.Equal(rates, []float64{1, 12.5, 1000000}) {
		t.Fatalf("unexpected sweep: %v %v", roots, rates)
	}
}

func TestParseSweepRejectsInvalidValues(t *testing.T) {
	cases := []struct{ name, roots, rates string }{
		{"empty roots", "", "1"},
		{"empty rates", "1", ""},
		{"zero roots", "0", "1"},
		{"negative roots", "-1", "1"},
		{"duplicate roots", "1,1", "1"},
		{"descending roots", "2,1", "1"},
		{"fractional roots", "1.5", "1"},
		{"overflow roots", "999999999999999999999999", "1"},
		{"trailing roots delimiter", "1,", "1"},
		{"zero rate", "1", "0"},
		{"negative rate", "1", "-1"},
		{"below minimum rate", "1", "0.9"},
		{"above maximum rate", "1", "1000000.1"},
		{"duplicate rate", "1", "1,1"},
		{"descending rate", "1", "2,1"},
		{"nan rate", "1", "NaN"},
		{"positive infinite rate", "1", "+Inf"},
		{"negative infinite rate", "1", "-Inf"},
		{"overflow rate", "1", "1e999"},
		{"trailing rate delimiter", "1", "1,"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			roots, rates, err := parseSweep(tt.roots, tt.rates)
			if err == nil || roots != nil || rates != nil {
				t.Fatalf("invalid sweep accepted: %v %v %v", roots, rates, err)
			}
		})
	}
}

func validTestOptions(t *testing.T) options {
	t.Helper()
	assets := t.TempDir()
	for _, name := range []string{"kube-apiserver", "etcd", "kubectl"} {
		path := filepath.Join(assets, name)
		if err := os.WriteFile(path, []byte("placeholder; not executed by validation tests\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return options{Scenario: "workload", Roots: []int{1}, Rates: []float64{1}, SetupRate: 1, Parallel: 1, MaxSamples: 1, MaxMemoryMiB: 1, StageTimeout: time.Second, MaxDuration: time.Minute, MaxStartLag: 50 * time.Millisecond, QuietPeriod: time.Second, Assets: assets}
}

func TestOptionsValidationRejectsUnsafeBudgets(t *testing.T) {
	valid := validTestOptions(t)
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*options)
	}{
		{"parallel", func(c *options) { c.Parallel = 0 }},
		{"workers", func(c *options) { c.Workers = -1 }},
		{"samples", func(c *options) { c.MaxSamples = 0 }},
		{"memory", func(c *options) { c.MaxMemoryMiB = 0 }},
		{"memory byte overflow", func(c *options) { c.MaxMemoryMiB = math.MaxUint64/(1024*1024) + 1 }},
		{"phase timeout", func(c *options) { c.StageTimeout = 0 }},
		{"run timeout", func(c *options) { c.MaxDuration = -1 }},
		{"arrival lag budget", func(c *options) { c.MaxStartLag = 0 }},
		{"quiet period", func(c *options) { c.QuietPeriod = 0 }},
		{"setup minimum", func(c *options) { c.SetupRate = .5 }},
		{"setup maximum", func(c *options) { c.SetupRate = 1000001 }},
		{"setup nan", func(c *options) { c.SetupRate = math.NaN() }},
		{"setup infinity", func(c *options) { c.SetupRate = math.Inf(1) }},
		{"no assets", func(c *options) { c.Assets = "" }},
		{"unknown scenario", func(c *options) { c.Scenario = "other" }},
		{"missing Karta CRD", func(c *options) { c.Scenario, c.CRDDirectory = "karta-update", t.TempDir() }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.change(&cfg)
			if cfg.validate() == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
}

func TestOptionsValidationRequiresExecutableFiles(t *testing.T) {
	for _, mode := range []string{"missing", "directory", "not executable"} {
		t.Run(mode, func(t *testing.T) {
			cfg := validTestOptions(t)
			path := filepath.Join(cfg.Assets, "etcd")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "not executable":
				if err := os.WriteFile(path, []byte("placeholder"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if cfg.validate() == nil {
				t.Fatal("unusable envtest asset accepted")
			}
		})
	}
}
