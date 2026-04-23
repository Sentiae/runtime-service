# Build Stage - Compile Go Application
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Configure private module access
ARG GITHUB_TOKEN
ENV GOPRIVATE=github.com/sentiae/*
RUN git config --global url."https://${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"

WORKDIR /app

# Copy local replace dependencies
COPY platform-kit/ /platform-kit/
COPY foundry-service/ /foundry-service/

# Copy go.mod and go.sum first for better caching
COPY runtime-service/go.mod runtime-service/go.sum ./
RUN go mod download && go mod verify

# Copy source code
COPY runtime-service/ .

# Build the application with optimizations and security flags
ARG VERSION=dev
ARG BUILD_TIME
RUN CGO_ENABLED=0 go build \
    -a \
    -installsuffix cgo \
    -ldflags="-w -s -X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME}" \
    -o /build/bin/runtime-service \
    ./cmd/server/

# Verify the binary was built
RUN test -f /build/bin/runtime-service || (echo "Binary not found" && exit 1)

# Create optional dirs so COPY won't fail
RUN mkdir -p /build/migrations /build/configs

# Runtime Stage - Minimal Production Image
FROM alpine:3.19

# Install runtime dependencies (includes openssh-client for VM communication)
RUN apk --no-cache add \
    ca-certificates \
    tzdata \
    wget \
    openssh-client \
    && update-ca-certificates

# Create non-root user for security
RUN addgroup -g 1000 runtime && \
    adduser -D -u 1000 -G runtime runtime

# Set working directory
WORKDIR /app

# Copy binary from builder
COPY --from=builder --chown=runtime:runtime /build/bin/runtime-service /app/runtime-service

# Copy migrations directory
COPY --from=builder --chown=runtime:runtime /build/migrations /app/migrations

# Copy configuration template (optional)
COPY --from=builder --chown=runtime:runtime /build/configs /app/configs

# Switch to non-root user
USER runtime

# Expose HTTP and gRPC ports
EXPOSE 8090 50060

# Health check configuration
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8090/health | grep -q healthy || exit 1

# Set environment defaults
ENV PORT=8090 \
    GRPC_PORT=50060 \
    ENVIRONMENT=production \
    LOG_LEVEL=info

# Run the application
ENTRYPOINT ["/app/runtime-service"]
