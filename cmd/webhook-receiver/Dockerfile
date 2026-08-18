# Build
# Native builder, cross-compiled output - see the note in the root Dockerfile.
FROM --platform=$BUILDPLATFORM golang:1.25.8-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o webhook-receiver cmd/webhook-receiver/main.go

# Runtime
FROM alpine:3.20
WORKDIR /app
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/webhook-receiver .
EXPOSE 9999
ENTRYPOINT ["/app/webhook-receiver"]
