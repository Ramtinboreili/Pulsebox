// Command pulsebox is a Kubernetes-native cluster health exporter with an
// embedded live topology dashboard. It watches the cluster through client-go
// informers, exposes Prometheus metrics, and serves an interactive graph.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Ramtinboreili/Pulsebox/internal/api"
	"github.com/Ramtinboreili/Pulsebox/internal/collector"
	pbmetrics "github.com/Ramtinboreili/Pulsebox/internal/metrics"
	"github.com/Ramtinboreili/Pulsebox/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var (
		metricsAddr = flag.String("metrics-addr", env("PULSEBOX_METRICS_ADDR", ":8037"), "address for the Prometheus /metrics endpoint")
		httpAddr    = flag.String("http-addr", env("PULSEBOX_HTTP_ADDR", ":8080"), "address for the dashboard and topology API")
		kubeconfig  = flag.String("kubeconfig", env("PULSEBOX_KUBECONFIG", ""), "path to kubeconfig for out-of-cluster/dev use")
		namespace   = flag.String("namespace", env("PULSEBOX_NAMESPACE", ""), "restrict watches to a single namespace (empty = cluster-wide)")
		pvcGrace    = flag.Duration("pvc-pending-grace", durEnv("PULSEBOX_PVC_PENDING_GRACE", 2*time.Minute), "a PVC pending longer than this is unhealthy")
	)
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("pulsebox ")
	log.Println("starting Pulsebox cluster health exporter")

	cfg, err := buildKubeConfig(*kubeconfig)
	if err != nil {
		log.Fatalf("kube config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	col := collector.New(clientset, collector.Config{
		Namespace:       *namespace,
		ResyncPeriod:    10 * time.Minute,
		RebuildDebounce: 750 * time.Millisecond,
		PVCPendingGrace: *pvcGrace,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := col.Start(ctx); err != nil {
		log.Fatalf("collector start: %v", err)
	}

	// Prometheus registry: Pulsebox metrics + standard process/go metrics.
	reg := prometheus.NewRegistry()
	reg.MustRegister(pbmetrics.New(col))
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// --- metrics server (Prometheus scrape target) ---
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	metricsMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !col.Synced() {
			http.Error(w, "syncing", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready"))
	})

	// --- dashboard + API server ---
	httpMux := http.NewServeMux()
	api.New(col).RegisterRoutes(httpMux)
	httpMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	httpMux.Handle("/", web.Handler())

	metricsSrv := &http.Server{Addr: *metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
	httpSrv := &http.Server{Addr: *httpAddr, Handler: httpMux, ReadHeaderTimeout: 5 * time.Second}

	go serve(metricsSrv, "metrics", *metricsAddr)
	go serve(httpSrv, "dashboard/api", *httpAddr)

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = metricsSrv.Shutdown(shutdownCtx)
	_ = httpSrv.Shutdown(shutdownCtx)
}

func serve(srv *http.Server, name, addr string) {
	log.Printf("%s server listening on %s", name, addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("%s server: %v", name, err)
	}
}

// buildKubeConfig prefers in-cluster config, falling back to a kubeconfig file
// (explicit flag, then KUBECONFIG, then ~/.kube/config) for local development.
func buildKubeConfig(explicit string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		log.Println("using in-cluster config")
		return cfg, nil
	}
	path := explicit
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".kube", "config")
		}
	}
	if path == "" {
		return nil, errors.New("no in-cluster config and no kubeconfig found")
	}
	log.Printf("using kubeconfig %s", path)
	return clientcmd.BuildConfigFromFlags("", path)
}

func durEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
