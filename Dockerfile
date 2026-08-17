# Build Stage
# Pinned to the build host's own architecture: the binary is pure Go with CGO
# off, so we cross-compile for the target below instead of emulating the
# builder under QEMU.
FROM --platform=$BUILDPLATFORM golang:1.25.8-alpine AS builder

WORKDIR /app

# Install build dependencies (if needed, e.g. for CGO, though we aim for pure Go)
RUN apk add --no-cache git

# Install swag for Swagger docs generation (pinned version, same as Makefile)
RUN go install github.com/swaggo/swag/cmd/swag@v1.16.3

# Copy Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy Source
COPY . .

# Generate Swagger docs
RUN swag init -g cmd/tokayops/main.go -o docs

# Build metadata. CI passes these via --build-arg (see .github/workflows/ci.yml);
# for a plain `docker build` they fall back to the .git in the build context, then "unknown".
ARG GIT_COMMIT=""
ARG GIT_BRANCH=""
ARG BUILD_DATE=""

# Target platform, supplied by buildx. Defaults keep a plain `docker build`
# (which sets neither) producing a linux binary for the host architecture.
ARG TARGETOS=linux
ARG TARGETARCH

# Build Binary
# CGO_ENABLED=0 helps with static linking for Alpine
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-X main.buildBranch=${GIT_BRANCH:-$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)} \
              -X main.buildCommit=${GIT_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)} \
              -X main.buildDate=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" \
    -o tokayops cmd/tokayops/main.go

# Final Stage
FROM alpine:3.20

WORKDIR /app

# Add certificates for HTTPS (Slack/Telegram APIs) and tzdata for timezones
RUN apk --no-cache add ca-certificates tzdata

# Copy Binary from Builder
COPY --from=builder /app/tokayops .

# Copy Web UI static files
COPY --from=builder /app/web ./web

# Copy Config (Optional: In k8s/prod this acts as a default or is overwritten by ConfigMap)
COPY tokay.yaml .

EXPOSE 8080 9090

ENTRYPOINT ["/app/tokayops"]
