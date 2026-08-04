# Build Stage - Compile Go Application
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Configure private module access
ARG GITHUB_TOKEN
RUN git config --global url."https://${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"

WORKDIR /app

# Deploy provenance (#mesh-images-have-no-deploy-provenance-stamp). VCS_REVISION
# is REQUIRED and comes from deploy.sh, which derives it from the staged
# committed tree it is about to ship — never from the working tree and never
# from an operator-supplied value. The checkout-root .dockerignore excludes
# .git, so Go's automatic VCS stamping cannot work here and MUST NOT be
# "fixed" by importing repository metadata into the build context; explicit
# injection is the mechanism.
#
# The check below is the fail-closed control: an absent, short, uppercase or
# non-hex revision aborts the image build rather than producing an image whose
# source cannot be identified. VCS_MODIFIED is likewise constrained to the two
# values that mean something.
ARG VCS_REVISION
ARG VCS_MODIFIED
RUN case "$VCS_REVISION" in \
      *[!0-9a-f]* | "") echo "BUILD REFUSED: VCS_REVISION must be a full 40-char lowercase hex commit (got: '${VCS_REVISION}')" >&2; exit 1 ;; \
    esac; \
    [ "${#VCS_REVISION}" -eq 40 ] || { echo "BUILD REFUSED: VCS_REVISION must be 40 chars (got ${#VCS_REVISION}: '${VCS_REVISION}')" >&2; exit 1; }; \
    case "$VCS_MODIFIED" in \
      true|false) ;; \
      *) echo "BUILD REFUSED: VCS_MODIFIED must be 'true' or 'false' (got: '${VCS_MODIFIED}')" >&2; exit 1 ;; \
    esac

# Copy local replace dependencies
COPY foundry-service/ /foundry-service/
COPY canvas-service/ /canvas-service/

# Copy go.mod and go.sum first for better caching
COPY runtime-service/go.mod runtime-service/go.sum ./
# Fetch platform-kit (and all other modules) through the Athens Go proxy.
# GOPROXY has no ",direct" fallback on purpose: Athens is the single source of
# truth, so a proxy miss must FAIL rather than silently fall back to direct git
# (which would re-expose the drift the substrate design forbids). GOPRIVATE and
# GONOPROXY are deliberately left UNSET so ALL modules — public and private
# sentiae alike — route through Athens by construction. GONOSUMDB scopes the
# public checksum-DB skip to the private module only (public modules still
# verify against sumdb); platform-kit itself is verified against the committed
# go.sum hashes, so no global GOSUMDB=off is needed.
ARG GOPROXY
ARG GOFLAGS
ENV GOPROXY=${GOPROXY} \
    GOFLAGS=${GOFLAGS} \
    GONOSUMDB=github.com/sentiae/* \
    GOTOOLCHAIN=auto

RUN go mod download && go mod verify

# Copy source code
COPY runtime-service/ .

# Build the application with optimizations and security flags
ARG VERSION=dev
ARG BUILD_TIME
RUN CGO_ENABLED=0 go build \
    -a \
    -installsuffix cgo \
    -ldflags="-w -s -X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME} -X github.com/sentiae/platform-kit/buildinfo.Revision=${VCS_REVISION} -X github.com/sentiae/platform-kit/buildinfo.Modified=${VCS_MODIFIED}" \
    -o /build/bin/runtime-service \
    ./cmd/server/

# Verify the binary was built
RUN test -f /build/bin/runtime-service || (echo "Binary not found" && exit 1)

# Create optional dirs so COPY won't fail
RUN mkdir -p /build/migrations /build/configs

# Runtime Stage - Minimal Production Image
FROM alpine:3.19

# Same build argument that produced the binary's linked revision, so the image
# label and the binary's /health report cannot disagree. Re-declared because a
# stage does not inherit ARGs from a previous stage.
ARG VCS_REVISION
LABEL org.opencontainers.image.revision="${VCS_REVISION}"

# Install runtime dependencies (includes openssh-client for VM communication, and
# docker-cli so the ProjectCompiler can `docker run` an ephemeral build container
# against the host daemon via the mounted /var/run/docker.sock — docker-out-of-docker).
RUN apk --no-cache add \
    ca-certificates \
    tzdata \
    wget \
    openssh-client \
    docker-cli \
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
