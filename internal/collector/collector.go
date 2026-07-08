// Package collector watches the Kubernetes API through shared informers,
// maintains in-memory caches, and produces both the topology graph and the
// inputs for Prometheus metrics. It never polls on a timer: it reacts to
// informer events and rebuilds a debounced snapshot.
package collector

import (
	"context"
	"log"
	"sync"
	"time"

	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/Ramtinboreili/Pulsebox/internal/topology"
)

// Config controls collector behaviour.
type Config struct {
	// Namespace, when non-empty, scopes all watches to a single namespace
	// (namespaced RBAC mode). Cluster-scoped resources (nodes, PVs) are then
	// skipped. Empty means cluster-wide.
	Namespace string
	// ResyncPeriod for the informers (0 disables periodic resync).
	ResyncPeriod time.Duration
	// RebuildDebounce coalesces bursts of informer events into one rebuild.
	RebuildDebounce time.Duration
	// PVCPendingGrace: a PVC pending longer than this is unhealthy.
	PVCPendingGrace time.Duration
}

func (c Config) withDefaults() Config {
	if c.RebuildDebounce == 0 {
		c.RebuildDebounce = 750 * time.Millisecond
	}
	if c.PVCPendingGrace == 0 {
		c.PVCPendingGrace = 2 * time.Minute
	}
	return c
}

// Collector holds informer listers and the derived topology snapshot.
type Collector struct {
	cfg          Config
	clusterScope bool
	factory      informers.SharedInformerFactory

	nodeLister   corelisters.NodeLister
	nsLister     corelisters.NamespaceLister
	podLister    corelisters.PodLister
	pvLister     corelisters.PersistentVolumeLister
	pvcLister    corelisters.PersistentVolumeClaimLister
	svcLister    corelisters.ServiceLister
	epLister     corelisters.EndpointsLister
	eventLister  corelisters.EventLister
	deployLister appslisters.DeploymentLister
	stsLister    appslisters.StatefulSetLister
	dsLister     appslisters.DaemonSetLister
	rsLister     appslisters.ReplicaSetLister

	restarts *restartTracker
	broker   *broker

	mu        sync.RWMutex
	lastGraph topology.Graph
	synced    bool

	dirty chan struct{}
}

// New builds a collector and wires up all informers, but does not start them.
func New(clientset kubernetes.Interface, cfg Config) *Collector {
	cfg = cfg.withDefaults()
	clusterScope := cfg.Namespace == ""

	var factory informers.SharedInformerFactory
	if clusterScope {
		factory = informers.NewSharedInformerFactory(clientset, cfg.ResyncPeriod)
	} else {
		factory = informers.NewSharedInformerFactoryWithOptions(
			clientset, cfg.ResyncPeriod, informers.WithNamespace(cfg.Namespace))
	}

	c := &Collector{
		cfg:          cfg,
		clusterScope: clusterScope,
		factory:      factory,
		restarts:     newRestartTracker(),
		broker:       newBroker(),
		dirty:        make(chan struct{}, 1),
	}

	// Namespaced resources (available in both modes).
	c.nsLister = factory.Core().V1().Namespaces().Lister()
	c.podLister = factory.Core().V1().Pods().Lister()
	c.pvcLister = factory.Core().V1().PersistentVolumeClaims().Lister()
	c.svcLister = factory.Core().V1().Services().Lister()
	c.epLister = factory.Core().V1().Endpoints().Lister()
	c.eventLister = factory.Core().V1().Events().Lister()
	c.deployLister = factory.Apps().V1().Deployments().Lister()
	c.stsLister = factory.Apps().V1().StatefulSets().Lister()
	c.dsLister = factory.Apps().V1().DaemonSets().Lister()
	c.rsLister = factory.Apps().V1().ReplicaSets().Lister()

	informerList := []cache.SharedIndexInformer{
		factory.Core().V1().Namespaces().Informer(),
		factory.Core().V1().Pods().Informer(),
		factory.Core().V1().PersistentVolumeClaims().Informer(),
		factory.Core().V1().Services().Informer(),
		factory.Core().V1().Endpoints().Informer(),
		factory.Core().V1().Events().Informer(),
		factory.Apps().V1().Deployments().Informer(),
		factory.Apps().V1().StatefulSets().Informer(),
		factory.Apps().V1().DaemonSets().Informer(),
		factory.Apps().V1().ReplicaSets().Informer(),
	}

	// Cluster-scoped resources only in cluster-wide mode.
	if clusterScope {
		c.nodeLister = factory.Core().V1().Nodes().Lister()
		c.pvLister = factory.Core().V1().PersistentVolumes().Lister()
		informerList = append(informerList,
			factory.Core().V1().Nodes().Informer(),
			factory.Core().V1().PersistentVolumes().Informer(),
		)
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { c.markDirty() },
		UpdateFunc: func(interface{}, interface{}) { c.markDirty() },
		DeleteFunc: func(interface{}) { c.markDirty() },
	}
	for _, inf := range informerList {
		// Event handlers registered before Start are honoured on initial sync.
		_, _ = inf.AddEventHandler(handler)
	}

	return c
}

// markDirty signals that a rebuild is needed without blocking the informer.
func (c *Collector) markDirty() {
	select {
	case c.dirty <- struct{}{}:
	default:
	}
}

// Start launches the informers, waits for their caches to sync, then runs the
// debounced rebuild loop until ctx is cancelled.
func (c *Collector) Start(ctx context.Context) error {
	c.factory.Start(ctx.Done())
	log.Println("collector: waiting for informer caches to sync...")
	for typ, ok := range c.factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			log.Printf("collector: cache for %v failed to sync", typ)
		}
	}
	c.mu.Lock()
	c.synced = true
	c.mu.Unlock()
	log.Println("collector: caches synced")

	// Build the first snapshot immediately.
	c.rebuild()
	go c.rebuildLoop(ctx)
	return nil
}

// rebuildLoop coalesces dirty signals and rebuilds at most once per debounce.
func (c *Collector) rebuildLoop(ctx context.Context) {
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.dirty:
			if timer == nil {
				timer = time.NewTimer(c.cfg.RebuildDebounce)
				timerC = timer.C
			}
		case <-timerC:
			timer = nil
			timerC = nil
			c.rebuild()
		}
	}
}

// Synced reports whether the informer caches have completed their initial sync.
func (c *Collector) Synced() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.synced
}

// Snapshot returns the most recent full topology graph.
func (c *Collector) Snapshot() topology.Graph {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastGraph
}

// NamespaceSnapshot returns the graph filtered to a single namespace (plus the
// cluster node and any nodes/PVs referenced by that namespace's pods/PVCs).
func (c *Collector) NamespaceSnapshot(ns string) topology.Graph {
	full := c.Snapshot()
	keep := map[string]bool{}
	for _, n := range full.Nodes {
		if n.Namespace == ns || (n.Kind == topology.KindNamespace && n.Name == ns) || n.Kind == topology.KindCluster {
			keep[n.ID] = true
		}
	}
	// Pull in cluster-scoped neighbours (nodes, PVs) referenced by kept nodes.
	for _, e := range full.Edges {
		if keep[e.Source] {
			keep[e.Target] = true
		}
	}
	out := topology.Graph{UpdatedAt: full.UpdatedAt}
	for _, n := range full.Nodes {
		if keep[n.ID] {
			out.Nodes = append(out.Nodes, n)
		}
	}
	for _, e := range full.Edges {
		if keep[e.Source] && keep[e.Target] {
			out.Edges = append(out.Edges, e)
		}
	}
	return out
}

// Subscribe registers a stream subscriber. It returns the current graph (as a
// snapshot to send first), the diff channel, and an unsubscribe func. Snapshot
// capture and registration happen under the same lock as diff broadcast, so no
// diff is lost or duplicated across the handoff.
func (c *Collector) Subscribe() (topology.Diff, <-chan topology.Diff, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ch := c.broker.subscribe()
	snap := c.lastGraph.AsSnapshot()
	return snap, ch, func() { c.broker.unsubscribe(id) }
}

// StreamSubscribers returns the number of active WebSocket subscribers.
func (c *Collector) StreamSubscribers() int { return c.broker.count() }
