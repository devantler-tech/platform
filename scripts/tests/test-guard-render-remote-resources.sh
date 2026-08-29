#!/usr/bin/env bash
#
# Pins the render remote-resource guard's verdict in all THREE directions.
#
#   exit 0  every entry resolves in-repo, or carries a reviewed exception row
#   exit 1  an unexcepted remote entry, or an exception row that has gone stale
#   exit 2  the guard could not check — bad usage, missing root, unparseable YAML,
#           an entry it cannot classify, a malformed exception row, or either
#           anti-vacuity case
#
# The assertions that matter most are NOT the happy path. A guard that has only
# seen passing input has proved nothing about what it rejects, and three of these
# cases exist because the obvious implementation gets them wrong:
#
#   * SCHEMELESS REMOTE. `github.com/org/repo//path?ref=v1` is a kustomize remote
#     base containing no `://`, so the natural scheme test reports it clean.
#   * HOST-NAMED LOCAL DIRECTORY. This is a Kubernetes repository, so a directory
#     named after an API group (`cert-manager.k8s.cloudflare.com`) is ordinary. A
#     guard that repairs the case above by matching host-shaped strings then
#     REFUSES a correct file.
#   * STALE EXCEPTION ROW. An exception outliving its subject silently becomes a
#     standing permission, pre-approving that URL on the day it returns.
#
# Every fixture carries its OWN distinctive resource name, so an assertion can only
# be satisfied by the case it belongs to.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-render-remote-resources.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

run_guard() { # <root> [exceptions-file]
  # `if` rather than toggling errexit: this file runs under `set -uo pipefail` with
  # NO errexit, so `set -e` here would enable a mode the script never had.
  if GUARD_OUT="$(RENDER_REMOTE_EXCEPTIONS="${2:-$scratch/empty-exceptions.tsv}" "$guard" "$1" 2>&1)"; then
    GUARD_RC=0
  else
    GUARD_RC=$?
  fi
}

assert_rc() { # <label> <expected-rc> <actual-rc>
  assertions=$((assertions + 1))
  if [ "$2" = "$3" ]; then
    printf '  ok   %s (exit %s)\n' "$1" "$3"
  else
    printf '  FAIL %s: expected exit %s, got %s\n' "$1" "$2" "$3"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

assert_contains() { # <label> <needle>
  assertions=$((assertions + 1))
  # A here-string, NOT a pipe: `grep -q` exits on the first match, and the SIGPIPE
  # the upstream `printf` then takes becomes the pipeline status under `pipefail`,
  # so a needle matching EARLY would read as a failed assertion.
  if grep -qF -- "$2" <<<"$GUARD_OUT"; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s: output did not contain %s\n' "$1" "$2"
    printf '%s\n' "$GUARD_OUT" | sed 's/^/       | /'
    failures=$((failures + 1))
  fi
}

# A fixture that did not actually get written passes a "the guard rejected it"
# assertion for the wrong reason, so every fixture is verified to contain the line
# that makes it the case it claims to be.
fixture() { # <path> <marker>
  assertions=$((assertions + 1))
  if grep -qF -- "$2" "$1" 2>/dev/null; then
    printf '  ok   fixture built: %s\n' "$(basename "$(dirname "$1")")"
  else
    printf '  FAIL fixture NOT built as intended: %s missing %s\n' "$1" "$2"
    failures=$((failures + 1))
  fi
}

# Deliberately EMPTY, and that is a case in its own right: once #3196's children
# vendor the two live entries, the committed exceptions file has zero rows. An
# earlier draft of the guard died on that, which would have failed CI at the exact
# moment the defect was fixed. Every case below that expects a clean pass is also
# an assertion that an empty disposition list is a legal, clean state.
: >"$scratch/empty-exceptions.tsv"

mk() { # <name> -> echoes the root dir
  mkdir -p "$scratch/$1"
  printf '%s' "$scratch/$1"
}

printf '\n== exit 0: a clean tree ==\n'
r=$(mk clean); mkdir -p "$r/base"
cat >"$r/base/cm.yaml" <<'Y'
apiVersion: v1
kind: ConfigMap
metadata:
  name: clean-case-marker
Y
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - base/cm.yaml
Y
fixture "$r/kustomization.yaml" "base/cm.yaml"
run_guard "$r"
assert_rc "clean tree passes" 0 "$GUARD_RC"

printf '\n== exit 0: a LOCAL directory named like a host is not a remote ==\n'
r=$(mk hostnamed); mkdir -p "$r/cert-manager.k8s.cloudflare.com"
cat >"$r/cert-manager.k8s.cloudflare.com/cm.yaml" <<'Y'
apiVersion: v1
kind: ConfigMap
metadata:
  name: host-named-dir-marker
Y
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - cert-manager.k8s.cloudflare.com/cm.yaml
Y
fixture "$r/kustomization.yaml" "cert-manager.k8s.cloudflare.com/cm.yaml"
run_guard "$r"
assert_rc "host-named local directory is NOT refused" 0 "$GUARD_RC"

printf '\n== exit 1: an unexcepted https remote ==\n'
r=$(mk httpsremote)
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://example.invalid/https-remote-marker/install.yaml
Y
fixture "$r/kustomization.yaml" "https-remote-marker"
run_guard "$r"
assert_rc "unexcepted https remote is rejected" 1 "$GUARD_RC"
assert_contains "names the offending url" "https-remote-marker"

printf '\n== exit 1: a SCHEMELESS remote (no :// at all) ==\n'
r=$(mk schemeless)
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - github.com/org/schemeless-remote-marker//deploy?ref=v1.2.3
Y
fixture "$r/kustomization.yaml" "schemeless-remote-marker"
run_guard "$r"
assert_rc "schemeless remote shorthand is rejected" 1 "$GUARD_RC"
assert_contains "names the schemeless url" "schemeless-remote-marker"

printf '\n== exit 1: a remote hidden in the deprecated bases: field ==\n'
r=$(mk basesfield)
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
bases:
  - https://example.invalid/bases-field-marker/install.yaml
Y
fixture "$r/kustomization.yaml" "bases-field-marker"
run_guard "$r"
assert_rc "remote under bases: is rejected" 1 "$GUARD_RC"
assert_contains "names the bases url" "bases-field-marker"

printf '\n== exit 1: a kustomize-native helm fetch ==\n'
r=$(mk helmchart); mkdir -p "$r/local"
cat >"$r/local/cm.yaml" <<'Y'
apiVersion: v1
kind: ConfigMap
metadata:
  name: helm-case-marker
Y
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - local/cm.yaml
helmCharts:
  - name: whatever
    repo: https://example.invalid/helm-repo-marker
Y
fixture "$r/kustomization.yaml" "helm-repo-marker"
run_guard "$r"
assert_rc "helmCharts remote repo is rejected" 1 "$GUARD_RC"
assert_contains "names the helm repo" "helm-repo-marker"

printf '\n== exit 1: an exception row that has gone stale ==\n'
r=$(mk stale); mkdir -p "$r/base"
cat >"$r/base/cm.yaml" <<'Y'
apiVersion: v1
kind: ConfigMap
metadata:
  name: stale-row-case-marker
Y
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - base/cm.yaml
Y
printf 'https://example.invalid/stale-exception-marker/x.yaml\t#3196\tno longer referenced\n' >"$scratch/stale.tsv"
fixture "$scratch/stale.tsv" "stale-exception-marker"
run_guard "$r" "$scratch/stale.tsv"
assert_rc "stale exception row is rejected" 1 "$GUARD_RC"
assert_contains "names the stale row" "stale-exception-marker"

printf '\n== exit 0: a remote WITH a matching exception row ==\n'
r=$(mk excepted)
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://example.invalid/excepted-remote-marker/install.yaml
Y
printf 'https://example.invalid/excepted-remote-marker/install.yaml\t#3196\ttracked, pending vendoring\n' >"$scratch/ok.tsv"
fixture "$scratch/ok.tsv" "excepted-remote-marker"
run_guard "$r" "$scratch/ok.tsv"
assert_rc "an excepted remote passes" 0 "$GUARD_RC"
assert_contains "reports it as a tracked exception" "1 tracked exception"

printf '\n== exit 2: an exception row naming no issue ==\n'
printf 'https://example.invalid/no-issue-marker/x.yaml\t\tmissing the issue column\n' >"$scratch/noissue.tsv"
fixture "$scratch/noissue.tsv" "no-issue-marker"
run_guard "$(mk noissue_root)" "$scratch/noissue.tsv"
assert_rc "exception row without a tracking issue is cannot-check" 2 "$GUARD_RC"
assert_contains "says why the row is unreviewable" "names no tracking issue"

printf '\n== exit 2: an entry that resolves to nothing and is not a known remote ==\n'
r=$(mk dangling)
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./dangling-entry-marker/does-not-exist.yaml
Y
fixture "$r/kustomization.yaml" "dangling-entry-marker"
run_guard "$r"
assert_rc "an unclassifiable entry is cannot-check" 2 "$GUARD_RC"
assert_contains "says it cannot classify it" "cannot classify"

printf '\n== exit 2: anti-vacuity — a tree with no kustomization at all ==\n'
r=$(mk emptytree); mkdir -p "$r/sub"
printf 'not a kustomization\n' >"$r/sub/readme.txt"
run_guard "$r"
assert_rc "no kustomization found is cannot-check, NOT a clean pass" 2 "$GUARD_RC"
assert_contains "says the selector matched nothing" "selector matched nothing"

printf '\n== exit 2: anti-vacuity — kustomizations exist but declare no entries ==\n'
r=$(mk noentries)
cat >"$r/kustomization.yaml" <<'Y'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
Y
fixture "$r/kustomization.yaml" "kind: Kustomization"
run_guard "$r"
assert_rc "zero entries examined is cannot-check" 2 "$GUARD_RC"
assert_contains "says it read no entry" "not one resource entry"

printf '\n== exit 2: unparseable YAML ==\n'
r=$(mk badyaml)
printf 'resources:\n  - [unclosed\n' >"$r/kustomization.yaml"
fixture "$r/kustomization.yaml" "unclosed"
run_guard "$r"
assert_rc "unparseable YAML is cannot-check" 2 "$GUARD_RC"
assert_contains "says it could not parse" "could not parse"

printf '\n== exit 2: usage and missing root ==\n'
if GUARD_OUT="$("$guard" 2>&1)"; then GUARD_RC=0; else GUARD_RC=$?; fi
assert_rc "no argument is cannot-check" 2 "$GUARD_RC"
if GUARD_OUT="$("$guard" "$scratch/definitely-absent" 2>&1)"; then GUARD_RC=0; else GUARD_RC=$?; fi
assert_rc "missing root is cannot-check" 2 "$GUARD_RC"

printf '\n%d assertion(s), %d failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ] || exit 1
printf 'test-guard-render-remote-resources: OK\n'
