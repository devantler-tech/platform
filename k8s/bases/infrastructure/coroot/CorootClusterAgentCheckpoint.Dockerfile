FROM docker.io/library/golang:1.27.3-bookworm@sha256:e482b31b5d30c31f9e495f4b8cbdf41429b2d74955dfbcd9c0a904483d738390 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./...
ARG VERSION=unknown
RUN go build -mod=readonly -ldflags "-X main.version=$VERSION" -o /out/coroot-cluster-agent .

FROM ghcr.io/coroot/coroot-cluster-agent:1.11.8@sha256:a5bdb68c23fac361de19531cac8e9a0fcef4692ea10edac3d2ef9e6769414de7
COPY --from=builder --chmod=0755 /out/coroot-cluster-agent /usr/bin/coroot-cluster-agent
# Restate the upstream image's own non-root identity; its config User is 65534:65534.
USER 65534:65534
# Kubernetes never runs an image health check, so this image declares none.
HEALTHCHECK NONE
RUN ["/usr/bin/coroot-cluster-agent", "--help"]
