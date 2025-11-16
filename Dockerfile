FROM golang:1.19-alpine AS builder

WORKDIR /app

# Install git and required tools
RUN apk add --no-cache git

# Copy dependency files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go ./

# Build the application
RUN CGO_ENNABLED=0 GOOS=linux go build -a -installsuffix cgo -o pulsebox .

FROM alpine:latest
RUN apk --no-cache add ca-certificates docker-cli

WORKDIR /root/
COPY --from=builder /app/pulsebox .

# Expose port 8037
EXPOSE 8037

VOLUME ["/var/run/docker.sock"]

CMD ["./pulsebox"]
