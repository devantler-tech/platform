FROM docker.io/library/golang:1.26.10-bookworm@sha256:23ce71337b726608afb1bc00986e630a05034a87a87ea1fc2cd1ae5cf33387eb AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/docker-credential-acr-env/ ./third_party/docker-credential-acr-env/
RUN go mod download && go mod verify
COPY . .
RUN timeout 900s go test -mod=readonly -race ./pkg/background/... ./cmd/internal/... \
    ./pkg/image/... ./pkg/tracing/... ./pkg/metrics/... \
    github.com/chrismellard/docker-credential-acr-env/pkg/credhelper
RUN GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 \
    && timeout 300s golangci-lint run --timeout=4m ./pkg/background/... ./cmd/background-controller/...
ARG VERSION=unknown
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath \
    -ldflags="-s -w -X github.com/kyverno/kyverno/pkg/version.BuildVersion=${VERSION}" \
    -o /out/background-controller ./cmd/background-controller \
    && go version /out/background-controller \
    && test "$(go version /out/background-controller)" = "/out/background-controller: go1.26.10"

FROM reg.kyverno.io/kyverno/background-controller:v1.19.2@sha256:c46e124f21ef8a6bb4b48de57106808b1007edcd5ddd4ff7df5d23b9efa60ce2
COPY --from=builder --chmod=0755 /out/background-controller /ko-app/background-controller
RUN ["/ko-app/background-controller", "--help"]
