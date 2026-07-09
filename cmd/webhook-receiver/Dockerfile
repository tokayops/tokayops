# Build
FROM golang:1.25.8-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o webhook-receiver cmd/webhook-receiver/main.go

# Runtime
FROM alpine:3.20
WORKDIR /app
RUN apk --no-cache add ca-certificates
COPY --from=builder /app/webhook-receiver .
EXPOSE 9999
ENTRYPOINT ["/app/webhook-receiver"]
