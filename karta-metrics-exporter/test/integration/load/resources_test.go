// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"math"
	"reflect"
	"testing"
)

func TestParseProcessUsageSelectsOwnedProcesses(t *testing.T) {
	fixture := `
 100 1 1024 00:01.25 /tmp/load
 101 100 2048 01:02 /tmp/exporter
 102 100 1 00:00 /bin/sh
 103 102 4096 01:02:03 /tmp/control plane/kube-apiserver
 104 102 8192 2-03:04:05.50 /tmp/etcd
 200 1 999999 00:09 /other/kube-apiserver
 201 200 999999 00:09 /other/etcd
 105 100 999999 00:09 /bin/unrelated
 106 107 999999 00:09 /tmp/etcd
 107 106 0 00:00 /bin/sh
 108 999 999999 00:09 /tmp/etcd
`
	got, err := parseProcessUsage(fixture, 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	want := []ProcessUsage{
		{PID: 100, Role: "driver", RSSBytes: 1024 * 1024, CPUSeconds: 1.25},
		{PID: 101, Role: "exporter", RSSBytes: 2048 * 1024, CPUSeconds: 62},
		{PID: 103, Role: "apiserver", RSSBytes: 4096 * 1024, CPUSeconds: 3723},
		{PID: 104, Role: "etcd", RSSBytes: 8192 * 1024, CPUSeconds: 183845.5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestParseProcessUsageBeforeExporterStarts(t *testing.T) {
	got, err := parseProcessUsage("100 1 12 00:00 /tmp/load\n", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Role != "driver" || got[0].RSSBytes != 12*1024 {
		t.Fatalf("usage = %+v", got)
	}
}

func TestParseProcessUsageRejectsMissingAndInvalidMeasurements(t *testing.T) {
	for name, fixture := range map[string]string{
		"empty": "", "missing driver": "101 1 1 00:00 exporter",
		"missing exporter": "100 1 1 00:00 driver",
		"bad RSS":          "100 1 invalid 00:00 driver\n101 100 1 00:00 exporter",
		"overflow RSS":     "100 1 18446744073709551615 00:00 driver\n101 100 1 00:00 exporter",
		"bad CPU":          "100 1 1 bad driver\n101 100 1 00:00 exporter",
		"incomplete":       "100 1 1 00:00",
		"bad PID":          "bad 1 1 00:00 driver",
		"bad parent":       "100 bad 1 00:00 driver",
		"duplicate":        "100 1 1 00:00 driver\n100 1 1 00:00 driver",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := parseProcessUsage(fixture, 100, 101); err == nil || got != nil {
				t.Fatalf("got %+v, %v; want error without measurements", got, err)
			}
		})
	}
}

func TestParseCPUSeconds(t *testing.T) {
	for input, want := range map[string]float64{"00:00": 0, "00:01.23": 1.23, "12:34.56": 754.56, "01:02:03": 3723, "125:00:00": 450000, "125:00": 7500, "2-03:04:05.50": 183845.5} {
		t.Run(input, func(t *testing.T) {
			got, err := parseCPUSeconds(input)
			if err != nil || math.Abs(got-want) > 0.000001 {
				t.Fatalf("got %v, %v; want %v", got, err, want)
			}
		})
	}
	for _, input := range []string{"", "1", "1:2:3:4", "-1:02", "1-02:03", "1-24:00:00", "00:60", "01:60:00", "00:NaN", "00:Inf", "00:-1", "bad:00", "1:bad:00"} {
		t.Run("invalid_"+input, func(t *testing.T) {
			if _, err := parseCPUSeconds(input); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
