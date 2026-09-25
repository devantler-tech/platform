#!/usr/bin/env bash
# Hermetic tests for scripts/check-upbound-package-signatures.sh (#4189).
# cosign is substituted; CI runs the real check against the real registry.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

good_identity='https://github.com/upbound/upbound-official-build/.github/workflows/supplychain.yml@refs/heads/main'

cat >"$scratch/rendered.yaml" <<'YAML'
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-family-aws
spec:
  package: xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
---
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-aws-iam
spec:
  package: xpkg.upbound.io/upbound/provider-aws-iam:v2.6.1
---
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: provider-upjet-github
spec:
  package: ghcr.io/crossplane-contrib/provider-upjet-github:v0.20.0
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: not-a-provider
data:
  package: xpkg.upbound.io/upbound/not-a-provider:v1.0.0
YAML

cat >"$scratch/lookalikes.yaml" <<'YAML'
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: lookalike-org
spec:
  package: xpkg.upbound.io/upbound-evil/provider-family-aws:v2.6.1
---
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: lookalike-registry
spec:
  package: evil.example/xpkg.upbound.io/upbound/provider-family-aws:v2.6.1
YAML

# Fake cosign: `cosign verify --certificate-oidc-issuer I --certificate-identity ID REF`.
# COSIGN_MODE picks the behaviour; every call is logged so the test can see
# which references were verified.
cat >"$scratch/cosign" <<SH
#!/usr/bin/env bash
set -euo pipefail
[[ "\$1" == verify && "\$2" == --certificate-oidc-issuer && "\$4" == --certificate-identity ]] || exit 99
[[ "\$3" == https://token.actions.githubusercontent.com ]] || exit 1
identity="\$5" ref="\$6"
echo "\$ref" >>"$scratch/calls.log"
calls=\$(wc -l <"$scratch/calls.log" | tr -d ' ')
case "\${COSIGN_MODE:-normal}" in
  normal) [[ "\$identity" == '$good_identity' ]] ;;
  unsigned-iam) [[ "\$ref" != *provider-aws-iam* && "\$identity" == '$good_identity' ]] ;;
  accepts-anything) exit 0 ;;
  outage-after-first) [[ "\$calls" -eq 1 && "\$identity" == '$good_identity' ]] ;;
  *) exit 99 ;;
esac
SH
chmod +x "$scratch/cosign"
export COSIGN="$scratch/cosign"

failures=0
# expect NAME WANT_STATUS WANT_TEXT FIXTURE [MODE]
expect() {
  local name="$1" want="$2" text="$3" fixture="$4" mode="${5:-normal}" status=0
  : >"$scratch/calls.log"
  COSIGN_MODE="$mode" bash scripts/check-upbound-package-signatures.sh --rendered "$fixture" \
    >"$scratch/out.log" 2>&1 || status=$?
  if [[ "$status" -ne "$want" ]] || ! grep -qF -- "$text" "$scratch/out.log"; then
    echo "FAIL: $name — exit $status (want $want), output:" >&2
    sed 's/^/  /' "$scratch/out.log" >&2
    failures=$((failures + 1))
    return
  fi
  echo "ok: $name"
}

expect 'both signed Upbound packages verify' 0 \
  '2 Upbound package signatures verified' "$scratch/rendered.yaml"
if grep -qv '^xpkg\.upbound\.io/upbound/provider-\(family-aws\|aws-iam\):v2\.6\.1$' "$scratch/calls.log"; then
  echo 'FAIL: a non-Upbound or non-Provider reference was sent to cosign:' >&2
  sed 's/^/  /' "$scratch/calls.log" >&2
  failures=$((failures + 1))
fi
expect 'a package not signed by Upbound is refused' 1 \
  'FAIL: xpkg.upbound.io/upbound/provider-aws-iam:v2.6.1 is not signed' "$scratch/rendered.yaml" unsigned-iam
expect 'a verifier that accepts any identity is caught' 1 \
  'verified against a deliberately wrong identity' "$scratch/rendered.yaml" accepts-anything
expect 'an outage during the negative control is not read as a refusal' 1 \
  'stopped verifying after the negative control' "$scratch/rendered.yaml" outage-after-first
expect 'nothing to verify fails rather than passes' 1 \
  'refusing to pass vacuously' "$scratch/lookalikes.yaml"

if [[ "$failures" -ne 0 ]]; then
  echo "$failures case(s) failed" >&2
  exit 1
fi
echo 'all Upbound package signature check cases passed'
