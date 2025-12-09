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

# Create non-root user
RUN addgroup -g 1001 -S sabre && \
    adduser -u 1001 -S sabre -G sabre

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/sabre .

# Copy config file
COPY --from=builder /app/config.toml .

# Create cache directory for the application
RUN mkdir -p .data/sabre

# Change ownership to non-root user
RUN chown -R sabre:sabre /app

# Switch to non-root user
USER sabre

# Expose port
EXPOSE 3000

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:3000/health || exit 1

# Run the application
ENTRYPOINT ["./sabre"]
CMD ["-c", "config.toml"]
