// Package metrics exposes Pulsebox's Kubernetes health signals as Prometheus
// gauges/counters. It is a pull-based prometheus.Collector: every scrape reads
// the informer caches directly, so values are always current, and all scoring
// goes through the shared health package (never re-implemented here).
package metrics

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Ramtinboreili/Pulsebox/internal/collector"
	"github.com/Ramtinboreili/Pulsebox/internal/health"
	"github.com/Ramtinboreili/Pulsebox/internal/topology"
)

// Collector adapts a *collector.Collector to prometheus.Collector.
type Collector struct {
	col *collector.Collector

	nodeReady      *prometheus.Desc
	nodeCondition  *prometheus.Desc
	podHealth      *prometheus.Desc
	podRestarts    *prometheus.Desc
	podRestartRate *prometheus.Desc
	wlReady        *prometheus.Desc
	wlDesired      *prometheus.Desc
	pvStatus       *prometheus.Desc
	pvcStatus      *prometheus.Desc
	pvcPending     *prometheus.Desc
	svcEndpoints   *prometheus.Desc
	nsScore        *prometheus.Desc
	clusterScore   *prometheus.Desc
	synced         *prometheus.Desc
}

// New builds the metrics collector.
func New(col *collector.Collector) *Collector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	return &Collector{
		col:            col,
		nodeReady:      d("pulsebox_node_ready", "1 if the node's Ready condition is True, else 0.", "node"),
		nodeCondition:  d("pulsebox_node_condition", "1 if the named node pressure/availability condition is active.", "node", "condition"),
		podHealth:      d("pulsebox_pod_health_status", "Pod health: 0=unhealthy, 1=healthy, 2=starting/degraded, 3=unknown.", "namespace", "pod", "node", "phase"),
		podRestarts:    d("pulsebox_pod_container_restarts_total", "Cumulative container restart count.", "namespace", "pod", "container"),
		podRestartRate: d("pulsebox_pod_restart_rate_per_minute", "Recently observed pod container restart rate (restarts/min).", "namespace", "pod"),
		wlReady:        d("pulsebox_workload_replicas_ready", "Ready/available replicas for a workload.", "namespace", "kind", "name"),
		wlDesired:      d("pulsebox_workload_replicas_desired", "Desired replicas for a workload.", "namespace", "kind", "name"),
		pvStatus:       d("pulsebox_pv_status", "1 for the PersistentVolume's current phase (see phase label).", "pv", "phase"),
		pvcStatus:      d("pulsebox_pvc_status", "1 for the PersistentVolumeClaim's current phase (see phase label).", "namespace", "pvc", "phase"),
		pvcPending:     d("pulsebox_pvc_pending_seconds", "Seconds a PVC has been Pending (0 if bound).", "namespace", "pvc"),
		svcEndpoints:   d("pulsebox_service_endpoints_ready", "Number of ready endpoint addresses backing a Service.", "namespace", "service"),
		nsScore:        d("pulsebox_namespace_health_score", "Aggregate namespace health score, 0.0-1.0.", "namespace"),
		clusterScore:   d("pulsebox_cluster_health_score", "Aggregate cluster health score, 0.0-1.0."),
		synced:         d("pulsebox_collector_synced", "1 once the informer caches have completed their initial sync."),
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.nodeReady, c.nodeCondition, c.podHealth, c.podRestarts, c.podRestartRate,
		c.wlReady, c.wlDesired, c.pvStatus, c.pvcStatus, c.pvcPending,
		c.svcEndpoints, c.nsScore, c.clusterScore, c.synced,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	g := func(d *prometheus.Desc, v float64, lbl ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, lbl...)
	}

	synced := 0.0
	if c.col.Synced() {
		synced = 1.0
	}
	g(c.synced, synced)

	// Nodes.
	for _, n := range c.col.NodeList() {
		ready := 0.0
		conds := map[corev1.NodeConditionType]corev1.ConditionStatus{}
		for _, cond := range n.Status.Conditions {
			conds[cond.Type] = cond.Status
		}
		if conds[corev1.NodeReady] == corev1.ConditionTrue {
			ready = 1
		}
		g(c.nodeReady, ready, n.Name)
		for _, t := range []corev1.NodeConditionType{
			corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable,
		} {
			v := 0.0
			if conds[t] == corev1.ConditionTrue {
				v = 1
			}
			g(c.nodeCondition, v, n.Name, string(t))
		}
	}

	// Pods.
	for _, p := range c.col.PodList() {
		rate := c.col.RestartRate(p.Namespace, p.Name)
		res := health.Pod(p, rate)
		g(c.podHealth, health.PodPhaseValue(res.State), p.Namespace, p.Name, p.Spec.NodeName, string(p.Status.Phase))
		g(c.podRestartRate, rate, p.Namespace, p.Name)
		for _, cs := range p.Status.ContainerStatuses {
			ch <- prometheus.MustNewConstMetric(c.podRestarts, prometheus.CounterValue, float64(cs.RestartCount), p.Namespace, p.Name, cs.Name)
		}
	}

	// Workloads.
	for _, d := range c.col.DeploymentList() {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		g(c.wlReady, float64(d.Status.ReadyReplicas), d.Namespace, "Deployment", d.Name)
		g(c.wlDesired, float64(desired), d.Namespace, "Deployment", d.Name)
	}
	for _, s := range c.col.StatefulSetList() {
		desired := int32(1)
		if s.Spec.Replicas != nil {
			desired = *s.Spec.Replicas
		}
		g(c.wlReady, float64(s.Status.ReadyReplicas), s.Namespace, "StatefulSet", s.Name)
		g(c.wlDesired, float64(desired), s.Namespace, "StatefulSet", s.Name)
	}
	for _, ds := range c.col.DaemonSetList() {
		g(c.wlReady, float64(ds.Status.NumberReady), ds.Namespace, "DaemonSet", ds.Name)
		g(c.wlDesired, float64(ds.Status.DesiredNumberScheduled), ds.Namespace, "DaemonSet", ds.Name)
	}

	// PVs.
	for _, pv := range c.col.PVList() {
		g(c.pvStatus, 1, pv.Name, string(pv.Status.Phase))
	}

	// PVCs.
	grace := c.col.PVCPendingGrace()
	_ = grace
	now := time.Now()
	for _, pvc := range c.col.PVCList() {
		g(c.pvcStatus, 1, pvc.Namespace, pvc.Name, string(pvc.Status.Phase))
		pending := 0.0
		if pvc.Status.Phase == corev1.ClaimPending {
			pending = now.Sub(pvc.CreationTimestamp.Time).Seconds()
		}
		g(c.pvcPending, pending, pvc.Namespace, pvc.Name)
	}

	// Services.
	for _, svc := range c.col.ServiceList() {
		g(c.svcEndpoints, float64(c.col.ReadyEndpoints(svc.Namespace, svc.Name)), svc.Namespace, svc.Name)
	}

	// Namespace + cluster roll-ups: read the already-computed snapshot so the
	// numbers are identical to what the dashboard shows.
	snap := c.col.Snapshot()
	for _, node := range snap.Nodes {
		switch node.Kind {
		case topology.KindNamespace:
			g(c.nsScore, node.Score, node.Name)
		case topology.KindCluster:
			g(c.clusterScore, node.Score)
		}
	}
}
