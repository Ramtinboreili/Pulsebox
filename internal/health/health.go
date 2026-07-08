// Package health contains the single source of truth for translating raw
// Kubernetes resource status into a Pulsebox health verdict.
//
// Every scoring decision lives here so that the Prometheus metrics path and the
// topology/API path can never disagree: both call the same functions.
package health

import (
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// State is the coarse health classification of a resource.
type State string

const (
	// Healthy: the resource is fully operational.
	Healthy State = "healthy"
	// Degraded: the resource is working but not at full strength, or is still
	// converging (starting, partially rolled out, minor pressure).
	Degraded State = "degraded"
	// Unhealthy: the resource is failing and needs attention.
	Unhealthy State = "unhealthy"
	// Unknown: not enough information to judge.
	Unknown State = "unknown"
)

// Result is the outcome of scoring a single resource.
type Result struct {
	State   State    `json:"state"`
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons,omitempty"`
}

// stateScore is the canonical numeric score for a bare state, used when a
// resource has nothing more nuanced to say.
func stateScore(s State) float64 {
	switch s {
	case Healthy:
		return 1.0
	case Degraded:
		return 0.5
	case Unhealthy:
		return 0.0
	default:
		return 0.5 // unknown sits in the middle; it is not evidence of failure.
	}
}

func result(s State, reasons ...string) Result {
	return Result{State: s, Score: stateScore(s), Reasons: reasons}
}

// PodPhaseValue maps a health result onto the pulsebox_pod_health_status gauge
// enum: 0=unhealthy, 1=healthy, 2=starting/degraded, 3=unknown.
func PodPhaseValue(s State) float64 {
	switch s {
	case Healthy:
		return 1
	case Unhealthy:
		return 0
	case Degraded:
		return 2
	default:
		return 3
	}
}

// containerWaitReasonsDown are waiting reasons that indicate a hard failure
// rather than a transient startup state.
var containerWaitReasonsDown = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"InvalidImageName":           true,
	"RunContainerError":          true,
}

// Node scores a node from its Ready condition and pressure/availability
// conditions, plus cordon state.
func Node(node *corev1.Node) Result {
	reasons := []string{}
	ready := false
	var readyStatus corev1.ConditionStatus = corev1.ConditionUnknown

	for _, c := range node.Status.Conditions {
		switch c.Type {
		case corev1.NodeReady:
			readyStatus = c.Status
			ready = c.Status == corev1.ConditionTrue
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if c.Status == corev1.ConditionTrue {
				reasons = append(reasons, string(c.Type))
			}
		case corev1.NodeNetworkUnavailable:
			if c.Status == corev1.ConditionTrue {
				reasons = append(reasons, "NetworkUnavailable")
			}
		}
	}

	if readyStatus == corev1.ConditionUnknown {
		return Result{State: Unknown, Score: stateScore(Unknown), Reasons: []string{"NodeReady=Unknown (kubelet not reporting)"}}
	}

	if !ready {
		return Result{State: Unhealthy, Score: 0, Reasons: append([]string{"NodeReady=False"}, reasons...)}
	}

	if node.Spec.Unschedulable {
		reasons = append(reasons, "cordoned (SchedulingDisabled)")
	}

	if len(reasons) > 0 {
		// Ready but with pressure or cordoned: degraded, scaled by how many.
		score := 0.7
		if len(reasons) > 1 {
			score = 0.5
		}
		return Result{State: Degraded, Score: score, Reasons: reasons}
	}

	return result(Healthy)
}

// Pod scores a pod from its phase and container statuses. restartRatePerMin is
// the recently observed container restart rate (see collector.RestartTracker);
// a burst of restarts degrades or fails an otherwise-Running pod.
func Pod(pod *corev1.Pod, restartRatePerMin float64) Result {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		// Terminal success (e.g. a completed Job pod). Not a live workload; do
		// not flag it as down.
		return Result{State: Healthy, Score: 1, Reasons: []string{"Succeeded (terminal)"}}
	case corev1.PodFailed:
		reason := pod.Status.Reason
		if reason == "" {
			reason = "pod Failed"
		}
		return Result{State: Unhealthy, Score: 0, Reasons: []string{reason}}
	}

	// Inspect container statuses for both hard failures and startup states.
	reasons := []string{}
	starting := false
	notReady := 0
	total := len(pod.Status.ContainerStatuses)

	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			notReady++
		}
		if cs.State.Waiting != nil {
			r := cs.State.Waiting.Reason
			if containerWaitReasonsDown[r] {
				return Result{State: Unhealthy, Score: 0, Reasons: []string{fmt.Sprintf("%s: %s", cs.Name, r)}}
			}
			if r == "ContainerCreating" || r == "PodInitializing" || r == "" {
				starting = true
			} else {
				reasons = append(reasons, fmt.Sprintf("%s waiting: %s", cs.Name, r))
			}
		}
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 && pod.Status.Phase == corev1.PodRunning {
			reasons = append(reasons, fmt.Sprintf("%s terminated: %s", cs.Name, cs.State.Terminated.Reason))
		}
	}

	if pod.Status.Phase == corev1.PodPending || starting {
		return Result{State: Degraded, Score: 0.4, Reasons: append([]string{"Pending/ContainerCreating"}, reasons...)}
	}

	// A high sustained restart rate is a failure signal even while Running.
	if restartRatePerMin >= 1.0 {
		return Result{State: Unhealthy, Score: 0.2, Reasons: append(reasons, fmt.Sprintf("restarting rapidly (%.1f/min)", restartRatePerMin))}
	}
	if restartRatePerMin >= 0.2 {
		reasons = append(reasons, fmt.Sprintf("elevated restarts (%.1f/min)", restartRatePerMin))
	}

	if pod.Status.Phase == corev1.PodRunning && notReady == 0 && len(reasons) == 0 {
		return result(Healthy)
	}

	if notReady > 0 && notReady < total {
		reasons = append([]string{fmt.Sprintf("%d/%d containers not ready", notReady, total)}, reasons...)
		return Result{State: Degraded, Score: 0.5, Reasons: reasons}
	}
	if notReady > 0 {
		reasons = append([]string{"no containers ready"}, reasons...)
		return Result{State: Unhealthy, Score: 0.1, Reasons: reasons}
	}

	if len(reasons) > 0 {
		return Result{State: Degraded, Score: 0.6, Reasons: reasons}
	}
	return result(Healthy)
}

// Workload scores a Deployment/StatefulSet/DaemonSet-style controller by
// comparing desired against ready/available replicas.
func Workload(kind string, desired, ready, available int32) Result {
	if desired == 0 {
		return Result{State: Healthy, Score: 1, Reasons: []string{"scaled to 0 (desired)"}}
	}
	eff := ready
	if available < eff {
		eff = available
	}
	if eff >= desired {
		return result(Healthy)
	}
	reason := fmt.Sprintf("%d/%d ready", eff, desired)
	if eff == 0 {
		return Result{State: Unhealthy, Score: 0, Reasons: []string{reason + " (none available)"}}
	}
	return Result{State: Degraded, Score: float64(eff) / float64(desired), Reasons: []string{reason}}
}

// Deployment adapts a Deployment to Workload scoring.
func Deployment(d *appsv1.Deployment) Result {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return Workload("Deployment", desired, d.Status.ReadyReplicas, d.Status.AvailableReplicas)
}

// StatefulSet adapts a StatefulSet to Workload scoring.
func StatefulSet(s *appsv1.StatefulSet) Result {
	desired := int32(1)
	if s.Spec.Replicas != nil {
		desired = *s.Spec.Replicas
	}
	return Workload("StatefulSet", desired, s.Status.ReadyReplicas, s.Status.CurrentReplicas)
}

// DaemonSet adapts a DaemonSet to Workload scoring (desired = scheduled onto
// eligible nodes).
func DaemonSet(d *appsv1.DaemonSet) Result {
	desired := d.Status.DesiredNumberScheduled
	return Workload("DaemonSet", desired, d.Status.NumberReady, d.Status.NumberAvailable)
}

// PV scores a PersistentVolume from its phase.
func PV(phase corev1.PersistentVolumePhase) Result {
	switch phase {
	case corev1.VolumeBound, corev1.VolumeAvailable:
		return result(Healthy)
	case corev1.VolumeReleased:
		return Result{State: Degraded, Score: 0.5, Reasons: []string{"Released (awaiting reclaim)"}}
	case corev1.VolumeFailed:
		return Result{State: Unhealthy, Score: 0, Reasons: []string{"Failed"}}
	case corev1.VolumePending:
		return Result{State: Degraded, Score: 0.4, Reasons: []string{"Pending"}}
	default:
		return result(Unknown)
	}
}

// PVC scores a PersistentVolumeClaim. A claim stuck Pending beyond pendingGrace
// (measured from createdAt to now) is treated as unhealthy — this is the common
// Longhorn RWO failure mode we want surfaced.
func PVC(phase corev1.PersistentVolumeClaimPhase, createdAt, now time.Time, pendingGrace time.Duration) Result {
	switch phase {
	case corev1.ClaimBound:
		return result(Healthy)
	case corev1.ClaimLost:
		return Result{State: Unhealthy, Score: 0, Reasons: []string{"Lost (bound PV gone)"}}
	case corev1.ClaimPending:
		age := now.Sub(createdAt)
		if age > pendingGrace {
			return Result{State: Unhealthy, Score: 0, Reasons: []string{fmt.Sprintf("Pending for %s (> %s grace)", age.Round(time.Second), pendingGrace)}}
		}
		return Result{State: Degraded, Score: 0.5, Reasons: []string{"Pending (within grace)"}}
	default:
		return result(Unknown)
	}
}

// Service scores a Service from the number of ready endpoint addresses backing
// it. Zero ready endpoints is a strong silent-outage signal. headless/no-selector
// services should pass hasSelector=false so they are not falsely flagged.
func Service(hasSelector bool, readyEndpoints int) Result {
	if !hasSelector {
		return Result{State: Unknown, Score: stateScore(Unknown), Reasons: []string{"no selector (headless/external)"}}
	}
	if readyEndpoints == 0 {
		return Result{State: Unhealthy, Score: 0, Reasons: []string{"0 ready endpoints"}}
	}
	return result(Healthy)
}

// Aggregate rolls a set of child results up into one score (used for namespaces
// and the whole cluster). Unknown children are ignored so that unscored objects
// do not drag the score down. The rolled-up state is derived from the worst
// present child and the mean score.
func Aggregate(children []Result) Result {
	var sum float64
	var n int
	worst := Healthy
	worstRank := stateRank(Healthy)
	reasons := map[string]int{}

	for _, c := range children {
		if c.State == Unknown {
			continue
		}
		sum += c.Score
		n++
		if r := stateRank(c.State); r > worstRank {
			worstRank = r
			worst = c.State
		}
		if c.State != Healthy {
			for _, reason := range c.Reasons {
				reasons[reason]++
			}
		}
	}

	if n == 0 {
		return result(Unknown)
	}
	mean := sum / float64(n)

	// The mean can round a couple of failures away in a big namespace, so let a
	// present hard failure pin the state even if the average looks fine.
	state := worst
	if mean >= 0.99 {
		state = Healthy
	} else if state == Healthy {
		state = Degraded
	}

	return Result{State: state, Score: mean, Reasons: topReasons(reasons, 5)}
}

func stateRank(s State) int {
	switch s {
	case Healthy:
		return 0
	case Degraded:
		return 1
	case Unhealthy:
		return 2
	default:
		return -1
	}
}

func topReasons(counts map[string]int, limit int) []string {
	if len(counts) == 0 {
		return nil
	}
	type kv struct {
		k string
		v int
	}
	list := make([]kv, 0, len(counts))
	for k, v := range counts {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	out := []string{}
	for i, e := range list {
		if i >= limit {
			break
		}
		if e.v > 1 {
			out = append(out, fmt.Sprintf("%s (x%d)", e.k, e.v))
		} else {
			out = append(out, e.k)
		}
	}
	return out
}

// Worst returns the more severe of two states.
func Worst(a, b State) State {
	if stateRank(a) >= stateRank(b) {
		return a
	}
	return b
}

// NormalizeReasons trims and de-duplicates a reason list for stable output.
func NormalizeReasons(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}
