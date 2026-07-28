# -------------------------------------------------------------
# Stage 1: Build Environment
# -------------------------------------------------------------
FROM golang:1.26.5-alpine  AS builder

WORKDIR /app

# Download dependencies first (leverages Docker layer caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy application source code
COPY . .

# Build a static binary pointing to your entrypoint (cmd/api/main.go)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /app/transacta-api ./cmd/api

# -------------------------------------------------------------
# Stage 2: Minimal Production Image
# -------------------------------------------------------------
FROM alpine:latest

WORKDIR /app

# Install CA certificates for secure HTTPS/outbound webhooks
RUN apk --no-cache add ca-certificates

# Copy compiled binary from the builder stage
COPY --from=builder /app/transacta-api .

# Expose API port
EXPOSE 8080

# Run the API binary
CMD ["./transacta-api"]