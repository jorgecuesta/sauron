FROM golang:1.27.3-alpine AS builder

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build; VERSION is what `sauron -version` prints
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.version=${VERSION}" -o sauron .

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates curl jq wget

# Install grpcurl for the image's own architecture (TARGETARCH is set by
# buildx: amd64 or arm64; grpcurl names amd64 x86_64)
ARG TARGETARCH
RUN case "${TARGETARCH:-amd64}" in \
      amd64) arch=x86_64 ;; \
      arm64) arch=arm64 ;; \
      *) echo "no grpcurl build for TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    wget -qO- "https://github.com/fullstorydev/grpcurl/releases/download/v1.9.1/grpcurl_1.9.1_linux_${arch}.tar.gz" | \
    tar -xz -C /usr/local/bin grpcurl && \
    chmod +x /usr/local/bin/grpcurl

WORKDIR /app

COPY --from=builder /build/sauron .

ENTRYPOINT ["/app/sauron"]
CMD ["-config", "/app/config.yaml"]
