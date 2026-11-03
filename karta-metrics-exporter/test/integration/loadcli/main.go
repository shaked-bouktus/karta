// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/app"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/bootstrap"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/config"
	inventory "github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/kartadefinitioninventory"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/labelresolver"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/catalog"
)

type options struct {
	roots, concurrency    int
	rate                  float64
	phase, overall, quiet time.Duration
	output, crds          string
}
type report struct {
	MeasuredInvalidPublications, StartupLifecycleInvalidPublications    int
	Options                                                             optionsJSON
	Started                                                             time.Time
	ColdStartupNS                                                       int64
	Phases                                                              []phaseReport
	PublicationWrites, InvalidPublications, ObservationOverflow, Errors int
	LifecyclePassed                                                     bool
	Error                                                               string
	Limitations                                                         []string
}
type optionsJSON struct {
	Roots, Concurrency                        int
	UpdatesPerSecond                          float64
	PhaseTimeout, OverallTimeout, QuietWindow string
}
type phaseReport struct {
	InvalidExamples                              []invalidExample
	InvalidPublications                          int
	Name                                         string
	Roots, Pods                                  distribution
	Offered, Started, Completed, Missed, Errored int
	StartLagNS                                   []int64
	AchievedPerSecond                            float64
	GeneratorNS, DrainNS, ValidationNS           int64
	DefinitionUID, DefinitionRV                  string
	Error                                        string
}

func main() {
	o := options{}
	flag.IntVar(&o.roots, "roots", 20, "number of Deployment -> ReplicaSet -> Pod chains")
	flag.Float64Var(&o.rate, "rate", 10, "offered workload updates per second in each finite wave")
	flag.IntVar(&o.concurrency, "concurrency", 16, "maximum concurrent API updates; busy arrivals are missed")
	flag.DurationVar(&o.phase, "phase-timeout", 2*time.Minute, "bound for startup and each scenario")
	flag.DurationVar(&o.overall, "timeout", 10*time.Minute, "overall execution bound, excluding bounded cleanup")
	flag.DurationVar(&o.quiet, "quiet", 250*time.Millisecond, "required publication-quiet warmup")
	flag.StringVar(&o.output, "output", "load-result.json", "JSON evidence output")
	flag.StringVar(&o.crds, "crds", "../../../charts/karta/crds", "CRD directory, relative to integration module")
	flag.Parse()
	r := report{Started: time.Now(), Options: optionsJSON{o.roots, o.concurrency, o.rate, o.phase.String(), o.overall.String(), o.quiet.String()}, Limitations: []string{
		"Finite open-loop waves, one update per chain; not sustained churn or maximum capacity.",
		"Exporter, observer, generator and validation share one process; CPU/RSS are not measured. API server and etcd are separate envtest children.",
		"Synchronous observer locking and identity checks add instrumentation overhead. No metrics scraping load.",
		"Quiet is a publication heuristic, not proof that controller queues are empty. Durations are nanoseconds; validation polling is separate.",
	}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, o.overall)
	defer cancel()
	err := execute(ctx, o, &r)
	if err != nil {
		r.Error = err.Error()
		r.Errors++
	}
	data, jsonErr := json.MarshalIndent(r, "", "  ")
	if jsonErr == nil {
		jsonErr = os.WriteFile(o.output, append(data, '\n'), 0600)
	}
	if jsonErr != nil {
		err = errors.Join(err, jsonErr)
	}
	for _, p := range r.Phases {
		fmt.Printf("%s: root %d/%d, pod %d/%d; operations %d/%d, missed %d, errors %d\n", p.Name, p.Roots.Completed, p.Roots.Expected, p.Pods.Completed, p.Pods.Expected, p.Completed, p.Offered, p.Missed, p.Errored)
		for name, d := range map[string]distribution{"root": p.Roots, "pod": p.Pods} {
			if d.Completed > 0 {
				fmt.Printf("  %s latency p50=%s p90=%s p99=%s\n", name, time.Duration(*d.P50), time.Duration(*d.P90), time.Duration(*d.P99))
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("PASS: %s\n", o.output)
}
func execute(ctx context.Context, o options, r *report) (err error) {
	if o.roots < 1 || o.concurrency < 1 || math.IsNaN(o.rate) || o.rate <= 0 || o.rate > 1e6 || o.phase <= 0 || o.overall <= 0 || o.quiet <= 0 {
		return errors.New("positive bounded roots, concurrency, rate (<=1000000), and durations required")
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return errors.New("KUBEBUILDER_ASSETS is required")
	}
	crds, err := filepath.Abs(o.crds)
	if err != nil {
		return err
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{crds}, ErrorIfCRDPathMissing: true, UseExistingCluster: new(bool), ControlPlaneStartTimeout: 30 * time.Second, ControlPlaneStopTimeout: 10 * time.Second}
	environment.ControlPlane.GetAPIServer().Configure().Append("feature-gates", "WatchList=true")
	defer func() { err = errors.Join(err, environment.Stop()) }()
	kube, err := environment.Start()
	if err != nil {
		return err
	}
	kube.QPS = 1000
	kube.Burst = 2000
	kube.Timeout = 5 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, v1alpha1.AddToScheme} {
		if err = add(scheme); err != nil {
			return err
		}
	}
	api, err := client.New(kube, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	setup, finish := context.WithTimeout(ctx, o.phase)
	defer finish()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "load-cli"}}
	if err = api.Create(setup, ns); err != nil {
		return err
	}
	definition, err := catalog.Get(deployGVK)
	if err != nil {
		return err
	}
	definition.Name = "load-cli"
	if err = api.Create(setup, definition); err != nil {
		return err
	}
	chains := make([]chain, 0, o.roots)

	for i := 0; i < o.roots; i++ {
		c, e := createChain(setup, api, fmt.Sprintf("chain-%06d", i))
		if e != nil {
			return e
		}
		chains = append(chains, c)
	}
	obs := newObserver(o.roots*8 + 32)
	defer func() {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		r.PublicationWrites = obs.writes
		r.InvalidPublications = obs.invalid
		r.MeasuredInvalidPublications = obs.measuredInvalid
		r.StartupLifecycleInvalidPublications = obs.invalid - obs.measuredInvalid
		r.ObservationOverflow = obs.overflow
		if obs.measuredInvalid+obs.overflow > 0 {
			err = errors.Join(err, fmt.Errorf("measured invalid publications=%d, observation overflow=%d", obs.measuredInvalid, obs.overflow))
		}
	}()
	cfg := config.DefaultConfig()
	cfg.Workers = 4
	cfg.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns.Name}}
	cfg.KartaDefinitionSource.Catalog.Enabled = false
	cfg.KartaDefinitionSource.Cluster.Enabled = true
	cfg.KartaDefinitionSource.Cluster.Include = []string{definition.Name}
	cfg.APIQPS = 1000
	cfg.APIBurst = 2000
	cfg.RequestTimeout = "5s"
	cfg.Rules = []labelresolver.LabelRule{
		{Name: "root_revision", Metrics: []string{labelresolver.MetricPodWorkloadInfo, labelresolver.MetricWorkloadStatus}, Sources: []labelresolver.Source{labelresolver.SourceWorkload}, Expression: `object.metadata.labels["revision"]`},
		{Name: "pod_revision", Metrics: []string{labelresolver.MetricPodWorkloadInfo}, Sources: []labelresolver.Source{labelresolver.SourcePod}, Expression: `object.metadata.labels["revision"]`},
	}
	components, err := bootstrap.NewWithOptions(cfg, kube, bootstrap.Options{Publications: obs, ObserveEvent: obs.event, ObserveDefinition: obs.definition})
	if err != nil {
		return err
	}
	runCtx, stopRuntime := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- app.RunController(runCtx, components) }()
	defer func() {
		stopRuntime()
		select {
		case e := <-done:
			err = errors.Join(err, e)
		case <-time.After(15 * time.Second):
			err = errors.Join(err, errors.New("exporter shutdown timeout"))
		}
	}()
	if err = wait(setup, func() error {
		if !components.Runtime.Ready() {
			return errors.New("runtime not ready")
		}
		return validate(obs, chains, "deployment", "Running")
	}); err != nil {
		return err
	}
	r.ColdStartupNS = time.Since(r.Started).Nanoseconds()
	for _, kind := range []string{"root", "pod"} {
		p, e := workloadWave(ctx, o, api, obs, chains, kind)
		r.Phases = append(r.Phases, p)
		if e != nil {
			return e
		}
	}
	p, err := definitionWave(ctx, o, api, obs, chains, definition)
	r.Phases = append(r.Phases, p)
	if err != nil {
		return err
	}
	selection, selectionErr := components.Definitions.(inventory.Reader).Lookup(deployGVK)
	if selectionErr != nil {
		return selectionErr
	}
	if selection.Origin.UID != definition.UID || selection.Origin.ResourceVersion != definition.ResourceVersion || selection.Origin.Source != inventory.SourceCluster || selection.Origin.Name != definition.Name {
		return errors.New("selected definition identity differs from API update response")
	}
	phase, stopPhase := context.WithTimeout(ctx, o.phase)
	defer stopPhase()
	if err = quiet(phase, obs, o.quiet); err != nil {
		return err
	}
	old := chains[0]
	if err = deleteChain(phase, api, old); err != nil {
		return err
	}
	if err = wait(phase, func() error {
		v := obs.Capture()
		if obs.Pod(old.pod.UID) != nil || obs.Workload(old.root.UID) != nil || len(v.Pods) != o.roots-1 || len(v.Workloads) != o.roots-1 {
			return errors.New("deleted chain remains")
		}
		return nil
	}); err != nil {
		return err
	}
	replacement, err := createChain(phase, api, old.root.Name)
	if err != nil {
		return err
	}
	chains[0] = replacement
	if replacement.root.UID == old.root.UID || replacement.rs.UID == old.rs.UID || replacement.pod.UID == old.pod.UID {
		return errors.New("recreated chain reused identity")
	}
	if err = wait(phase, func() error { return validate(obs, chains, "changed", "Completed") }); err != nil {
		return err
	}
	r.LifecyclePassed = true
	return nil
}
func wait(ctx context.Context, check func() error) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := check()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}
func quiet(ctx context.Context, o *observer, window time.Duration) error {
	start := time.Now()
	return wait(ctx, func() error {
		o.mu.Lock()
		last := o.last
		o.mu.Unlock()
		if last.Before(start) {
			last = start
		}
		if time.Since(last) < window {
			return errors.New("publication quiet gate")
		}
		return nil
	})
}
