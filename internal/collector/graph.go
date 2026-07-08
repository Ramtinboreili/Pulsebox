package collector

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/Ramtinboreili/Pulsebox/internal/health"
	"github.com/Ramtinboreili/Pulsebox/internal/topology"
)

// id builds a stable graph node id. Cluster-scoped kinds pass ns="".
func id(kind, ns, name string) string {
	if ns == "" {
		return kind + "/" + name
	}
	return kind + "/" + ns + "/" + name
}

const clusterID = topology.KindCluster

// rebuild constructs a fresh graph and publishes the diff to subscribers.
func (c *Collector) rebuild() {
	g := c.buildGraph()

	c.mu.Lock()
	prev := c.lastGraph
	c.lastGraph = g
	var d topology.Diff
	if prev.UpdatedAt.IsZero() {
		d = g.AsSnapshot()
	} else {
		d = g.DiffAgainst(prev)
	}
	if !d.Empty() {
		c.broker.broadcast(d)
	}
	c.mu.Unlock()
}

// buildGraph reads the informer caches and assembles the topology graph with
// health scored via the shared health package.
func (c *Collector) buildGraph() topology.Graph {
	now := time.Now()
	nodes := []topology.Node{}
	edges := []topology.Edge{}
	exists := map[string]bool{}

	add := func(n topology.Node) {
		nodes = append(nodes, n)
		exists[n.ID] = true
	}
	addEdge := func(src, dst, kind string) {
		edges = append(edges, topology.Edge{
			ID:     kind + ":" + src + "->" + dst,
			Source: src, Target: dst, Kind: kind,
		})
	}

	// Health results accumulated for roll-up.
	nsChildren := map[string][]health.Result{}
	var clusterChildren []health.Result

	// --- Nodes (cluster-scoped) -------------------------------------------
	if c.clusterScope && c.nodeLister != nil {
		kNodes, _ := c.nodeLister.List(labels.Everything())
		for _, n := range kNodes {
			res := health.Node(n)
			clusterChildren = append(clusterChildren, res)
			gpu, gpuCount := nodeGPU(n)
			meta := map[string]string{
				"schedulable": fmt.Sprintf("%t", !n.Spec.Unschedulable),
				"kubelet":     n.Status.NodeInfo.KubeletVersion,
				"roles":       strings.Join(nodeRoles(n), ","),
			}
			if gpu {
				meta["gpu"] = "true"
				if gpuCount != "" {
					meta["gpuCount"] = gpuCount
				}
			}
			if taints := nodeTaints(n); taints != "" {
				meta["taints"] = taints
			}
			add(topology.Node{
				ID: id(topology.KindNode, "", n.Name), Kind: topology.KindNode, Name: n.Name,
				Health: string(res.State), Score: res.Score, Reasons: health.NormalizeReasons(res.Reasons), Meta: meta,
			})
			addEdge(clusterID, id(topology.KindNode, "", n.Name), topology.EdgeContains)
		}
	}

	// --- PersistentVolumes (cluster-scoped) -------------------------------
	if c.clusterScope && c.pvLister != nil {
		pvs, _ := c.pvLister.List(labels.Everything())
		for _, pv := range pvs {
			res := health.PV(pv.Status.Phase)
			meta := map[string]string{"phase": string(pv.Status.Phase)}
			if pv.Spec.StorageClassName != "" {
				meta["storageClass"] = pv.Spec.StorageClassName
			}
			if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
				meta["capacity"] = q.String()
			}
			add(topology.Node{
				ID: id(topology.KindPV, "", pv.Name), Kind: topology.KindPV, Name: pv.Name,
				Health: string(res.State), Score: res.Score, Reasons: health.NormalizeReasons(res.Reasons), Meta: meta,
			})
		}
	}

	// --- Workloads --------------------------------------------------------
	deploys, _ := c.deployLister.List(labels.Everything())
	for _, d := range deploys {
		res := health.Deployment(d)
		nsChildren[d.Namespace] = append(nsChildren[d.Namespace], res)
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		add(workloadNode(topology.KindDeployment, d.Namespace, d.Name, res, d.Status.ReadyReplicas, desired))
		addEdge(id(topology.KindNamespace, "", d.Namespace), id(topology.KindDeployment, d.Namespace, d.Name), topology.EdgeContains)
	}
	statefulsets, _ := c.stsLister.List(labels.Everything())
	for _, s := range statefulsets {
		res := health.StatefulSet(s)
		nsChildren[s.Namespace] = append(nsChildren[s.Namespace], res)
		desired := int32(1)
		if s.Spec.Replicas != nil {
			desired = *s.Spec.Replicas
		}
		add(workloadNode(topology.KindStatefulSet, s.Namespace, s.Name, res, s.Status.ReadyReplicas, desired))
		addEdge(id(topology.KindNamespace, "", s.Namespace), id(topology.KindStatefulSet, s.Namespace, s.Name), topology.EdgeContains)
	}
	daemonsets, _ := c.dsLister.List(labels.Everything())
	for _, ds := range daemonsets {
		res := health.DaemonSet(ds)
		nsChildren[ds.Namespace] = append(nsChildren[ds.Namespace], res)
		add(workloadNode(topology.KindDaemonSet, ds.Namespace, ds.Name, res, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
		addEdge(id(topology.KindNamespace, "", ds.Namespace), id(topology.KindDaemonSet, ds.Namespace, ds.Name), topology.EdgeContains)
	}

	// --- PVCs -------------------------------------------------------------
	pvcs, _ := c.pvcLister.List(labels.Everything())
	for _, pvc := range pvcs {
		res := health.PVC(pvc.Status.Phase, pvc.CreationTimestamp.Time, now, c.cfg.PVCPendingGrace)
		nsChildren[pvc.Namespace] = append(nsChildren[pvc.Namespace], res)
		meta := map[string]string{"phase": string(pvc.Status.Phase)}
		if pvc.Spec.StorageClassName != nil {
			meta["storageClass"] = *pvc.Spec.StorageClassName
		}
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			meta["capacity"] = q.String()
		}
		add(topology.Node{
			ID: id(topology.KindPVC, pvc.Namespace, pvc.Name), Kind: topology.KindPVC, Name: pvc.Name, Namespace: pvc.Namespace,
			Health: string(res.State), Score: res.Score, Reasons: health.NormalizeReasons(res.Reasons), Meta: meta,
		})
		addEdge(id(topology.KindNamespace, "", pvc.Namespace), id(topology.KindPVC, pvc.Namespace, pvc.Name), topology.EdgeContains)
		if pvc.Spec.VolumeName != "" {
			addEdge(id(topology.KindPVC, pvc.Namespace, pvc.Name), id(topology.KindPV, "", pvc.Spec.VolumeName), topology.EdgeBacks)
		}
	}

	// --- Services ---------------------------------------------------------
	services, _ := c.svcLister.List(labels.Everything())
	for _, svc := range services {
		ready, targets := c.endpointsFor(svc.Namespace, svc.Name)
		res := health.Service(len(svc.Spec.Selector) > 0, ready)
		nsChildren[svc.Namespace] = append(nsChildren[svc.Namespace], res)
		meta := map[string]string{
			"type":      string(svc.Spec.Type),
			"clusterIP": svc.Spec.ClusterIP,
			"endpoints": fmt.Sprintf("%d", ready),
		}
		add(topology.Node{
			ID: id(topology.KindService, svc.Namespace, svc.Name), Kind: topology.KindService, Name: svc.Name, Namespace: svc.Namespace,
			Health: string(res.State), Score: res.Score, Reasons: health.NormalizeReasons(res.Reasons), Meta: meta,
		})
		addEdge(id(topology.KindNamespace, "", svc.Namespace), id(topology.KindService, svc.Namespace, svc.Name), topology.EdgeContains)
		for _, podName := range targets {
			addEdge(id(topology.KindService, svc.Namespace, svc.Name), id(topology.KindPod, svc.Namespace, podName), topology.EdgeRoutes)
		}
	}

	// --- Pods -------------------------------------------------------------
	pods, _ := c.podLister.List(labels.Everything())
	livePods := map[string]bool{}
	liveContainers := map[string]bool{}
	for _, pod := range pods {
		podKey := pod.Namespace + "/" + pod.Name
		livePods[podKey] = true

		// Update restart tracking and compute the pod's max restart rate.
		var podRate float64
		var totalRestarts int32
		for _, cs := range pod.Status.ContainerStatuses {
			ckey := podKey + "/" + cs.Name
			liveContainers[ckey] = true
			r := c.restarts.observe(ckey, cs.RestartCount, now)
			if r > podRate {
				podRate = r
			}
			totalRestarts += cs.RestartCount
		}
		c.restarts.setPodRate(podKey, podRate)

		res := health.Pod(pod, podRate)
		nsChildren[pod.Namespace] = append(nsChildren[pod.Namespace], res)

		meta := map[string]string{
			"phase":    string(pod.Status.Phase),
			"restarts": fmt.Sprintf("%d", totalRestarts),
		}
		if pod.Spec.NodeName != "" {
			meta["node"] = pod.Spec.NodeName
		}
		if len(pod.Spec.Containers) > 0 {
			meta["image"] = pod.Spec.Containers[0].Image
		}
		add(topology.Node{
			ID: id(topology.KindPod, pod.Namespace, pod.Name), Kind: topology.KindPod, Name: pod.Name, Namespace: pod.Namespace,
			Health: string(res.State), Score: res.Score, Reasons: health.NormalizeReasons(res.Reasons), Meta: meta,
		})

		// Parent: owning workload if resolvable, else the namespace.
		if wid, ok := c.workloadIDForPod(pod); ok {
			addEdge(wid, id(topology.KindPod, pod.Namespace, pod.Name), topology.EdgeContains)
		} else {
			addEdge(id(topology.KindNamespace, "", pod.Namespace), id(topology.KindPod, pod.Namespace, pod.Name), topology.EdgeContains)
		}
		// Placement.
		if pod.Spec.NodeName != "" {
			addEdge(id(topology.KindPod, pod.Namespace, pod.Name), id(topology.KindNode, "", pod.Spec.NodeName), topology.EdgeRunsOn)
		}
		// Mounted claims.
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				addEdge(id(topology.KindPod, pod.Namespace, pod.Name), id(topology.KindPVC, pod.Namespace, v.PersistentVolumeClaim.ClaimName), topology.EdgeMounts)
			}
		}
	}
	c.restarts.prune(livePods, liveContainers)

	// --- Namespaces (with rolled-up health) -------------------------------
	if c.clusterScope && c.nsLister != nil {
		namespaces, _ := c.nsLister.List(labels.Everything())
		for _, ns := range namespaces {
			res := health.Aggregate(nsChildren[ns.Name])
			clusterChildren = append(clusterChildren, res)
			add(topology.Node{
				ID: id(topology.KindNamespace, "", ns.Name), Kind: topology.KindNamespace, Name: ns.Name,
				Health: string(res.State), Score: res.Score, Reasons: res.Reasons,
				Meta: map[string]string{"phase": string(ns.Status.Phase)},
			})
			addEdge(clusterID, id(topology.KindNamespace, "", ns.Name), topology.EdgeContains)
		}
	} else {
		// Namespaced mode: synthesize the single watched namespace node.
		ns := c.cfg.Namespace
		res := health.Aggregate(nsChildren[ns])
		clusterChildren = append(clusterChildren, res)
		add(topology.Node{
			ID: id(topology.KindNamespace, "", ns), Kind: topology.KindNamespace, Name: ns,
			Health: string(res.State), Score: res.Score, Reasons: res.Reasons,
		})
		addEdge(clusterID, id(topology.KindNamespace, "", ns), topology.EdgeContains)
	}

	// --- Cluster root -----------------------------------------------------
	clusterRes := health.Aggregate(clusterChildren)
	scope := "cluster-wide"
	if !c.clusterScope {
		scope = "namespace:" + c.cfg.Namespace
	}
	add(topology.Node{
		ID: clusterID, Kind: topology.KindCluster, Name: "cluster",
		Health: string(clusterRes.State), Score: clusterRes.Score, Reasons: clusterRes.Reasons,
		Meta: map[string]string{"scope": scope},
	})

	// Keep only edges whose both endpoints exist (drops dangling references,
	// e.g. pods on nodes we can't see in namespaced mode).
	kept := edges[:0]
	seen := map[string]bool{}
	for _, e := range edges {
		if exists[e.Source] && exists[e.Target] && !seen[e.ID] {
			seen[e.ID] = true
			kept = append(kept, e)
		}
	}

	// Deterministic ordering keeps diffs minimal and output stable.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	sort.Slice(kept, func(i, j int) bool { return kept[i].ID < kept[j].ID })

	return topology.Graph{Nodes: nodes, Edges: kept, UpdatedAt: now}
}

func workloadNode(kind, ns, name string, res health.Result, ready, desired int32) topology.Node {
	return topology.Node{
		ID: id(kind, ns, name), Kind: kind, Name: name, Namespace: ns,
		Health: string(res.State), Score: res.Score, Reasons: health.NormalizeReasons(res.Reasons),
		Meta: map[string]string{"ready": fmt.Sprintf("%d/%d", ready, desired)},
	}
}

// workloadIDForPod resolves the top-level workload that owns a pod, following
// ReplicaSet -> Deployment. Returns false for bare/Job pods.
func (c *Collector) workloadIDForPod(pod *corev1.Pod) (string, bool) {
	for _, o := range pod.OwnerReferences {
		switch o.Kind {
		case "ReplicaSet":
			rs, err := c.rsLister.ReplicaSets(pod.Namespace).Get(o.Name)
			if err == nil {
				for _, ro := range rs.OwnerReferences {
					if ro.Kind == "Deployment" {
						return id(topology.KindDeployment, pod.Namespace, ro.Name), true
					}
				}
			}
		case "StatefulSet":
			return id(topology.KindStatefulSet, pod.Namespace, o.Name), true
		case "DaemonSet":
			return id(topology.KindDaemonSet, pod.Namespace, o.Name), true
		}
	}
	return "", false
}

// endpointsFor returns the ready endpoint count and the names of pods backing a
// service (ready and not-ready, for edge rendering).
func (c *Collector) endpointsFor(ns, name string) (readyCount int, podNames []string) {
	ep, err := c.epLister.Endpoints(ns).Get(name)
	if err != nil {
		return 0, nil
	}
	seen := map[string]bool{}
	collect := func(addrs []corev1.EndpointAddress) {
		for _, a := range addrs {
			if a.TargetRef != nil && a.TargetRef.Kind == "Pod" && !seen[a.TargetRef.Name] {
				seen[a.TargetRef.Name] = true
				podNames = append(podNames, a.TargetRef.Name)
			}
		}
	}
	for _, s := range ep.Subsets {
		readyCount += len(s.Addresses)
		collect(s.Addresses)
		collect(s.NotReadyAddresses)
	}
	return readyCount, podNames
}

func nodeGPU(n *corev1.Node) (bool, string) {
	for res, q := range n.Status.Capacity {
		if strings.Contains(strings.ToLower(string(res)), "gpu") && !q.IsZero() {
			return true, q.String()
		}
	}
	if n.Labels["accelerator"] != "" || n.Labels["nvidia.com/gpu.present"] == "true" {
		return true, ""
	}
	return false, ""
}

func nodeRoles(n *corev1.Node) []string {
	roles := []string{}
	for k := range n.Labels {
		if strings.HasPrefix(k, "node-role.kubernetes.io/") {
			if r := strings.TrimPrefix(k, "node-role.kubernetes.io/"); r != "" {
				roles = append(roles, r)
			}
		}
	}
	sort.Strings(roles)
	return roles
}

func nodeTaints(n *corev1.Node) string {
	parts := []string{}
	for _, t := range n.Spec.Taints {
		parts = append(parts, t.Key+"="+t.Value+":"+string(t.Effect))
	}
	return strings.Join(parts, ",")
}
