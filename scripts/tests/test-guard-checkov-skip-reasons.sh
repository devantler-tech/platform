#!/usr/bin/env bash
#
# Pins the checkov skip-reason guard's verdict in all THREE directions.
#
# The guard's job is to make a lost suppression reason impossible. It enforces that
# structurally — every `checkov.io/skipN` value must be an explicitly QUOTED scalar,
# because a quoted `#` is literal and cannot open a comment. So the cases that matter
# are:
#
#   exit 1  a value that is unquoted, or carries no reason at all
#   exit 0  a value that is quoted and complete — INCLUDING the two shapes that a
#           source-text matcher necessarily gets wrong (a trailing comment after a
#           complete reason, and a commented-out template)
#   exit 2  the guard could not check — a missing tool, or an annotation the parser
#           cannot see because a block scalar hides it
#
# Every fixture carries its OWN filename and its OWN needle, so an assertion can only
# be satisfied by the case it belongs to. (A previous revision named every fixture
# `resource.yaml`, which discriminated nothing beyond "some report was emitted".)

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
guard="$repo_root/scripts/guard-checkov-skip-reasons.sh"
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

failures=0
assertions=0

run_guard() { # <tree> [PATH-override]
  set +e
  if [ $# -ge 2 ]; then
    GUARD_OUT="$(PATH="$2" /bin/bash "$guard" "$1" 2>&1)"
  else
    GUARD_OUT="$("$guard" "$1" 2>&1)"
  fi
  GUARD_RC=$?
  set -e
}

# A fixture tree holding one file, named distinctly, with content supplied verbatim.
tree_with() { # <name> <basename>  (body on stdin)
  local dir="$scratch/$1"
  mkdir -p "$dir"
  cat >"$dir/$2"
  printf '%s' "$dir"
}

# The common shape: one annotation whose scalar is supplied verbatim, so neighbouring
# cases differ in exactly one scalar.
scalar_fixture() { # <name> <raw-scalar-text>
  tree_with "$1" "$1.yaml" <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $1
  annotations:
    checkov.io/skip1: $2
spec:
  replicas: 1
YAML
}

expect() { # <case> <expected-rc> <tree> [<needle>]
  local case_name=$1 want=$2 tree=$3 needle=${4-}
  assertions=$((assertions + 1))
  run_guard "$tree"
  if [ "$GUARD_RC" -ne "$want" ]; then
    printf 'FAIL %s: expected exit %s, got %s\n%s\n' "$case_name" "$want" "$GUARD_RC" "$GUARD_OUT" >&2
    failures=$((failures + 1))
    return
  fi
  if [ -n "$needle" ] && ! printf '%s' "$GUARD_OUT" | grep -qF -- "$needle"; then
    printf 'FAIL %s: exit %s was right, but the report never named %s\n%s\n' \
      "$case_name" "$want" "$needle" "$GUARD_OUT" >&2
    failures=$((failures + 1))
    return
  fi
  printf 'ok   %s (exit %s)\n' "$case_name" "$want"
}

# ---------------------------------------------------------------------------
# exit 0 — quoted and complete
# ---------------------------------------------------------------------------
expect 'double-quoted reason passes' 0 \
  "$(scalar_fixture okdouble '"CKV_K8S_1=okdouble the reason"')" '1 annotation(s) checked'

expect 'single-quoted reason passes' 0 \
  "$(scalar_fixture oksingle "'CKV_K8S_1=oksingle the reason'")" 'all quoted'

# THE POINT OF THE WHOLE DESIGN: a quoted reason may contain ' #' and keep it.
expect 'quoted reason containing a hash keeps its whole reason' 0 \
  "$(scalar_fixture okhash '"CKV_K8S_1=okhash deferred to #3202 -- and this tail survives"')" 'all quoted'

# Defect 5: a complete reason followed by an ordinary trailing comment. Lexically
# identical to a truncation, so a source-text matcher MUST get one of them wrong.
expect 'trailing comment after a quoted reason is not a finding' 0 \
  "$(scalar_fixture oktrailing '"CKV_K8S_1=oktrailing complete reason" # yamllint disable-line')" 'all quoted'

# Defect 6: a commented-out template is not an annotation at all.
expect 'commented-out template is not a finding' 0 \
  "$(
    tree_with okcommented okcommented.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: okcommented
  annotations:
    # checkov.io/skip1: CKV_K8S_1=okcommented template line
    app: real
YAML
  )" '0 annotation(s) checked'

# Defect 7: a merge key has tag !!merge; calling test() on it used to abort the run.
expect 'a YAML merge key does not abort the guard' 0 \
  "$(
    tree_with okmerge okmerge.yaml <<'YAML'
defaults: &defaults
  team: platform
apiVersion: v1
kind: Pod
metadata:
  <<: *defaults
  name: okmerge
  annotations:
    checkov.io/skip1: "CKV_K8S_1=okmerge the reason"
YAML
  )" 'all quoted'

# A prose cross-reference must not inflate the reconciliation count.
expect 'prose mentioning the annotation does not break reconciliation' 0 \
  "$(
    tree_with okprose okprose.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: okprose
  annotations:
    checkov.io/skip1: "CKV_K8S_1=okprose the reason"
    # see the checkov.io/skip1 rationale above
YAML
  )" 'all quoted'

# A FLOW MAP annotation. The reconciliation counts key-shaped source occurrences,
# so the character before the key matters: `{` is a legitimate delimiter and must
# not make the file look uncheckable. Reported by CodeRabbit on #3222 and real —
# the first-entry form returned exit 2 on correct YAML.
expect 'a flow-map annotation as the first entry is checked, not rejected' 0 \
  "$(
    tree_with okflowfirst okflowfirst.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: okflowfirst
  annotations: {checkov.io/skip1: "CKV_K8S_1=okflowfirst the reason"}
YAML
  )" '1 annotation(s) checked'

expect 'a flow-map annotation after another entry is checked' 0 \
  "$(
    tree_with okflowsecond okflowsecond.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: okflowsecond
  annotations: {app: real,checkov.io/skip1: "CKV_K8S_1=okflowsecond the reason"}
YAML
  )" '1 annotation(s) checked'

# TWO annotations on ONE line. The reconciliation counts key occurrences, so counting
# matching *lines* here under-counts (1 line vs 2 parsed entries) and rejects valid
# YAML as uncheckable. Reported by CodeRabbit on #3222 after the flow-map fix.
expect 'two annotations in one flow map are both counted' 0 \
  "$(
    tree_with oktwoflow oktwoflow.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: oktwoflow
  annotations: {checkov.io/skip1: "CKV_K8S_1=oktwoflow first",checkov.io/skip2: "CKV_K8S_2=oktwoflow second"}
YAML
  )" '2 annotation(s) checked'

# Directive-shaped text inside a QUOTED scalar value. This is textually identical
# to a key, so no source-text scan can separate them — which is why the check asks
# yq which scalars are block content instead of counting occurrences. Reported by
# CodeRabbit on #3222; it exited 2 and blocked CI on this valid file.
expect 'directive-shaped text in a quoted scalar is not counted as a key' 0 \
  "$(
    tree_with okmention okmention.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: okmention
  annotations:
    app.example/note: "See checkov.io/skip1: in the security guide."
    checkov.io/skip1: "CKV_K8S_1=okmention valid reason"
YAML
  )" '1 annotation(s) checked'

# The mirror of the above: widening what may precede the key must NOT start
# counting keys that merely END with the annotation name.
expect 'look-alike keys are not counted as annotations' 0 \
  "$(
    tree_with oklookalike oklookalike.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: oklookalike
  annotations:
    xcheckov.io/skip1: not-ours-and-plain
    mycorp.checkov.io/skip1: also-not-ours
    checkov.io/skip1: "CKV_K8S_1=oklookalike the real one"
YAML
  )" '1 annotation(s) checked'

expect 'a tree with no annotations passes' 0 \
  "$(
    tree_with okempty okempty.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: okempty
YAML
  )" 'no annotations'

expect 'multi-document files are checked' 0 \
  "$(
    tree_with okmultidoc okmultidoc.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: okmultidoc-a
  annotations:
    checkov.io/skip1: "CKV_K8S_1=okmultidoc first"
---
apiVersion: v1
kind: Pod
metadata:
  name: okmultidoc-b
  annotations:
    checkov.io/skip2: "CKV_K8S_2=okmultidoc second"
YAML
  )" '2 annotation(s) checked'

# ---------------------------------------------------------------------------
# exit 1 — rejected
# ---------------------------------------------------------------------------
expect 'a plain (unquoted) value is rejected' 1 \
  "$(scalar_fixture badplain 'CKV_K8S_1=badplain the reason')" 'badplain.yaml'

# Defect 1: the KEY is quoted and the VALUE is not. Reading the value node's style
# catches this; matching source text after the digits does not.
expect 'a quoted key with a plain value is rejected' 1 \
  "$(
    tree_with badquotedkey badquotedkey.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: badquotedkey
  annotations:
    "checkov.io/skip1": CKV_K8S_1=badquotedkey the reason
YAML
  )" 'badquotedkey.yaml'

# Defect 2: the value sits on the FOLLOWING line — still a plain scalar, still a
# comment site.
expect 'a plain value on the following line is rejected' 1 \
  "$(
    tree_with badnextline badnextline.yaml <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: badnextline
  annotations:
    checkov.io/skip1:
      CKV_K8S_1=badnextline the reason
YAML
  )" 'badnextline.yaml'

expect 'a quoted value with no "=" is rejected' 1 \
  "$(scalar_fixture badnoeq '"CKV_K8S_1 badnoeq no separator"')" 'no "=" separator'

expect 'a quoted value with an empty reason is rejected' 1 \
  "$(scalar_fixture badempty '"CKV_K8S_1="')" 'the reason is empty'

expect 'a quoted value whose reason is only whitespace is rejected' 1 \
  "$(scalar_fixture badblank '"CKV_K8S_1=   "')" 'the reason is empty'

# ---------------------------------------------------------------------------
# exit 2 — the guard could not check
# ---------------------------------------------------------------------------
# Defect 4: an annotation inside a block scalar is opaque to the parser. Reporting a
# contented "0 checked" there is the exact failure this exit distinguishes.
expect 'an annotation hidden in a block scalar is reported as uncheckable' 2 \
  "$(
    tree_with badblockscalar badblockscalar.yaml <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
patches:
  - target:
      kind: Deployment
    patch: |
      metadata:
        annotations:
          checkov.io/skip1: "CKV_K8S_1=badblockscalar hidden reason"
YAML
  )" 'cannot be checked'

# A hidden directive written in FLOW form inside the block scalar. The detection
# above matches key-shaped text in block content, so the character that may precede
# a key matters here too — `{` is not whitespace. Without the widened class this
# patch slips through and the file reports a contented "0 checked".
expect 'a flow-form directive hidden in a block scalar is still caught' 2 \
  "$(
    tree_with badblockflow badblockflow.yaml <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
patches:
  - target:
      kind: Deployment
    patch: |
      metadata:
        annotations: {checkov.io/skip1: "CKV_K8S_1=badblockflow hidden"}
YAML
  )" 'cannot be checked'

expect 'a missing root directory is reported' 2 "$scratch/does-not-exist" 'not a directory'

# Fail-closed on tooling. An empty PATH removes yq and jq; the guard must say so
# rather than report a clean tree.
assertions=$((assertions + 1))
run_guard "$(scalar_fixture oktooling '"CKV_K8S_1=oktooling the reason"')" "$scratch/empty-path"
mkdir -p "$scratch/empty-path"
if [ "$GUARD_RC" -ne 2 ]; then
  printf 'FAIL missing tooling: expected exit 2, got %s\n%s\n' "$GUARD_RC" "$GUARD_OUT" >&2
  failures=$((failures + 1))
else
  printf 'ok   missing tooling is reported (exit 2)\n'
fi

# Fail-closed on the block-scalar scan itself. The guard asks jq to render the
# block content it just got from yq; if that render fails, the guard has NOT
# checked the file and must say so rather than fall through to a clean verdict.
#
# The stub fails ONLY the block-scan invocation (`-r` with `.[]`) and delegates
# every other call to the real jq, so the fixture still reaches that line with the
# earlier per-annotation checks working normally. Without that isolation a broken
# jq would abort earlier and this would prove nothing about the scan.
assertions=$((assertions + 1))
jq_stub_dir="$scratch/jq-stub-bin"
mkdir -p "$jq_stub_dir"
real_jq="$(command -v jq)"
jq_stub_log="$scratch/jq-stub.log"
: >"$jq_stub_log"
cat >"$jq_stub_dir/jq" <<STUB
#!/usr/bin/env bash
if [ "\$1" = "-r" ] && [ "\$2" = ".[]" ]; then
  echo fired >> "$jq_stub_log"
  echo 'stubbed jq failure' >&2
  exit 9
fi
exec "$real_jq" "\$@"
STUB
chmod +x "$jq_stub_dir/jq"
run_guard "$(scalar_fixture blockscanjq '"CKV_K8S_1=blockscanjq the reason"')" \
  "$jq_stub_dir:$PATH"
if [ "$GUARD_RC" -ne 2 ]; then
  printf 'FAIL block-scan jq failure: expected exit 2, got %s\n%s\n' "$GUARD_RC" "$GUARD_OUT" >&2
  failures=$((failures + 1))
elif ! printf '%s' "$GUARD_OUT" | grep -qF -- 'could not read block scalars'; then
  printf 'FAIL block-scan jq failure: exit 2 was right, but the report never named the cause\n%s\n' "$GUARD_OUT" >&2
  failures=$((failures + 1))
else
  printf 'ok   a failing block-scalar scan is reported, not passed over (exit 2)\n'
fi

# Non-vacuity control for the case above: the stub must actually have been reached.
# A stub that never fires would make that assertion pass for the wrong reason — the
# fixture is clean, so exit 2 has to come from the stub and nothing else.
assertions=$((assertions + 1))
if [ ! -s "$jq_stub_log" ]; then
  printf 'FAIL control: the jq stub never fired, so the block-scan assertion proves nothing\n' >&2
  failures=$((failures + 1))
else
  printf 'ok   control: the jq stub really was reached\n'
fi

# ---------------------------------------------------------------------------
# Meta-assertion: the suite must be able to FAIL. A fixture that is rejected by the
# guard is asserted to pass, and that assertion is required to break — this is what
# proves the needles and exit codes above are doing real work.
# ---------------------------------------------------------------------------
run_guard "$(scalar_fixture controlplain 'CKV_K8S_1=controlplain reason')"
if [ "$GUARD_RC" -eq 0 ]; then
  printf 'FAIL control: an unquoted fixture passed the guard, so these assertions prove nothing\n' >&2
  failures=$((failures + 1))
else
  printf 'ok   control: the unquoted fixture really is rejected (exit %s)\n' "$GUARD_RC"
fi
# And the needle check must reject a needle that belongs to a DIFFERENT fixture.
if printf '%s' "$GUARD_OUT" | grep -qF -- 'badquotedkey.yaml'; then
  printf 'FAIL control: output matched an unrelated fixture needle\n' >&2
  failures=$((failures + 1))
else
  printf 'ok   control: an unrelated needle does not match\n'
fi

if [ "$failures" -ne 0 ]; then
  printf '\n%d of %d assertion(s) failed\n' "$failures" "$assertions" >&2
  exit 1
fi
printf '\nall %d assertion(s) passed\n' "$assertions"
