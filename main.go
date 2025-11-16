package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ContainerHealthExporter main exporter structure
type ContainerHealthExporter struct {
	dockerClient *client.Client

	// Prometheus metrics
	containerHealthStatus         *prometheus.GaugeVec
	containerHealthCheckDuration  *prometheus.GaugeVec
}

// NewContainerHealthExporter creates a new instance of the exporter
func NewContainerHealthExporter() (*ContainerHealthExporter, error) {
	// Connect to Docker daemon
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %v", err)
	}

	exporter := &ContainerHealthExporter{
		dockerClient: cli,
	}

	// Define metrics
	exporter.containerHealthStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "container_health_status",
			Help: "Health status of containers (0=unhealthy, 1=healthy, 2=starting, 3=unset)",
		},
		[]string{"container_id", "container_name", "image"},
	)

	exporter.containerHealthCheckDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "container_health_check_duration_seconds",
			Help: "Duration of the last health check in seconds",
		},
		[]string{"container_id", "container_name"},
	)

	// Register metrics
	prometheus.MustRegister(exporter.containerHealthStatus)
	prometheus.MustRegister(exporter.containerHealthCheckDuration)

	return exporter, nil
}

// mapHealthStatusToNumber converts health status string to numeric value
func mapHealthStatusToNumber(status string) float64 {
	switch status {
	case "healthy":
		return 1
	case "unhealthy":
		return 0
	case "starting":
		return 2
	default:
		return 3 // none or unset
	}
}

// CollectMetrics collects metrics from containers
func (e *ContainerHealthExporter) CollectMetrics() {
	ctx := context.Background()

	// Filter for running containers only
	filter := filters.NewArgs()
	filter.Add("status", "running")

	containers, err := e.dockerClient.ContainerList(ctx, types.ContainerListOptions{
		Filters: filter,
	})

	if err != nil {
		log.Printf("Error listing containers: %v", err)
		return
	}

	// Reset previous metrics
	e.containerHealthStatus.Reset()
	e.containerHealthCheckDuration.Reset()

	for _, container := range containers {
		containerInfo, err := e.dockerClient.ContainerInspect(ctx, container.ID)
		if err != nil {
			log.Printf("Error inspecting container %s: %v", container.ID, err)
			continue
		}

		containerName := containerInfo.Name
		if len(containerInfo.Name) > 0 && containerInfo.Name[0] == '/' {
			containerName = containerInfo.Name[1:]
		}

		// Check health status
		healthStatus := "none"
		var healthCheckDuration float64

		if containerInfo.State != nil && containerInfo.State.Health != nil {
			healthStatus = containerInfo.State.Health.Status

			// Calculate health check duration
			if containerInfo.State.Health.Log != nil && len(containerInfo.State.Health.Log) > 0 {
				lastCheck := containerInfo.State.Health.Log[len(containerInfo.State.Health.Log)-1]
				if !lastCheck.End.IsZero() && lastCheck.End.After(lastCheck.Start) {
					healthCheckDuration = lastCheck.End.Sub(lastCheck.Start).Seconds()
				}
			}
		}

		// Set metrics
		e.containerHealthStatus.WithLabelValues(
			container.ID[:12],
			containerName,
			containerInfo.Config.Image,
		).Set(mapHealthStatusToNumber(healthStatus))

		if healthCheckDuration > 0 {
			e.containerHealthCheckDuration.WithLabelValues(
				container.ID[:12],
				containerName,
			).Set(healthCheckDuration)
		}

		log.Printf("Container: %s, Health: %s, Duration: %.2fs",
			containerName, healthStatus, healthCheckDuration)
	}
}

// StartMetricsCollection starts periodic metrics collection
func (e *ContainerHealthExporter) StartMetricsCollection(interval time.Duration) {
	// First run
	e.CollectMetrics()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.CollectMetrics()
		}
	}
}

func main() {
	log.Println("Starting PulseBox Container Health Exporter...")

	// Create exporter
	exporter, err := NewContainerHealthExporter()
	if err != nil {
		log.Fatalf("Failed to create exporter: %v", err)
	}

	// Start metrics collection every 30 seconds
	go exporter.StartMetricsCollection(30 * time.Second)

	// Setup Prometheus endpoint
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`
			<html>
			<head><title>PulseBox - Container Health Exporter</title></head>
			<body>
				<h1>PulseBox - Container Health Exporter</h1>
				<p><a href="/metrics">Metrics</a></p>
			</body>
			</html>
		`))
	})

	// Start server on port 8037
	log.Println("Server listening on :8037")
	log.Fatal(http.ListenAndServe(":8037", nil))
}
