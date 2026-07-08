package collector

import (
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// These accessors expose the informer caches to the metrics and API layers
// without letting them reach into informer internals. They read shared caches,
// which client-go guarantees are safe for concurrent read.

func (c *Collector) ClusterScope() bool             { return c.clusterScope }
func (c *Collector) PVCPendingGrace() time.Duration { return c.cfg.PVCPendingGrace }

func (c *Collector) NodeList() []*corev1.Node {
	if c.nodeLister == nil {
		return nil
	}
	out, _ := c.nodeLister.List(labels.Everything())
	return out
}

func (c *Collector) PodList() []*corev1.Pod {
	out, _ := c.podLister.List(labels.Everything())
	return out
}

func (c *Collector) NamespaceList() []*corev1.Namespace {
	if c.nsLister == nil {
		return nil
	}
	out, _ := c.nsLister.List(labels.Everything())
	return out
}

func (c *Collector) PVList() []*corev1.PersistentVolume {
	if c.pvLister == nil {
		return nil
	}
	out, _ := c.pvLister.List(labels.Everything())
	return out
}

func (c *Collector) PVCList() []*corev1.PersistentVolumeClaim {
	out, _ := c.pvcLister.List(labels.Everything())
	return out
}

func (c *Collector) ServiceList() []*corev1.Service {
	out, _ := c.svcLister.List(labels.Everything())
	return out
}

func (c *Collector) DeploymentList() []*appsv1.Deployment {
	out, _ := c.deployLister.List(labels.Everything())
	return out
}

func (c *Collector) StatefulSetList() []*appsv1.StatefulSet {
	out, _ := c.stsLister.List(labels.Everything())
	return out
}

func (c *Collector) DaemonSetList() []*appsv1.DaemonSet {
	out, _ := c.dsLister.List(labels.Everything())
	return out
}

// RestartRate returns the recently observed restart rate (restarts/min) for a
// pod, shared with the topology scoring so both output paths agree.
func (c *Collector) RestartRate(ns, pod string) float64 {
	return c.restarts.PodRate(ns + "/" + pod)
}

// ReadyEndpoints returns the count of ready endpoint addresses for a service.
func (c *Collector) ReadyEndpoints(ns, name string) int {
	n, _ := c.endpointsFor(ns, name)
	return n
}

// GetResource returns the full cached object for a kind/namespace/name, for the
// detail endpoint. kind is matched case-insensitively against the topology kind.
func (c *Collector) GetResource(kind, ns, name string) (interface{}, error) {
	switch strings.ToLower(kind) {
	case "pod":
		return c.podLister.Pods(ns).Get(name)
	case "node":
		if c.nodeLister == nil {
			return nil, fmt.Errorf("nodes not watched in namespaced mode")
		}
		return c.nodeLister.Get(name)
	case "namespace":
		if c.nsLister == nil {
			return nil, fmt.Errorf("namespaces not watched in namespaced mode")
		}
		return c.nsLister.Get(name)
	case "deployment":
		return c.deployLister.Deployments(ns).Get(name)
	case "statefulset":
		return c.stsLister.StatefulSets(ns).Get(name)
	case "daemonset":
		return c.dsLister.DaemonSets(ns).Get(name)
	case "service":
		return c.svcLister.Services(ns).Get(name)
	case "persistentvolumeclaim", "pvc":
		return c.pvcLister.PersistentVolumeClaims(ns).Get(name)
	case "persistentvolume", "pv":
		if c.pvLister == nil {
			return nil, fmt.Errorf("persistentvolumes not watched in namespaced mode")
		}
		return c.pvLister.Get(name)
	default:
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
}

// WarningEvents returns recent Warning-type event messages for an object, most
// recent first, capped at limit. Used for tooltips/detail, never as a primary
// health source.
func (c *Collector) WarningEvents(kind, ns, name string, limit int) []string {
	sel := labels.Everything()
	scope := ns
	if kind == "Node" || kind == "PersistentVolume" {
		scope = "" // cluster-scoped objects have events in the default namespace
	}
	var evs []*corev1.Event
	if scope == "" {
		evs, _ = c.eventLister.List(sel)
	} else {
		evs, _ = c.eventLister.Events(scope).List(sel)
	}
	type msg struct {
		at   time.Time
		text string
	}
	matches := []msg{}
	for _, e := range evs {
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		if e.InvolvedObject.Kind != kind || e.InvolvedObject.Name != name {
			continue
		}
		t := e.LastTimestamp.Time
		if t.IsZero() {
			t = e.EventTime.Time
		}
		matches = append(matches, msg{at: t, text: fmt.Sprintf("%s: %s", e.Reason, strings.TrimSpace(e.Message))})
	}
	// most recent first
	for i := 0; i < len(matches); i++ {
		for j := i + 1; j < len(matches); j++ {
			if matches[j].at.After(matches[i].at) {
				matches[i], matches[j] = matches[j], matches[i]
			}
		}
	}
	out := []string{}
	for i, m := range matches {
		if i >= limit {
			break
		}
		out = append(out, m.text)
	}
	return out
}
