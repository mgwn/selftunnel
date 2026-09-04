# Multi-stage Dockerfile for selftunnel.
#   docker build -t selftunnel:latest .
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t mgwn/selftunnel:latest --push .

# ---- build environment ----
# --platform=$BUILDPLATFORM keeps this stage on the runner's native
# architecture; TARGETARCH makes the Go cross-compiler emit the target
# binary, so multi-arch builds need no QEMU emulation.
# Pinned to the exact toolchain of go.mod, so no toolchain download or
# switching happens inside the image.
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine AS build-env
ARG TARGETARCH
WORKDIR /src

# Install git for fetching private modules if needed, and ca-certificates.
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags='-w -s' \
    -o selftunnel-server ./cmd/selftunnel-server

# ---- runtime environment ----
FROM scratch
WORKDIR /app

COPY --from=build-env /src/selftunnel-server /app/selftunnel-server

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/app/selftunnel-server"]
CMD ["-addr", ":8080", "-data", "/data"]
