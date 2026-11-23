FROM docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./...
ARG VERSION=unknown
RUN go build -mod=readonly -ldflags "-X main.version=$VERSION" -o /out/coroot-cluster-agent .

FROM ghcr.io/coroot/coroot-cluster-agent:1.12.0@sha256:b64e9f567ce03670fd3dd74905c61de5956defda9a8b26a0c721ef9a168ab2af
COPY --from=builder --chmod=0755 /out/coroot-cluster-agent /usr/bin/coroot-cluster-agent
# Restate the upstream image's own non-root identity; its config User is 65534:65534.
USER 65534:65534
# Kubernetes never runs an image health check, so this image declares none.
HEALTHCHECK NONE
RUN ["/usr/bin/coroot-cluster-agent", "--help"]
