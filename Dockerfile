# Build stage. --platform=$BUILDPLATFORM makes the builder always run natively
# on the build host and cross-compile for the target platform, so multi-arch
# images never compile Go under QEMU emulation. This requires CGO_ENABLED=0,
# which the pure-Go sqlite driver (modernc.org/sqlite) allows.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies with cache mount
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy embedded assets and source code
COPY templates/ ./templates/
COPY static/ ./static/
COPY *.go ./

# Release version injected by CI (see release.yml); defaults to "dev" for local builds
ARG VERSION=dev

# Cross-compile a fully static binary for the target platform
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags "-X main.version=${VERSION}" -o main .

# Final stage
FROM alpine:latest

# Install runtime dependencies (the binary is static; only CA certs are needed
# for HTTPS). --no-scripts works around Alpine 3.23 trigger script issues with
# QEMU emulation on arm64.
RUN apk update && apk --no-cache --no-scripts add ca-certificates

# Create app directory
WORKDIR /app

# Copy the binary from builder stage (templates and static assets are embedded)
COPY --from=builder /app/main .

# Create directory for database
RUN mkdir -p /app/data

# Expose port
EXPOSE 5000

# Set environment variables
ENV GIN_MODE=release
ENV FILABRIDGE_DB_PATH=/app/data

# Liveness check against the built-in health endpoint (busybox wget ships with Alpine)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:5000/healthz || exit 1

# Run the application
CMD ["./main"]
