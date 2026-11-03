// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package loadtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/app"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/bootstrap"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/config"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/labelresolver"
	"github.com/dsx-ai-factory/workload-map/karta-metrics-exporter/pkg/store"
	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/catalog"
)

type chain struct {
	Root *appsv1.Deployment
	RS   *appsv1.ReplicaSet
	Pod  *corev1.Pod
}
type run struct {
	t                *testing.T
	ctx              context.Context
	api              client.Client
	observer         *observation
	chains           []chain
	definition       *v1alpha1.Karta
	report           *report
	component, phase string
}
type report struct {
	Success                  bool
	Roots, Rate, Concurrency int
	ColdStartupNS            int64
	Phases                   []*phaseReport
	Errors                   []string
	Limitations              []string
}
type phaseReport struct {
	Name                                                       string
	Offered, Started, Completed, Missed, Errored               int
	StartLagNS                                                 []int64
	LateWriterLagNS                                            []int64
	AchievedRate                                               float64
	OfferDurationNS, APIDrainNS, ConvergenceNS                 int64
	Writes, InvalidPublications, ObservationOverflow, Timeouts int
	Definition                                                 *identity
	Roots, Pods                                                distribution
	Errors                                                     []string
	InvalidExamples                                            []invalidExample
}

var deploymentGVK = appsv1.SchemeGroupVersion.WithKind("Deployment")

func TestExporterLoad(t *testing.T) {
	if os.Getenv("KARTA_LOAD") != "1" {
		t.Skip("set KARTA_LOAD=1 to run the real envtest experiment")
	}
	roots, rate := positiveEnv(t, "KARTA_LOAD_ROOTS", 20), positiveEnv(t, "KARTA_LOAD_RATE", 10)
	result := &report{Roots: roots, Rate: rate, Concurrency: 16, Limitations: []string{"Finite one-update-per-object waves, not sustained churn or maximum capacity.", "No CPU or RSS measurement. Exporter, observer, fixture generation and validation share one process; envtest control plane is separate.", "Quiet is a 250ms publication heuristic, not proof queues are empty. Observer locks and retained snapshots add overhead.", "Generator admits at most 16 concurrent requests, drops occupied slots or arrivals more than one interval late; no postponed arrivals."}}
	t.Cleanup(func() {
		result.Success = !t.Failed()
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		path := os.Getenv("KARTA_LOAD_OUTPUT")
		if path == "" {
			path = "load-result.json"
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Error(err)
		}
		t.Logf("evidence: %s", path)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	startupCtx, startupCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer startupCancel()
	r := &run{t: t, ctx: startupCtx, report: result, observer: newObservation(roots*100 + 1000), component: "deployment", phase: "Running"}
	start := time.Now()
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../../../charts/karta/crds"}, ErrorIfCRDPathMissing: true, UseExistingCluster: new(bool), ControlPlaneStartTimeout: 60 * time.Second, ControlPlaneStopTimeout: 20 * time.Second}
	env.ControlPlane.GetAPIServer().Configure().Append("feature-gates", "WatchList=true")
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			result.Errors = append(result.Errors, "control plane stop: "+err.Error())
			t.Error(err)
		}
	})
	kube, err := env.Start()
	r.must(err)
	kube.QPS = 1000
	kube.Burst = 2000
	kube.Timeout = 10 * time.Second
	scheme := runtime.NewScheme()
	r.must(corev1.AddToScheme(scheme))
	r.must(appsv1.AddToScheme(scheme))
	r.must(v1alpha1.AddToScheme(scheme))
	r.api, err = client.New(kube, client.Options{Scheme: scheme})
	r.must(err)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "load-test"}}
	r.must(r.api.Create(ctx, ns))
	r.definition, err = catalog.Get(deploymentGVK)
	r.must(err)
	r.definition.Name = "load-deployment"
	r.must(r.api.Create(ctx, r.definition))
	for i := 0; i < roots; i++ {
		r.chains = append(r.chains, r.createChain(fmt.Sprintf("chain-%05d", i)))
	}
	cfg := config.DefaultConfig()
	cfg.KartaDefinitionSource.Catalog.Enabled = false
	cfg.KartaDefinitionSource.Cluster.Enabled = true
	cfg.KartaDefinitionSource.Cluster.Include = []string{r.definition.Name}
	cfg.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns.Name}}
	cfg.APIQPS = 1000
	cfg.APIBurst = 2000
	cfg.Workers = 4
	cfg.RequestTimeout = "10s"
	cfg.Rules = []labelresolver.LabelRule{
		{Name: "root_revision", Metrics: []string{labelresolver.MetricWorkloadStatus, labelresolver.MetricPodWorkloadInfo}, Sources: []labelresolver.Source{labelresolver.SourceWorkload}, Expression: `object.metadata.labels["load.example/revision"]`},
		{Name: "pod_revision", Metrics: []string{labelresolver.MetricPodWorkloadInfo}, Sources: []labelresolver.Source{labelresolver.SourcePod}, Expression: `object.metadata.labels["load.example/revision"]`},
	}
	components, err := bootstrap.NewWithOptions(cfg, kube, bootstrap.Options{Publications: r.observer, ObserveEvent: r.observer.event, ObserveDefinition: r.observer.definition})
	r.must(err)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- app.RunController(runCtx, components) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				result.Errors = append(result.Errors, err.Error())
				t.Error(err)
			}
		case <-time.After(20 * time.Second):
			result.Errors = append(result.Errors, "exporter shutdown timed out")
			t.Error("exporter shutdown timed out")
		}
	})
	r.wait("cold startup", 60*time.Second, func() error {
		if !components.Runtime.Ready() {
			return fmt.Errorf("runtime not ready")
		}
		return r.validate()
	})
	result.ColdStartupNS = time.Since(start).Nanoseconds()
	startupCancel()
	r.ctx = ctx
	r.rootUpdates()
	r.podUpdates()
	r.kartaUpdate()
	r.liveLifecycle()
	r.must(r.validate())
}

func positiveEnv(t *testing.T, name string, fallback int) int {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		t.Fatalf("%s must be a positive integer", name)
	}
	return n
}
func (r *run) must(err error) {
	r.t.Helper()
	if err != nil {
		r.report.Errors = append(r.report.Errors, err.Error())
		r.t.Fatal(err)
	}
}
func (r *run) wait(name string, timeout time.Duration, check func() error) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var err error
	for {
		err = check()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			r.must(fmt.Errorf("%s: %w: last check: %v", name, ctx.Err(), err))
		case <-ticker.C:
		}
	}
}
func (r *run) quiet() {
	r.wait("publication quiet", 30*time.Second, func() error {
		r.observer.mu.Lock()
		defer r.observer.mu.Unlock()
		if time.Since(r.observer.last) < 250*time.Millisecond {
			return fmt.Errorf("publications still arriving")
		}
		return nil
	})
}
func (r *run) startPhase(name string) *phaseReport {
	r.quiet()
	p := &phaseReport{Name: name}
	r.report.Phases = append(r.report.Phases, p)
	r.observer.begin()
	return p
}

func (r *run) rootUpdates() {
	p := r.startPhase("root-update")
	targets := r.updateWave(p, false)
	r.finishPhase(p, targets, true, false, nil)
}
func (r *run) podUpdates() {
	p := r.startPhase("pod-update")
	targets := r.updateWave(p, true)
	r.finishPhase(p, targets, false, true, nil)
}
func (r *run) updateWave(p *phaseReport, pods bool) []chain {
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Minute+time.Duration(len(r.chains)/r.report.Rate)*time.Second)
	defer cancel()
	interval := time.Second / time.Duration(r.report.Rate)
	start := time.Now()
	slots := make(chan struct{}, r.report.Concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var targets []chain
	dispatchMissed := 0
	for i := range r.chains {
		scheduled := start.Add(time.Duration(i) * interval)
		timer := time.NewTimer(time.Until(scheduled))
		select {
		case <-ctx.Done():
			timer.Stop()
			dispatchMissed += len(r.chains) - i
			p.Offered += len(r.chains) - i
			goto drain
		case <-timer.C:
		}
		p.Offered++
		if time.Since(scheduled) > interval {
			dispatchMissed++
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			dispatchMissed++
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			lag, started, err := r.updateObject(ctx, i, pods, scheduled, interval)
			mu.Lock()
			defer mu.Unlock()
			if !started {
				p.LateWriterLagNS = append(p.LateWriterLagNS, lag.Nanoseconds())
				p.Missed++
				return
			}
			p.StartLagNS = append(p.StartLagNS, lag.Nanoseconds())
			p.Started++
			if err != nil {
				p.Errored++
				p.Errors = append(p.Errors, err.Error())
			} else {
				p.Completed++
				targets = append(targets, r.chains[i])
			}
		}(i)
	}
drain:
	p.OfferDurationNS = time.Since(start).Nanoseconds()
	drain := time.Now()
	wg.Wait()
	p.Missed += dispatchMissed
	p.APIDrainNS = time.Since(drain).Nanoseconds()
	p.AchievedRate = float64(p.Completed) / time.Since(start).Seconds()
	return targets
}

func (r *run) updateObject(ctx context.Context, i int, pods bool, scheduled time.Time, interval time.Duration) (time.Duration, bool, error) {
	lag := time.Since(scheduled)
	if lag > interval || ctx.Err() != nil {
		return lag, false, nil
	}
	c := &r.chains[i]
	if pods {
		pod := c.Pod.DeepCopy()
		pod.Labels["load.example/revision"] = "1"
		if err := r.api.Update(ctx, pod); err != nil {
			return lag, true, err
		}
		c.Pod = pod
	} else {
		root := c.Root.DeepCopy()
		root.Labels["load.example/revision"] = "1"
		if err := r.api.Update(ctx, root); err != nil {
			return lag, true, err
		}
		c.Root = root
	}
	return lag, true, nil
}

func (r *run) kartaUpdate() {
	p := r.startPhase("karta-fanout")
	root := &r.definition.Spec.StructureDefinition.RootComponent
	root.Name = "reinterpreted"
	for i := range r.definition.Spec.StructureDefinition.ChildComponents {
		c := &r.definition.Spec.StructureDefinition.ChildComponents[i]
		if c.OwnerRef != nil && *c.OwnerRef == "deployment" {
			c.OwnerRef = ptr.To(root.Name)
		}
	}
	root.StatusDefinition.StatusMappings.Completed = root.StatusDefinition.StatusMappings.Running
	root.StatusDefinition.StatusMappings.Running = nil
	p.Offered = 1
	p.Started = 1
	if err := r.api.Update(r.ctx, r.definition); err != nil {
		p.Errored++
		p.Errors = append(p.Errors, err.Error())
		r.must(err)
	}
	p.Completed = 1
	key := identity{r.definition.UID, r.definition.ResourceVersion}
	p.Definition = &key
	r.component = "reinterpreted"
	r.phase = "Completed"
	r.finishPhase(p, r.chains, true, true, &key)
	for _, c := range r.chains {
		for _, object := range []client.Object{c.Root, c.RS, c.Pod} {
			copy := object.DeepCopyObject().(client.Object)
			r.must(r.api.Get(r.ctx, client.ObjectKeyFromObject(object), copy))
			if copy.GetUID() != object.GetUID() || copy.GetResourceVersion() != object.GetResourceVersion() {
				r.must(fmt.Errorf("Karta changed workload identity %s", object.GetName()))
			}
		}
	}
}

func (r *run) finishPhase(p *phaseReport, targets []chain, roots, pods bool, definition *identity) {
	r.must(r.drainPhase(p, targets, roots, pods, definition))
	r.t.Logf("%s: root %d/%d, Pod %d/%d", p.Name, p.Roots.Completed, p.Roots.Expected, p.Pods.Completed, p.Pods.Expected)
}

func (r *run) drainPhase(p *phaseReport, targets []chain, roots, pods bool, definition *identity) error {
	expected := p.Offered
	if definition != nil {
		expected = len(r.chains)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.ctx, 90*time.Second)
	defer cancel()
	var missing []string
	for {
		var rootSamples, podSamples []sample
		missing = nil
		for _, c := range targets {
			if roots {
				s, err := r.observer.match("root", identity{c.Root.UID, c.Root.ResourceVersion}, definition, func(pub publication) bool { return r.rootMatches(pub.Root, c) })
				if err != nil {
					missing = append(missing, err.Error())
				} else {
					rootSamples = append(rootSamples, s)
				}
			}
			if pods {
				s, err := r.observer.match("pod", identity{c.Pod.UID, c.Pod.ResourceVersion}, definition, func(pub publication) bool { return r.podMatches(pub.Pod, c) })
				if err != nil {
					missing = append(missing, err.Error())
				} else {
					podSamples = append(podSamples, s)
				}
			}
		}
		if roots {
			p.Roots = summarize(expected, rootSamples)
		}
		if pods {
			p.Pods = summarize(expected, podSamples)
		}
		if err := r.validate(); err != nil {
			missing = append(missing, err.Error())
		}
		if len(missing) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			p.Timeouts++
			p.Errors = append(p.Errors, missing...)
			goto finished
		case <-time.After(25 * time.Millisecond):
		}
	}
finished:
	p.ConvergenceNS = time.Since(start).Nanoseconds()
	r.observer.mu.Lock()
	p.Writes = r.observer.writes
	p.InvalidPublications = r.observer.invalid
	p.InvalidExamples = append([]invalidExample(nil), r.observer.invalidExamples...)
	p.ObservationOverflow = r.observer.overflow
	r.observer.active = false
	r.observer.mu.Unlock()
	if p.Missed+p.Errored+p.InvalidPublications+p.ObservationOverflow+p.Timeouts+p.Roots.Missing+p.Pods.Missing > 0 || len(missing) > 0 {
		return fmt.Errorf("phase %s failed: missed=%d API errors=%d invalid=%d overflow=%d timeout=%d missing=%d", p.Name, p.Missed, p.Errored, p.InvalidPublications, p.ObservationOverflow, p.Timeouts, p.Roots.Missing+p.Pods.Missing)
	}
	return nil
}

func (r *run) rootMatches(w *store.WorkloadResult, c chain) bool {
	if w == nil {
		return false
	}
	ref := w.Record.Ref
	return w.HasValue &&
		w.Error == "" &&
		w.StatusError == "" &&
		w.Record.UID == c.Root.UID &&
		ref.UID == c.Root.UID &&
		ref.ResourceVersion == c.Root.ResourceVersion &&
		ref.GVK == deploymentGVK &&
		ref.Name == c.Root.Name &&
		ref.Namespace == c.Root.Namespace &&
		w.Record.Karta == r.definition.Name &&
		w.Record.KartaSource == "cluster" &&
		w.Record.OwnerResolution == store.Resolved &&
		w.Record.HasStatus &&
		reflect.DeepEqual(w.Record.Phases, []string{r.phase}) &&
		w.Record.CreatedTimestamp == c.Root.CreationTimestamp.Unix() &&
		reflect.DeepEqual(w.Record.StatusLabels, map[string]string{"root_revision": c.Root.Labels["load.example/revision"]})
}
func (r *run) podMatches(p *store.PodResult, c chain) bool {
	if p == nil {
		return false
	}
	ref := p.Record.Workload
	return p.HasValue &&
		p.Error == "" &&
		p.Record.UID == c.Pod.UID &&
		p.Input.UID == c.Pod.UID &&
		p.Input.ResourceVersion == c.Pod.ResourceVersion &&
		p.Input.GVK == corev1.SchemeGroupVersion.WithKind("Pod") &&
		p.Input.Name == c.Pod.Name &&
		p.Input.Namespace == c.Pod.Namespace &&
		p.Record.Name == c.Pod.Name &&
		p.Record.Namespace == c.Pod.Namespace &&
		ref.UID == c.Root.UID &&
		ref.ResourceVersion == c.Root.ResourceVersion &&
		ref.GVK == deploymentGVK &&
		ref.Name == c.Root.Name &&
		ref.Namespace == c.Root.Namespace &&
		p.Record.Karta == r.definition.Name &&
		p.Record.KartaSource == "cluster" &&
		p.Record.OwnerResolution == store.Resolved &&
		p.Record.Component == r.component &&
		p.Record.Instance == "" &&
		p.Record.Group == "" &&
		reflect.DeepEqual(p.Record.Labels, map[string]string{"root_revision": c.Root.Labels["load.example/revision"], "pod_revision": c.Pod.Labels["load.example/revision"]})
}
func (r *run) validate() error {
	view := r.observer.Capture()
	if len(view.Workloads) != len(r.chains) || len(view.Pods) != len(r.chains) {
		return fmt.Errorf("Store counts: roots=%d Pods=%d expected=%d", len(view.Workloads), len(view.Pods), len(r.chains))
	}
	if len(view.Failures) > 0 {
		return fmt.Errorf("discovery failures: %d", len(view.Failures))
	}
	if len(view.Sources) == 0 {
		return fmt.Errorf("no source health")
	}
	for key, health := range view.Sources {
		if !health.Synced || (health.Error != "" && health.Error != store.ReasonNone) {
			return fmt.Errorf("unhealthy source %s: %+v", key.Resource, health)
		}
	}
	for _, c := range r.chains {
		w, p := r.observer.Workload(c.Root.UID), r.observer.Pod(c.Pod.UID)
		if !r.rootMatches(w, c) {
			return fmt.Errorf("root semantics mismatch %s", c.Root.Name)
		}
		if !r.podMatches(p, c) {
			return fmt.Errorf("Pod semantics mismatch %s", c.Pod.Name)
		}
		if valid, reason := view.Valid(w.Sources, w.Error); !valid {
			return fmt.Errorf("root invalid: %s", reason)
		}
		if valid, reason := view.Valid(p.Sources, p.Error); !valid {
			return fmt.Errorf("Pod invalid: %s", reason)
		}
	}
	return nil
}

func (r *run) liveLifecycle() {
	previous := r.ctx
	ctx, cancel := context.WithTimeout(previous, 5*time.Minute)
	defer cancel()
	r.ctx = ctx
	defer func() { r.ctx = previous }()
	r.quiet()
	old := r.chains
	for _, c := range old {
		for _, object := range []client.Object{c.Pod, c.RS, c.Root} {
			r.must(r.api.Delete(r.ctx, object, client.GracePeriodSeconds(0)))
			r.wait("API deletion", 15*time.Second, func() error {
				copy := object.DeepCopyObject().(client.Object)
				err := r.api.Get(r.ctx, client.ObjectKeyFromObject(object), copy)
				if apierrors.IsNotFound(err) {
					return nil
				}
				if err != nil {
					r.must(err)
				}
				return fmt.Errorf("object still exists")
			})
		}
	}
	r.chains = nil
	r.wait("all publications removed", 90*time.Second, r.validate)
	for _, c := range old {
		next := r.createChain(c.Root.Name)
		if next.Root.UID == c.Root.UID || next.RS.UID == c.RS.UID || next.Pod.UID == c.Pod.UID {
			r.must(fmt.Errorf("recreation reused UID for %s", c.Root.Name))
		}
		r.chains = append(r.chains, next)
	}
	r.wait("live recreation", 90*time.Second, r.validate)
	for _, c := range old {
		if r.observer.Pod(c.Pod.UID) != nil || r.observer.Workload(c.Root.UID) != nil {
			r.must(fmt.Errorf("old UID retained for %s", c.Root.Name))
		}
	}
	r.t.Logf("live lifecycle: deleted and recreated all %d chains with new UIDs", len(old))
}

func (r *run) createChain(name string) chain {
	labels := map[string]string{"app": name, "load.example/revision": "0"}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "example.test/image:v1"}}}}
	root := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "load-test", Labels: map[string]string{"load.example/revision": "0"}}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}, Template: template}}
	r.must(r.api.Create(r.ctx, root))
	root.Status = appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1, Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}, {Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"}}}
	r.must(r.api.Status().Update(r.ctx, root))
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: root.Namespace, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(root, deploymentGVK)}}, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(1)), Selector: root.Spec.Selector, Template: template}}
	r.must(r.api.Create(r.ctx, rs))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: root.Namespace, Labels: map[string]string{"load.example/revision": "0"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}}, Spec: template.Spec}
	r.must(r.api.Create(r.ctx, pod))
	return chain{Root: root, RS: rs, Pod: pod}
}

// Verify that the observer implements every publication Store method.
var _ store.PublicationStore = (*observation)(nil)
