FROM docker.io/library/node:24-bookworm@sha256:f9b3548b20b15b705b66aeecaeac7dd690c73d5e4661357aac02e60e3d38c88a AS frontend
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

FROM ghcr.io/coroot/coroot:1.28.1@sha256:1b908030e1cce068ce6fb907cd02c41552e27aab382e61f1bbb523ceb50a0913
COPY --from=builder --chmod=0755 /out/coroot /usr/bin/coroot
RUN ["/usr/bin/coroot", "--help"]
