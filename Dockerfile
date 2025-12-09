# Build stage
FROM golang:1.23.1-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s -X main.Version=${VERSION:-dev} -X main.BuildTime=$(date -u '+%Y-%m-%d_%H:%M:%S')" \
    -a -installsuffix cgo \
    -o sabre ./cmd/sabre

# Production stage
FROM alpine:latest

# Install runtime dependencies
RUN apk --no-cache add ca-certificates tzdata

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/sabre .

# Copy config file
COPY --from=builder /app/config.toml .

# Expose port
EXPOSE 3000

# Run the application
ENTRYPOINT ["./sabre"]
CMD ["-c", "config.toml"]
