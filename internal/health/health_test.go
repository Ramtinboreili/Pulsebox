package health

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func nodeWith(ready corev1.ConditionStatus, unschedulable bool, pressures ...corev1.NodeConditionType) *corev1.Node {
	conds := []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}
	for _, p := range pressures {
		conds = append(conds, corev1.NodeCondition{Type: p, Status: corev1.ConditionTrue})
	}
	return &corev1.Node{
		Spec:   corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{Conditions: conds},
	}
}

func TestNode(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want State
	}{
		{"ready", nodeWith(corev1.ConditionTrue, false), Healthy},
		{"notready", nodeWith(corev1.ConditionFalse, false), Unhealthy},
		{"unknown", nodeWith(corev1.ConditionUnknown, false), Unknown},
		{"cordoned", nodeWith(corev1.ConditionTrue, true), Degraded},
		{"mempressure", nodeWith(corev1.ConditionTrue, false, corev1.NodeMemoryPressure), Degraded},
		{"twopressure", nodeWith(corev1.ConditionTrue, false, corev1.NodeMemoryPressure, corev1.NodeDiskPressure), Degraded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Node(tt.node); got.State != tt.want {
				t.Fatalf("Node()=%s reasons=%v, want %s", got.State, got.Reasons, tt.want)
			}
		})
	}
}

func podWith(phase corev1.PodPhase, statuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{Phase: phase, ContainerStatuses: statuses}}
}

func running(ready bool) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "c", Ready: ready, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

func waiting(reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "c", Ready: false, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
}

func TestPod(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		rate float64
		want State
	}{
		{"healthy", podWith(corev1.PodRunning, running(true)), 0, Healthy},
		{"crashloop", podWith(corev1.PodRunning, waiting("CrashLoopBackOff")), 0, Unhealthy},
		{"imagepull", podWith(corev1.PodRunning, waiting("ImagePullBackOff")), 0, Unhealthy},
		{"configerror", podWith(corev1.PodRunning, waiting("CreateContainerConfigError")), 0, Unhealthy},
		{"creating", podWith(corev1.PodPending, waiting("ContainerCreating")), 0, Degraded},
		{"pending", podWith(corev1.PodPending), 0, Degraded},
		{"succeeded", podWith(corev1.PodSucceeded), 0, Healthy},
		{"failed", podWith(corev1.PodFailed), 0, Unhealthy},
		{"partial-notready", podWith(corev1.PodRunning, running(true), running(false)), 0, Degraded},
		{"restart-burst", podWith(corev1.PodRunning, running(true)), 2.0, Unhealthy},
		{"restart-elevated", podWith(corev1.PodRunning, running(true)), 0.3, Degraded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Pod(tt.pod, tt.rate); got.State != tt.want {
				t.Fatalf("Pod()=%s reasons=%v, want %s", got.State, got.Reasons, tt.want)
			}
		})
	}
}

func TestWorkload(t *testing.T) {
	if got := Workload("Deployment", 3, 3, 3); got.State != Healthy {
		t.Fatalf("full rollout: got %s", got.State)
	}
	if got := Workload("Deployment", 3, 1, 1); got.State != Degraded {
		t.Fatalf("partial: got %s", got.State)
	}
	if got := Workload("Deployment", 3, 0, 0); got.State != Unhealthy {
		t.Fatalf("none: got %s", got.State)
	}
	if got := Workload("Deployment", 0, 0, 0); got.State != Healthy {
		t.Fatalf("scaled to zero: got %s", got.State)
	}
	d := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr(int32(2))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	if got := Deployment(d); got.State != Healthy {
		t.Fatalf("deployment adapter: got %s", got.State)
	}
}

func TestPVC(t *testing.T) {
	now := time.Now()
	grace := 2 * time.Minute
	if got := PVC(corev1.ClaimBound, now, now, grace); got.State != Healthy {
		t.Fatalf("bound: got %s", got.State)
	}
	if got := PVC(corev1.ClaimPending, now.Add(-30*time.Second), now, grace); got.State != Degraded {
		t.Fatalf("pending-in-grace: got %s", got.State)
	}
	if got := PVC(corev1.ClaimPending, now.Add(-5*time.Minute), now, grace); got.State != Unhealthy {
		t.Fatalf("pending-stuck: got %s", got.State)
	}
}

func TestPV(t *testing.T) {
	if got := PV(corev1.VolumeBound); got.State != Healthy {
		t.Fatalf("bound: got %s", got.State)
	}
	if got := PV(corev1.VolumeFailed); got.State != Unhealthy {
		t.Fatalf("failed: got %s", got.State)
	}
}

func TestService(t *testing.T) {
	if got := Service(true, 0); got.State != Unhealthy {
		t.Fatalf("no endpoints: got %s", got.State)
	}
	if got := Service(true, 3); got.State != Healthy {
		t.Fatalf("endpoints: got %s", got.State)
	}
	if got := Service(false, 0); got.State != Unknown {
		t.Fatalf("headless: got %s", got.State)
	}
}

func TestAggregate(t *testing.T) {
	empty := Aggregate(nil)
	if empty.State != Unknown {
		t.Fatalf("empty: got %s", empty.State)
	}
	allHealthy := Aggregate([]Result{result(Healthy), result(Healthy)})
	if allHealthy.State != Healthy || allHealthy.Score != 1.0 {
		t.Fatalf("all healthy: got %s %.2f", allHealthy.State, allHealthy.Score)
	}
	// One hard failure should pin state to unhealthy even if the mean is high.
	mixed := Aggregate([]Result{result(Healthy), result(Healthy), result(Healthy), {State: Unhealthy, Score: 0}})
	if mixed.State != Unhealthy {
		t.Fatalf("mixed: got %s (score %.2f)", mixed.State, mixed.Score)
	}
	if mixed.Score <= 0.5 || mixed.Score >= 1.0 {
		t.Fatalf("mixed score out of expected range: %.2f", mixed.Score)
	}
	// Unknown children are ignored.
	ignoreUnknown := Aggregate([]Result{result(Healthy), result(Unknown)})
	if ignoreUnknown.State != Healthy {
		t.Fatalf("ignore unknown: got %s", ignoreUnknown.State)
	}
}

func ptr[T any](v T) *T { return &v }
