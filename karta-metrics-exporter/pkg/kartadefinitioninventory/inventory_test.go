// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package kartadefinitioninventory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	v1alpha1 "github.com/dsx-ai-factory/workload-map/karta/sdk/pkg/api/runai/v1alpha1"
)

// The reflector backs off for up to a few seconds between attempts.
const (
	eventuallyTimeout  = 10 * time.Second
	eventuallyInterval = 200 * time.Millisecond
)

var (
	kartasGVR        = v1alpha1.GroupVersion.WithResource("kartas")
	kartasGR         = kartasGVR.GroupResource()
	withCatalog      = Options{Catalog: SourceOptions{Enabled: true, Exclude: []string{"core-pod-v1"}}, Cluster: SourceOptions{Enabled: true}}
	catalogJobOrigin = Origin{Source: SourceCatalog, Name: "batch-job-v1"}
)

// pausedStore holds its first Replace until released, so a spec can act after
// a streaming list ends and before the store applies it.
type pausedStore struct {
	cache.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *pausedStore) Replace(list []any, resourceVersion string) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	return s.Store.Replace(list, resourceVersion)
}

// lateStatus holds the forwarder while it decodes a watch error, so a spec can
// stop the watch before the error is recorded.
type lateStatus struct {
	*unstructured.Unstructured
	read    chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *lateStatus) UnstructuredContent() map[string]any {
	s.once.Do(func() {
		close(s.read)
		<-s.release
	})
	return s.Object
}

func unstructuredKarta(k *v1alpha1.Karta) *unstructured.Unstructured {
	GinkgoHelper()
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(k)
	Expect(err).NotTo(HaveOccurred())
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Karta"))
	return u
}

func newFakeClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{kartasGVR: "KartaList"}, objects...)
}

// start runs the inventory until the spec ends and checks that Run returns cleanly.
func start(inventory *Inventory) context.CancelFunc {
	GinkgoHelper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inventory.Run(ctx) }()
	DeferCleanup(func() {
		cancel()
		Eventually(done, eventuallyTimeout, eventuallyInterval).Should(Receive(BeNil()))
	})
	return cancel
}

func lookupOrigin(inventory *Inventory, gvk schema.GroupVersionKind) Origin {
	GinkgoHelper()
	sel, err := inventory.Lookup(gvk)
	Expect(err).NotTo(HaveOccurred())
	return sel.Origin
}

var _ = Describe("Inventory", func() {
	It("rejects a missing client when the cluster source is enabled", func() {
		_, err := New(nil, DefaultOptions())

		Expect(err).To(MatchError(ContainSubstring("requires a client")))
	})

	It("rejects invalid options", func() {
		_, err := New(newFakeClient(), Options{Catalog: SourceOptions{Include: []string{"nope"}}})

		Expect(err).To(MatchError(ErrInvalidOptions))
	})

	It("serves a catalog-only configuration before Run and after it returns", func() {
		inventory, err := New(nil, Options{Catalog: SourceOptions{Enabled: true}})
		Expect(err).NotTo(HaveOccurred())
		Expect(inventory.Ready()).To(Succeed())
		Expect(lookupOrigin(inventory, jobGVK)).To(Equal(catalogJobOrigin))

		start(inventory)()

		Consistently(func(g Gomega) {
			sel, err := inventory.Lookup(jobGVK)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sel.Origin).To(Equal(catalogJobOrigin))
		}, time.Second, eventuallyInterval).Should(Succeed())
	})

	It("is not ready before Run, and serves nothing from the catalog meanwhile", func() {
		inventory, err := New(newFakeClient(), withCatalog)
		Expect(err).NotTo(HaveOccurred())

		Expect(inventory.Ready()).To(MatchError(ErrNotReady))
		_, err = inventory.Lookup(jobGVK)
		Expect(err).To(MatchError(ErrNotReady))
		listed, err := inventory.Selections()
		Expect(err).To(MatchError(ErrNotReady))
		Expect(listed).To(BeNil())
	})

	It("refuses a second Run", func() {
		inventory, err := New(newFakeClient(), withCatalog)
		Expect(err).NotTo(HaveOccurred())
		start(inventory)
		Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())

		Expect(inventory.Run(context.Background())).To(MatchError(ContainSubstring("already started")))
	})

	Context("with a cluster", func() {
		var (
			client    *fake.FakeDynamicClient
			inventory *Inventory
		)

		BeforeEach(func() {
			client = newFakeClient(unstructuredKarta(newKarta("cluster-job", jobGVK, fromCluster("uid-1", "1"))))
			var err error
			inventory, err = New(client, withCatalog)
			Expect(err).NotTo(HaveOccurred())
		})

		It("serves cluster definitions once the initial list is in", func() {
			start(inventory)

			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())
			Expect(lookupOrigin(inventory, jobGVK)).To(Equal(Origin{Source: SourceCluster, Name: "cluster-job", UID: "uid-1", ResourceVersion: "1"}))
			Expect(lookupOrigin(inventory, deploymentGVK)).To(Equal(Origin{Source: SourceCatalog, Name: "apps-deployment-v1"}))
		})

		It("follows adds, updates and deletes, and serves the catalog again after a delete", func() {
			start(inventory)
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())
			resource := client.Resource(kartasGVR)
			ctx := context.Background()

			_, err := resource.Create(ctx, unstructuredKarta(newKarta("cluster-deployment", deploymentGVK, fromCluster("uid-2", "1"))), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				sel, err := inventory.Lookup(deploymentGVK)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sel.Origin.Name).To(Equal("cluster-deployment"))
			}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

			_, err = resource.Update(ctx, unstructuredKarta(newKarta("cluster-deployment", deploymentGVK, fromCluster("uid-2", "2"))), metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				sel, err := inventory.Lookup(deploymentGVK)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sel.Origin.ResourceVersion).To(Equal("2"))
			}, eventuallyTimeout, eventuallyInterval).Should(Succeed())

			Expect(resource.Delete(ctx, "cluster-job", metav1.DeleteOptions{})).To(Succeed())
			Eventually(func(g Gomega) {
				sel, err := inventory.Lookup(jobGVK)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sel.Origin).To(Equal(catalogJobOrigin))
			}, eventuallyTimeout, eventuallyInterval).Should(Succeed())
		})

		It("reports an invalid cluster winner without falling back", func() {
			start(inventory)
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())

			_, err := client.Resource(kartasGVR).Update(context.Background(),
				unstructuredKarta(newKarta("cluster-job", jobGVK, fromCluster("uid-1", "2"), invalid)), metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() error {
				_, err := inventory.Lookup(jobGVK)
				return err
			}, eventuallyTimeout, eventuallyInterval).Should(MatchError(ErrInvalidDefinition))
			Expect(inventory.Ready()).To(Succeed())
		})

		It("fails closed on a Karta it cannot decode, and recovers once it is gone", func() {
			start(inventory)
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())
			resource := client.Resource(kartasGVR)
			broken := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "run.ai/v1alpha1", "kind": "Karta",
				"metadata": map[string]any{"name": "broken"},
				"spec":     map[string]any{"structureDefinition": "not-an-object"},
			}}

			_, err := resource.Create(context.Background(), broken, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(MatchError(ContainSubstring(`decode Karta "broken"`)))
			Expect(inventory.Ready()).To(MatchError(ErrUnavailable))
			_, err = inventory.Lookup(jobGVK)
			Expect(err).To(MatchError(ErrUnavailable))

			Expect(resource.Delete(context.Background(), "broken", metav1.DeleteOptions{})).To(Succeed())
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())
		})

		It("reports the source as stopped after Run returns", func() {
			cancel := start(inventory)
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())

			cancel()

			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(MatchError(ErrUnavailable))
			Expect(inventory.Ready()).To(MatchError(errStopped))
		})

		It("serves concurrent readers while definitions change", func() {
			start(inventory)
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())
			resource := client.Resource(kartasGVR)

			var wg sync.WaitGroup
			stop := make(chan struct{})
			for range 8 {
				wg.Go(func() {
					defer GinkgoRecover()
					for {
						select {
						case <-stop:
							return
						default:
						}
						sel, err := inventory.Lookup(jobGVK)
						Expect(err).NotTo(HaveOccurred())
						Expect(sel.Definition).NotTo(BeNil())
						_, err = inventory.Selections()
						Expect(err).NotTo(HaveOccurred())
					}
				})
			}
			for n := range 20 {
				k := unstructuredKarta(newKarta("churn", deploymentGVK))
				if n%2 == 0 {
					_, err := resource.Create(context.Background(), k, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
				} else {
					Expect(resource.Delete(context.Background(), "churn", metav1.DeleteOptions{})).To(Succeed())
				}
			}
			close(stop)
			wg.Wait()
		})
	})

	Context("with a failing source", func() {
		var (
			client    *fake.FakeDynamicClient
			inventory *Inventory
			listErr   atomic.Pointer[error]
			watchErr  atomic.Pointer[error]
			watches   atomic.Pointer[watch.RaceFreeFakeWatcher]
		)

		BeforeEach(func() {
			client = newFakeClient(unstructuredKarta(newKarta("cluster-job", jobGVK)))
			listErr.Store(nil)
			watchErr.Store(nil)
			watches.Store(nil)
			client.PrependReactor("list", "kartas", func(ktesting.Action) (bool, runtime.Object, error) {
				if err := listErr.Load(); err != nil {
					return true, nil, *err
				}
				return false, nil, nil
			})
			// A watch delivers only what a spec sends to it, so it never replays on its own.
			client.PrependWatchReactor("kartas", func(ktesting.Action) (bool, watch.Interface, error) {
				if err := watchErr.Load(); err != nil {
					return true, nil, *err
				}
				w := watch.NewRaceFreeFake()
				watches.Store(w)
				return true, w, nil
			})
			var err error
			inventory, err = New(client, withCatalog)
			Expect(err).NotTo(HaveOccurred())
		})

		fail := func(err error) { listErr.Store(&err) }
		// established waits for the reflector's watch, which opens after Replace has made the inventory ready.
		established := func() *watch.RaceFreeFakeWatcher {
			GinkgoHelper()
			Eventually(watches.Load, eventuallyTimeout, eventuallyInterval).ShouldNot(BeNil())
			return watches.Load()
		}

		DescribeTable("classifies a failing LIST and never serves the catalog meanwhile",
			func(cause error, want error) {
				fail(cause)
				start(inventory)

				Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(MatchError(want))
				_, err := inventory.Lookup(jobGVK)
				Expect(err).To(MatchError(want))
				Expect(err).To(MatchError(cause))
			},
			Entry("forbidden", apierrors.NewForbidden(kartasGR, "", errors.New("no list permission")), ErrPermissionDenied),
			Entry("unauthorized", apierrors.NewUnauthorized("unknown token"), ErrPermissionDenied),
			Entry("a missing Karta CRD, which the API server reports as not found", apierrors.NewNotFound(kartasGR, ""), ErrUnavailable),
			Entry("a server error", apierrors.NewInternalError(errors.New("etcd is down")), ErrUnavailable),
		)

		It("recovers once LIST succeeds again", func() {
			fail(apierrors.NewForbidden(kartasGR, "", errors.New("no list permission")))
			start(inventory)
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(MatchError(ErrPermissionDenied))

			listErr.Store(nil)

			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())
			Expect(lookupOrigin(inventory, jobGVK).Source).To(Equal(SourceCluster))
		})

		It("reports a refused watch, and recovers through a relist rather than a resumed watch that has not replayed", func() {
			start(inventory)
			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())
			Expect(lookupOrigin(inventory, deploymentGVK)).To(Equal(Origin{Source: SourceCatalog, Name: "apps-deployment-v1"}))
			resumable := established()

			refused := fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED)
			fail(refused)
			watchErr.Store(&refused)
			// An event before the close makes the reflector resume the watch instead of relisting.
			bookmark := &unstructured.Unstructured{}
			bookmark.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Karta"))
			bookmark.SetResourceVersion("2")
			resumable.Action(watch.Bookmark, bookmark)
			resumable.Stop()

			Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(MatchError(ErrUnavailable))
			_, err := inventory.Lookup(jobGVK)
			Expect(err).To(MatchError(ErrUnavailable))

			_, err = client.Resource(kartasGVR).Create(context.Background(),
				unstructuredKarta(newKarta("cluster-deployment", deploymentGVK)), metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			listErr.Store(nil)
			watchErr.Store(nil)

			Eventually(func(g Gomega) {
				sel, err := inventory.Lookup(deploymentGVK)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(sel.Origin.Name).To(Equal("cluster-deployment"))
			}, eventuallyTimeout, eventuallyInterval).Should(Succeed())
		})
	})

	Context("while a streaming list is being applied", func() {
		DescribeTable("keeps an error that arrives before Replace, and recovers only through a fresh list",
			func(cause metav1.Status, want error) {
				streams := make(chan *watch.RaceFreeFakeWatcher, 8)
				var resumed atomic.Int32
				client := newFakeClient()
				client.PrependWatchReactor("kartas", func(action ktesting.Action) (bool, watch.Interface, error) {
					w := watch.NewRaceFreeFake()
					if initial := action.(ktesting.WatchActionImpl).ListOptions.SendInitialEvents; initial != nil && *initial {
						streams <- w
					} else {
						resumed.Add(1)
					}
					return true, w, nil
				})
				inventory, err := New(client, withCatalog)
				Expect(err).NotTo(HaveOccurred())
				paused := &pausedStore{Store: cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc), entered: make(chan struct{}), release: make(chan struct{})}
				store := observedStore{Store: paused, inventory: inventory}
				inventory.update(func() { inventory.store = store })
				// Without the fake client's opt-out, the reflector uses streaming lists.
				reflector := cache.NewReflectorWithOptions(
					cache.ToListWatcherWithWatchListSemantics(inventory.listWatch(client.Resource(kartasGVR)), nil),
					&unstructured.Unstructured{}, store, cache.ReflectorOptions{})
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					reflector.RunWithContext(ctx)
				}()
				release := sync.OnceFunc(func() { close(paused.release) })
				DeferCleanup(func() {
					cancel()
					release()
					Eventually(done, eventuallyTimeout).Should(BeClosed())
				})
				sendInitialEvents := func(stream *watch.RaceFreeFakeWatcher) {
					stream.Add(unstructuredKarta(newKarta("cluster-job", jobGVK, fromCluster("uid-1", "1"))))
					end := &unstructured.Unstructured{}
					end.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("Karta"))
					end.SetResourceVersion("2")
					end.SetAnnotations(map[string]string{metav1.InitialEventsAnnotationKey: "true"})
					stream.Action(watch.Bookmark, end)
				}

				var first *watch.RaceFreeFakeWatcher
				Eventually(streams, eventuallyTimeout).Should(Receive(&first))
				sendInitialEvents(first)
				Eventually(paused.entered, eventuallyTimeout).Should(BeClosed())

				first.Error(&cause)
				Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(MatchError(want))

				release()

				var second *watch.RaceFreeFakeWatcher
				Eventually(streams, eventuallyTimeout).Should(Receive(&second))
				Expect(inventory.Ready()).To(MatchError(want))
				Expect(resumed.Load()).To(BeZero())

				sendInitialEvents(second)
				Eventually(inventory.Ready, eventuallyTimeout, eventuallyInterval).Should(Succeed())
				Expect(lookupOrigin(inventory, jobGVK).Name).To(Equal("cluster-job"))
			},
			Entry("forbidden", apierrors.NewForbidden(kartasGR, "", errors.New("permission revoked")).ErrStatus, ErrPermissionDenied),
			Entry("too many requests, after which the reflector would resume the watch",
				apierrors.NewTooManyRequests("slow down", 1).ErrStatus, ErrUnavailable),
		)
	})

	Context("health between calls and store writes", func() {
		var (
			ctx       = context.Background()
			inventory *Inventory
			store     observedStore
			lw        *cache.ListWatch
			listErr   atomic.Pointer[error]
			watchErr  atomic.Pointer[error]
			served    atomic.Pointer[watch.RaceFreeFakeWatcher]
		)

		list := func() { _, _ = lw.ListWithContextFunc(ctx, metav1.ListOptions{}) }
		watchCall := func(streaming bool) {
			options := metav1.ListOptions{}
			if streaming {
				options.SendInitialEvents = &streaming
			}
			if w, err := lw.WatchFuncWithContext(ctx, options); err == nil {
				w.Stop()
			}
		}
		replace := func(definitions ...*v1alpha1.Karta) {
			GinkgoHelper()
			items := make([]any, 0, len(definitions))
			for _, k := range definitions {
				items = append(items, unstructuredKarta(k))
			}
			Expect(store.Replace(items, "1")).To(Succeed())
		}

		BeforeEach(func() {
			listErr.Store(nil)
			watchErr.Store(nil)
			served.Store(nil)
			client := newFakeClient()
			client.PrependReactor("list", "kartas", func(ktesting.Action) (bool, runtime.Object, error) {
				if err := listErr.Load(); err != nil {
					return true, nil, *err
				}
				return false, nil, nil
			})
			client.PrependWatchReactor("kartas", func(ktesting.Action) (bool, watch.Interface, error) {
				if err := watchErr.Load(); err != nil {
					return true, nil, *err
				}
				w := watch.NewRaceFreeFake()
				served.Store(w)
				return true, w, nil
			})
			var err error
			inventory, err = New(client, withCatalog)
			Expect(err).NotTo(HaveOccurred())
			store = observedStore{Store: cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc), inventory: inventory}
			inventory.update(func() { inventory.store = store })
			lw = inventory.listWatch(client.Resource(kartasGVR))
		})

		It("stays pending after a successful LIST until Replace applies it", func() {
			list()
			Expect(inventory.Ready()).To(MatchError(ErrNotReady))

			replace()

			Expect(inventory.Ready()).To(Succeed())
		})

		It("keeps a LIST failure past a successful LIST, until an empty Replace applies fresh data", func() {
			replace(newKarta("cluster-job", jobGVK))
			Expect(lookupOrigin(inventory, jobGVK).Source).To(Equal(SourceCluster))
			denied := error(apierrors.NewForbidden(kartasGR, "", errors.New("revoked")))
			listErr.Store(&denied)
			list()
			Expect(inventory.Ready()).To(MatchError(ErrPermissionDenied))

			listErr.Store(nil)
			list()

			Expect(inventory.Ready()).To(MatchError(ErrPermissionDenied))
			_, err := inventory.Lookup(jobGVK)
			Expect(err).To(MatchError(ErrPermissionDenied))

			replace()

			Expect(inventory.Ready()).To(Succeed())
			Expect(lookupOrigin(inventory, jobGVK)).To(Equal(catalogJobOrigin))
		})

		It("keeps a failure through a streaming list until its Replace arrives", func() {
			replace()
			down := error(apierrors.NewServiceUnavailable("restarting"))
			listErr.Store(&down)
			list()
			Expect(inventory.Ready()).To(MatchError(ErrUnavailable))

			listErr.Store(nil)
			watchCall(true)

			Expect(inventory.Ready()).To(MatchError(ErrUnavailable))

			replace(newKarta("cluster-job", jobGVK))

			Expect(inventory.Ready()).To(Succeed())
			Expect(lookupOrigin(inventory, jobGVK).Name).To(Equal("cluster-job"))
		})

		DescribeTable("reports a refused watch call, which the reflector retries on its own",
			func(streaming bool) {
				replace()
				refused := fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED)
				watchErr.Store(&refused)

				watchCall(streaming)

				Expect(inventory.Ready()).To(MatchError(ErrUnavailable))
				Expect(inventory.Ready()).To(MatchError(refused))
			},
			Entry("a resumed watch", false),
			Entry("a streaming list, retried without falling back to LIST", true),
		)

		It("does not let a later page clear a failure recorded after its list began", func() {
			list()
			inventory.update(func() { inventory.fail(apierrors.NewServiceUnavailable("after the first page")) })
			_, err := lw.ListWithContextFunc(ctx, metav1.ListOptions{Continue: "page-2"})
			Expect(err).NotTo(HaveOccurred())

			replace()

			Expect(inventory.Ready()).To(MatchError(ErrUnavailable))
			list()
			replace()
			Expect(inventory.Ready()).To(Succeed())
		})

		It("drops an error from a watch stopped before a fresh list", func() {
			replace()
			w, err := lw.WatchFuncWithContext(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			late := &lateStatus{
				Unstructured: &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "Forbidden", "code": int64(403),
				}},
				read:    make(chan struct{}),
				release: make(chan struct{}),
			}
			release := sync.OnceFunc(func() { close(late.release) })
			DeferCleanup(release)
			served.Load().Error(late)
			Eventually(late.read, eventuallyTimeout).Should(BeClosed())
			w.Stop()
			list()
			replace()

			release()

			Eventually(w.ResultChan(), eventuallyTimeout).Should(BeClosed())
			Expect(inventory.Ready()).To(Succeed())
		})

		It("does not count a server without streaming lists as a failure, since LIST follows", func() {
			// What an API server with the WatchList feature gate off answers.
			rejected := error(apierrors.NewInvalid(schema.GroupKind{Group: "meta.k8s.io", Kind: "ListOptions"}, "",
				field.ErrorList{field.Forbidden(field.NewPath("sendInitialEvents"), "sendInitialEvents is forbidden for watch unless the WatchList feature gate is enabled")}))
			watchErr.Store(&rejected)

			watchCall(true)
			Expect(inventory.Ready()).To(MatchError(ErrNotReady))

			replace()
			watchCall(true)
			Expect(inventory.Ready()).To(Succeed())
		})

		It("records an error sent on an established watch before passing it on, and passes on every event in order", func() {
			replace()
			w, err := lw.WatchFuncWithContext(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(w.Stop)
			server := served.Load()
			server.Add(unstructuredKarta(newKarta("cluster-job", jobGVK)))
			server.Error(&apierrors.NewForbidden(kartasGR, "", errors.New("revoked")).ErrStatus)

			var event watch.Event
			Eventually(w.ResultChan(), eventuallyTimeout).Should(Receive(&event))
			Expect(event.Type).To(Equal(watch.Added))
			Eventually(w.ResultChan(), eventuallyTimeout).Should(Receive(&event))
			Expect(event.Type).To(Equal(watch.Error))

			Expect(inventory.Ready()).To(MatchError(ErrPermissionDenied))
		})

		It("stops the server's watch and closes when stopped with an event still pending", func() {
			w, err := lw.WatchFuncWithContext(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			server := served.Load()
			server.Add(unstructuredKarta(newKarta("cluster-job", jobGVK)))

			w.Stop()

			Eventually(server.IsStopped, eventuallyTimeout, eventuallyInterval).Should(BeTrue())
			Eventually(w.ResultChan(), eventuallyTimeout).Should(BeClosed())
		})
	})
})
