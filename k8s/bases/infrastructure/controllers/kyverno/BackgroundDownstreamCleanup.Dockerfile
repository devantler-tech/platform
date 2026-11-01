FROM docker.io/library/golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS builder
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
    -ldflags="-X github.com/kyverno/kyverno/pkg/version.BuildVersion=${VERSION}" \
    -o /out/background-controller ./cmd/background-controller \
    && go version /out/background-controller \
    && test "$(go version /out/background-controller)" = "/out/background-controller: go1.26.10" \
    && go version -m /out/background-controller > /tmp/build-info \
    && grep -Eq '^[[:space:]]+mod[[:space:]]+github\.com/kyverno/kyverno[[:space:]]+v1\.19\.2[[:space:]]' /tmp/build-info \
    && grep -Eq '^[[:space:]]+build[[:space:]]+vcs\.modified=false$' /tmp/build-info \
    && go tool nm /out/background-controller > /tmp/symbols \
    && grep -Eq '[[:space:]]T main\.main$' /tmp/symbols \
    && if grep -Fq 'golang.org/x/crypto/openpgp' /tmp/symbols; then \
         echo 'Unmaintained OpenPGP package compiled into the controller'; exit 1; \
       else test "$?" -eq 1; fi
# Only these exact module records are accepted. The three Kyverno v1.19.2 records have the same
# adjudication and removal as scripts/tests/tenant-webapp-admission/.govulncheck-allow.txt
# (platform#5154). GO-2026-5932 names x/crypto/openpgp and has no fixed version; the symbol check
# above fails the build if that package is ever compiled in. The list must match exactly: another
# advisory fails the build, and so does an empty list, because that means the report was not read
# or a record needs removing here.
RUN GOBIN=/usr/local/bin go install golang.org/x/vuln/cmd/govulncheck@v1.8.0 \
    && timeout 600s govulncheck ./cmd/background-controller \
    && timeout 300s govulncheck -mode=binary -format json /out/background-controller > /tmp/binary-scan.json \
    && grep -Eq '"scan_level": *"symbol"' /tmp/binary-scan.json \
    && timeout 300s govulncheck -mode=extract /out/background-controller > /tmp/binary-extract.json \
    && grep -Eq '"pkgSymbols": *\[[^]]+\]' /tmp/binary-extract.json \
    && grep -Eo '"osv": *"[^"]+"' /tmp/binary-scan.json | grep -Eo 'GO-[0-9]+-[0-9]+' | sort -u > /tmp/binary-findings \
    && cat /tmp/binary-findings \
    && test "$(tr '\n' ' ' < /tmp/binary-findings)" = 'GO-2025-3615 GO-2026-5337 GO-2026-5594 GO-2026-5932 '

FROM reg.kyverno.io/kyverno/background-controller:v1.19.2@sha256:c46e124f21ef8a6bb4b48de57106808b1007edcd5ddd4ff7df5d23b9efa60ce2
COPY --from=builder --chmod=0755 /out/background-controller /ko-app/background-controller
RUN ["/ko-app/background-controller", "--help"]
