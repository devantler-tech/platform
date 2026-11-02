FROM docker.io/library/golang:1.27.3-bookworm@sha256:fea12ba16096adec0f699ce1c4da2154d320450e56c1f63671f95d2a3551c77b AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./...
ARG VERSION=unknown
RUN go build -mod=readonly -ldflags "-X main.version=$VERSION" -o /out/coroot-cluster-agent .

FROM ghcr.io/coroot/coroot-cluster-agent:1.11.8@sha256:a5bdb68c23fac361de19531cac8e9a0fcef4692ea10edac3d2ef9e6769414de7
COPY --from=builder --chmod=0755 /out/coroot-cluster-agent /usr/bin/coroot-cluster-agent
RUN ["/usr/bin/coroot-cluster-agent", "--help"]
