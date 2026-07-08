# syntax=docker/dockerfile:1

# ---- build stage --------------------------------------------------------
# The dashboard frontend is bundled into the binary via go:embed, so there is
# no separate asset build step and no CDN dependency at runtime.
FROM golang:1.24-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# Copy the full source (cmd/, internal/ including internal/web/static which is
# embedded). Keep this after mod download to preserve layer caching.
COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/pulsebox ./cmd/pulsebox

# ---- final stage --------------------------------------------------------
# Distroless static + nonroot: minimal, no shell, no package manager, runs as
# an unprivileged user by default. No Docker socket, no extra tooling.
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /out/pulsebox /usr/local/bin/pulsebox

# 8037 = Prometheus /metrics, 8080 = dashboard + topology API.
EXPOSE 8037 8080

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/pulsebox"]
