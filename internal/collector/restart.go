package collector

import (
	"math"
	"sync"
	"time"
)

// restartTracker estimates a per-pod container restart rate (restarts/min) from
// successive observations of the cumulative RestartCount. It is the shared
// source of the "restarting rapidly" signal used by both the metrics path and
// the topology path, so the two never disagree.
type restartTracker struct {
	mu       sync.Mutex
	samples  map[string]restartSample // key: namespace/pod/container
	podRates map[string]float64       // key: namespace/pod -> max container rate
}

type restartSample struct {
	count int32
	at    time.Time
	rate  float64
}

func newRestartTracker() *restartTracker {
	return &restartTracker{
		samples:  map[string]restartSample{},
		podRates: map[string]float64{},
	}
}

// observe records a container's cumulative restart count and returns the
// current estimated rate. New restarts push the rate up; quiet time decays it.
func (t *restartTracker) observe(key string, count int32, now time.Time) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.samples[key]
	rate := 0.0
	if ok {
		dt := now.Sub(prev.at).Minutes()
		switch {
		case count > prev.count && dt > 0:
			rate = float64(count-prev.count) / dt
		case count < prev.count:
			// Container/pod was replaced; counter reset.
			rate = 0
		case dt > 0:
			// No new restarts: exponential decay with a ~1min half-life.
			rate = prev.rate * math.Pow(0.5, dt)
			if rate < 0.05 {
				rate = 0
			}
		default:
			rate = prev.rate
		}
	}
	t.samples[key] = restartSample{count: count, at: now, rate: rate}
	return rate
}

// setPodRate stores the aggregate (max) rate for a pod.
func (t *restartTracker) setPodRate(podKey string, rate float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.podRates[podKey] = rate
}

// PodRate returns the last computed restart rate for a pod (0 if unseen).
func (t *restartTracker) PodRate(podKey string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.podRates[podKey]
}

// prune drops tracking state for pods/containers that no longer exist.
func (t *restartTracker) prune(livePods, liveContainers map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.samples {
		if !liveContainers[k] {
			delete(t.samples, k)
		}
	}
	for k := range t.podRates {
		if !livePods[k] {
			delete(t.podRates, k)
		}
	}
}
