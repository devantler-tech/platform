#!/usr/bin/env bash
# Every file that names the production Talos version is either moved by Renovate
# or named on the checklist Renovate puts on each Talos pull request (#4746).
#
# Renovate moves the `version:` lines of ksail.prod.yaml and lines carrying a
# `# renovate: … depName=siderolabs/talos` annotation, and its gomod manager moves
# the grouped machinery module. Any other pin is moved by hand, and the person
# adapting the pull request only learns about it from the checklist. A pin that
# is on neither list goes stale silently, or turns a check red a CI cycle later.
# So this fails when a tracked file names the current version on a line Renovate
# does not move and the checklist does not name the file, and when the checklist
# names a file that does not exist.

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly root_dir

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

command -v git >/dev/null 2>&1 || fail 'git is required to list tracked files'
command -v jq >/dev/null 2>&1 || fail 'jq is required to read the Renovate policy'
command -v yq >/dev/null 2>&1 || fail 'yq is required to read the Talos pin'

# check_root <dir>: prints one line per finding; exits 0 clean, 1 findings, 2 unreadable.
check_root() {
  local dir="$1" version notes paths hits findings=0
  local file line text prev escaped

  version="$(yq -e '.spec.cluster.talos.version' "$dir/ksail.prod.yaml" 2>/dev/null)" || {
    echo "cannot read .spec.cluster.talos.version from $dir/ksail.prod.yaml"
    return 2
  }
  [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "the Talos pin must be an exact vX.Y.Z version, got: $version"
    return 2
  }

  notes="$(jq -er '
    [.packageRules[] | select((.matchPackageNames // []) | index("siderolabs/talos"))]
    | if length == 1 then .[0].prBodyNotes // [] | join("\n") else error("expected one Talos rule") end
  ' "$dir/.github/renovate.json" 2>/dev/null)" || {
    echo 'the Renovate policy must hold exactly one siderolabs/talos rule'
    return 2
  }
  # File paths the checklist names in backticks; other backticked terms are not files.
  # shellcheck disable=SC2016 # the backticks are literal Markdown, not command substitution
  paths="$(grep -oE '`[A-Za-z0-9_./-]+\.(ya?ml|sh|go|tsv|json|md)`' <<<"$notes" | tr -d '`' | sort -u)"
  [ -n "$paths" ] || {
    echo 'the Talos checklist names no files'
    return 2
  }

  while IFS= read -r file; do
    if ! git -C "$dir" ls-files --error-unmatch -- "$file" >/dev/null 2>&1; then
      echo "the Talos checklist names $file, which is not a tracked file"
      findings=$((findings + 1))
    fi
  done <<<"$paths"

  escaped="${version#v}"
  escaped="${escaped//./\\.}"
  local rc=0
  hits="$(git -C "$dir" grep -I -n -E "(^|[^0-9.])v?${escaped}([^0-9]|$)")" || rc=$?
  if [ "$rc" -gt 1 ]; then
    echo "git grep failed in $dir"
    return 2
  fi
  [ -n "$hits" ] || {
    echo "no tracked file names $version, not even ksail.prod.yaml"
    return 2
  }

  while IFS= read -r hit; do
    file="${hit%%:*}"
    hit="${hit#*:}"
    line="${hit%%:*}"
    text="${hit#*:}"
    # Moved by Renovate: a line its ksail.prod.yaml manager matches, an annotated line, or
    # the grouped machinery module.
    if [ "$file" = ksail.prod.yaml ] && [[ "$text" =~ ^[[:space:]]+version:[[:space:]]*v[0-9.]+([[:space:]]|$) ]]; then
      continue
    fi
    if [ "$line" -gt 1 ]; then
      prev="$(sed -n "$((line - 1))p" "$dir/$file")"
      if [[ "$prev" =~ ^[[:space:]]*#[[:space:]]*renovate:.*depName=siderolabs/talos([[:space:]]|$) ]]; then
        continue
      fi
    fi
    if { [ "$file" = go.mod ] || [ "$file" = go.sum ]; } &&
      [[ "$text" == *github.com/siderolabs/talos/pkg/machinery* ]]; then
      continue
    fi
    if ! grep -qxF -- "$file" <<<"$paths"; then
      echo "$file:$line names Talos $version on a line Renovate does not move, and the Talos checklist does not name $file"
      findings=$((findings + 1))
    fi
  done <<<"$hits"

  [ "$findings" -eq 0 ] || return 1
}

# The repository as committed.
rc=0
out="$(check_root "$root_dir")" || rc=$?
[ "$rc" -eq 0 ] || fail "$(printf 'the Talos version pins and the checklist disagree:\n%s' "$out")"

# The check must run unconditionally among the repository guards.
guards_job="$(awk '
  /^  validate-repository-guards:$/ { in_guards = 1 }
  in_guards && /^  [[:alnum:]_-]+:$/ && $0 != "  validate-repository-guards:" { exit }
  in_guards { print }
' "$root_dir/.github/workflows/ci.yaml")"
grep -Fq 'bash scripts/tests/test-talos-version-pins.sh' <<<"$guards_job" ||
  fail 'the Talos version pin check must run unconditionally in the repository guards job'

# Negative controls on a small fixture repository: each must be refused for its own reason.
scratch="$(mktemp -d "${TMPDIR:-/tmp}/talos-version-pins.XXXXXX")"
trap 'rm -rf -- "$scratch"' EXIT

fixture() {
  local dir="$scratch/$1"
  mkdir -p "$dir/.github/scripts" "$dir/scripts"
  printf 'spec:\n  cluster:\n    talos:\n      version: v1.2.3\n' >"$dir/ksail.prod.yaml"
  jq -n '{packageRules: [{matchPackageNames: ["siderolabs/talos"],
    prBodyNotes: ["- [ ] `scripts/checked.tsv`, after checking it."]}]}' >"$dir/.github/renovate.json"
  printf '# renovate: datasource=github-releases depName=siderolabs/talos\nTALOS_VERSION="1.2.3"\n' \
    >"$dir/.github/scripts/setup.sh"
  printf 'row\tv1.2.3\n' >"$dir/scripts/checked.tsv"
  printf 'require github.com/siderolabs/talos/pkg/machinery v1.2.3\n' >"$dir/go.mod"
  printf 'unrelated 1.2.30 and 11.2.3\n' >"$dir/scripts/other.txt"
  git -C "$dir" init -q
}

expect() {
  local name="$1" want="$2" reason="$3" got=0 out
  out="$(check_root "$scratch/$name")" || got=$?
  [ "$got" -eq "$want" ] || fail "$name: expected exit $want, got $got: $out"
  [ -z "$reason" ] || grep -Fq -- "$reason" <<<"$out" || fail "$name: expected '$reason', got: $out"
}

fixture covered
git -C "$scratch/covered" add -A
expect covered 0 ''

fixture uncovered
printf '# Keep Talos v1.2.3 here.\n' >"$scratch/uncovered/scripts/stray.yaml"
git -C "$scratch/uncovered" add -A
expect uncovered 1 'scripts/stray.yaml:1 names Talos v1.2.3'

fixture unannotated
printf 'TALOS_VERSION="1.2.3"\n' >"$scratch/unannotated/.github/scripts/setup.sh"
git -C "$scratch/unannotated" add -A
expect unannotated 1 '.github/scripts/setup.sh:1 names Talos v1.2.3'

fixture missing-path
jq '.packageRules[0].prBodyNotes += ["- [ ] `scripts/gone.go`."]' "$scratch/missing-path/.github/renovate.json" \
  >"$scratch/missing-path/renovate.json" && mv "$scratch/missing-path/renovate.json" "$scratch/missing-path/.github/renovate.json"
git -C "$scratch/missing-path" add -A
expect missing-path 1 'names scripts/gone.go, which is not a tracked file'

fixture untracked
printf 'v1.2.3\n' >"$scratch/untracked/scripts/scratch.yaml"
git -C "$scratch/untracked" add -A
git -C "$scratch/untracked" rm -q --cached scripts/scratch.yaml
expect untracked 0 ''

fixture no-rule
jq '.packageRules = []' "$scratch/no-rule/.github/renovate.json" >"$scratch/no-rule/renovate.json" &&
  mv "$scratch/no-rule/renovate.json" "$scratch/no-rule/.github/renovate.json"
git -C "$scratch/no-rule" add -A
expect no-rule 2 'exactly one siderolabs/talos rule'

printf 'PASS: every file naming the Talos version is moved by Renovate or on the Talos checklist\n'
