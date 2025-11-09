FROM golang:1.24-alpine AS builder

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o sauron .

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates curl jq wget

# Install grpcurl
RUN wget -qO- https://github.com/fullstorydev/grpcurl/releases/download/v1.9.1/grpcurl_1.9.1_linux_x86_64.tar.gz | \
    tar -xz -C /usr/local/bin grpcurl && \
    chmod +x /usr/local/bin/grpcurl

WORKDIR /app

COPY --from=builder /build/sauron .

ENTRYPOINT ["/app/sauron"]
CMD ["-config", "/app/config.yaml"]
