FROM docker.io/library/node:26-bookworm@sha256:6eca1d4ba35cef57a8a41008310420541a2a9ffdcd4883841463ccb39a588bd8 AS frontend
WORKDIR /src/front
COPY front/package.json front/package-lock.json ./
RUN npm ci
COPY front/ ./
RUN npm run build-prod

FROM docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS builder
RUN apt-get update && apt-get install --yes --no-install-recommends liblz4-dev=1.9.4-1 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/static ./static
RUN go test ./...
ARG VERSION=unknown
RUN go build -mod=readonly -ldflags "-X main.version=$VERSION" -o /out/coroot .

FROM ghcr.io/coroot/coroot:1.29.4@sha256:372c1695c6418457fc892fcf35a8840aa65f77881ae34cecafa948bc34133430
COPY --from=builder --chmod=0755 /out/coroot /usr/bin/coroot
# Restate the upstream image's own non-root identity; its config User is 65534:65534.
USER 65534:65534
# Kubernetes never runs an image health check, so this image declares none.
HEALTHCHECK NONE
RUN ["/usr/bin/coroot", "--help"]
