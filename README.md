# Pulsebox

Pulsebox is a lightweight Go-based exporter that exposes Docker container health status as Prometheus‑friendly metrics.  
It is designed for teams that want a clean, direct, and minimal way to monitor container health without extra complexity.

## Features
- Collects health status of all running Docker containers
- Exposes metrics via an HTTP endpoint (default: `:8080/metrics`)
- Designed for Prometheus scraping
- Fast, minimal, production‑ready Go implementation
- Docker & Docker Compose support

## Usage

### Run with Docker Compose
```bash
docker compose up -d
```

## Prometheus Metrics
Pulsebox exposes metrics such as:

```
pulsebox_container_health_status{container="myapp"} 1
```

Where:
- `0` = unhealthy
- `1` = healthy
- `2` = starting   
- `3` = unset

## Requirements
- Docker Engine
- Go 1.19+
- Prometheus (optional, for scraping)

## Author
Developed by **Ramtin Boreili**  
LinkedIn: https://www.linkedin.com/in/ramtin-boreili/  
GitHub: https://github.com/Ramtinboreili/Pulsebox

## License
MIT License
