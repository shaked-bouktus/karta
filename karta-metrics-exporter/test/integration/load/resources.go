// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type ProcessUsage struct {
	PID        int     `json:"pid"`
	Role       string  `json:"role"`
	RSSBytes   uint64  `json:"rss_bytes"`
	CPUSeconds float64 `json:"cpu_seconds"`
}

type processRow struct {
	parent            int
	rss, cpu, command string
}

func sampleProcesses(ctx context.Context, driverPID, exporterPID int) ([]ProcessUsage, error) {
	// comm excludes arguments, which may contain credentials.
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,rss=,time=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("sample processes: %w", err)
	}
	return parseProcessUsage(string(output), driverPID, exporterPID)
}

func parseProcessUsage(output string, driverPID, exporterPID int) ([]ProcessUsage, error) {
	if driverPID <= 0 || exporterPID < 0 || exporterPID == driverPID {
		return nil, fmt.Errorf("invalid driver/exporter process IDs")
	}
	rows := make(map[int]processRow)
	var order []int
	for i, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 5 {
			return nil, fmt.Errorf("incomplete ps row %d", i+1)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("invalid PID in ps row %d", i+1)
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil || parent < 0 {
			return nil, fmt.Errorf("invalid parent PID in ps row %d", i+1)
		}
		if _, exists := rows[pid]; exists {
			return nil, fmt.Errorf("duplicate PID in ps row %d", i+1)
		}
		rows[pid] = processRow{parent: parent, rss: fields[2], cpu: fields[3], command: strings.Join(fields[4:], " ")}
		order = append(order, pid)
	}
	if _, exists := rows[driverPID]; !exists {
		return nil, fmt.Errorf("driver process %d missing from ps snapshot", driverPID)
	}
	if exporterPID > 0 {
		if _, exists := rows[exporterPID]; !exists {
			return nil, fmt.Errorf("exporter process %d missing from ps snapshot", exporterPID)
		}
	}
	var usage []ProcessUsage
	for _, pid := range order {
		row := rows[pid]
		var role string
		switch pid {
		case driverPID:
			role = "driver"
		case exporterPID:
			role = "exporter"
		default:
			switch filepath.Base(row.command) {
			case "kube-apiserver":
				role = "apiserver"
			case "etcd":
				role = "etcd"
			default:
				continue
			}
			ancestor := row.parent
			visited := map[int]bool{}
			for ancestor != driverPID && ancestor != 0 && !visited[ancestor] {
				visited[ancestor] = true
				ancestor = rows[ancestor].parent
			}
			if ancestor != driverPID {
				continue
			}
		}
		rssKiB, err := strconv.ParseUint(row.rss, 10, 64)
		if err != nil || rssKiB > math.MaxUint64/1024 {
			return nil, fmt.Errorf("invalid RSS for %s process %d", role, pid)
		}
		seconds, err := parseCPUSeconds(row.cpu)
		if err != nil {
			return nil, fmt.Errorf("CPU time for %s process %d: %w", role, pid, err)
		}
		usage = append(usage, ProcessUsage{PID: pid, Role: role, RSSBytes: rssKiB * 1024, CPUSeconds: seconds})
	}
	return usage, nil
}

func parseCPUSeconds(value string) (float64, error) {
	original := value
	days := uint64(0)
	hasDays := strings.Contains(value, "-")
	if hasDays {
		prefix, rest, _ := strings.Cut(value, "-")
		var err error
		days, err = strconv.ParseUint(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid CPU duration %q", original)
		}
		value = rest
	}
	fields := strings.Split(value, ":")
	if len(fields) < 2 || len(fields) > 3 || (hasDays && len(fields) != 3) {
		return 0, fmt.Errorf("invalid CPU duration %q", original)
	}
	hours := uint64(0)
	hasHours := len(fields) == 3
	if len(fields) == 3 {
		var err error
		hours, err = strconv.ParseUint(fields[0], 10, 64)
		if err != nil || (hasDays && hours >= 24) {
			return 0, fmt.Errorf("invalid CPU duration %q", original)
		}
		fields = fields[1:]
	}
	minutes, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || (hasHours && minutes >= 60) {
		return 0, fmt.Errorf("invalid CPU duration %q", original)
	}
	seconds, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds >= 60 {
		return 0, fmt.Errorf("invalid CPU duration %q", original)
	}
	return float64(days)*86400 + float64(hours)*3600 + float64(minutes)*60 + seconds, nil
}
