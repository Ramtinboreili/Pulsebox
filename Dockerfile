# Stage 1: Builder با Go 1.19
FROM golang:1.19-alpine AS builder

WORKDIR /app

# نصب ابزارهای لازم
RUN apk add --no-cache git ca-certificates

# کپی فایل‌های وابستگی
COPY go.mod go.sum ./

# دانلود وابستگی‌ها
RUN go mod download && go mod verify

# کپی سورس کد
COPY main.go ./

# ساخت برنامه
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o container-health-exporter .

# Stage 2: Runtime
FROM alpine:latest

RUN apk --no-cache add docker-cli

WORKDIR /root/

# کپی باینری از stage builder
COPY --from=builder /app/container-health-exporter .

EXPOSE 8080

VOLUME ["/var/run/docker.sock"]

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/metrics || exit 1

CMD ["./container-health-exporter"]
