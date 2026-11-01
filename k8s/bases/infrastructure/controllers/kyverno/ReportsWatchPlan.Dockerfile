FROM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS builder
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/docker-credential-acr-env/ ./third_party/docker-credential-acr-env/
RUN go mod download && go mod verify
COPY . .
RUN timeout 900s go test -mod=readonly -race ./pkg/controllers/report/resource ./pkg/controllers/report/aggregate ./pkg/webhooks/resource/validation
RUN timeout 300s go test -mod=readonly -race ./cmd/internal/... ./pkg/image/... ./pkg/tracing/... ./pkg/metrics/... \
    github.com/chrismellard/docker-credential-acr-env/pkg/...
# Upstream code is not ours to restyle: only lines the hotfix patch changes can fail this gate.
# Include every Go package changed by the runtime patch, including its local replacement module.
RUN GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 \
    && timeout 900s golangci-lint run --timeout=14m --new-from-patch=reports-watch-plan.patch \
        ./pkg/controllers/report/resource/... ./cmd/internal/... \
        github.com/chrismellard/docker-credential-acr-env/pkg/...
ARG VERSION=unknown
# Preserve the symbol table so executable scans can distinguish linked packages
# from conservative module-level findings; omit only DWARF debug information.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -buildvcs=true -trimpath \
    -ldflags="-w -X github.com/kyverno/kyverno/pkg/version.BuildVersion=${VERSION}" \
    -o /out/reports-controller ./cmd/reports-controller \
    && test "$(go version /out/reports-controller)" = "/out/reports-controller: go1.27.2" \
    && go version -m /out/reports-controller > /tmp/reports-build-info \
    && awk '$1 == "mod" && $2 == "github.com/kyverno/kyverno" && $3 == "v1.19.2+dirty" {module++} \
      $1 == "build" && $2 == "vcs.revision=e63308f1e16c05cd009ac4864be6ec25c66b49be" {revision++} \
      END {exit(module != 1 || revision != 1)}' /tmp/reports-build-info
RUN go list -mod=readonly -deps ./cmd/reports-controller > /tmp/reports-packages \
    && if grep -Eq '^golang.org/x/crypto/openpgp(/|$)' /tmp/reports-packages; then \
         echo 'Unsafe OpenPGP package in reports runtime'; exit 1; \
       else test "$?" -eq 1; fi
RUN GOBIN=/usr/local/bin go install golang.org/x/vuln/cmd/govulncheck@v1.8.0 \
    && timeout 300s govulncheck ./cmd/reports-controller \
    && timeout 120s govulncheck -mode=binary /out/reports-controller

FROM reg.kyverno.io/kyverno/reports-controller:v1.19.2@sha256:5fb6d3e738050dbce84f60ecabaede7665fbe0578d07c1e8c912443b035e79c3
COPY --from=builder --chmod=0755 /out/reports-controller /ko-app/reports-controller
RUN ["/ko-app/reports-controller", "--help"]
