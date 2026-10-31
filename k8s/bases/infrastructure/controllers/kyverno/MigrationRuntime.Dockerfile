FROM docker.io/library/golang:1.26.10-bookworm@sha256:23ce71337b726608afb1bc00986e630a05034a87a87ea1fc2cd1ae5cf33387eb AS builder
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY . .
# Load only packages used by this runtime, then verify the downloaded modules.
RUN go list -mod=readonly -deps ./cmd/platform-migrate > /tmp/migration-packages \
    && go mod verify \
    && if grep -Eq '^(github.com/kyverno/kyverno/pkg/engine/|golang.org/x/crypto/openpgp|github.com/chrismellard/docker-credential-acr-env|go.etcd.io/etcd/client/)' /tmp/migration-packages; then \
         echo 'Unexpected policy engine or unrelated credential/storage dependency in migration runtime'; exit 1; \
       else test "$?" -eq 1; fi
RUN timeout 300s go test -mod=readonly -race ./cmd/platform-migrate ./cmd/cli/kubectl-kyverno/commands/migrate
RUN GOBIN=/usr/local/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4 \
    && timeout 300s golangci-lint run --timeout=4m ./cmd/platform-migrate ./cmd/cli/kubectl-kyverno/commands/migrate
ARG VERSION=development
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=readonly -trimpath \
    -ldflags="-X main.buildVersion=${VERSION}" \
    -o /out/kubectl-kyverno ./cmd/platform-migrate \
    && test "$(go version /out/kubectl-kyverno)" = "/out/kubectl-kyverno: go1.26.10" \
    && go version -m /out/kubectl-kyverno > /tmp/build-info \
    && grep -Eq '^[[:space:]]+mod[[:space:]]+github\.com/kyverno/kyverno[[:space:]]+v1\.19\.2[[:space:]]' /tmp/build-info \
    && grep -Eq '^[[:space:]]+build[[:space:]]+vcs\.modified=false$' /tmp/build-info \
    && go tool nm /out/kubectl-kyverno > /tmp/symbols \
    && grep -Eq '[[:space:]]T main\.main$' /tmp/symbols \
    && test "$(/out/kubectl-kyverno --version)" = "kubectl-kyverno version ${VERSION}" \
    && /out/kubectl-kyverno migrate --help \
    && if /out/kubectl-kyverno apply --help > /tmp/unsupported-command 2>&1; then \
         echo 'Unsupported CLI command exposed by migration runtime'; exit 1; \
       else test "$?" -eq 1 && grep -F 'unknown command "apply"' /tmp/unsupported-command; fi
# Only these exact whole-module records for Kyverno v1.19.2 are accepted, with the same
# adjudication and removal as scripts/tests/tenant-webapp-admission/.govulncheck-allow.txt
# (platform#5154). The list must match exactly: another advisory fails the build, and so does
# an empty list, because that means the report was not read or the record needs removing here.
RUN GOBIN=/usr/local/bin go install golang.org/x/vuln/cmd/govulncheck@v1.8.0 \
    && timeout 300s govulncheck ./cmd/platform-migrate \
    && timeout 120s govulncheck -mode=binary -format json /out/kubectl-kyverno > /tmp/binary-scan.json \
    && grep -Eq '"scan_level": *"symbol"' /tmp/binary-scan.json \
    && timeout 120s govulncheck -mode=extract /out/kubectl-kyverno > /tmp/binary-extract.json \
    && grep -Eq '"pkgSymbols": *\[[^]]+\]' /tmp/binary-extract.json \
    && grep -Eo '"osv": *"[^"]+"' /tmp/binary-scan.json | grep -Eo 'GO-[0-9]+-[0-9]+' | sort -u > /tmp/binary-findings \
    && cat /tmp/binary-findings \
    && test "$(tr '\n' ' ' < /tmp/binary-findings)" = 'GO-2025-3615 GO-2026-5337 GO-2026-5594 '

FROM reg.kyverno.io/kyverno/kyverno-cli:v1.19.2@sha256:322ec5044d600930cb3af0e56dc33154a2d1d1fba58f4cadad3f47be7e78b11c
COPY --from=builder --chmod=0755 /out/kubectl-kyverno /ko-app/kubectl-kyverno
# Match the released migration Job's explicit execution identity.
USER 65534:65534
ENTRYPOINT ["/ko-app/kubectl-kyverno"]
RUN ["/ko-app/kubectl-kyverno", "--version"]
RUN ["/ko-app/kubectl-kyverno", "migrate", "--help"]
