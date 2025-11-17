# Pulsebox

Pulsebox is a lightweight **Go-based Docker health exporter** that
exposes container health states as **Prometheus-compatible metrics**.\
It provides a clean, minimal, and dependency-free way to monitor Docker
container health without adding unnecessary complexity or overhead.

## 📌 Features

-   🚀 **Monitors health of all running Docker containers**
-   📊 **Exposes Prometheus-friendly metrics** via HTTP endpoint\
    Default endpoint: **`:8037/metrics`**
-   🐳 **Seamless Docker & Docker Compose support**
-   ⚡ **Fast and production-ready Go implementation**
-   🔧 Minimal configuration --- works out-of-the-box
-   📈 Ideal for Prometheus, Grafana, and alerting pipelines

## 📦 Installation

### Option 1 --- Docker Compose (recommended)

``` bash
docker compose up -d
```

### Option 2 --- Run with Docker

``` bash
docker build -t pulsebox .
docker run -d   --name pulsebox   -p 8037:8037   -v /var/run/docker.sock:/var/run/docker.sock   pulsebox
```

### Option 3 --- Run from source

``` bash
go mod tidy
go run main.go
```

## 🔧 Configuration

Pulsebox is intentionally minimal. By default it:

-   Binds to port **8037**
-   Reads container states via the Docker Engine API
-   Exposes one primary Prometheus metric

You can modify ports or extend functionality by editing the source
before building your own image.

## 📈 Prometheus Metrics

Pulsebox exposes metrics like:

    pulsebox_container_health_status{container="myapp"} 1

Where health states map to:

  Value   Meaning
  ------- -----------
  `0`     Unhealthy
  `1`     Healthy
  `2`     Starting
  `3`     Unset

### Example `prometheus.yml` scrape config

``` yaml
scrape_configs:
  - job_name: "pulsebox"
    static_configs:
      - targets: ["pulsebox:8037"]
```

## 🧪 Example Dashboard (Grafana)

You can build simple dashboards around:

-   Container health over time
-   Alerts for unhealthy or restarting containers
-   Container count changes

## 📁 Project Structure

    Pulsebox/
    ├── Dockerfile
    ├── docker-compose.yml
    ├── main.go
    ├── go.mod
    └── ...etc

## 🛠 Requirements

-   Docker Engine
-   Go **1.19+** (for local builds)
-   Prometheus (optional, for scraping)

## 🧩 Troubleshooting

### ❓ Metrics endpoint returns empty

Make sure containers have `HEALTHCHECK` defined in their Dockerfile.

### ❓ Cannot connect to Docker

Ensure Pulsebox has access to the Docker socket:

    /var/run/docker.sock

### ❓ Port already in use

Map Pulsebox to another port:

``` bash
docker run -p 9000:8037 ...
```

## 👤 Author

Developed by **Ramtin Boreili**\
🔗 LinkedIn: https://www.linkedin.com/in/ramtin-boreili/\
🐙 GitHub: https://github.com/Ramtinboreili/Pulsebox

## 📄 License

Pulsebox is licensed under the **Apache License 2.0**.
