FROM docker.io/library/golang:1.26.9-bookworm@sha256:d9c68c2c51161e12fd77e4c6320687c9cd86e1af1e3ad6e6cd63ff970641453c AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN timeout 900s go test -mod=readonly -race ./pkg/background/...
RUN GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 \
    && timeout 300s golangci-lint run --timeout=4m ./pkg/background/... ./cmd/background-controller/...
ARG VERSION=unknown
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath \
    -ldflags="-s -w -X github.com/kyverno/kyverno/pkg/version.BuildVersion=${VERSION}" \
    -o /out/background-controller ./cmd/background-controller

FROM reg.kyverno.io/kyverno/background-controller:v1.19.2@sha256:c46e124f21ef8a6bb4b48de57106808b1007edcd5ddd4ff7df5d23b9efa60ce2
COPY --from=builder --chmod=0755 /out/background-controller /ko-app/background-controller
RUN ["/ko-app/background-controller", "--help"]
