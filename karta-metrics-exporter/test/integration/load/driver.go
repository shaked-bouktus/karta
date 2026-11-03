// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kuberuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/config"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
)

type runReport struct {
	Started       time.Time         `json:"started"`
	Finished      time.Time         `json:"finished"`
	Config        options           `json:"config"`
	GoVersion     string            `json:"go_version"`
	OS            string            `json:"os"`
	Arch          string            `json:"arch"`
	LogicalCPUs   int               `json:"logical_cpus"`
	GOMAXPROCS    int               `json:"gomaxprocs"`
	Revision      string            `json:"revision"`
	WorkingTree   string            `json:"working_tree_status"`
	BuildInfo     *debug.BuildInfo  `json:"build_info,omitempty"`
	AssetVersions map[string]string `json:"asset_versions"`
	Trials        []trialReport     `json:"trials"`
	Error         string            `json:"error,omitempty"`
	Exporter      map[string]any    `json:"exporter_configuration"`
}

type trialReport struct {
	ConcurrentUpdate  *concurrentPhaseReport `json:"concurrent_update,omitempty"`
	Roots             int                    `json:"roots"`
	KubernetesObjects int                    `json:"kubernetes_objects"`
	Outcome           string                 `json:"outcome"`
	Error             string                 `json:"error,omitempty"`
	Setup             LoadResult             `json:"setup"`
	SetupAPI          APIStats               `json:"setup_api"`
	StartupSeconds    float64                `json:"startup_seconds"`
	Phases            []phaseReport          `json:"phases"`
	DefinitionUpdate  *definitionPhaseReport `json:"definition_update,omitempty"`
	PeakRSSBytes      map[string]uint64      `json:"peak_rss_bytes"`
}

type phaseReport struct {
	Preparation       bool         `json:"preparation,omitempty"`
	Name              string       `json:"name"`
	OfferedRate       float64      `json:"offered_rate_per_second"`
	AchievedRate      float64      `json:"achieved_rate_per_second"`
	Load              LoadResult   `json:"load"`
	API               APIStats     `json:"api"`
	DrainSeconds      float64      `json:"drain_seconds"`
	QuiescenceSeconds float64      `json:"pre_load_quiescence_seconds"`
	QuiescenceWrites  uint64       `json:"pre_load_publication_writes"`
	Valid             bool         `json:"valid"`
	Validation        string       `json:"validation,omitempty"`
	Measurement       *Measurement `json:"reflector_to_store"`
	Outcome           string       `json:"outcome"`
	Error             string       `json:"error,omitempty"`
}

type resourceSample struct {
	Time      time.Time      `json:"time"`
	Phase     string         `json:"phase"`
	Processes []ProcessUsage `json:"processes"`
}

func runSweep(ctx context.Context, cfg options) (resultErr error) {
	var err error
	cfg.Artifacts, err = filepath.Abs(cfg.Artifacts)
	if err != nil {
		return err
	}
	cfg.Assets, err = filepath.Abs(cfg.Assets)
	if err != nil {
		return err
	}
	if cfg.Scenario == "karta-update" || cfg.Scenario == "karta-concurrent" {
		cfg.CRDDirectory, err = filepath.Abs(cfg.CRDDirectory)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Artifacts), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(cfg.Artifacts, 0700); err != nil {
		return fmt.Errorf("create new artifacts directory: %w", err)
	}
	report := runReport{Started: time.Now().UTC(), Config: cfg, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, LogicalCPUs: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0), AssetVersions: make(map[string]string)}
	exporterDefaults := config.DefaultConfig()
	workers := cfg.Workers
	if workers == 0 {
		workers = exporterDefaults.Workers
	}
	report.Exporter = map[string]any{"root_workers": workers, "pod_workers": workers, "api_qps": exporterDefaults.APIQPS, "api_burst": exporterDefaults.APIBurst, "catalog": "apps-deployment-v1", "cluster_definitions": false, "metrics_http": false}
	report.Exporter["enabled"] = !cfg.DiagnosticAPIOnly
	if cfg.Scenario == "karta-update" || cfg.Scenario == "karta-concurrent" {
		report.Exporter["catalog"] = "disabled"
		report.Exporter["cluster_definitions"] = true
		report.Exporter["definition"] = liveDefinitionName
		if cfg.Scenario == "karta-concurrent" {
			delete(report.Exporter, "definition")
			report.Exporter["definitions"] = concurrentNames()
		}
	}
	defer func() {
		report.Finished = time.Now().UTC()
		if resultErr != nil {
			report.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, writeJSON(filepath.Join(cfg.Artifacts, "summary.json"), report))
		fmt.Fprintf(os.Stderr, "Results: %s\n", cfg.Artifacts)
	}()
	ctx, cancel := context.WithTimeout(ctx, cfg.MaxDuration)
	defer cancel()
	report.BuildInfo, _ = debug.ReadBuildInfo()
	for _, command := range []struct {
		name string
		args []string
	}{
		{"revision", []string{"rev-parse", "HEAD"}},
		{"status", []string{"status", "--short"}},
	} {
		output, commandErr := exec.CommandContext(ctx, "git", command.args...).Output()
		if commandErr == nil {
			if command.name == "revision" {
				report.Revision = strings.TrimSpace(string(output))
			} else {
				report.WorkingTree = string(output)
			}
		}
	}
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		output, commandErr := exec.CommandContext(ctx, filepath.Join(cfg.Assets, binary), "--version").CombinedOutput()
		if commandErr != nil {
			return fmt.Errorf("read %s version: %w", binary, commandErr)
		}
		report.AssetVersions[binary] = strings.TrimSpace(string(output))
	}
	for _, roots := range cfg.Roots {
		trial, err := runTrial(ctx, cfg, roots)
		report.Trials = append(report.Trials, trial)
		if err != nil {
			return err
		}
	}
	return nil
}

func runTrial(parent context.Context, cfg options, roots int) (report trialReport, resultErr error) {
	report = trialReport{Roots: roots, KubernetesObjects: 3 * roots, Outcome: "incomplete", PeakRSSBytes: make(map[string]uint64)}
	if cfg.Scenario == "karta-concurrent" {
		report.KubernetesObjects = 2*roots + (roots+2)/3
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	dir := filepath.Join(cfg.Artifacts, fmt.Sprintf("roots-%d", roots))
	if err := os.Mkdir(dir, 0700); err != nil {
		return report, err
	}
	defer func() {
		if resultErr != nil {
			report.Error = resultErr.Error()
			if strings.HasPrefix(report.Outcome, "passed_") {
				report.Outcome = "artifact_or_cleanup_error"
			} else if report.Outcome == "incomplete" {
				report.Outcome = "infrastructure_or_budget_limit"
			}
		}
		if err := writeJSON(filepath.Join(dir, "trial.json"), report); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("write trial artifact: %w", err))
			report.Outcome = "artifact_error"
			report.Error = resultErr.Error()
		}
	}()
	controlLog, err := os.OpenFile(filepath.Join(dir, "control-plane.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, controlLog.Close()) }()
	environment := &envtest.Environment{
		UseExistingCluster: ptr.To(false), BinaryAssetsDirectory: cfg.Assets,
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second,
		ControlPlane: envtest.ControlPlane{
			APIServer:   &envtest.APIServer{Path: filepath.Join(cfg.Assets, "kube-apiserver"), Out: controlLog, Err: controlLog},
			Etcd:        &envtest.Etcd{Path: filepath.Join(cfg.Assets, "etcd"), Out: controlLog, Err: controlLog},
			KubectlPath: filepath.Join(cfg.Assets, "kubectl"),
		},
	}
	environment.ControlPlane.APIServer.Configure().Set("feature-gates", "WatchList=true")
	if cfg.Scenario == "karta-update" || cfg.Scenario == "karta-concurrent" {
		environment.CRDDirectoryPaths = []string{cfg.CRDDirectory}
		environment.ErrorIfCRDPathMissing = true
	}
	fmt.Fprintf(os.Stderr, "roots=%d objects=%d: starting isolated control plane\n", roots, report.KubernetesObjects)
	apiConfig, err := environment.Start()
	if err != nil {
		return report, errors.Join(fmt.Errorf("start envtest: %w", err), environment.Stop())
	}
	defer func() { resultErr = errors.Join(resultErr, environment.Stop()) }()
	var exporterPID atomic.Int64
	var currentPhase atomic.Value
	currentPhase.Store("preload")
	resourceCtx, stopResources := context.WithCancel(ctx)
	resourceDone := make(chan error, 1)
	go func() {
		resourceDone <- monitorResources(resourceCtx, cfg, dir, &exporterPID, &currentPhase, report.PeakRSSBytes, cancel)
	}()
	defer func() {
		stopResources()
		resultErr = errors.Join(resultErr, <-resourceDone)
	}()
	meter := NewAPIMeter(cfg.MaxSamples)
	generatorConfig := rest.CopyConfig(apiConfig)
	// The generator has its own scheduler. The exporter keeps production client limits.
	generatorConfig.QPS, generatorConfig.Burst = -1, 1
	generatorConfig.Timeout = 30 * time.Second
	generatorConfig.WrapTransport = meter.Wrap
	api, err := kubernetes.NewForConfig(generatorConfig)
	if err != nil {
		return report, err
	}
	if _, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "karta-load"}}, metav1.CreateOptions{}); err != nil {
		return report, err
	}
	var definitions []*v1alpha1.Karta
	var definition *v1alpha1.Karta
	var definitionClient client.Client
	if cfg.Scenario == "karta-update" || cfg.Scenario == "karta-concurrent" {
		scheme := kuberuntime.NewScheme()
		if err := appsv1.AddToScheme(scheme); err != nil {
			return report, err
		}
		if err := corev1.AddToScheme(scheme); err != nil {
			return report, err
		}
		if err := v1alpha1.AddToScheme(scheme); err != nil {
			return report, err
		}
		definitionClient, err = client.New(generatorConfig, client.Options{Scheme: scheme})
		if err != nil {
			return report, err
		}
		if cfg.Scenario == "karta-concurrent" {
			definitions, err = newConcurrentDefinitions()
			if err != nil {
				return report, err
			}
			for _, item := range definitions {
				if err := definitionClient.Create(ctx, item); err != nil {
					return report, err
				}
			}
		} else {
			definition, err = newLiveDefinition()
			if err != nil {
				return report, err
			}
			if err := definitionClient.Create(ctx, definition); err != nil {
				return report, fmt.Errorf("create live Karta: %w", err)
			}
		}
	}
	fixtures := make([]Fixture, roots)
	meter.Begin()
	setupCtx, setupCancel := context.WithTimeout(ctx, cfg.StageTimeout)
	report.Setup = runLoad(setupCtx, roots, cfg.SetupRate, cfg.Parallel, false, cfg.MaxStartLag, func(ctx context.Context, i int) error {
		var err error
		if cfg.Scenario == "karta-concurrent" {
			fixtures[i], err = createConcurrentFixture(ctx, api, "karta-load", fmt.Sprintf("workload-%06d", i), concurrentKinds[i%3])
		} else {
			fixtures[i], err = createFixture(ctx, api, "karta-load", fmt.Sprintf("workload-%06d", i))
		}
		return err
	})
	setupCancel()
	report.SetupAPI = meter.Snapshot()
	if report.Setup.Completed != roots || report.SetupAPI.Overflow != 0 {
		report.Outcome = "preload_limit"
		return report, fmt.Errorf("preload completed %d/%d: %s (cause: %v)", report.Setup.Completed, roots, report.Setup.FirstError, context.Cause(ctx))
	}
	if cfg.DiagnosticAPIOnly {
		phase, err := runAPIOnly(ctx, cfg, api, meter, fixtures, &currentPhase, dir)
		report.Phases = append(report.Phases, phase)
		report.Outcome = phase.Outcome
		return report, err
	}
	privateDir, err := os.MkdirTemp("", "karta-load-credentials-")
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(privateDir)) }()
	kubeconfig := filepath.Join(privateDir, "config")
	if err := writeKubeconfig(kubeconfig, apiConfig); err != nil {
		return report, err
	}
	executable, err := os.Executable()
	if err != nil {
		return report, err
	}
	currentPhase.Store("cold-start")
	started := time.Now()
	child, err := startExporter(ctx, executable, kubeconfig, filepath.Join(dir, "exporter.log"), cfg.MaxSamples, cfg.Workers, cfg.Scenario)
	if err != nil {
		return report, err
	}
	exporterPID.Store(int64(child.PID()))
	defer func() {
		// Stop resource sampling before intentionally removing its expected process.
		stopResources()
		resultErr = errors.Join(resultErr, child.Close())
	}()
	if cfg.Scenario == "karta-concurrent" {
		report.KubernetesObjects = 2*roots + (roots+2)/3
		cohorts := concurrentCohorts(definitions, fixtures, false)
		if _, err := child.Request(ctx, Command{Operation: "expect-concurrent", Cohorts: cohorts}); err != nil {
			return report, err
		}
		if _, err := waitConcurrent(ctx, cfg.StageTimeout, child); err != nil {
			report.Outcome = "cold_start_limit"
			return report, err
		}
		report.StartupSeconds = time.Since(started).Seconds()
		phase, err := runConcurrentPhase(ctx, cfg, child, definitionClient, definitions, meter, fixtures, &currentPhase, dir)
		report.ConcurrentUpdate = &phase
		report.Outcome = phase.Outcome
		return report, err
	}
	expectation := Command{Operation: "expect", Fixtures: fixtures, RootRevision: "0", PodRevision: "0"}
	if cfg.Scenario == "karta-update" || cfg.Scenario == "karta-concurrent" {
		expectation.Definition = &DefinitionExpectation{Name: liveDefinitionName, Source: "cluster", Component: "deployment", Phase: "Initializing"}
	}
	if _, err := child.Request(ctx, expectation); err != nil {
		return report, err
	}
	if _, err := waitConvergence(ctx, cfg.StageTimeout, child, true); err != nil {
		report.Outcome = "cold_start_limit"
		return report, err
	}
	report.StartupSeconds = time.Since(started).Seconds()
	fmt.Fprintf(os.Stderr, "roots=%d: cold start %.2fs, all publications validated\n", roots, report.StartupSeconds)
	if cfg.Scenario == "karta-update" || cfg.Scenario == "karta-concurrent" {
		phase, err := runDefinitionPhase(ctx, cfg, child, definitionClient, definition, meter, fixtures, &currentPhase, dir)
		report.DefinitionUpdate = &phase
		report.Outcome = phase.Outcome
		return report, err
	}
	rootRevision, podRevision := "0", "0"
	selected := cfg.WorkloadPhase
	if selected == "" {
		selected = "all"
	}
	for index, rate := range cfg.Rates {
		for _, name := range []string{"root-update", "pod-update"} {
			if selected != "all" && selected != name {
				continue
			}
			if name == "root-update" {
				rootRevision = fmt.Sprint(index + 1)
			} else {
				podRevision = fmt.Sprint(index + 1)
			}
			phase, err := runPhase(ctx, cfg, child, api, meter, fixtures, name, rate, rootRevision, podRevision, &currentPhase, dir)
			report.Phases = append(report.Phases, phase)
			if err != nil {
				report.Outcome = phase.Outcome
				return report, err
			}
			if cfg.DiagnosticRootOnly {
				report.Outcome = "passed_diagnostic_root_wave"
				return report, nil
			}
		}
	}
	for _, name := range []string{"delete", "live-create"} {
		preparation := selected == "live-create" && name == "delete"
		if selected != "all" && selected != name && !preparation {
			continue
		}
		if name == "live-create" {
			rootRevision, podRevision = "0", "0"
		}
		phase, err := runPhase(ctx, cfg, child, api, meter, fixtures, name, cfg.SetupRate, rootRevision, podRevision, &currentPhase, dir)
		phase.Preparation = preparation
		report.Phases = append(report.Phases, phase)
		if err != nil {
			report.Outcome = phase.Outcome
			return report, err
		}
	}

	if _, err := child.Request(ctx, Command{Operation: "heap", ProfilePath: filepath.Join(dir, "exporter-final.heap")}); err != nil {
		return report, err
	}
	report.Outcome = "passed_finite_waves"
	return report, nil
}

func runPhase(ctx context.Context, cfg options, child *exporterProcess, api kubernetes.Interface, meter *APIMeter, fixtures []Fixture, name string, rate float64, rootRevision, podRevision string, currentPhase *atomic.Value, dir string) (report phaseReport, resultErr error) {
	report = phaseReport{Name: name, OfferedRate: rate, Outcome: "incomplete"}
	defer func() {
		if resultErr != nil {
			report.Error = resultErr.Error()
			if report.Outcome == "incomplete" {
				report.Outcome = "infrastructure_or_budget_limit"
			}
		}
	}()
	phaseID := fmt.Sprintf("%s-%g", name, rate)
	defer func() {
		diagnosticCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		response, err := child.Request(diagnosticCtx, Command{Operation: "health-trace"})
		if err == nil {
			err = writeJSON(filepath.Join(dir, phaseID+"-source-health.json"), response.HealthTrace)
		}
		resultErr = errors.Join(resultErr, err)
	}()
	currentPhase.Store(phaseID + "-settle")
	settleStart := time.Now()
	writes, err := waitPublicationQuiet(ctx, cfg.StageTimeout, cfg.QuietPeriod, child)
	report.QuiescenceSeconds = time.Since(settleStart).Seconds()
	report.QuiescenceWrites = writes
	if err != nil {
		report.Outcome, report.Error = "quiescence_limit", err.Error()
		return report, err
	}
	currentPhase.Store(phaseID)
	fmt.Fprintf(os.Stderr, "%s: %d operations at %.0f/s\n", phaseID, len(fixtures), rate)
	if _, err := child.Request(ctx, Command{Operation: "begin"}); err != nil {
		return report, err
	}
	meter.Begin()
	loadCtx, cancelLoad := context.WithTimeout(ctx, cfg.StageTimeout)
	progressCtx, stopProgress := context.WithCancel(ctx)
	progressDone := make(chan error, 1)
	go func() {
		progressDone <- monitorProgress(progressCtx, child, filepath.Join(dir, phaseID+"-progress.jsonl"))
	}()
	arrivals := make([]Arrival, min(len(fixtures), cfg.MaxSamples))
	report.Load = runLoad(loadCtx, len(fixtures), rate, cfg.Parallel, name == "root-update" || name == "pod-update", cfg.MaxStartLag, func(ctx context.Context, i int) error {
		switch name {
		case "root-update":
			return updateRoot(ctx, api, &fixtures[i], rootRevision)
		case "pod-update":
			return updatePod(ctx, api, &fixtures[i], podRevision)
		case "delete":
			return deleteFixture(ctx, api, fixtures[i])
		case "live-create":
			var err error
			fixtures[i], err = createFixture(ctx, api, fixtures[i].Namespace, fixtures[i].Name)
			return err
		default:
			return fmt.Errorf("unknown load phase %s", name)
		}
	}, arrivals)
	artifactErr := writeJSON(filepath.Join(dir, phaseID+"-arrivals.json"), arrivals)
	defer func() { resultErr = errors.Join(resultErr, artifactErr) }()
	drainStart := time.Now()
	cancelLoad()
	stopProgress()
	progressErr := <-progressDone
	report.API = meter.Snapshot()
	if report.Load.DurationSeconds > 0 {
		report.AchievedRate = float64(report.Load.Completed) / report.Load.DurationSeconds
	}
	if progressErr != nil {
		report.Outcome = "instrumentation_limit"
		return report, errors.Join(progressErr, context.Cause(ctx))
	}
	var expected []Expectation
	for i, fixture := range fixtures {
		if !report.Load.Success[i] {
			continue
		}
		items := fixtureExpectations(fixture, name == "delete")
		switch name {
		case "root-update":
			expected = append(expected, items[0])
		case "pod-update":
			expected = append(expected, items[1])
		default:
			expected = append(expected, items...)
		}
	}
	complete := report.Load.Completed == len(fixtures) && report.API.Overflow == 0
	if _, err := child.Request(ctx, Command{Operation: "expect", Expected: expected, Fixtures: fixtures, RootRevision: rootRevision, PodRevision: podRevision, Deleted: name == "delete"}); err != nil {
		return report, err
	}
	currentPhase.Store(phaseID + "-drain")
	response, convergenceErr := waitConvergence(ctx, cfg.StageTimeout, child, complete)
	report.DrainSeconds = time.Since(drainStart).Seconds()
	// Collect even after the convergence budget expires, while the trial is alive.
	if ctx.Err() == nil {
		collected, err := child.Request(ctx, Command{Operation: "collect"})
		if err == nil {
			response = collected
		} else {
			convergenceErr = errors.Join(convergenceErr, err)
		}
	}
	report.Valid, report.Validation, report.Measurement = response.Valid, response.Validation, response.Measurement
	switch {
	case ctx.Err() != nil:
		report.Outcome = "infrastructure_or_budget_limit"
		resultErr = context.Cause(ctx)
	case report.Load.Errors > 0 || report.API.Failed > 0:
		report.Outcome = "api_or_generator_error"
		resultErr = fmt.Errorf("API/generator errors: %s", report.Load.FirstError)
	case report.Load.Missed > 0:
		report.Outcome = "generator_limited"
		resultErr = fmt.Errorf("generator missed %d/%d arrivals", report.Load.Missed, report.Load.Scheduled)
	case report.API.Overflow > 0 || (report.Measurement != nil && report.Measurement.Overflow > 0):
		report.Outcome = "instrumentation_limit"
		resultErr = errors.New("measurement sample budget exceeded")
	case convergenceErr != nil || !report.Valid:
		report.Outcome = "convergence_limit"
		resultErr = errors.Join(fmt.Errorf("publication convergence failed: %s", response.Validation), convergenceErr)
	default:
		report.Outcome = "passed_finite_wave"
	}
	if resultErr != nil {
		report.Error = resultErr.Error()
		if ctx.Err() == nil {
			_, profileErr := child.Request(ctx, Command{Operation: "heap", ProfilePath: filepath.Join(dir, phaseID+"-limit.heap")})
			resultErr = errors.Join(resultErr, profileErr)
		}
	}
	if report.Measurement != nil {
		if err := writeJSON(filepath.Join(dir, phaseID+"-samples.json"), report.Measurement.Samples); err != nil {
			report.Outcome = "artifact_error"
			report.Error = err.Error()
			resultErr = errors.Join(resultErr, err)
		}
		report.Measurement.Samples = nil
		if latency := report.Measurement.Latency; latency != nil {
			fmt.Fprintf(os.Stderr, "%s: p50=%.2fms p90=%.2fms p99=%.2fms completed=%d/%d outcome=%s\n", phaseID, latency.P50MS, latency.P90MS, latency.P99MS, report.Measurement.Completed, report.Measurement.Expected, report.Outcome)
		}
	}
	return report, resultErr
}

func waitConvergence(ctx context.Context, budget time.Duration, child *exporterProcess, requireValid bool) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var response Response
	for {
		latest, err := child.Request(ctx, Command{Operation: "status"})
		if err != nil {
			return response, err
		}
		response = latest
		if response.Valid || (!requireValid && response.Measurement != nil && response.Measurement.Missing == 0) {
			return response, nil
		}
		select {
		case <-ctx.Done():
			return response, context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

func waitPublicationQuiet(ctx context.Context, budget, quietPeriod time.Duration, child *exporterProcess) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(min(250*time.Millisecond, quietPeriod))
	defer ticker.Stop()
	lastChange := time.Now()
	var previous, first uint64
	initialized := false
	for {
		response, err := child.Request(ctx, Command{Operation: "progress"})
		if err != nil {
			return previous - first, err
		}
		writes := response.Stats.PublicationWrites
		if !initialized {
			first = writes
			initialized = true
		}
		if writes != previous {
			lastChange = time.Now()
		}
		previous = writes
		if time.Since(lastChange) >= quietPeriod {
			return writes - first, nil
		}
		select {
		case <-ctx.Done():
			return writes - first, context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

func monitorResources(ctx context.Context, cfg options, dir string, exporterPID *atomic.Int64, phase *atomic.Value, peaks map[string]uint64, cancel context.CancelCauseFunc) (resultErr error) {
	file, err := os.OpenFile(filepath.Join(dir, "resources.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		cancel(err)
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	encoder := json.NewEncoder(file)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		pid := int(exporterPID.Load())
		usage, err := sampleProcesses(ctx, os.Getpid(), pid)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			cancel(err)
			return err
		}
		roles := make(map[string]bool, len(usage))
		for _, process := range usage {
			roles[process.Role] = true
		}
		required := []string{"driver", "apiserver", "etcd"}
		if pid > 0 {
			required = append(required, "exporter")
		}
		for _, role := range required {
			if !roles[role] {
				err := fmt.Errorf("resource sample missing required %s process", role)
				cancel(err)
				return err
			}
		}
		var combined uint64
		for _, process := range usage {
			peaks[process.Role] = max(peaks[process.Role], process.RSSBytes)
			combined += process.RSSBytes
		}
		peaks["combined"] = max(peaks["combined"], combined)
		if err := encoder.Encode(resourceSample{Time: time.Now().UTC(), Phase: phase.Load().(string), Processes: usage}); err != nil {
			cancel(err)
			return err
		}
		if combined > cfg.MaxMemoryMiB*1024*1024 {
			err := fmt.Errorf("combined RSS %.0f MiB exceeded budget %d MiB", float64(combined)/(1024*1024), cfg.MaxMemoryMiB)
			cancel(err)
			return err
		}
	}
}

func writeKubeconfig(path string, cfg *rest.Config) error {
	data, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:  map[string]*clientcmdapi.Cluster{"envtest": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"load": {ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}},
		Contexts:  map[string]*clientcmdapi.Context{"load": {Cluster: "envtest", AuthInfo: "load"}}, CurrentContext: "load",
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func writeJSON(path string, value any) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return errors.Join(encoder.Encode(value), file.Close())
}
