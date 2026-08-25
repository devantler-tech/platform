#!/usr/bin/env bash
# Hermetic coverage for scripts/inventory-first-party-image-signatures.sh.
#
# Every network and cluster call is stubbed through the script's declared seams, so nothing here
# depends on a registry, a credential, or the fleet. What is under test is the DECISION LOGIC —
# which rule an image is held to, and whether an unverifiable image is reported as a failure, as
# unknown, or (the bug this guards) as nothing at all.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${repo_root}/scripts/inventory-first-party-image-signatures.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0
check() { # label expected_exit expected_grep actual_exit output
  local label="$1" want_rc="$2" want="$3" got_rc="$4" out="$5"
  if [ "$got_rc" != "$want_rc" ]; then
    echo "FAIL  ${label}: expected exit ${want_rc}, got ${got_rc}"
    printf '%s\n' "$out" | sed 's/^/        /' | head -12
    failures=$((failures + 1))
    return
  fi
  if [ -n "$want" ] && ! printf '%s\n' "$out" | grep -qE "$want"; then
    echo "FAIL  ${label}: output did not match /${want}/"
    printf '%s\n' "$out" | sed 's/^/        /' | head -12
    failures=$((failures + 1))
    return
  fi
  echo "ok    ${label}"
}

# --- fixtures --------------------------------------------------------------
# Three rules in the same shape and ORDER as the real file, so first-match-wins is exercised.
cat >"${work}/rules.yaml" <<'EOF'
apiVersion: v1alpha1
kind: ImageVerificationConfig
rules:
  - image: ghcr.io/example/ksail*
    keyless:
      issuer: https://token.actions.githubusercontent.com
      subjectRegex: ^KSAIL$
  - image: ghcr.io/example/provider-upjet-*
    keyless:
      issuer: https://token.actions.githubusercontent.com
      subjectRegex: ^PROVIDER$
  - image: ghcr.io/example/*
    keyless:
      issuer: https://token.actions.githubusercontent.com
      subjectRegex: ^APP$
EOF

# verify stub: accepts only when the subjectRegex it was handed is in ACCEPT_SUBJECTS.
cat >"${work}/verify" <<'EOF'
#!/usr/bin/env bash
for s in ${ACCEPT_SUBJECTS:-}; do [ "$3" = "$s" ] && exit 0; done
exit 1
EOF
chmod +x "${work}/verify"

# probe stub: returns the status named for that image in PROBE_MAP ("<substr>=<code> ...").
cat >"${work}/probe" <<'EOF'
#!/usr/bin/env bash
for pair in ${PROBE_MAP:-}; do
  case "$1" in *"${pair%%=*}"*) echo "${pair##*=}"; exit 0 ;; esac
done
echo 200
EOF
chmod +x "${work}/probe"

run() { # images_multiline  -> sets RC / OUT
  printf '%s\n' "$1" >"${work}/images"
  set +e
  OUT="$(INVENTORY_VERIFY_CMD="${work}/verify" INVENTORY_PROBE_CMD="${work}/probe" \
    "$script" --rules "${work}/rules.yaml" --images "${work}/images" 2>&1)"
  RC=$?
  set -e
}

# --- 1. every matched image verifies -> exit 0 -----------------------------
ACCEPT_SUBJECTS='^KSAIL$ ^PROVIDER$ ^APP$' PROBE_MAP='' \
  run 'ghcr.io/example/ksail:v1
ghcr.io/example/provider-upjet-unifi:v0.1.0
ghcr.io/example/wedding-app@sha256:aa'
check "all matched images verify -> 0" 0 'PASS=3 FAIL=0 UNKNOWN=0' "$RC" "$OUT"

# --- 2. FIRST MATCH WINS ---------------------------------------------------
# ksail matches BOTH rule 1 and the catch-all. Held to rule 1, it verifies; if the catch-all won,
# the ^APP$ identity would be applied and it would be reported FAIL. This is the assertion that
# a re-sorted or greedily-matched rules file cannot pass.
ACCEPT_SUBJECTS='^KSAIL$' PROBE_MAP='' run 'ghcr.io/example/ksail:v1'
check "ksail is held to rule 1, not the catch-all" 0 'ghcr\.io/example/ksail\*' "$RC" "$OUT"

ACCEPT_SUBJECTS='^APP$' PROBE_MAP='' run 'ghcr.io/example/ksail:v1'
check "the catch-all identity does NOT satisfy ksail" 1 'FAIL' "$RC" "$OUT"

# --- 3. unverifiable + READABLE repository -> FAIL -------------------------
ACCEPT_SUBJECTS='' PROBE_MAP='wedding-app=200' run 'ghcr.io/example/wedding-app@sha256:aa'
check "readable but unsigned -> FAIL" 1 'FAIL.*would be REFUSED at pull' "$RC" "$OUT"

# --- 4. unverifiable + UNREADABLE repository -> UNKNOWN, never FAIL --------
# The distinction this script exists for: DENIED from a private package says nothing about the
# signature, and calling it FAIL would invent a blast radius that has not been measured.
for code in 401 403; do
  ACCEPT_SUBJECTS='' PROBE_MAP="wedding-app=${code}" run 'ghcr.io/example/wedding-app@sha256:aa'
  check "HTTP ${code} on the image manifest -> UNKNOWN (not FAIL)" 1 'UNKNOWN.*unreadable' "$RC" "$OUT"
  if printf '%s\n' "$OUT" | grep -q 'FAIL='"[1-9]"; then
    echo "FAIL  HTTP ${code} was counted as a FAIL"
    failures=$((failures + 1))
  fi
done

# --- 5. the probe itself could not run -> UNKNOWN --------------------------
ACCEPT_SUBJECTS='' PROBE_MAP='wedding-app=000' run 'ghcr.io/example/wedding-app@sha256:aa'
check "probe unavailable -> UNKNOWN" 1 'UNKNOWN.*probe could not run' "$RC" "$OUT"

# --- 5b. a manifest read that did not SUCCEED -> UNKNOWN, never FAIL ------
# 404/429/5xx are not "readable but unsigned": the read did not succeed, so nothing about the
# signature was established. Counting them FAIL invents images that would be refused and overstates
# the very blast radius the activation decision rests on.
for code in 404 429 500; do
  ACCEPT_SUBJECTS='' PROBE_MAP="wedding-app=${code}" run 'ghcr.io/example/wedding-app@sha256:aa'
  check "HTTP ${code} on the image manifest -> UNKNOWN (not FAIL)" 1 'UNKNOWN.*read failed' "$RC" "$OUT"
  if printf '%s\n' "$OUT" | grep -q 'FAIL=[1-9]'; then
    echo "FAIL  HTTP ${code} was counted as a FAIL"
    failures=$((failures + 1))
  fi
done

# --- 5c. only a 2xx read may produce FAIL ----------------------------------
# The positive control for 5b: the same unsigned image, read successfully, MUST still be FAIL.
# Without this, 5b would also pass if the classifier stopped producing FAIL at all.
ACCEPT_SUBJECTS='' PROBE_MAP='wedding-app=204' run 'ghcr.io/example/wedding-app@sha256:aa'
check "a 2xx read of an unsigned image is still FAIL" 1 'FAIL=1' "$RC" "$OUT"

# --- 5d. cosign absent is a measurement that did not run -------------------
# Without cosign every verification returns non-zero, so each 2xx image would be reported FAIL — a
# blast radius produced by a missing tool rather than by the fleet. The gate runs after the rules
# are validated, so this needs a real toolchain; the PATH below carries every binary the script
# reaches before the gate, and deliberately not cosign. Listing them explicitly means a future
# dependency shows up as a loud failure here rather than as a silently skipped check.
mkdir -p "${work}/nocosign"
for b in bash sh env yq grep sed sort cat tr awk; do
  real="$(command -v "$b" 2>/dev/null)" && ln -sf "$real" "${work}/nocosign/${b}"
done
printf '%s\n' 'ghcr.io/example/wedding-app@sha256:aa' >"${work}/images"
set +e
OUT="$(PATH="${work}/nocosign" INVENTORY_PROBE_CMD="${work}/probe" \
  "$script" --rules "${work}/rules.yaml" --images "${work}/images" 2>&1)"
RC=$?
set -e
check "cosign absent -> exit 2, never a fabricated FAIL" 2 'cosign is required' "$RC" "$OUT"

# --- 5e. a rules error still wins over the cosign gate ---------------------
# Ordering assertion: config problems must report themselves, not be masked by a missing tool.
set +e
OUT="$(PATH="${work}/nocosign" INVENTORY_PROBE_CMD="${work}/probe" \
  "$script" --rules "${work}/nonexistent.yaml" --images "${work}/images" 2>&1)"
RC=$?
set -e
check "an unreadable rules file outranks the cosign gate" 2 'not readable' "$RC" "$OUT"

# --- 6. an UNKNOWN alone is enough to withhold the all-clear ---------------
ACCEPT_SUBJECTS='^KSAIL$' PROBE_MAP='wedding-app=401' \
  run 'ghcr.io/example/ksail:v1
ghcr.io/example/wedding-app@sha256:aa'
check "one UNKNOWN withholds exit 0" 1 'PASS=1 FAIL=0 UNKNOWN=1' "$RC" "$OUT"

# --- 7. an EMPTY enumeration is never "nothing to fix" ---------------------
# The fail-open with the highest cost: a broken enumerator and a clean cluster look identical,
# and only one of them should exit 0.
: >"${work}/empty"
set +e
OUT="$(INVENTORY_VERIFY_CMD="${work}/verify" INVENTORY_PROBE_CMD="${work}/probe" \
  "$script" --rules "${work}/rules.yaml" --images "${work}/empty" 2>&1)"
RC=$?
set -e
check "empty enumeration -> exit 2, never 0" 2 'no images at all' "$RC" "$OUT"

# --- 8. images present but NONE match -> exit 2 ----------------------------
ACCEPT_SUBJECTS='' PROBE_MAP='' run 'registry.k8s.io/pause:3.9
docker.io/library/nginx:1.27'
check "zero matches out of a non-empty enumeration -> exit 2" 2 'matcher or the enumeration is broken' "$RC" "$OUT"

# --- 9. a failing enumerator is a producer error, not a clean run ----------
set +e
OUT="$(INVENTORY_ENUMERATE_CMD='exit 7' INVENTORY_VERIFY_CMD="${work}/verify" \
  INVENTORY_PROBE_CMD="${work}/probe" "$script" --rules "${work}/rules.yaml" 2>&1)"
RC=$?
set -e
check "enumerator failure -> exit 2" 2 'enumeration command failed' "$RC" "$OUT"

# --- 10. malformed and empty rule sets fail closed ------------------------
printf 'ghcr.io/example/wedding-app@sha256:aa\n' >"${work}/images-app"

cat >"${work}/rules-incomplete.yaml" <<'EOF'
apiVersion: v1alpha1
kind: ImageVerificationConfig
rules:
  - image: ghcr.io/example/*
    keyless:
      issuer: https://token.actions.githubusercontent.com
EOF
set +e
OUT="$("$script" --rules "${work}/rules-incomplete.yaml" --images "${work}/images-app" 2>&1)"
RC=$?
set -e
check "a rule missing its subjectRegex -> exit 2" 2 'incomplete rule' "$RC" "$OUT"

printf 'apiVersion: v1alpha1\nkind: ImageVerificationConfig\nrules: []\n' >"${work}/rules-empty.yaml"
set +e
OUT="$("$script" --rules "${work}/rules-empty.yaml" --images "${work}/images-app" 2>&1)"
RC=$?
set -e
check "an empty rule set -> exit 2" 2 'no rules found' "$RC" "$OUT"

set +e
OUT="$("$script" --rules "${work}/nope.yaml" --images "${work}/images-app" 2>&1)"
RC=$?
set -e
check "an unreadable rules file -> exit 2" 2 'not readable' "$RC" "$OUT"

# --- 11. the REAL rules file parses, and keeps its three ordered rules -----
# Guards the shape this script reads from drifting without anyone noticing: the catch-all must stay
# LAST, or ksail and the provider packages get held to the app identity and go ImagePullBackOff.
real="${repo_root}/talos/cluster/verify-first-party-images.yaml"
# macOS ships bash 3.2, which has no `mapfile` — read the globs without it.
real_globs="$(yq -r '.rules[].image' "$real")"
real_count="$(printf '%s\n' "$real_globs" | grep -c .)"
real_last="$(printf '%s\n' "$real_globs" | grep . | tail -1)"
if [ "$real_count" -ne 3 ]; then
  echo "FAIL  the real rules file has ${real_count} rules, expected 3"
  failures=$((failures + 1))
elif [ "$real_last" != 'ghcr.io/devantler-tech/*' ]; then
  echo "FAIL  the catch-all is not the last rule (${real_last})"
  failures=$((failures + 1))
else
  echo "ok    real rules: three rules, catch-all last"
fi

# The real file must also satisfy the completeness gate the fixtures exercise.
set +e
OUT="$(INVENTORY_VERIFY_CMD="${work}/verify" INVENTORY_PROBE_CMD="${work}/probe" \
  "$script" --rules "$real" --images "${work}/images-app" 2>&1)"
RC=$?
set -e
if printf '%s\n' "$OUT" | grep -qE 'incomplete rule|no rules found|has no .rules sequence'; then
  echo "FAIL  the real rules file does not pass the completeness gate"
  printf '%s\n' "$OUT" | sed 's/^/        /' | head -5
  failures=$((failures + 1))
else
  echo "ok    real rules: passes the completeness gate"
fi

echo
if [ "$failures" -eq 0 ]; then echo "all checks passed"; else
  echo "${failures} check(s) failed"
  exit 1
fi
