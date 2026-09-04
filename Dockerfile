# Multi-stage Dockerfile for selftunnel.
#   docker build --target build-env -t selftunnel:build .
#   docker build -t selftunnel:latest .

# ---- build environment ----
# Pinned to the exact toolchain of go.mod, so no toolchain download or
# switching happens inside the image.
FROM golang:1.26.8-alpine AS build-env
WORKDIR /src

# Install git for fetching private modules if needed, and ca-certificates.
RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo \
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
