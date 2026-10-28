FROM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN timeout 900s go test -mod=readonly -race ./pkg/controllers/report/resource ./pkg/controllers/report/aggregate ./pkg/webhooks/resource/validation
RUN GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 \
    && timeout 900s golangci-lint run --timeout=14m
ARG VERSION=unknown
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath \
    -ldflags="-s -w -X github.com/kyverno/kyverno/pkg/version.BuildVersion=${VERSION}" \
    -o /out/reports-controller ./cmd/reports-controller

FROM reg.kyverno.io/kyverno/reports-controller:v1.19.2@sha256:5fb6d3e738050dbce84f60ecabaede7665fbe0578d07c1e8c912443b035e79c3
COPY --from=builder --chmod=0755 /out/reports-controller /ko-app/reports-controller
RUN ["/ko-app/reports-controller", "--help"]
