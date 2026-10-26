// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package kartadefinitioninventory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
	"github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/catalog"
)

var (
	ErrNotReady          = errors.New("definition sources not ready")
	ErrUnavailable       = errors.New("definition source unavailable")
	ErrPermissionDenied  = errors.New("definition source permission denied")
	ErrInvalidDefinition = errors.New("invalid Karta definition")

	errStopped = errors.New("inventory stopped")
	errRelist  = errors.New("watch not resumed after a failure; relisting to confirm fresh data")
)

// Inventory serves definitions from the catalog and the cluster. Reads never
// call the API server.
type Inventory struct {
	client        dynamic.Interface
	catalog       []*v1alpha1.Karta
	catalogFilter sourceFilter
	clusterFilter sourceFilter
	log           *slog.Logger

	runOnce sync.Once

	mu    sync.Mutex
	store cache.Store
	// synced turns true at the first Replace, when a complete list is in the store.
	synced   bool
	failure  error
	failures uint64
	// listedFrom is the failure count when the latest list began.
	listedFrom uint64
	stopped    bool
}

// observedStore marks the source synced, and clears older failures, when a list is applied.
type observedStore struct {
	cache.Store
	inventory *Inventory
}

var _ Interface = (*Inventory)(nil)

// New needs a client only when the cluster source is enabled.
func New(client dynamic.Interface, options Options) (*Inventory, error) {
	catalogFilter, clusterFilter, err := options.compile()
	if err != nil {
		return nil, err
	}
	if clusterFilter.enabled && client == nil {
		return nil, errors.New("the cluster definition source requires a client")
	}
	i := &Inventory{
		client:        client,
		catalog:       catalog.List(),
		catalogFilter: catalogFilter,
		clusterFilter: clusterFilter,
		log:           slog.Default(),
	}
	if !catalogFilter.enabled && !clusterFilter.enabled {
		i.log.Warn("both Karta definition sources are disabled; no workload has a definition")
	}
	return i, nil
}

// Run blocks until ctx ends. Afterwards cluster lookups fail; a catalog-only
// inventory keeps serving.
func (i *Inventory) Run(ctx context.Context) error {
	// The Once only claims the run, so a second call fails at once instead of
	// waiting for the first Run to end.
	first := false
	i.runOnce.Do(func() { first = true })
	if !first {
		return errors.New("definition inventory already started")
	}
	if !i.clusterFilter.enabled {
		<-ctx.Done()
		return nil
	}

	store := observedStore{Store: cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc), inventory: i}
	i.update(func() { i.store = store })
	resource := i.client.Resource(v1alpha1.GroupVersion.WithResource("kartas"))
	reflector := cache.NewReflectorWithOptions(
		cache.ToListWatcherWithWatchListSemantics(i.listWatch(resource), i.client),
		&unstructured.Unstructured{}, store,
		cache.ReflectorOptions{Name: "karta-definitions", TypeDescription: "Karta definitions"})
	reflector.RunWithContext(ctx)
	i.update(func() { i.stopped = true })
	return nil
}

// listWatch records every failure; the reflector retries some without reporting them.
func (i *Inventory) listWatch(resource dynamic.ResourceInterface) *cache.ListWatch {
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
			// Later pages continue the same list.
			if options.Continue == "" {
				i.mu.Lock()
				i.listedFrom = i.failures
				i.mu.Unlock()
			}
			list, err := resource.List(ctx, options)
			if err != nil {
				i.update(func() { i.fail(err) })
			}
			return list, err
		},
		WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
			streaming := options.SendInitialEvents != nil && *options.SendInitialEvents
			i.mu.Lock()
			failed := i.failure != nil
			if streaming {
				i.listedFrom = i.failures
			}
			i.mu.Unlock()
			// A resumed watch never marks the end of its replay; only a new list is fresh.
			if failed && !streaming {
				return nil, errRelist
			}
			w, err := resource.Watch(ctx, options)
			switch {
			// A server without streaming lists answers invalid; LIST follows.
			case err != nil && streaming && apierrors.IsInvalid(err):
				return nil, err
			case err != nil:
				i.update(func() { i.fail(err) })
				return nil, err
			}
			events := make(chan watch.Event)
			proxy := watch.NewProxyWatcher(events)
			go func() {
				defer close(events)
				defer w.Stop()
				for {
					select {
					case event, ok := <-w.ResultChan():
						if !ok {
							return
						}
						// Recorded before the reflector sees it. A stopped watch is abandoned.
						if event.Type == watch.Error {
							err := apierrors.FromObject(event.Object)
							i.update(func() {
								if !proxy.Stopping() {
									i.fail(err)
								}
							})
						}
						select {
						case events <- event:
						case <-proxy.StopChan():
							return
						}
					case <-proxy.StopChan():
						return
					}
				}
			}()
			return proxy, nil
		},
	}
}

// fail records err; callers hold i.mu.
func (i *Inventory) fail(err error) {
	i.failure = err
	i.failures++
}

func (s observedStore) Replace(list []any, resourceVersion string) error {
	err := s.Store.Replace(list, resourceVersion)
	first := false
	s.inventory.update(func() {
		first = !s.inventory.synced
		s.inventory.synced = true
		// A failure after this list began is newer than the list.
		if s.inventory.failures == s.inventory.listedFrom {
			s.inventory.failure = nil
		}
	})
	if first {
		s.inventory.log.Info("Karta definition inventory synced", "listed", len(list), "error", s.inventory.Ready())
	}
	return err
}

// update applies change under i.mu.
func (i *Inventory) update(change func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	change()
}

// Ready is nil once lookups can succeed.
func (i *Inventory) Ready() error {
	_, err := i.elect()
	return err
}

// Lookup ignores the requested version and never falls back while a source fails.
func (i *Inventory) Lookup(gvk schema.GroupVersionKind) (Selection, error) {
	elected, err := i.elect()
	if err != nil {
		return Selection{}, err
	}
	return elected.lookup(gvk)
}

// Selections returns nothing while a source fails.
func (i *Inventory) Selections() ([]Selection, error) {
	elected, err := i.elect()
	if err != nil {
		return nil, err
	}
	return elected.list()
}

// elect runs the election over the catalog and the cluster store on every read.
// Karta definitions are few and rarely change, so no result is kept between reads.
func (i *Inventory) elect() (snapshot, error) {
	i.mu.Lock()
	err, store := i.health(), i.store
	i.mu.Unlock()
	if err != nil {
		return snapshot{}, err
	}
	var definitions []*v1alpha1.Karta
	if store != nil {
		for _, obj := range store.List() {
			u := obj.(*unstructured.Unstructured)
			var k v1alpha1.Karta
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &k); err != nil {
				return snapshot{}, fmt.Errorf("%w: decode Karta %q: %w", ErrUnavailable, u.GetName(), err)
			}
			definitions = append(definitions, &k)
		}
	}
	return resolve(i.catalog, definitions, i.catalogFilter, i.clusterFilter), nil
}

// health reports why the cluster source cannot be read; callers hold i.mu.
func (i *Inventory) health() error {
	switch {
	case !i.clusterFilter.enabled:
		return nil
	case i.stopped:
		return fmt.Errorf("%w: %w", ErrUnavailable, errStopped)
	case i.failure != nil && (apierrors.IsForbidden(i.failure) || apierrors.IsUnauthorized(i.failure)):
		return fmt.Errorf("%w: %w", ErrPermissionDenied, i.failure)
	case i.failure != nil:
		return fmt.Errorf("%w: %w", ErrUnavailable, i.failure)
	case !i.synced:
		return ErrNotReady
	}
	return nil
}
