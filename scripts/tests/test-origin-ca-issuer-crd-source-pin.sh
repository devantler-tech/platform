#!/usr/bin/env bash
#
# Pins how the vendored origin-ca-issuer CRDs follow their upstream source (#4136).
#
#   1. Renovate can see the pin. Exactly one custom manager extracts the source commit from the REAL
#      updater, on a datasource whose updates carry a release timestamp, and its updates never
#      automerge.
#   2. A bump of that commit alone cannot go green. `--validate-committed` fails until the CRDs are
#      re-fetched at the new commit, and a record it cannot read is exit 2, never a match.
#   3. The refresh is what moves the record. `--render-remotes` fetches at the pinned commit and
#      records it, and a refresh that fails leaves every committed file as it was.
#
# Parts 2 and 3 run a COPY of the real updater inside a scratch tree, with stub `go`, `curl` and
# `checkov` first on PATH, so nothing here reaches the network. The stubs replace only what has a
# suite of its own (the Go annotator) or cannot run offline (the downloads and the Checkov scan);
# SHA-256 verification, the cert-approver image-pin guard and the updater's own control flow are real.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly updater="$repo_root/scripts/update-vendored-operators.sh"
readonly renovate_config="$repo_root/.github/renovate.json"
readonly crd_dir='k8s/providers/hetzner/infrastructure/controllers/origin-ca-issuer'
readonly approver_dir='k8s/providers/hetzner/infrastructure/controllers/kubelet-serving-cert-approver'
readonly record="$crd_dir/custom-resource-definitions.source-commit"
readonly bumped='1111111111111111111111111111111111111111'

die() {
  printf 'test-origin-ca-issuer-crd-source-pin: %s\n' "$*" >&2
  exit 2
}

for tool in jq yq sha256sum sed cmp; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

scratch="$(mktemp -d)"
readonly scratch
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

pass() {
  assertions=$((assertions + 1))
  printf '  ok   %s\n' "$1"
}

fail() {
  assertions=$((assertions + 1))
  failures=$((failures + 1))
  printf '  FAIL %s\n' "$1"
}

assert_eq() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then
    pass "$1"
  else
    fail "$1: expected '$2', got '$3'"
  fi
}

assert_rc() { # <label> <expected-rc>
  if [ "$2" = "$RC" ]; then
    pass "$1 (exit $RC)"
  else
    fail "$1: expected exit $2, got $RC"
    printf '%s\n' "$OUT" | sed 's/^/       | /'
  fi
}

assert_contains() { # <label> <needle>
  # A here-string, not a pipe: under pipefail an early `grep -q` match SIGPIPEs the writer and
  # inverts the verdict.
  if grep -qF -- "$2" <<<"$OUT"; then
    pass "$1"
  else
    fail "$1: output did not contain '$2'"
    printf '%s\n' "$OUT" | sed 's/^/       | /'
  fi
}

assert_same_file() { # <label> <expected-file> <actual-file>
  if cmp -s "$2" "$3"; then
    pass "$1"
  else
    fail "$1: $3 differs from $2"
  fi
}

constant() { # <name> -> the value of `readonly <name>='…'` in the updater
  sed -n "s/^readonly $1='\\(.*\\)'\$/\\1/p" "$updater"
}

pinned="$(constant origin_ca_issuer_commit)"
readonly pinned
[[ "$pinned" =~ ^[0-9a-f]{40}$ ]] || die "the updater must declare one origin_ca_issuer_commit; read '${pinned:-<nothing>}'"

echo "== Renovate tracks the source commit =="

managers="$(jq -c '[.customManagers[] | select(any(.matchStrings[]; contains("origin_ca_issuer_commit")))]' "$renovate_config")" ||
  die "cannot parse $renovate_config"
readonly managers
assert_eq "exactly one custom manager names origin_ca_issuer_commit" 1 "$(jq -r 'length' <<<"$managers")"

manager_field() { # <jq-path> -> that field of the manager, or an empty string
  jq -r "(.[0] // {}) | $1 // \"\"" <<<"$managers"
}

assert_eq "the manager is a regex manager" regex "$(manager_field .customType)"
assert_eq "the manager tracks the upstream repository" cloudflare/origin-ca-issuer "$(manager_field .packageNameTemplate)"
assert_eq "the manager follows the branch the CRDs came from" trunk "$(manager_field .currentValueTemplate)"
# github-digest reports the branch head together with its commit date. git-refs resolves the same
# commit with no date at all, and Renovate withholds an undated update whenever a release-age
# cooldown applies to it.
assert_eq "the datasource dates the branch head" github-digest "$(manager_field .datasourceTemplate)"

file_pattern="$(manager_field '.managerFilePatterns | if length == 1 then .[0] else "" end')"
file_pattern="${file_pattern#/}"
file_pattern="${file_pattern%/}"
if [ -n "$file_pattern" ] && [[ 'scripts/update-vendored-operators.sh' =~ $file_pattern ]]; then
  pass "the manager reads the updater"
else
  fail "the manager's only file pattern must match scripts/update-vendored-operators.sh, got '$file_pattern'"
fi

# What Renovate will rewrite: every currentDigest the manager's own expressions capture from the
# real updater. More than one capture, or none, means the bump would not move the pin this test binds.
extracted="$(jq -rn --rawfile script "$updater" --argjson managers "$managers" '
  ($managers[0].matchStrings // [])[] as $expression
  | $script
  | [match($expression; "g")][]
  | .captures[]
  | select(.name == "currentDigest")
  | .string
')" || extracted='<jq failed>'
readonly extracted
assert_eq "the manager extracts exactly the pinned commit from the updater" "$pinned" "$extracted"

rules="$(jq -c '
  [.packageRules | to_entries[]
    | select((.value.matchPackageNames // []) | index("cloudflare/origin-ca-issuer"))
    | .value + {position: .key}]
' "$renovate_config")" || die "cannot parse $renovate_config"
readonly rules
assert_eq "exactly one package rule covers the source" 1 "$(jq -r 'length' <<<"$rules")"

rule_field() { # <jq-expression over the rule> -> its value, or an empty string
  jq -r "(.[0] // {}) | $1" <<<"$rules"
}

assert_eq "the rule is scoped to the digest datasource" '["github-digest"]' "$(rule_field '.matchDatasources // [] | tojson')"
assert_eq "a source bump never automerges" false "$(rule_field 'if has("automerge") then .automerge else "unset" end')"
# The age of a branch head is the age of its newest commit, so every upstream commit restarts the
# repository-wide cooldown and a branch that moves weekly would never be proposed. The update is a
# signal that cannot merge without the refresh, so it is raised at once.
assert_eq "the cooldown does not withhold the signal" '0 days' "$(rule_field '.minimumReleaseAge // "unset"')"
# Renovate applies package rules in order and a later match wins, so a rule after this one that turns
# automerge on could undo it. No matcher is evaluated here: any later rule doing so fails.
later_automerge="$(jq -r --argjson rules "$rules" '
  ($rules[0].position // -1) as $position
  | [.packageRules | to_entries[] | select($position >= 0 and .key > $position and .value.automerge == true)]
  | length
' "$renovate_config")" || later_automerge='<jq failed>'
assert_eq "no later package rule turns automerge back on" 0 "$later_automerge"

# --- A scratch copy of everything the updater reads, and the stubs it runs against ---------------

checkov_version="$(constant checkov_version)"
image_digest="$(constant cert_approver_image_digest)"
readonly checkov_version image_digest
readonly stubs="$scratch/bin"
mkdir -p "$stubs"

# `go run ./scripts/annotate-vendored-checkov …`: accept every validation, copy the annotate pass
# through, and produce one resource for the split.
cat >"$stubs/go" <<'STUB'
#!/usr/bin/env bash
split_dir=''
validating=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --split-resources)
      split_dir="$2"
      shift
      ;;
    --validate-*) validating=1 ;;
  esac
  shift
done
if [ -n "$split_dir" ]; then
  mkdir -p "$split_dir"
  cat >"$split_dir/deployment.yaml"
elif [ "$validating" -eq 1 ]; then
  cat >/dev/null
else
  cat
fi
STUB

# The updater runs Checkov under `env -i`, so the pinned version is written into the stub. A secrets
# scan must exit 1: the updater requires its synthetic canary to be found.
cat >"$stubs/checkov" <<STUB
#!/usr/bin/env bash
case " \$* " in
  *' --version '*) printf '%s\n' '$checkov_version' ;;
  *' --framework secrets '*)
    printf '{}\n'
    exit 1
    ;;
  *) printf '{}\n' ;;
esac
STUB

# Serves the fixture directory named by STUB_UPSTREAM and logs every URL asked for.
cat >"$stubs/curl" <<'STUB'
#!/usr/bin/env bash
output=''
url=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      output="$2"
      shift
      ;;
    --proto | --retry | -H) shift ;;
    https://*) url="$1" ;;
  esac
  shift
done
printf '%s\n' "$url" >>"$STUB_LOG"
case "$url" in
  https://ghcr.io/token*) printf '{"token":"stub"}' ;;
  https://ghcr.io/v2/*) printf 'Docker-Content-Digest: %s\r\n' "$STUB_IMAGE_DIGEST" ;;
  */cert-manager.k8s.cloudflare.com_clusteroriginissuers.yaml) cp "$STUB_UPSTREAM/clusteroriginissuers.yaml" "$output" ;;
  */cert-manager.k8s.cloudflare.com_originissuers.yaml) cp "$STUB_UPSTREAM/originissuers.yaml" "$output" ;;
  */deploy/ha-install.yaml) cp "$STUB_UPSTREAM/ha-install.yaml" "$output" ;;
  *)
    printf 'stub curl: unexpected URL %s\n' "$url" >&2
    exit 22
    ;;
esac
STUB
chmod +x "$stubs/go" "$stubs/checkov" "$stubs/curl"

# Upstream as it is today, and upstream after one CRD changed.
readonly upstream_same="$scratch/upstream-same"
readonly upstream_changed="$scratch/upstream-changed"
mkdir -p "$upstream_same" "$upstream_changed"
cp "$repo_root/$crd_dir/custom-resource-definition-clusteroriginissuers.yaml" "$upstream_same/clusteroriginissuers.yaml"
cp "$repo_root/$crd_dir/custom-resource-definition-originissuers.yaml" "$upstream_same/originissuers.yaml"
printf 'kind: Deployment\n' >"$upstream_same/ha-install.yaml"
cp "$upstream_same"/*.yaml "$upstream_changed/"
printf '# a later upstream revision\n' >>"$upstream_changed/originissuers.yaml"
bundle_sha256="$(sha256sum "$upstream_same/ha-install.yaml")"
readonly bundle_sha256="${bundle_sha256%% *}"

set_constant() { # <tree> <name> <value>
  local script="$1/scripts/update-vendored-operators.sh"
  sed "s|^readonly $2='[^']*'|readonly $2='$3'|" "$script" >"$script.new" || die "cannot rewrite $2"
  # Overwrite in place: replacing the file would drop its executable bit.
  cat "$script.new" >"$script"
  rm "$script.new"
  grep -qF -- "readonly $2='$3'" "$script" || die "the copy does not declare $2"
}

# Every tree but the first starts from a record that agrees with the pin, so each case fails for the
# one condition it sets up. `committed` keeps the record exactly as it is committed.
make_tree() { # <name> [committed] -> echoes a scratch root holding a copy of everything the updater reads
  local root="$scratch/$1"
  mkdir -p "$root/scripts" "$root/$crd_dir" "$root/$approver_dir" \
    "$root/k8s/bases/infrastructure/controllers/cdi" "$root/k8s/bases/infrastructure/controllers/kubevirt"
  cp -p "$updater" "$repo_root/scripts/megalinter-scan-counts.sh" \
    "$repo_root/scripts/guard-cert-approver-image-pin.sh" "$root/scripts/"
  cp -p "$repo_root/$crd_dir"/custom-resource-definition* "$root/$crd_dir/"
  if [ "${2:-}" != committed ]; then
    printf '%s\n' "$pinned" >"$root/$record"
  fi
  cp -p "$repo_root/$approver_dir/kustomization.yaml" "$root/$approver_dir/"
  # Only ever read by the stubbed Go helper.
  : >"$root/k8s/bases/infrastructure/controllers/cdi/cdi-operator.yaml"
  : >"$root/k8s/bases/infrastructure/controllers/kubevirt/kubevirt-operator.yaml"
  # The stub upstream serves a one-line cert-approver bundle, so bind the copy to its digest.
  set_constant "$root" cert_approver_sha256 "$bundle_sha256"
  printf '%s' "$root"
}

run_updater() { # <tree> <upstream-fixture> [updater arguments]; sets OUT and RC
  local tree="$1" upstream="$2"
  shift 2
  : >"$tree/curl.log"
  if OUT="$(PATH="$stubs:$PATH" STUB_LOG="$tree/curl.log" STUB_UPSTREAM="$upstream" \
    STUB_IMAGE_DIGEST="$image_digest" "$tree/scripts/update-vendored-operators.sh" "$@" 2>&1)"; then
    RC=0
  else
    RC=$?
  fi
}

# The one case that reads the record as committed, which makes it the live gate as well: on a pull
# request that moves only the pin, this is the assertion that fails, with the updater's own message.
echo "== the committed tree validates =="
tree="$(make_tree committed committed)" || die "cannot build the committed scratch tree"
run_updater "$tree" "$upstream_same" --validate-committed
assert_rc "the committed pin, record and CRDs agree" 0

echo "== a bump of the commit alone fails =="
tree="$(make_tree bump-only)" || die "cannot build the bump-only scratch tree"
set_constant "$tree" origin_ca_issuer_commit "$bumped"
run_updater "$tree" "$upstream_same" --validate-committed
assert_rc "commit-only bump" 1
assert_contains "names the commit the pin moved to" "origin_ca_issuer_commit is $bumped"
assert_contains "names the commit the CRDs were fetched at" "fetched at $pinned"
assert_contains "names the refresh that resolves it" "--render-remotes"

echo "== a record that cannot be read is not a match =="
tree="$(make_tree record-missing)" || die "cannot build the record-missing scratch tree"
rm -f "$tree/$record"
run_updater "$tree" "$upstream_same" --validate-committed
assert_rc "missing record" 2
assert_contains "names the unreadable record" "$record"

unreadable_record() { # <case-name> <label> <record-content>
  tree="$(make_tree "$1")" || die "cannot build the $1 scratch tree"
  printf '%s' "$3" >"$tree/$record"
  run_updater "$tree" "$upstream_same" --validate-committed
  assert_rc "$2" 2
  assert_contains "$2 is reported as unreadable" "could not read exactly one 40-character commit SHA"
}
unreadable_record record-empty "empty record" ''
unreadable_record record-branch "record names a branch" $'trunk\n'
unreadable_record record-short "record holds an abbreviated commit" "${pinned:0:39}"$'\n'
unreadable_record record-two "record holds two commits" "$pinned"$'\n'"$pinned"$'\n'
# Command substitution strips every trailing newline, so these two read back as a bare commit.
unreadable_record record-blank-line "record holds a commit and a blank line" "$pinned"$'\n\n'
unreadable_record record-unterminated "record holds a commit with no line ending" "$pinned"

echo "== the pin must be an immutable commit =="
tree="$(make_tree pin-branch)" || die "cannot build the pin-branch scratch tree"
set_constant "$tree" origin_ca_issuer_commit trunk
run_updater "$tree" "$upstream_same" --validate-committed
assert_rc "pin names a branch" 2
assert_contains "names the malformed pin" "origin_ca_issuer_commit"
run_updater "$tree" "$upstream_same" --render-remotes
assert_rc "a branch pin is refused before any download" 2
assert_eq "nothing at all was requested for a branch pin" '' "$(cat "$tree/curl.log")"

echo "== the digests still bind the bytes =="
tree="$(make_tree bytes-edited)" || die "cannot build the bytes-edited scratch tree"
printf '# edited by hand\n' >>"$tree/$crd_dir/custom-resource-definition-originissuers.yaml"
run_updater "$tree" "$upstream_same" --validate-committed
assert_rc "edited CRD" 1
assert_contains "the edited CRD fails its digest" "custom-resource-definition-originissuers.yaml: FAILED"

echo "== a refresh at the bumped commit records it =="
tree="$(make_tree refresh)" || die "cannot build the refresh scratch tree"
set_constant "$tree" origin_ca_issuer_commit "$bumped"
run_updater "$tree" "$upstream_same" --render-remotes
assert_rc "refresh with unchanged upstream bytes" 0
assert_eq "the record names the fetched commit" "$bumped" "$(cat "$tree/$record" 2>/dev/null)"
assert_eq "both CRDs were fetched at the bumped commit" 2 \
  "$(grep -cF "https://raw.githubusercontent.com/cloudflare/origin-ca-issuer/$bumped/deploy/crds/" "$tree/curl.log")"
run_updater "$tree" "$upstream_same" --validate-committed
assert_rc "the refreshed tree validates" 0

echo "== a refresh that meets changed upstream bytes changes nothing =="
tree="$(make_tree refresh-changed)" || die "cannot build the refresh-changed scratch tree"
set_constant "$tree" origin_ca_issuer_commit "$bumped"
cp "$tree/$record" "$scratch/record-before-refresh"
run_updater "$tree" "$upstream_changed" --render-remotes
assert_rc "refresh with a changed CRD" 1
changed_sha256="$(sha256sum "$upstream_changed/originissuers.yaml")"
assert_contains "names the digest the new bytes hash to" "hashes to ${changed_sha256%% *}"
# The leading space keeps clusteroriginissuers_sha256 from satisfying this.
assert_contains "names the constant to review" " originissuers_sha256 is "
assert_same_file "the record is untouched" "$scratch/record-before-refresh" "$tree/$record"
assert_same_file "the committed CRD is untouched" \
  "$repo_root/$crd_dir/custom-resource-definition-originissuers.yaml" \
  "$tree/$crd_dir/custom-resource-definition-originissuers.yaml"

printf '\n%d assertion(s), %d failure(s)\n' "$assertions" "$failures"
[ "$failures" -eq 0 ]
