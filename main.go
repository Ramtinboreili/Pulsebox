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

// ContainerHealthExporter ساختار اصلی اکسپورتر
type ContainerHealthExporter struct {
    dockerClient *client.Client
    
    // متریک‌های Prometheus
    containerHealthStatus *prometheus.GaugeVec
    containerHealthCheckDuration *prometheus.GaugeVec
}

// NewContainerHealthExporter ایجاد یک نمونه جدید از اکسپورتر
func NewContainerHealthExporter() (*ContainerHealthExporter, error) {
    // اتصال به Docker daemon
    cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return nil, fmt.Errorf("failed to create Docker client: %v", err)
    }

    exporter := &ContainerHealthExporter{
        dockerClient: cli,
    }

    // تعریف متریک‌ها
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

    // ثبت متریک‌ها
    prometheus.MustRegister(exporter.containerHealthStatus)
    prometheus.MustRegister(exporter.containerHealthCheckDuration)

    return exporter, nil
}

// mapHealthStatusToNumber تبدیل وضعیت سلامت به عدد
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

// CollectMetrics جمع‌آوری متریک‌ها از کانتینرها
func (e *ContainerHealthExporter) CollectMetrics() {
    ctx := context.Background()
    
    // فیلتر برای گرفتن فقط کانتینرهای در حال اجرا
    filter := filters.NewArgs()
    filter.Add("status", "running")
    
    containers, err := e.dockerClient.ContainerList(ctx, types.ContainerListOptions{
        Filters: filter,
    })
    
    if err != nil {
        log.Printf("Error listing containers: %v", err)
        return
    }

    // ریست کردن متریک‌های قبلی
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

        // بررسی وضعیت سلامت
        healthStatus := "none"
        var healthCheckDuration float64

        if containerInfo.State != nil && containerInfo.State.Health != nil {
            healthStatus = containerInfo.State.Health.Status
            
            // محاسبه مدت زمان چک سلامت
            if containerInfo.State.Health.Log != nil && len(containerInfo.State.Health.Log) > 0 {
                lastCheck := containerInfo.State.Health.Log[len(containerInfo.State.Health.Log)-1]
                if !lastCheck.End.IsZero() && lastCheck.End.After(lastCheck.Start) {
                    healthCheckDuration = lastCheck.End.Sub(lastCheck.Start).Seconds()
                }
            }
        }

        // تنظیم متریک‌ها
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

// StartMetricsCollection شروع جمع‌آوری دوره‌ای متریک‌ها
func (e *ContainerHealthExporter) StartMetricsCollection(interval time.Duration) {
    // اولین اجرا
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
    log.Println("Starting Container Health Exporter...")

    // ایجاد اکسپورتر
    exporter, err := NewContainerHealthExporter()
    if err != nil {
        log.Fatalf("Failed to create exporter: %v", err)
    }

    // شروع جمع‌آوری متریک‌ها هر 30 ثانیه
    go exporter.StartMetricsCollection(30 * time.Second)

    // تنظیم endpoint برای Prometheus
    http.Handle("/metrics", promhttp.Handler())
    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(`
            <html>
            <head><title>Container Health Exporter</title></head>
            <body>
                <h1>Container Health Exporter</h1>
                <p><a href="/metrics">Metrics</a></p>
            </body>
            </html>
        `))
    })

    log.Println("Server listening on :8080")
    log.Fatal(http.ListenAndServe(":8080", nil))
}
