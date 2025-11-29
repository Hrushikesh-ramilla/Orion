# ---- Builder Stage ----
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Use GOTOOLCHAIN=auto so go 1.23 bootstrap can fetch the required toolchain
ENV GOTOOLCHAIN=auto

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Build the binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /server ./cmd/server

# ---- Runtime Stage ----
FROM alpine:3.21

RUN apk --no-cache add ca-certificates

# Non-root user for security
RUN adduser -D -u 1000 scheduler
USER scheduler

WORKDIR /app

COPY --from=builder /server .

# WAL data directory
RUN mkdir -p /app/data
VOLUME ["/app/data"]

ENV WAL_PATH=/app/data/wal.json

EXPOSE 8080

ENTRYPOINT ["./server"]
