FROM golang:1.19-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o pulsebox .

FROM alpine:latest
RUN apk --no-cache add docker-cli

WORKDIR /root/
COPY --from=builder /app/pulsebox .

EXPOSE 8037
VOLUME ["/var/run/docker.sock"]

CMD ["./pulsebox"]
