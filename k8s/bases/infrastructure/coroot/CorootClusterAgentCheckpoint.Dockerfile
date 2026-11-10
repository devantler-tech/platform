FROM docker.io/library/golang:1.27.3-bookworm@sha256:fea12ba16096adec0f699ce1c4da2154d320450e56c1f63671f95d2a3551c77b AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./...
ARG VERSION=unknown
RUN go build -mod=readonly -ldflags "-X main.version=$VERSION" -o /out/coroot-cluster-agent .

FROM ghcr.io/coroot/coroot-cluster-agent:1.11.9@sha256:2b010cec8c1e898da578125187355c82ddd40cc28b508c014a8a1cb15cc53ed8
COPY --from=builder --chmod=0755 /out/coroot-cluster-agent /usr/bin/coroot-cluster-agent
# Restate the upstream image's own non-root identity; its config User is 65534:65534.
USER 65534:65534
# Kubernetes never runs an image health check, so this image declares none.
HEALTHCHECK NONE
RUN ["/usr/bin/coroot-cluster-agent", "--help"]
