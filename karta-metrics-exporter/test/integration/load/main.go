// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type options struct {
	WorkloadPhase      string        `json:"workload_phase"`
	DiagnosticRootOnly bool          `json:"diagnostic_root_only"`
	DiagnosticAPIOnly  bool          `json:"diagnostic_api_only"`
	Scenario           string        `json:"scenario"`
	CRDDirectory       string        `json:"crd_directory,omitempty"`
	Roots              []int         `json:"roots"`
	Rates              []float64     `json:"rates_per_second"`
	SetupRate          float64       `json:"setup_chains_per_second"`
	Parallel           int           `json:"generator_parallelism"`
	Workers            int           `json:"exporter_workers"`
	MaxSamples         int           `json:"max_samples_per_phase"`
	MaxMemoryMiB       uint64        `json:"max_combined_rss_mib"`
	MaxStartLag        time.Duration `json:"max_start_lag_ns"`
	QuietPeriod        time.Duration `json:"publication_quiet_period_ns"`
	StageTimeout       time.Duration `json:"phase_budget_ns"`
	MaxDuration        time.Duration `json:"run_budget_ns"`
	Assets             string        `json:"assets"`
	Artifacts          string        `json:"artifacts"`
}

func main() {
	var cfg options
	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	var roots, rates, kubeconfig string
	var child bool
	flags.BoolVar(&cfg.DiagnosticRootOnly, "diagnostic-root-only", false, "diagnostic: stop after the first root update wave")
	flags.BoolVar(&cfg.DiagnosticAPIOnly, "diagnostic-api-only", false, "diagnostic: preload and first root wave without exporter; no Store latency or validation")
	flags.BoolVar(&child, "child", false, "internal exporter subprocess mode")
	flags.StringVar(&cfg.WorkloadPhase, "workload-phase", "all", "all, root-update, pod-update, delete, or live-create")
	flags.StringVar(&cfg.Scenario, "scenario", "workload", "workload, karta-update, or karta-concurrent")
	flags.StringVar(&cfg.CRDDirectory, "crds", "charts/karta/crds", "Karta CRD directory for karta-update, relative to the working directory")
	flags.StringVar(&kubeconfig, "kubeconfig", "", "internal subprocess kubeconfig")
	flags.StringVar(&roots, "roots", "1000", "increasing comma-separated root populations; each has one ReplicaSet and one Pod")
	flags.StringVar(&rates, "rates", "100", "comma-separated offered mutation rates per second")
	flags.Float64Var(&cfg.SetupRate, "setup-rate", 200, "maximum fixture chains per second during preload and lifecycle phases")
	flags.IntVar(&cfg.Parallel, "parallel", 32, "maximum concurrent generator operations")
	flags.IntVar(&cfg.Workers, "workers", 0, "exporter worker setting; zero keeps production default")
	flags.IntVar(&cfg.MaxSamples, "max-samples", 1000000, "maximum observed events and API samples retained per phase")
	flags.Uint64Var(&cfg.MaxMemoryMiB, "max-memory-mib", 12288, "sampled combined RSS stop budget for driver, exporter, API server, and etcd")
	flags.DurationVar(&cfg.MaxStartLag, "max-start-lag", 50*time.Millisecond, "maximum accepted arrival scheduling lag; later arrivals are counted as missed")
	flags.DurationVar(&cfg.QuietPeriod, "quiet-period", 2*time.Second, "required publication inactivity before each load phase")
	flags.DurationVar(&cfg.StageTimeout, "phase-timeout", 5*time.Minute, "budget for each setup, load, or convergence phase")
	flags.DurationVar(&cfg.MaxDuration, "max-duration", 30*time.Minute, "total run time budget")
	flags.StringVar(&cfg.Assets, "assets", os.Getenv("KUBEBUILDER_ASSETS"), "directory of pinned envtest binaries")
	flags.StringVar(&cfg.Artifacts, "artifacts", filepath.Join(".artifacts", "exporter-load", time.Now().UTC().Format("20060102T150405.000000000Z")), "new directory for results and profiles")
	if err := flags.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	if child {
		err = runChild(ctx, kubeconfig, cfg.MaxSamples, cfg.Workers, cfg.Scenario)
	} else {
		cfg.Roots, cfg.Rates, err = parseSweep(roots, rates)
		if err == nil {
			err = cfg.validate()
		}
		if err == nil {
			err = runSweep(ctx, cfg)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseSweep(roots, rates string) ([]int, []float64, error) {
	var populations []int
	var offered []float64
	for _, value := range strings.Split(roots, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n <= 0 || (len(populations) > 0 && n <= populations[len(populations)-1]) {
			return nil, nil, fmt.Errorf("roots must be positive and strictly increasing: %q", roots)
		}
		populations = append(populations, n)
	}
	for _, value := range strings.Split(rates, ",") {
		n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || n < 1 || n > 1000000 || math.IsNaN(n) || math.IsInf(n, 0) || (len(offered) > 0 && n <= offered[len(offered)-1]) {
			return nil, nil, fmt.Errorf("rates must be increasing, finite, and between 1 and 1000000: %q", rates)
		}
		offered = append(offered, n)
	}
	return populations, offered, nil
}

func (cfg options) validate() error {
	if err := cfg.validateDiagnostics(); err != nil {
		return err
	}
	if cfg.Scenario != "workload" && cfg.Scenario != "karta-update" && cfg.Scenario != "karta-concurrent" {
		return fmt.Errorf("scenario must be workload, karta-update, or karta-concurrent")
	}
	if cfg.WorkloadPhase != "" && cfg.WorkloadPhase != "all" && cfg.WorkloadPhase != "root-update" && cfg.WorkloadPhase != "pod-update" && cfg.WorkloadPhase != "delete" && cfg.WorkloadPhase != "live-create" {
		return fmt.Errorf("invalid workload phase %q", cfg.WorkloadPhase)
	}
	if cfg.Scenario != "workload" && cfg.WorkloadPhase != "" && cfg.WorkloadPhase != "all" {
		return fmt.Errorf("workload-phase applies only to workload scenario")
	}
	if cfg.DiagnosticRootOnly && cfg.WorkloadPhase != "" && cfg.WorkloadPhase != "all" && cfg.WorkloadPhase != "root-update" {
		return fmt.Errorf("diagnostic-root-only conflicts with workload-phase")
	}
	if cfg.Scenario == "karta-concurrent" {
		for _, n := range cfg.Roots {
			if n < 3 {
				return fmt.Errorf("concurrent scenario requires at least three roots")
			}
		}
	}
	if cfg.Scenario == "karta-update" || cfg.Scenario == "karta-concurrent" {
		if info, err := os.Stat(filepath.Join(cfg.CRDDirectory, "run.ai_kartas.yaml")); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("karta-update requires --crds pointing to the Karta CRD directory")
		}
		for _, roots := range cfg.Roots {
			if cfg.Scenario == "karta-concurrent" {
				if cfg.MaxSamples < 3 || roots > (cfg.MaxSamples-3)/4 {
					return fmt.Errorf("karta-concurrent requires max-samples >= four times each root population plus three definition receipts")
				}
				continue
			}
			if roots > cfg.MaxSamples/2 {
				return fmt.Errorf("karta-update requires max-samples >= twice each root population")
			}
		}
	}
	if cfg.Parallel <= 0 || cfg.Workers < 0 || cfg.MaxSamples <= 0 || cfg.MaxMemoryMiB == 0 || cfg.MaxMemoryMiB > math.MaxUint64/(1024*1024) || cfg.StageTimeout <= 0 || cfg.MaxDuration <= 0 || cfg.MaxStartLag <= 0 || cfg.QuietPeriod <= 0 || cfg.SetupRate < 1 || cfg.SetupRate > 1000000 || math.IsNaN(cfg.SetupRate) {
		return fmt.Errorf("parallelism, sample/memory budgets, timeouts, and setup rate must be positive; workers must be nonnegative")
	}
	if cfg.Assets == "" {
		return fmt.Errorf("set --assets or KUBEBUILDER_ASSETS to pinned envtest binaries")
	}
	for _, binary := range []string{"kube-apiserver", "etcd", "kubectl"} {
		info, err := os.Stat(filepath.Join(cfg.Assets, binary))
		if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			return fmt.Errorf("envtest executable unavailable: %s", filepath.Join(cfg.Assets, binary))
		}
	}
	return nil
}
