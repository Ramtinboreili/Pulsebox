package collector_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Ramtinboreili/Pulsebox/internal/api"
	"github.com/Ramtinboreili/Pulsebox/internal/collector"
	pbmetrics "github.com/Ramtinboreili/Pulsebox/internal/metrics"
	"github.com/Ramtinboreili/Pulsebox/internal/topology"
)

// This exercises the whole read path against a fake API server: informers →
// graph → metrics and REST, standing in for acceptance criteria 2 and 3 without
// needing a live cluster.
func buildCollector(t *testing.T) (*collector.Collector, context.CancelFunc) {
	t.Helper()
	repl := int32(2)
	objs := []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker1"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu1"},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Capacity:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")},
			}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"},
			Spec:   appsv1.DeploymentSpec{Replicas: &repl},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-x", Namespace: "app"},
			Spec: corev1.PodSpec{NodeName: "worker1", Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "c", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "app"},
			Spec: corev1.PodSpec{NodeName: "worker1", Containers: []corev1.Container{{Name: "c", Image: "broken"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "c", Ready: false, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "app"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web-svc", Namespace: "app"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.0.0.1"}},
		&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "web-svc", Namespace: "app"},
			Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "1.2.3.4", TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "web-x"}}}}}},
	}
	client := fake.NewSimpleClientset(objs...)
	col := collector.New(client, collector.Config{ResyncPeriod: 0, RebuildDebounce: 50 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	if err := col.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start: %v", err)
	}
	return col, cancel
}

func findNode(g topology.Graph, id string) (topology.Node, bool) {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return topology.Node{}, false
}

func TestCollectorGraph(t *testing.T) {
	col, cancel := buildCollector(t)
	defer cancel()

	g := col.Snapshot()
	if len(g.Nodes) == 0 {
		t.Fatal("empty graph after sync")
	}
	if _, ok := findNode(g, "Cluster"); !ok {
		t.Fatal("missing cluster root node")
	}
	if n, ok := findNode(g, "Pod/app/web-x"); !ok || n.Health != "healthy" {
		t.Fatalf("web-x pod: found=%v health=%q", ok, n.Health)
	}
	if n, ok := findNode(g, "Pod/app/bad"); !ok || n.Health != "unhealthy" {
		t.Fatalf("bad pod: found=%v health=%q", ok, n.Health)
	}
	if n, ok := findNode(g, "Node/gpu1"); !ok || n.Meta["gpu"] != "true" {
		t.Fatalf("gpu node not flagged: found=%v meta=%v", ok, n.Meta)
	}
	if n, ok := findNode(g, "Service/app/web-svc"); !ok || n.Health != "healthy" {
		t.Fatalf("service with endpoints should be healthy: found=%v health=%q", ok, n.Health)
	}
	// The unhealthy pod should drag the namespace roll-up below perfect.
	if n, ok := findNode(g, "Namespace/app"); !ok || n.Score >= 1.0 {
		t.Fatalf("namespace score should reflect the failing pod: found=%v score=%.2f", ok, n.Score)
	}
	// pod -> node placement edge should exist.
	var hasRunsOn bool
	for _, e := range g.Edges {
		if e.Source == "Pod/app/web-x" && e.Target == "Node/worker1" && e.Kind == topology.EdgeRunsOn {
			hasRunsOn = true
		}
	}
	if !hasRunsOn {
		t.Fatal("missing pod->node runs-on edge")
	}
}

func TestMetricsOutput(t *testing.T) {
	col, cancel := buildCollector(t)
	defer cancel()

	reg := prometheus.NewRegistry()
	reg.MustRegister(pbmetrics.New(col))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]bool{}
	for _, mf := range mfs {
		seen[mf.GetName()] = true
	}
	for _, want := range []string{
		"pulsebox_cluster_health_score", "pulsebox_pod_health_status",
		"pulsebox_node_ready", "pulsebox_pvc_status", "pulsebox_service_endpoints_ready",
	} {
		if !seen[want] {
			t.Errorf("missing metric %s", want)
		}
	}
}

func TestTopologyAPI(t *testing.T) {
	col, cancel := buildCollector(t)
	defer cancel()

	mux := http.NewServeMux()
	api.New(col).RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/topology")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var g topology.Graph
	if err := json.NewDecoder(res.Body).Decode(&g); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(g.Nodes) == 0 {
		t.Fatal("topology API returned no nodes")
	}

	// namespace-scoped endpoint should return a subset.
	res2, err := http.Get(srv.URL + "/api/topology/app")
	if err != nil {
		t.Fatalf("get ns: %v", err)
	}
	defer res2.Body.Close()
	var ng topology.Graph
	if err := json.NewDecoder(res2.Body).Decode(&ng); err != nil {
		t.Fatalf("decode ns: %v", err)
	}
	if len(ng.Nodes) == 0 || len(ng.Nodes) > len(g.Nodes) {
		t.Fatalf("ns graph size unexpected: %d (full %d)", len(ng.Nodes), len(g.Nodes))
	}

	// resource detail endpoint.
	res3, err := http.Get(srv.URL + "/api/resource/pod/app/web-x")
	if err != nil {
		t.Fatalf("get resource: %v", err)
	}
	defer res3.Body.Close()
	body := map[string]interface{}{}
	if err := json.NewDecoder(res3.Body).Decode(&body); err != nil {
		t.Fatalf("decode resource: %v", err)
	}
	if body["resource"] == nil {
		t.Fatal("resource detail missing resource payload")
	}
}
