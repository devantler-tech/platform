FROM docker.io/library/node:26-bookworm@sha256:155f6779f557f863b16a6b77b3443b52f7dde209643eff9896d661bbd6829840 AS frontend
WORKDIR /src/front
COPY front/package.json front/package-lock.json ./
RUN npm ci
COPY front/ ./
RUN npm run build-prod

FROM docker.io/library/golang:1.27.3-bookworm@sha256:fea12ba16096adec0f699ce1c4da2154d320450e56c1f63671f95d2a3551c77b AS builder
RUN apt-get update && apt-get install --yes --no-install-recommends liblz4-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/static ./static
RUN go test ./...
ARG VERSION=unknown
RUN go build -mod=readonly -ldflags "-X main.version=$VERSION" -o /out/coroot .

FROM ghcr.io/coroot/coroot:1.29.1@sha256:7f5aa2982ff789f95d8326b35cafad3d127bb44669eed94a90a6baa563c342af
COPY --from=builder --chmod=0755 /out/coroot /usr/bin/coroot
RUN ["/usr/bin/coroot", "--help"]
